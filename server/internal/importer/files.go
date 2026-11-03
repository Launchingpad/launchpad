package importer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/dbq"
	"devops/internal/storage"
	"devops/internal/tracker"
)

// Files (T4b). Every uploads.linear.app file the export
// downloaded becomes ONE object in the attachment store and ONE `file`
// attachment row, and every text that refers to it is rewritten to that
// row's API download URL (decisions-T3b #12, decisions-T4b).
//
//   - A Linear attachment whose URL is an upload IS the file row (its
//     mapped id, kind file).
//   - A file only a text refers to gets a new row on the issue that refers
//     to it first (its description, then a comment on it).
//   - Ids are UUIDv7 at the owner's createdAt with random bits derived from
//     the org and the Linear source, so the object key is the same on every
//     run, even after one that rolled back: a re-run finds the object and
//     uploads nothing.

// ObjectStore is what the importer needs from the attachment store.
// *storage.S3 implements it.
type ObjectStore interface {
	Head(ctx context.Context, key string) (storage.ObjectInfo, error)
	Put(ctx context.Context, key, contentType string, size int64, body io.Reader) error
}

// FilePlan is one file the import stores, worked out by BuildPlan.
type FilePlan struct {
	URL          string
	File         File
	ContentType  string // the stored type (on the upload allowlist)
	Filename     string // the display filename
	IssueID      string // the Linear issue whose attachment row it is
	AttachmentID string // the Linear attachment that is this row ("" when only a text refers to it)
	At           time.Time
	Creator      string // Linear user id ("" for none)
}

// extTypes maps a filename extension to a stored type when Linear's own
// Content-Type is not on the allowlist.
var extTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".mp4": "video/mp4", ".pdf": "application/pdf", ".txt": "text/plain", ".csv": "text/plain",
	".md": "text/plain", ".log": "text/plain", ".json": "text/plain",
}

// fileContentType is the type a file is stored with: Linear's type when it
// is on the upload allowlist; any text/* as text/plain (a text/plain
// download is always served as an attachment, never rendered, so CSV,
// markdown or HTML text is safe). Only when Linear gave no real type (empty
// or application/octet-stream) does the extension decide, so a declared
// type off the allowlist (image/svg+xml named .png) is refused, never
// relabelled. ok is false for anything else.
func fileContentType(f File) (string, bool) {
	if ct, ok := tracker.AllowedContentType(f.ContentType); ok {
		return ct, true
	}
	if mt, _, err := mime.ParseMediaType(f.ContentType); err == nil && strings.HasPrefix(strings.ToLower(mt), "text/") {
		return "text/plain", true
	}
	if mt, _, _ := mime.ParseMediaType(f.ContentType); f.ContentType != "" && !strings.EqualFold(mt, "application/octet-stream") {
		return "", false
	}
	ct, ok := extTypes[strings.ToLower(path.Ext(f.Filename))]
	return ct, ok
}

// seededV7 is a UUIDv7 at t whose random bits come from sha256(seed): the
// same seed and time give the same id on every run.
func seededV7(t time.Time, seed string) (uuid.UUID, error) {
	sum := sha256.Sum256([]byte(seed))
	return newV7At(t, strings.NewReader(string(sum[:])))
}

// fileResolver rewrites uploads.linear.app URLs to download URLs.
type fileResolver struct {
	byURL   map[string]string // Linear URL -> download URL
	pending map[string]bool   // URLs left as they are
}

func newFileResolver() *fileResolver {
	return &fileResolver{byURL: map[string]string{}, pending: map[string]bool{}}
}

// rewrite replaces every upload URL in text whose file is stored. A URL
// with no stored file stays and is counted as pending.
func (r *fileResolver) rewrite(_ context.Context, text string, _ Owner) string {
	out, _ := RewriteUploads(text, func(u string) (string, bool) {
		to, ok := r.byURL[u]
		if !ok {
			r.pending[u] = true
		}
		return to, ok
	})
	return out
}

