package move

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/platform"
	"devops/internal/storage"
)

// ObjectStore is the part of the attachment store the move needs. *storage.S3
// is the real one.
type ObjectStore interface {
	Head(ctx context.Context, key string) (storage.ObjectInfo, error)
	Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error)
	Put(ctx context.Context, key, contentType string, size int64, body io.Reader) error
	Delete(ctx context.Context, key string) error
}

// ExportOptions are the inputs of Export.
type ExportOptions struct {
	Pool *pgxpool.Pool
	Keys Keys
	// Store reads the attachment objects. It may be nil only when
	// IncludeAttachments is false.
	Store ObjectStore
	// Passphrase encrypts the bundle; take it from GeneratePassphrase.
	Passphrase string
	// ExportedBy is the admin who asked; the zero value means a host operator.
	ExportedBy uuid.UUID
	AppVersion string
	// IncludeAttachments adds the attachment (and avatar) objects. Without it
	// the rows still travel, and their files do not.
	IncludeAttachments bool
	Log                *slog.Logger
	// ScryptWorkFactor is the log2 of the scrypt cost of the passphrase; 0
	// takes age's default (18: about 256 MiB of memory and a second of CPU,
	// for the export and again for the import). Tests lower it so a memory
	// measurement sees the stream and not the key derivation.
	ScryptWorkFactor int
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

type objectRef struct {
	key, contentType string
	size             int64
}

// Export writes one encrypted bundle of the whole install to w. It reads the
// database in ONE repeatable-read, read-only transaction, so the tables are
// one consistent snapshot. Nothing is written to disk by this function.
func Export(ctx context.Context, w io.Writer, o ExportOptions) (*Manifest, error) {
	if len(o.Passphrase) < minPassphraseLen {
		return nil, ErrWeakPassphrase
	}
	if o.IncludeAttachments && o.Store == nil {
		return nil, ErrNoStore
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}

	tx, err := o.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("move: begin snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = 0`); err != nil {
		return nil, fmt.Errorf("move: set timeout: %w", err)
	}

	version, err := migrationVersion(ctx, tx)
	if err != nil {
		return nil, err
	}
	tables, err := orderedTables(ctx, tx)
	if err != nil {
		return nil, err
	}

	m := &Manifest{Format: formatVersion, AppVersion: o.AppVersion, MigrationVersion: version,
		ExportedAt: now().UTC(), AttachmentsIncluded: o.IncludeAttachments}
	if o.ExportedBy != uuid.Nil {
		id := o.ExportedBy
		m.ExportedBy = &id
	}
	colsOf := map[string][]string{}
	for _, t := range tables {
		cols, err := tableColumns(ctx, tx, t.Name)
		if err != nil {
			return nil, err
		}
		colsOf[t.Name] = cols
		var n int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+quoteIdent(t.Name)+whereClause(t.Where)).Scan(&n); err != nil {
			return nil, fmt.Errorf("move: count %s: %w", t.Name, err)
		}
		m.Tables = append(m.Tables, TableCount{Name: t.Name, Rows: n})
		for _, s := range t.Sealed {
			var c int64
			q := `SELECT count(` + quoteIdent(s.Column) + `) FROM ` + quoteIdent(t.Name) + whereClause(t.Where)
			if err := tx.QueryRow(ctx, q).Scan(&c); err != nil {
				return nil, fmt.Errorf("move: count %s.%s: %w", t.Name, s.Column, err)
			}
			if c == 0 {
				continue
			}
			if !o.Keys.has(s.Ring) {
				return nil, &KeyMissingError{Keyring: keyName(s.Ring)}
			}
			m.Sealed = append(m.Sealed, SealedCount{Table: t.Name, Column: s.Column, Ring: s.Ring, Count: c})
		}
	}

	var objects []objectRef
	if o.IncludeAttachments {
		if objects, err = listObjects(ctx, tx, o.Store); err != nil {
			return nil, err
		}
		for _, ob := range objects {
			m.Bytes += ob.size
		}
		m.ObjectCount = int64(len(objects))
	}

	rec, err := age.NewScryptRecipient(o.Passphrase)
	if err != nil {
		return nil, fmt.Errorf("move: passphrase: %w", err)
	}
	if o.ScryptWorkFactor > 0 {
		rec.SetWorkFactor(o.ScryptWorkFactor)
	}
	aw, err := age.Encrypt(w, rec)
	if err != nil {
		return nil, fmt.Errorf("move: start bundle: %w", err)
	}
	fw := &frameWriter{w: aw}
	if _, err := io.WriteString(aw, magic); err != nil {
		return nil, err
	}
	if err := fw.writeJSON(frameManifest, m); err != nil {
		return nil, err
	}

	for i, t := range tables {
		cols := colsOf[t.Name]
		if err := fw.writeJSON(frameTable, tableHeader{Name: t.Name, Columns: cols}); err != nil {
			return nil, err
		}
		sink := &exportSink{fw: fw, t: t, cols: colIndex(cols), keys: o.Keys}
		sql := `COPY (SELECT ` + joinIdents(cols) + ` FROM ` + quoteIdent(t.Name) + whereClause(t.Where) + `) TO STDOUT`
		if _, err := tx.Conn().PgConn().CopyTo(ctx, sink, sql); err != nil {
			if sink.err != nil {
				return nil, sink.err
			}
			return nil, fmt.Errorf("move: copy out %s: %w", t.Name, dbErr(err))
		}
		if err := sink.finish(); err != nil {
			return nil, err
		}
		if sink.rows != m.Tables[i].Rows {
			return nil, fmt.Errorf("move: table %s changed during the snapshot (%d rows, expected %d)", t.Name, sink.rows, m.Tables[i].Rows)
		}
		if err := fw.writeJSON(frameTableEnd, tableEnd{Rows: sink.rows}); err != nil {
			return nil, err
		}
		log.Info("move: exported table", "table", t.Name, "rows", sink.rows)
	}
	// The snapshot is not needed for the objects: they come from the rows read above.
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return nil, fmt.Errorf("move: end snapshot: %w", err)
	}

	for _, ob := range objects {
		if err := writeObject(ctx, fw, o.Store, ob); err != nil {
			return nil, err
		}
	}
	if err := fw.write(frameEnd, nil); err != nil {
		return nil, err
	}
	if err := aw.Close(); err != nil {
		return nil, fmt.Errorf("move: finish bundle: %w", err)
	}
	log.Info("move: export done", "tables", len(m.Tables), "rows", m.TotalRows(), "objects", m.ObjectCount, "bytes", m.Bytes)
	return m, nil
}

