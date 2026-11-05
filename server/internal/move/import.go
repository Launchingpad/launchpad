package move

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/dbq"
	"devops/internal/storage"
)

// ImportOptions are the inputs of Import.
type ImportOptions struct {
	Pool *pgxpool.Pool
	// Keys are the IMPORTING server's keyrings: every sealed column is sealed
	// again with them.
	Keys Keys
	// Store receives the attachment objects. It may be nil for a bundle with none.
	Store      ObjectStore
	Passphrase string
	Log        *slog.Logger
	// Authorize, when set, runs inside the import transaction right after the
	// setup lock and the empty-install check, before any row is loaded. The
	// API consumes the setup link here, so two restores cannot both use it. An
	// error rolls the import back and is returned as it is.
	Authorize func(ctx context.Context, tx pgx.Tx) error
	// Finish, when set, runs inside the import transaction after every row is
	// loaded (and the setup links are closed), before any object is written
	// and before the commit. The API mints the session and the admin token
	// here, so an import without a way in never commits. An error rolls the
	// import back and is returned as it is.
	Finish func(ctx context.Context, tx pgx.Tx, m *Manifest) error
}

// importCtx is what a table's OnImport hook may use.
type importCtx struct {
	keys Keys
}

type deferredValue struct {
	table, column, id, value string
}