// Owner is the row whose text refers to a file.
type Owner struct {
	Entity  string
	ID      uuid.UUID
	IssueID *uuid.UUID
}

// storedFile is a file's row id and object key.
type storedFile struct {
	id  uuid.UUID
	key string
}

// fileSource is the mapping a planned file's row is recorded under.
func fileSource(fp FilePlan) (string, string) {
	if fp.AttachmentID != "" {
		return EntAttachment, fp.AttachmentID
	}
	return EntFile, fp.URL
}

// uploads gives every planned file its id and key, uploads the objects the
// store does not hold yet (none in a dry run), and fills the resolver.
func (r *run) uploads() error {
	if len(r.p.Files) == 0 {
		return nil
	}
	if r.store == nil {
		return errors.New("the export has files but no object store is configured (set S3_*, or pass --skip-files)")
	}
	// The object key of every file row an earlier run made, by file URL:
	// a row that is kept keeps its object, even when its owner (and so the
	// year in a newly built key) changed.
	objects, err := r.q.ImportListFileObjects(r.ctx, r.org)
	if err != nil {
		return fmt.Errorf("list file objects: %w", err)
	}
	keyOf := map[string]string{}
	for _, o := range objects {
		if o.ObjectKey != nil {
			keyOf[o.SourceID] = *o.ObjectKey
		}
	}
	r.stored = make([]storedFile, len(r.p.Files))
	var sent int64
	// The files phase counts bytes (decisions-T17 #40): done of the planned
	// size, a file counted once the store holds it (or, in a dry run, would).
	var planned, handled int64
	for _, fp := range r.p.Files {
		planned += fp.File.SizeBytes
	}
	r.report("files", 0, planned)
	progress := func(i int) {
		r.report("files", handled, planned)
		// A line every 25 files and at the end, so a stuck upload is seen.
		// Counts only: no URL, no key, no file name.
		if (i+1)%25 == 0 || i+1 == len(r.p.Files) {
			r.log.Info("import files", "done", i+1, "of", len(r.p.Files), "uploaded", r.rep.Uploads.Uploaded,
				"present", r.rep.Uploads.Present, "bytes_sent", sent)
		}
	}
	for i, fp := range r.p.Files {
		if err := r.ctx.Err(); err != nil {
			return fmt.Errorf("import files: %w", err)
		}
		handled += fp.File.SizeBytes
		ent, src := fileSource(fp)
		id, ok := r.fileTarget(fp)
		if !ok {
			var err error
			if id, err = seededV7(fp.At, "devops-import:"+r.org.String()+":"+ent+":"+src); err != nil {
				return err
			}
		}
		key := tracker.AttachmentObjectKey(r.org, fp.At.UTC().Year(), id, fp.Filename)
		if old, ok := keyOf[fp.URL]; ok {
			if fid, _ := r.target(EntFile, fp.URL); fid == id {
				key = old
			}
		}
		r.stored[i] = storedFile{id: id, key: key}
		r.rep.Uploads.Keys = append(r.rep.Uploads.Keys, key)
		r.files.byURL[fp.URL] = tracker.AttachmentDownloadURL(r.publicBase, r.orgSlug, id)

		info, err := r.store.Head(r.ctx, key)
		switch {
		case err == nil && info.Size == fp.File.SizeBytes:
			r.rep.Uploads.Present++
			progress(i)
			continue
		case err != nil && !errors.Is(err, storage.ErrNotFound):
			return fmt.Errorf("head object of file %d: %w", i, err)
		}
		if r.dryRun {
			r.rep.Uploads.Planned++
			progress(i)
			continue
		}
		if fp.File.SizeBytes >= 10<<20 {
			r.log.Info("import files: uploading a large file", "file", i+1, "of", len(r.p.Files), "bytes", fp.File.SizeBytes)
		}
		if err := r.putFile(key, fp); err != nil {
			return err
		}
		r.rep.Uploads.Uploaded++
		sent += fp.File.SizeBytes
		progress(i)
	}
	return nil
}

