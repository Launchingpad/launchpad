package tracker

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
	"devops/internal/storage"
)

// Attachments. A file attachment is three calls:
//
//  1. PrepareUpload checks the type and size and returns a presigned PUT
//     for org/{org_id}/{yyyy}/{attachment_id}/{filename}. Nothing is stored.
//  2. The client PUTs the bytes straight to the store.
//  3. CreateAttachment (kind file) HEADs the object, re-checks type and
//     size, inserts the row (id = the uuid in the key) and emits
//     attachment.created.
//
// Downloads are a 302 to a presigned GET (DownloadURL). A link attachment
// is one call (kind link). Rules and judgment calls: docs/internal/decisions-T3b.md.

// MaxAttachmentBytes is the largest file an attachment may hold (100 MiB).
const MaxAttachmentBytes int64 = 100 << 20

const (
	// uploadTTL is the life of a presigned PUT (architecture: 15 minutes).
	uploadTTL = 15 * time.Minute
	// downloadTTL is the life of the presigned GET behind a download 302.
	downloadTTL = 5 * time.Minute

	maxFilenameRunes = 255
	maxKeyNameBytes  = 100
	maxTitleRunes    = 255
	maxLinkURLBytes  = 2048
)

// allowedContentTypes is the upload allowlist (architecture "Attachments").
var allowedContentTypes = []string{"image/png", "image/jpeg", "image/gif", "video/mp4", "text/plain", "application/pdf"}

// errAttachmentsOff is the answer of every file route on a server with no
// object store.
var errAttachmentsOff = httpx.Errorf(http.StatusServiceUnavailable, httpx.CodeAttachmentsUnavailable,
	"attachments are off: this server has no object store (S3_*) configured")

// SetStore gives the service its object store. Nil (the default) turns the
// file routes off (503 attachments_unavailable); link attachments still work.
func (s *Service) SetStore(st storage.Store) { s.store = st }

// SetPublicBaseURL sets the origin of the API as clients see it
// (PUBLIC_BASE_URL). Download URLs are absolute on it; empty makes them
// paths ("/api/v1/...").
func (s *Service) SetPublicBaseURL(base string) { s.publicBaseURL = strings.TrimRight(base, "/") }

// PrepareUploadInput is the body of POST .../attachments/prepare.
type PrepareUploadInput struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
}

// PreparedUpload is a presigned PUT. UploadKey goes back to
// CreateAttachment once the PUT has succeeded.
type PreparedUpload struct {
	UploadKey   string            `json:"uploadKey"`
	UploadURL   string            `json:"uploadUrl"`
	Method      string            `json:"method"`
	Headers     map[string]string `json:"headers"`
	ContentType string            `json:"contentType"`
	SizeBytes   int64             `json:"sizeBytes"`
	ExpiresAt   time.Time         `json:"expiresAt"`
}

// CreateAttachmentInput is the body of POST .../issues/{issue}/attachments.
// kind file needs uploadKey (and takes filename); kind link needs url.
type CreateAttachmentInput struct {
	Kind      string `json:"kind"`
	UploadKey string `json:"uploadKey,omitempty"`
	Filename  string `json:"filename,omitempty"`
	URL       string `json:"url,omitempty"`
	Title     string `json:"title,omitempty"`
	Subtitle  string `json:"subtitle,omitempty"`
}

// normalizeContentType parses a media type and checks it against the
// allowlist. Parameters (charset) are dropped: the store holds the bare type.
func normalizeContentType(raw string) (string, error) {
	mt, _, err := mime.ParseMediaType(raw)
	if err == nil {
		mt = strings.ToLower(mt)
		for _, a := range allowedContentTypes {
			if mt == a {
				return mt, nil
			}
		}
	}
	return "", httpx.Invalid("contentType must be one of %s", strings.Join(allowedContentTypes, ", "))
}