// Import reads a bundle and loads it into an empty install, in ONE
// transaction under the setup advisory lock. Any failure rolls back the
// database and deletes the objects this call wrote; the install stays in
// setup state.
//
// Orphan objects: the objects are written BEFORE the commit (a committed
// install never points at a missing file). If the process dies between the
// first object write and the commit, the transaction rolls back by itself but
// the objects stay in the bucket. They are named by the exported rows (org
// id first in the key), nothing references them, and a retry refuses to run
// over them (ObjectExistsError): the operator empties the bucket or deletes
// the listed keys. An error return cleans up on its own.
func Import(ctx context.Context, r io.Reader, o ImportOptions) (m *Manifest, err error) {
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ident, err := age.NewScryptIdentity(NormalizePassphrase(o.Passphrase))
	if err != nil {
		return nil, ErrBadPassphrase
	}
	dr, err := age.Decrypt(r, ident)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return nil, ErrBadPassphrase
		}
		return nil, fmt.Errorf("%w", ErrBundleCorrupt)
	}
	head := make([]byte, len(magic))
	if _, err := io.ReadFull(dr, head); err != nil || string(head) != magic {
		return nil, fmt.Errorf("%w", ErrBundleCorrupt)
	}
	fr := &frameReader{r: dr}
	m = &Manifest{}
	if err := fr.expectJSON(frameManifest, m); err != nil {
		return nil, err
	}
	if m.Format != formatVersion {
		return nil, fmt.Errorf("%w: unknown bundle format %d", ErrBundleCorrupt, m.Format)
	}
	if m.ObjectCount > 0 && o.Store == nil {
		return nil, ErrNoStore
	}
	for _, s := range m.Sealed {
		if s.Count > 0 && !o.Keys.has(s.Ring) {
			return nil, &KeyMissingError{Keyring: keyName(s.Ring)}
		}
	}

	var created []string // keys of objects this call wrote
	tx, err := o.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("move: begin: %w", err)
	}
	defer func() {
		if err == nil {
			return
		}
		bg := context.WithoutCancel(ctx)
		_ = tx.Rollback(bg)
		cctx, cancel := context.WithTimeout(bg, 2*time.Minute)
		defer cancel()
		failed := 0
		for _, k := range created {
			if derr := o.Store.Delete(cctx, k); derr != nil {
				failed++
			}
		}
		if failed > 0 {
			// Counts only: a key names an org and a file.
			log.Error("move: import failed and some written objects could not be deleted; remove the stray objects by hand", "strayObjects", failed)
		}
		m = nil
	}()

	if _, err = tx.Exec(ctx, `SET LOCAL statement_timeout = 0`); err != nil {
		return nil, fmt.Errorf("move: set timeout: %w", err)
	}
	q := dbq.New(tx)
	if err = q.LockSetup(ctx); err != nil {
		return nil, fmt.Errorf("move: setup lock: %w", err)
	}
	var occupied bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users) OR EXISTS (SELECT 1 FROM orgs)`).Scan(&occupied); err != nil {
		return nil, fmt.Errorf("move: check empty: %w", err)
	}
	if occupied {
		return nil, ErrNotEmpty
	}
	if o.Authorize != nil {
		if err = o.Authorize(ctx, tx); err != nil {
			return nil, err
		}
	}
	var version uint
	if version, err = migrationVersion(ctx, tx); err != nil {
		return nil, err
	}
	if m.MigrationVersion != version {
		return nil, &VersionError{Bundle: m.MigrationVersion, Server: version}
	}
	tables, err := orderedTables(ctx, tx)
	if err != nil {
		return nil, err
	}
	if len(tables) != len(m.Tables) {
		return nil, fmt.Errorf("%w: table list", ErrBundleCorrupt)
	}
	for i, t := range tables {
		if m.Tables[i].Name != t.Name {
			return nil, fmt.Errorf("%w: table order", ErrBundleCorrupt)
		}
	}

	ic := &importCtx{keys: o.Keys}
	var deferred []deferredValue
	var total int64
	for i, t := range tables {
		var hdr tableHeader
		if err = fr.expectJSON(frameTable, &hdr); err != nil {
			return nil, err
		}
		cols, cerr := tableColumns(ctx, tx, t.Name)
		if cerr != nil {
			return nil, cerr
		}
		if hdr.Name != t.Name || !equalStrings(hdr.Columns, cols) {
			return nil, fmt.Errorf("%w: columns of %s", ErrBundleCorrupt, t.Name)
		}
		if t.ClearFirst {
			if _, err = tx.Exec(ctx, `DELETE FROM `+quoteIdent(t.Name)); err != nil {
				return nil, fmt.Errorf("move: clear %s: %w", t.Name, dbErr(err))
			}
		}
		tr := &tableReader{fr: fr, t: t, ic: ic, cols: colIndex(cols), table: t.Name}
		sql := `COPY ` + quoteIdent(t.Name) + ` (` + joinIdents(cols) + `) FROM STDIN`
		tag, cerr := tx.Conn().PgConn().CopyFrom(ctx, tr, sql)
		if tr.err != nil {
			return nil, tr.err
		}
		if cerr != nil {
			err = fmt.Errorf("move: copy in %s: %w", t.Name, dbErr(cerr))
			return nil, err
		}
		if !tr.done || tr.end.Rows != tr.rows || tag.RowsAffected() != tr.rows || tr.rows != m.Tables[i].Rows {
			return nil, fmt.Errorf("%w: row count of %s", ErrBundleCorrupt, t.Name)
		}
		total += tr.rows
		deferred = append(deferred, tr.deferred...)
		log.Info("move: imported table", "table", t.Name, "rows", tr.rows)
	}
	if total != m.TotalRows() {
		return nil, fmt.Errorf("%w: row total", ErrBundleCorrupt)
	}
	for _, d := range deferred {
		if _, err = tx.Exec(ctx, `UPDATE `+quoteIdent(d.table)+` SET `+quoteIdent(d.column)+` = $1::uuid WHERE id = $2::uuid`, d.value, d.id); err != nil {
			return nil, fmt.Errorf("move: link %s.%s: %w", d.table, d.column, dbErr(err))
		}
	}
	for _, s := range Sequences {
		if _, err = tx.Exec(ctx, fmt.Sprintf(`SELECT setval('%s', COALESCE((SELECT max(%s) FROM %s), 1), (SELECT count(*) > 0 FROM %s))`,
			s.Name, quoteIdent(s.Column), quoteIdent(s.Table), quoteIdent(s.Table))); err != nil {
			return nil, fmt.Errorf("move: sequence %s: %w", s.Name, dbErr(err))
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM setup_tokens`); err != nil {
		return nil, fmt.Errorf("move: close setup links: %w", dbErr(err))
	}
	if o.Finish != nil {
		if err = o.Finish(ctx, tx, m); err != nil {
			return nil, err
		}
	}

	if created, err = importObjects(ctx, tx, fr, o.Store, m, created); err != nil {
		return nil, err
	}
	if err = fr.readEnd(dr); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("move: commit: %w", dbErr(err))
	}
	log.Info("move: import done", "tables", len(m.Tables), "rows", m.TotalRows(), "objects", m.ObjectCount)
	return m, nil
}