func whereClause(w string) string {
	if w == "" {
		return ""
	}
	return " WHERE " + w
}

func joinIdents(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = quoteIdent(c)
	}
	return strings.Join(q, ", ")
}

// listObjects collects the objects the snapshot rows name and HEADs each one
// for its size.
func listObjects(ctx context.Context, tx pgx.Tx, store ObjectStore) ([]objectRef, error) {
	seen := map[string]bool{}
	var out []objectRef
	for _, src := range ObjectSources {
		q := fmt.Sprintf(`SELECT %s, %s FROM %s WHERE %s ORDER BY %s`,
			quoteIdent(src.KeyColumn), quoteIdent(src.TypeColumn), quoteIdent(src.Table), src.Where, quoteIdent(src.KeyColumn))
		rows, err := tx.Query(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("move: list objects of %s: %w", src.Table, err)
		}
		var refs []objectRef
		for rows.Next() {
			var key string
			var ct *string
			if err := rows.Scan(&key, &ct); err != nil {
				rows.Close()
				return nil, err
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			ob := objectRef{key: key, contentType: "application/octet-stream"}
			if ct != nil && *ct != "" {
				ob.contentType = *ct
			}
			refs = append(refs, ob)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		out = append(out, refs...)
	}
	for i := range out {
		info, err := store.Head(ctx, out[i].key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil, &ObjectMissingError{Key: out[i].key}
		}
		if err != nil {
			return nil, fmt.Errorf("move: head object: %w", err)
		}
		out[i].size = info.Size
	}
	return out, nil
}

// writeObject streams one object into the bundle: header, data frames, and
// the sha256 of the bytes it read.
func writeObject(ctx context.Context, fw *frameWriter, store ObjectStore, ob objectRef) error {
	rc, info, err := store.Get(ctx, ob.key)
	if errors.Is(err, storage.ErrNotFound) {
		return &ObjectMissingError{Key: ob.key}
	}
	if err != nil {
		return fmt.Errorf("move: read object: %w", err)
	}
	defer func() { _ = rc.Close() }()
	if info.Size < 0 || info.Size != ob.size {
		return fmt.Errorf("move: an object changed size during the export")
	}
	if err := fw.writeJSON(frameObject, objectHeader{Key: ob.key, ContentType: ob.contentType, Size: info.Size}); err != nil {
		return err
	}
	h := sha256.New()
	buf := make([]byte, dataChunk)
	var total int64
	for {
		n, rerr := rc.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			total += int64(n)
			if err := fw.write(frameObjectData, buf[:n]); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("move: read object: %w", rerr)
		}
	}
	if total != info.Size {
		return fmt.Errorf("move: an object was cut short while reading it")
	}
	return fw.write(frameObjectEnd, h.Sum(nil))
}

// exportSink receives the COPY text of one table in arbitrary pieces, opens
// the sealed columns row by row and writes whole rows as data frames.
type exportSink struct {
	fw      *frameWriter
	t       Table
	cols    map[string]int
	keys    Keys
	pending bytes.Buffer
	out     bytes.Buffer
	rows    int64
	err     error
}

func (s *exportSink) Write(p []byte) (int, error) {
	s.pending.Write(p)
	for {
		b := s.pending.Bytes()
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			break
		}
		line := s.pending.Next(i + 1)[:i]
		if err := s.line(line); err != nil {
			s.err = err
			return 0, err
		}
	}
	return len(p), nil
}

func (s *exportSink) line(line []byte) error {
	s.rows++
	if len(s.t.Sealed) == 0 {
		s.out.Write(line)
		s.out.WriteByte('\n')
	} else {
		r, err := newRow(s.cols, line)
		if err != nil {
			return err
		}
		for _, sc := range s.t.Sealed {
			if r.isNull(sc.Column) {
				continue
			}
			blob, err := r.bytea(sc.Column)
			if err != nil {
				return fmt.Errorf("%w: sealed column %s.%s", ErrBundleCorrupt, s.t.Name, sc.Column)
			}
			pt, err := s.keys.open(sc, r, blob)
			if err != nil {
				if errors.Is(err, platform.ErrUnreadable) {
					return &UnreadableSecretError{Table: s.t.Name, Column: sc.Column}
				}
				return err
			}
			r.setBytea(sc.Column, pt)
		}
		s.out.Write(r.bytes())
		s.out.WriteByte('\n')
	}
	if s.out.Len() >= dataChunk {
		return s.flush()
	}
	return nil
}

func (s *exportSink) flush() error {
	if s.out.Len() == 0 {
		return nil
	}
	err := s.fw.write(frameData, s.out.Bytes())
	s.out.Reset()
	return err
}

func (s *exportSink) finish() error {
	if s.pending.Len() != 0 {
		return fmt.Errorf("move: a row of %s was cut short", s.t.Name)
	}
	return s.flush()
}