func checkSize(n int64) error {
	if n < 1 || n > MaxAttachmentBytes {
		return httpx.Invalid("sizeBytes must be 1-%d (100 MiB)", MaxAttachmentBytes)
	}
	return nil
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

// validateFilename checks a display filename: 1-255 characters, valid UTF-8,
// no control characters and no path separators.
func validateFilename(raw string) (string, error) {
	f := strings.TrimSpace(raw)
	if f == "" || f == "." || f == ".." || !utf8.ValidString(f) || utf8.RuneCountInString(f) > maxFilenameRunes ||
		hasControl(f) || strings.ContainsAny(f, `/\`) {
		return "", httpx.Invalid("filename must be 1-%d characters, with no control characters, '/' or '\\'", maxFilenameRunes)
	}
	return f, nil
}

// keyName is the filename as it appears in an object key: [A-Za-z0-9._-]
// only (anything else becomes '_'), no leading dot, at most 100 bytes with
// the extension kept. keyName(keyName(x)) == keyName(x).
func keyName(filename string) string {
	var b strings.Builder
	for _, r := range filename {
		switch {
		case r < utf8.RuneSelf && (r == '.' || r == '-' || r == '_' ||
			'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9'):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	n := strings.TrimLeft(b.String(), ".")
	if len(n) > maxKeyNameBytes {
		ext := ""
		if i := strings.LastIndexByte(n, '.'); i > 0 && len(n)-i <= 16 {
			ext = n[i:]
		}
		n = strings.TrimLeft(n[:maxKeyNameBytes-len(ext)]+ext, ".")
	}
	if n == "" {
		return "file"
	}
	return n
}

// objectKey is org/{org_id}/{yyyy}/{attachment_id}/{filename}.
func objectKey(orgID uuid.UUID, year int, id uuid.UUID, name string) string {
	return fmt.Sprintf("org/%s/%04d/%s/%s", orgID, year, id, name)
}

// parseUploadKey checks that key is an object key this server would have
// minted for org, and returns its attachment id and filename segment. A key
// of another org is a 400 (a body reference to another org, decisions-T1).
func parseUploadKey(orgID uuid.UUID, key string) (uuid.UUID, string, error) {
	bad := httpx.Invalid("uploadKey is not a key returned by attachments/prepare")
	parts := strings.Split(key, "/")
	if len(parts) != 5 || parts[0] != "org" {
		return uuid.Nil, "", bad
	}
	if parts[1] != orgID.String() {
		return uuid.Nil, "", httpx.Invalid("uploadKey does not belong to this org")
	}
	if y, err := strconv.Atoi(parts[2]); err != nil || len(parts[2]) != 4 || y < 2000 {
		return uuid.Nil, "", bad
	}
	id, err := uuid.Parse(parts[3])
	if err != nil || id.String() != parts[3] || id.Version() != 7 {
		return uuid.Nil, "", bad
	}
	if parts[4] == "" || keyName(parts[4]) != parts[4] {
		return uuid.Nil, "", bad
	}
	return id, parts[4], nil
}

// PrepareUpload checks a planned upload and returns a presigned PUT. It
// writes nothing: the row is inserted by CreateAttachment after the PUT.
func (s *Service) PrepareUpload(ctx context.Context, org *auth.Org, in PrepareUploadInput) (PreparedUpload, error) {
	if s.store == nil {
		return PreparedUpload{}, errAttachmentsOff
	}
	name, err := validateFilename(in.Filename)
	if err != nil {
		return PreparedUpload{}, err
	}
	ct, err := normalizeContentType(in.ContentType)
	if err != nil {
		return PreparedUpload{}, err
	}
	if err := checkSize(in.SizeBytes); err != nil {
		return PreparedUpload{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return PreparedUpload{}, fmt.Errorf("attachment id: %w", err)
	}
	key := objectKey(org.ID, s.now().UTC().Year(), id, keyName(name))
	up := s.store.PresignPut(key, ct, in.SizeBytes, uploadTTL)
	return PreparedUpload{
		UploadKey: key, UploadURL: up.URL, Method: up.Method, Headers: up.Headers,
		ContentType: ct, SizeBytes: in.SizeBytes, ExpiresAt: up.Expires,
	}, nil
}

func validateAttachmentText(field, raw string) (string, error) {
	t := strings.TrimSpace(raw)
	if utf8.RuneCountInString(t) > maxTitleRunes || hasControl(t) {
		return "", httpx.Invalid("%s must be at most %d characters, with no control characters", field, maxTitleRunes)
	}
	return t, nil
}

func validateLinkURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > maxLinkURLBytes || (u.Scheme != "http" && u.Scheme != "https") ||
		u.Host == "" || u.User != nil || hasControl(raw) {
		return "", httpx.Invalid("url must be an absolute http or https URL of at most %d bytes, with no user info", maxLinkURLBytes)
	}
	return raw, nil
}

var errUploadTaken = errors.New("upload key taken")

// CreateAttachment adds a file (from a finished upload) or a link to an
// issue and emits attachment.created. A second attach of the same upload to
// the same issue returns the existing row with created=false and emits
// nothing (LAUN-106); the same upload on another issue is 409 conflict.
func (s *Service) CreateAttachment(ctx context.Context, org *auth.Org, p *auth.Principal, issueRef string, in CreateAttachmentInput) (Attachment, bool, error) {
	subtitle, err := validateAttachmentText("subtitle", in.Subtitle)
	if err != nil {
		return Attachment{}, false, err
	}
	title, err := validateAttachmentText("title", in.Title)
	if err != nil {
		return Attachment{}, false, err
	}
	var row dbq.InsertAttachmentParams
	var existing *Attachment
	switch in.Kind {
	case "file":
		if in.URL != "" {
			return Attachment{}, false, httpx.Invalid("url is for kind link; a file attachment takes uploadKey")
		}
		row, existing, err = s.fileAttachment(ctx, org, issueRef, in, title)
	case "link":
		if in.UploadKey != "" || in.Filename != "" {
			return Attachment{}, false, httpx.Invalid("uploadKey and filename are for kind file; a link attachment takes url")
		}
		row, err = s.linkAttachment(ctx, org, issueRef, in, title)
	default:
		return Attachment{}, false, httpx.Invalid("kind must be file or link")
	}
	if err != nil {
		return Attachment{}, false, err
	}
	if existing != nil {
		return *existing, false, nil
	}
	row.OrgID, row.Subtitle, row.CreatorID = org.ID, subtitle, &p.UserID

	var out dbq.InsertAttachmentRow
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		var err error
		out, err = q.InsertAttachment(ctx, row)
		if err != nil {
			switch {
			case database.IsPgCode(err, database.CodeUniqueViolation):
				return errUploadTaken
			case database.IsPgCode(err, database.CodeForeignKeyViolation) && database.ConstraintName(err) == "attachments_issue_fk":
				return httpx.NotFound("issue")
			}
			return fmt.Errorf("insert attachment: %w", err)
		}
		scope, err := issueEventScope(ctx, q, org.ID, row.IssueID)
		if err != nil {
			return err
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{
			Type: events.AttachmentCreated, EntityType: "attachment", EntityID: out.ID, ActorID: &p.UserID,
			Payload: map[string]any{"id": out.ID, "issueId": row.IssueID, "kind": row.Kind},
		}, scope)
		return err
	})
	if errors.Is(err, errUploadTaken) {
		// A parallel attach won the race: answer it the same way a later one is answered.
		id, _, perr := parseUploadKey(org.ID, in.UploadKey)
		if in.Kind == "file" && perr == nil {
			issueID, rerr := resolveIssueID(ctx, s.q, org, issueRef)
			if rerr != nil {
				return Attachment{}, false, rerr
			}
			existing, rerr := s.replayOf(ctx, org, issueID, id, in.UploadKey)
			if rerr != nil {
				return Attachment{}, false, rerr
			}
			if existing != nil {
				return *existing, false, nil
			}
		}
		return Attachment{}, false, conflict("this upload is already attached")
	}
	if err != nil {
		return Attachment{}, false, err
	}
	return s.toAttachment(org.Slug, dbq.ListIssueAttachmentsRow{
		ID: out.ID, IssueID: row.IssueID, Kind: row.Kind, Title: row.Title, Subtitle: row.Subtitle,
		Url: row.Url, ObjectKey: row.ObjectKey, Filename: row.Filename, ContentType: row.ContentType,
		SizeBytes: row.SizeBytes, CreatorID: &p.UserID, CreatedAt: out.CreatedAt,
		CreatorUsername: &p.Username, CreatorName: &p.Name,
	}), true, nil
}

// replayOf answers a second attach of an upload that is already attached.
// The same issue gets the existing row back (the attach is idempotent: a
// client retry is safe); any other issue is 409 conflict (LAUN-106).
//
// The lookup is scoped (T20 #11): an upload attached to an issue the caller
// cannot see is not found here, so the insert meets the unique key and the
// answer is the same 409 as for another visible issue.
func (s *Service) replayOf(ctx context.Context, org *auth.Org, issueID, id uuid.UUID, key string) (*Attachment, error) {
	r, err := s.getAttachmentRow(ctx, org, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get attachment: %w", err)
	}
	if r.IssueID != issueID || r.Kind != "file" || r.ObjectKey == nil || *r.ObjectKey != key {
		return nil, conflict("this upload is already attached")
	}
	a := s.toAttachment(org.Slug, dbq.ListIssueAttachmentsRow(r))
	return &a, nil
}

// fileAttachment checks a finished upload. The HEAD runs before the
// transaction, so no row lock is held across a network call. An object of
// the wrong type or size (it cannot pass the signed PUT, but the store is
// not trusted) is deleted and refused.
func (s *Service) fileAttachment(ctx context.Context, org *auth.Org, issueRef string, in CreateAttachmentInput, title string) (dbq.InsertAttachmentParams, *Attachment, error) {
	if s.store == nil {
		return dbq.InsertAttachmentParams{}, nil, errAttachmentsOff
	}
	if in.UploadKey == "" {
		return dbq.InsertAttachmentParams{}, nil, httpx.Invalid("uploadKey is required for kind file")
	}
	id, segment, err := parseUploadKey(org.ID, in.UploadKey)
	if err != nil {
		return dbq.InsertAttachmentParams{}, nil, err
	}
	filename := segment
	if in.Filename != "" {
		if filename, err = validateFilename(in.Filename); err != nil {
			return dbq.InsertAttachmentParams{}, nil, err
		}
	}
	issueID, err := resolveIssueID(ctx, s.q, org, issueRef)
	if err != nil {
		return dbq.InsertAttachmentParams{}, nil, err
	}
	if existing, err := s.replayOf(ctx, org, issueID, id, in.UploadKey); err != nil || existing != nil {
		return dbq.InsertAttachmentParams{}, existing, err
	}
	// replayOf is scoped, so it misses a row of an issue the caller cannot
	// see. Ask, unscoped, whether any row holds exactly this key (never the
	// id alone, #10) before the store is touched: a held key is the 409 of a key held by another visible
	// issue, with no Head and no Delete, so an object a row holds is never
	// deleted here (T20 #10, LAUN-219 review).
	held, err := s.q.AttachmentKeyHeldSystem(ctx, dbq.AttachmentKeyHeldSystemParams{OrgID: org.ID, ObjectKey: in.UploadKey})
	if err != nil {
		return dbq.InsertAttachmentParams{}, nil, fmt.Errorf("attachment key held: %w", err)
	}
	if held {
		return dbq.InsertAttachmentParams{}, nil, conflict("this upload is already attached")
	}
	info, err := s.store.Head(ctx, in.UploadKey)
	if errors.Is(err, storage.ErrNotFound) {
		return dbq.InsertAttachmentParams{}, nil, httpx.Invalid("no uploaded file for uploadKey: PUT the file to uploadUrl first")
	}
	if err != nil {
		return dbq.InsertAttachmentParams{}, nil, fmt.Errorf("head upload: %w", err)
	}
	ct, ctErr := normalizeContentType(info.ContentType)
	if ctErr != nil || checkSize(info.Size) != nil {
		if err := s.store.Delete(ctx, in.UploadKey); err != nil {
			return dbq.InsertAttachmentParams{}, nil, fmt.Errorf("delete rejected upload: %w", err)
		}
		return dbq.InsertAttachmentParams{}, nil, httpx.Invalid("the uploaded file is not an allowed type or is larger than %d bytes; it was deleted", MaxAttachmentBytes)
	}
	if title == "" {
		title = filename
	}
	key, size := in.UploadKey, info.Size
	return dbq.InsertAttachmentParams{
		ID: &id, IssueID: issueID, Kind: "file", Title: title,
		ObjectKey: &key, Filename: &filename, ContentType: &ct, SizeBytes: &size,
	}, nil, nil
}

func (s *Service) linkAttachment(ctx context.Context, org *auth.Org, issueRef string, in CreateAttachmentInput, title string) (dbq.InsertAttachmentParams, error) {
	if _, err := validateLinkURL(in.URL); err != nil {
		return dbq.InsertAttachmentParams{}, err
	}
	issueID, err := resolveIssueID(ctx, s.q, org, issueRef)
	if err != nil {
		return dbq.InsertAttachmentParams{}, err
	}
	return linkAttachmentParams(issueID, in.URL, title)
}

// linkAttachmentParams is the row of a link attachment: the URL checked by
// validateLinkURL and the title defaulted to the URL (cut to 255
// characters). The API route and the GitLab webhook (T5) both build link
// rows here; the caller sets OrgID and Subtitle, and CreatorID: the caller's
// user on the API path, nil (no user) for a link the webhook makes
// (attachments.creator_id is nullable).
func linkAttachmentParams(issueID uuid.UUID, rawURL, title string) (dbq.InsertAttachmentParams, error) {
	u, err := validateLinkURL(rawURL)
	if err != nil {
		return dbq.InsertAttachmentParams{}, err
	}
	if title == "" {
		title = u
		if utf8.RuneCountInString(title) > maxTitleRunes {
			title = string([]rune(title)[:maxTitleRunes])
		}
	}
	return dbq.InsertAttachmentParams{IssueID: issueID, Kind: "link", Title: title, Url: &u}, nil
}

// downloadPath is the API download URL of a file attachment. Markdown refers
// to this URL, never to a raw store URL (architecture "Attachments").
func (s *Service) downloadPath(orgSlug string, id uuid.UUID) string {
	return AttachmentDownloadURL(s.publicBaseURL, orgSlug, id)
}

// AttachmentDownloadURL is a file attachment's download URL on the API
// origin publicBase (PUBLIC_BASE_URL; empty gives a path). The importer
// writes it into markdown (decisions-T3b #12).
func AttachmentDownloadURL(publicBase, orgSlug string, id uuid.UUID) string {
	return strings.TrimRight(publicBase, "/") + "/api/v1/orgs/" + url.PathEscape(orgSlug) + "/attachments/" + id.String() + "/download"
}

// AttachmentObjectKey is the object key of a file attachment, as prepare
// mints it: org/{org_id}/{yyyy}/{id}/{safe filename}. The importer uses it,
// so its files have the same keys as uploaded ones.
func AttachmentObjectKey(orgID uuid.UUID, year int, id uuid.UUID, filename string) string {
	return objectKey(orgID, year, id, keyName(filename))
}

// AllowedContentType checks a media type against the upload allowlist and
// returns its bare form.
func AllowedContentType(raw string) (string, bool) {
	ct, err := normalizeContentType(raw)
	return ct, err == nil
}

// ValidAttachmentFilename reports whether a display filename passes the
// rules of a file attachment (1-255 characters, no control characters,
// no '/' or '\\').
func ValidAttachmentFilename(raw string) bool {
	_, err := validateFilename(raw)
	return err == nil
}

func (s *Service) toAttachment(orgSlug string, r dbq.ListIssueAttachmentsRow) Attachment {
	a := Attachment{
		ID: r.ID, IssueID: r.IssueID, Kind: r.Kind, Title: r.Title, Subtitle: r.Subtitle, URL: r.Url,
		Filename: r.Filename, ContentType: r.ContentType, SizeBytes: r.SizeBytes,
		Creator: userRef(r.CreatorID, r.CreatorUsername, r.CreatorName), CreatedAt: r.CreatedAt,
	}
	if r.Kind == "file" {
		d := s.downloadPath(orgSlug, r.ID)
		a.DownloadURL = &d
	}
	return a
}

func (s *Service) listAttachments(ctx context.Context, orgSlug string, orgID, issueID uuid.UUID) ([]Attachment, error) {
	rows, err := s.q.ListIssueAttachments(ctx, dbq.ListIssueAttachmentsParams{OrgID: orgID, IssueID: issueID})
	if err != nil {
		return nil, fmt.Errorf("list attachments: %w", err)
	}
	out := make([]Attachment, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.toAttachment(orgSlug, r))
	}
	return out, nil
}

// ListAttachments lists an issue's attachments, oldest first.
func (s *Service) ListAttachments(ctx context.Context, org *auth.Org, issueRef string) ([]Attachment, error) {
	id, err := resolveIssueID(ctx, s.q, org, issueRef)
	if err != nil {
		return nil, err
	}
	return s.listAttachments(ctx, org.Slug, org.ID, id)
}

// getAttachmentRow reads one attachment the caller may see: the predicate
// on the attachment's issue is in the lookup (T20 #11), so a hidden one and a
// missing one are the same pgx.ErrNoRows.
func (s *Service) getAttachmentRow(ctx context.Context, org *auth.Org, id uuid.UUID) (dbq.GetAttachmentRow, error) {
	scopeAll, scopeIDs := org.Access.Args()
	return s.q.GetAttachment(ctx, dbq.GetAttachmentParams{OrgID: org.ID, ID: id, AllTeams: scopeAll, TeamIds: scopeIDs})
}

// GetAttachment reads one attachment.
func (s *Service) GetAttachment(ctx context.Context, org *auth.Org, id uuid.UUID) (Attachment, error) {
	r, err := s.getAttachmentRow(ctx, org, id)
	if err != nil {
		return Attachment{}, notFound(err, "attachment")
	}
	return s.toAttachment(org.Slug, dbq.ListIssueAttachmentsRow(r)), nil
}

// DownloadURL returns the presigned GET a download redirects to. The store
// is told the stored Content-Type and a Content-Disposition: inline for
// images, video and PDF; attachment for plain text, so a browser never
// renders an uploaded text file as a page. The scoped lookup runs before the
// store is asked: an attachment of a hidden issue is 404 and no URL is signed
// (T20, inventory "download").
func (s *Service) DownloadURL(ctx context.Context, org *auth.Org, id uuid.UUID) (string, error) {
	r, err := s.getAttachmentRow(ctx, org, id)
	if err != nil {
		return "", notFound(err, "attachment")
	}
	if r.Kind != "file" || r.ObjectKey == nil || r.ContentType == nil || r.Filename == nil {
		return "", httpx.Invalid("attachment %s is a link and has no file; open its url", id)
	}
	if s.store == nil {
		return "", errAttachmentsOff
	}
	ct, disp := *r.ContentType, "inline"
	if ct == "text/plain" {
		ct, disp = "text/plain; charset=utf-8", "attachment"
	}
	return s.store.PresignGet(*r.ObjectKey, ct, contentDisposition(disp, *r.Filename), downloadTTL), nil
}

// contentDisposition builds the RFC 6266 header: the disposition, an ASCII
// filename and an RFC 5987 UTF-8 filename* parameter. The ASCII fallback
// has every non-printable, quote and backslash replaced, so the header
// cannot be broken out of.
func contentDisposition(disp, filename string) string {
	var ascii strings.Builder
	for _, r := range filename {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			ascii.WriteByte('_')
		} else {
			ascii.WriteRune(r)
		}
	}
	return disp + `; filename="` + ascii.String() + `"; filename*=UTF-8''` + attrEncode(filename)
}

// attrEncode is the RFC 5987 value encoding: ASCII letters, digits and
// "-._~" stay; every other byte is %XX.
func attrEncode(s string) string {
	const hexUpper = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexUpper[c>>4])
		b.WriteByte(hexUpper[c&15])
	}
	return b.String()
}