// readEnd reads the closing frame and requires the stream to end after it.
func (f *frameReader) readEnd(dr io.Reader) error {
	p, err := f.expect(frameEnd)
	if err != nil {
		return err
	}
	if len(p) != 0 {
		return fmt.Errorf("%w: end frame", ErrBundleCorrupt)
	}
	var one [1]byte
	if n, err := dr.Read(one[:]); n != 0 || err != io.EOF {
		return fmt.Errorf("%w: data after the end", ErrBundleCorrupt)
	}
	return nil
}

// tableReader gives COPY FROM STDIN the rows of one table: it pulls data
// frames, opens each row, seals the plaintext columns again, applies the
// table's import rules and hands on the COPY text.
type tableReader struct {
	fr       *frameReader
	t        Table
	ic       *importCtx
	cols     map[string]int
	table    string
	buf      bytes.Buffer
	rows     int64
	done     bool
	end      tableEnd
	err      error
	deferred []deferredValue
}

func (t *tableReader) Read(p []byte) (int, error) {
	for t.buf.Len() == 0 {
		if t.done {
			return 0, io.EOF
		}
		typ, payload, err := t.fr.read()
		if err != nil {
			t.err = err
			return 0, err
		}
		switch typ {
		case frameTableEnd:
			if jerr := unmarshal(payload, &t.end); jerr != nil {
				t.err = jerr
				return 0, jerr
			}
			t.done = true
		case frameData:
			if err := t.rowsOf(payload); err != nil {
				t.err = err
				return 0, err
			}
		default:
			t.err = fmt.Errorf("%w: unexpected frame in %s", ErrBundleCorrupt, t.table)
			return 0, t.err
		}
	}
	return t.buf.Read(p)
}

func (t *tableReader) rowsOf(payload []byte) error {
	if len(payload) == 0 || payload[len(payload)-1] != '\n' {
		return fmt.Errorf("%w: data frame of %s", ErrBundleCorrupt, t.table)
	}
	for len(payload) > 0 {
		i := bytes.IndexByte(payload, '\n')
		line := payload[:i]
		payload = payload[i+1:]
		t.rows++
		if len(t.t.Sealed) == 0 && len(t.t.Deferred) == 0 && t.t.OnImport == nil {
			t.buf.Write(line)
			t.buf.WriteByte('\n')
			continue
		}
		r, err := newRow(t.cols, line)
		if err != nil {
			return err
		}
		for _, sc := range t.t.Sealed {
			if r.isNull(sc.Column) {
				continue
			}
			pt, err := r.bytea(sc.Column)
			if err != nil {
				return fmt.Errorf("%w: sealed column %s.%s", ErrBundleCorrupt, t.table, sc.Column)
			}
			blob, err := t.ic.keys.seal(sc, r, pt)
			if err != nil {
				return err
			}
			r.setBytea(sc.Column, blob)
		}
		for _, d := range t.t.Deferred {
			if r.isNull(d.Column) {
				continue
			}
			t.deferred = append(t.deferred, deferredValue{table: t.table, column: d.Column, id: r.text("id"), value: r.text(d.Column)})
			r.setNull(d.Column)
		}
		if t.t.OnImport != nil {
			if err := t.t.OnImport(t.ic, r); err != nil {
				return err
			}
		}
		t.buf.Write(r.bytes())
		t.buf.WriteByte('\n')
	}
	return nil
}