func (r *run) putFile(key string, fp FilePlan) error {
	if r.dir == nil {
		return errors.New("no export directory to read files from")
	}
	f, err := r.dir.Open(fp.File.Path)
	if err != nil {
		return fmt.Errorf("open %s: %w", fp.File.Path, err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", fp.File.Path, err)
	}
	if st.Size() != fp.File.SizeBytes {
		return fmt.Errorf("%s is %d bytes, files.jsonl says %d", fp.File.Path, st.Size(), fp.File.SizeBytes)
	}
	if err := r.store.Put(r.ctx, key, fp.ContentType, st.Size(), f); err != nil {
		return fmt.Errorf("upload %s: %w", fp.File.Path, err)
	}
	return nil
}

// fileTarget is the row an earlier run made for planned file fp: its own
// mapping, else the file URL's. The second matters when the owner changed
// between exports (a Linear attachment was added for a URL a text alone
// referred to before): the row, its id and its object are kept, instead of
// a second row and a second upload under the attachment's seed.
func (r *run) fileTarget(fp FilePlan) (uuid.UUID, bool) {
	ent, src := fileSource(fp)
	if id, ok := r.target(ent, src); ok {
		return id, true
	}
	if ent != EntFile {
		return r.target(EntFile, fp.URL)
	}
	return uuid.Nil, false
}

// fileRow writes the `file` attachment row of planned file i, under its
// own mapping (the Linear attachment's, or the file URL's) and the file
// URL's mapping, which verify counts.
func (r *run) fileRow(i int, title, subtitle string, meta []byte, updated time.Time) error {
	fp, sf := r.p.Files[i], r.stored[i]
	issue, ok := r.target(EntIssue, fp.IssueID)
	if !ok {
		return fmt.Errorf("file %d: its issue was not written", i)
	}
	ent, src := fileSource(fp)
	_, existed := r.fileTarget(fp) // the row was made by an earlier run
	if strings.TrimSpace(title) == "" {
		title = fp.Filename
	}
	if len(meta) == 0 {
		meta = []byte(`{}`)
	}
	size, filename, ct, key := fp.File.SizeBytes, fp.Filename, fp.ContentType, sf.key
	_, err := r.q.ImportUpsertFileAttachment(r.ctx, dbq.ImportUpsertFileAttachmentParams{
		ID: sf.id, OrgID: r.org, IssueID: issue, Title: clip(title, 255, fp.Filename), Subtitle: subtitle,
		ObjectKey: &key, Filename: &filename, ContentType: &ct, SizeBytes: &size, Metadata: meta,
		CreatorID: r.user(fp.Creator), CreatedAt: fp.At, UpdatedAt: updated,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		// Before any other statement: an aborted transaction would hide it.
		return fmt.Errorf("write file %d: %w", i, err)
	}
	newMap, merr := r.mapTo(ent, src, sf.id)
	if merr != nil {
		return merr
	}
	if err := r.upserted(ent, src, existed, newMap, err); err != nil {
		return err
	}
	if ent == EntFile {
		return nil
	}
	_, hadFile := r.target(EntFile, fp.URL)
	fileNew, err := r.mapTo(EntFile, fp.URL, sf.id)
	if err != nil {
		return err
	}
	switch {
	case !hadFile: // a new file mapping, also when a T4 link row became this file
		r.note(EntFile, fp.URL, OutCreate)
	case fileNew:
		r.note(EntFile, fp.URL, OutUpdate)
	default:
		r.note(EntFile, fp.URL, OutSkip)
	}
	return nil
}

// textFiles writes the rows of the files only a text refers to.
func (r *run) textFiles() error {
	for i, fp := range r.p.Files {
		if fp.AttachmentID != "" {
			continue
		}
		if err := r.fileRow(i, fp.Filename, "", nil, fp.At); err != nil {
			return err
		}
	}
	return nil
}