// importObjects writes the bundle's objects to the store. Each key must be
// named by an imported row and must not exist yet. Every row's object must
// arrive when the bundle says attachments are included.
func importObjects(ctx context.Context, tx pgx.Tx, fr *frameReader, store ObjectStore, m *Manifest, created []string) ([]string, error) {
	want := map[string]bool{}
	for _, src := range ObjectSources {
		rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE %s`, quoteIdent(src.KeyColumn), quoteIdent(src.Table), src.Where))
		if err != nil {
			return created, fmt.Errorf("move: list objects of %s: %w", src.Table, dbErr(err))
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return created, err
			}
			want[k] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return created, err
		}
	}
	if !m.AttachmentsIncluded && m.ObjectCount != 0 {
		return created, fmt.Errorf("%w: object count", ErrBundleCorrupt)
	}
	got := map[string]bool{}
	var bytesIn int64
	for n := int64(0); n < m.ObjectCount; n++ {
		var h objectHeader
		if err := fr.expectJSON(frameObject, &h); err != nil {
			return created, err
		}
		if !want[h.Key] || got[h.Key] || h.Size < 0 {
			return created, fmt.Errorf("%w: object %d", ErrBundleCorrupt, n+1)
		}
		got[h.Key] = true
		if _, err := store.Head(ctx, h.Key); err == nil {
			return created, &ObjectExistsError{Key: h.Key}
		} else if !errors.Is(err, storage.ErrNotFound) {
			return created, fmt.Errorf("move: check object: %w", err)
		}
		created = append(created, h.Key)
		body := &objectReader{fr: fr, remaining: h.Size, hash: sha256.New()}
		var src io.Reader = body
		if h.Size == 0 {
			// Go sends a non-nil empty body of unknown length as chunked, and the store wants a length.
			src = http.NoBody
		}
		if err := store.Put(ctx, h.Key, h.ContentType, h.Size, src); err != nil {
			if body.err != nil {
				return created, body.err
			}
			return created, fmt.Errorf("move: write object: %w", err)
		}
		if err := body.finish(); err != nil {
			return created, err
		}
		bytesIn += h.Size
	}
	if m.AttachmentsIncluded {
		for k := range want {
			if !got[k] {
				return created, fmt.Errorf("%w: a file is missing from the bundle", ErrBundleCorrupt)
			}
		}
	}
	if bytesIn != m.Bytes {
		return created, fmt.Errorf("%w: object bytes", ErrBundleCorrupt)
	}
	return created, nil
}

// objectReader streams the data frames of one object and checks the sha256
// frame that follows them.
type objectReader struct {
	fr        *frameReader
	remaining int64
	hash      interface {
		io.Writer
		Sum([]byte) []byte
	}
	buf  []byte
	done bool
	sum  []byte
	err  error
}

func (o *objectReader) Read(p []byte) (int, error) {
	for len(o.buf) == 0 {
		if o.done {
			return 0, io.EOF
		}
		typ, payload, err := o.fr.read()
		if err != nil {
			o.err = err
			return 0, err
		}
		switch typ {
		case frameObjectData:
			if int64(len(payload)) > o.remaining {
				o.err = fmt.Errorf("%w: object longer than its header", ErrBundleCorrupt)
				return 0, o.err
			}
			o.remaining -= int64(len(payload))
			o.buf = payload
		case frameObjectEnd:
			o.done, o.sum = true, payload
			if o.remaining != 0 {
				o.err = fmt.Errorf("%w: object shorter than its header", ErrBundleCorrupt)
				return 0, o.err
			}
		default:
			o.err = fmt.Errorf("%w: unexpected frame in an object", ErrBundleCorrupt)
			return 0, o.err
		}
	}
	n := copy(p, o.buf)
	o.hash.Write(o.buf[:n])
	o.buf = o.buf[n:]
	return n, nil
}

// finish reads up to the sha256 frame and compares it.
func (o *objectReader) finish() error {
	var scratch [1]byte
	for !o.done {
		if _, err := o.Read(scratch[:]); err != nil && err != io.EOF {
			return err
		}
		if len(o.buf) > 0 { // data beyond what the store read
			return fmt.Errorf("%w: object longer than its header", ErrBundleCorrupt)
		}
	}
	if !bytes.Equal(o.sum, o.hash.Sum(nil)) {
		return ErrChecksum
	}
	return nil
}
