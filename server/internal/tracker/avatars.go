package tracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
	"devops/internal/storage"
)

// Avatars (LAUN-22): one small image for an org and one for each project.
// Upload is prepare (a presigned PUT), the client's PUT to the store, then a
// confirm that checks the object and stores key, type and a new version. The
// image is read back by the app and streamed (OpenAvatar). Rules and every
// judgment call: docs/internal/decisions-T24-avatars.md.

// MaxAvatarBytes is the largest avatar (2 MiB).
const MaxAvatarBytes int64 = 2 << 20

// avatarTypes is the allowlist. SVG is not on it: an SVG served from the app
// origin can carry script (decisions-T24 #8).
var avatarTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// PrepareAvatarInput is the body of POST .../avatar/prepare.
type PrepareAvatarInput struct {
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
}

// SetAvatarInput is the body of PUT .../avatar.
type SetAvatarInput struct {
	UploadKey string `json:"uploadKey"`
}

// AvatarFile is what the GET route needs to answer: the stored type, the
// version (ETag) and the object key (never sent to a client).
type AvatarFile struct {
	Key         string
	ContentType string
	Version     int32
}

// avatarOwner is the org (Project nil) or one of its projects.
type avatarOwner struct {
	org     *auth.Org
	project *uuid.UUID
}

func (o avatarOwner) kind() string {
	if o.project == nil {
		return "org"
	}
	return "project"
}

func (o avatarOwner) keyPrefix() string {
	if o.project == nil {
		return "org/" + o.org.ID.String() + "/avatars/org/"
	}
	return "org/" + o.org.ID.String() + "/avatars/project/" + o.project.String() + "/"
}

// avatarKeyOK reports whether key is exactly prefix + a canonical uuid.
func avatarKeyOK(prefix, key string) bool {
	rest, ok := strings.CutPrefix(key, prefix)
	if !ok {
		return false
	}
	id, err := uuid.Parse(rest)
	return err == nil && id.String() == rest
}

func normalizeAvatarType(raw string) (string, error) {
	mt := strings.ToLower(strings.TrimSpace(raw))
	for _, a := range avatarTypes {
		if mt == a {
			return mt, nil
		}
	}
	return "", httpx.Invalid("contentType must be one of %s (SVG is not allowed)", strings.Join(avatarTypes, ", "))
}

func checkAvatarSize(n int64) error {
	if n < 1 || n > MaxAvatarBytes {
		return httpx.Invalid("sizeBytes must be between 1 and %d (2 MiB)", MaxAvatarBytes)
	}
	return nil
}

// avatarMagic reports whether head starts like an image of type ct.
func avatarMagic(ct string, head []byte) bool {
	switch ct {
	case "image/png":
		return bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n"))
	case "image/jpeg":
		return bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF})
	case "image/gif":
		return bytes.HasPrefix(head, []byte("GIF87a")) || bytes.HasPrefix(head, []byte("GIF89a"))
	case "image/webp":
		return len(head) >= 12 && bytes.HasPrefix(head, []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP"))
	}
	return false
}

// avatarURL is the URL of an avatar on the API origin (a path when
// PUBLIC_BASE_URL is empty), or nil when none is set. projectID is empty for
// the org's.
func (s *Service) avatarURL(orgSlug, projectID string, has bool, version int32) *string {
	if !has {
		return nil
	}
	u := strings.TrimRight(s.publicBaseURL, "/") + "/api/v1/orgs/" + url.PathEscape(orgSlug)
	if projectID != "" {
		u += "/projects/" + projectID
	}
	u += "/avatar?v=" + strconv.Itoa(int(version))
	return &u
}

func (s *Service) setProjectAvatarURLs(orgSlug string, ps []Project) {
	for i := range ps {
		ps[i].AvatarURL = s.avatarURL(orgSlug, ps[i].ID.String(), ps[i].hasAvatar, ps[i].avatarVersion)
	}
}

type avatarRow struct {
	key, contentType *string
	version          int32
}

func (s *Service) readAvatar(ctx context.Context, q *dbq.Queries, o avatarOwner, lock bool) (avatarRow, error) {
	if o.project == nil {
		if lock {
			r, err := q.LockOrgAvatar(ctx, o.org.ID)
			return avatarRow{r.AvatarKey, r.AvatarContentType, r.AvatarVersion}, notFound(err, "org")
		}
		r, err := q.GetOrgAvatar(ctx, o.org.ID)
		return avatarRow{r.AvatarKey, r.AvatarContentType, r.AvatarVersion}, notFound(err, "org")
	}
	all, ids := o.org.Access.Args()
	if lock {
		r, err := q.LockProjectAvatar(ctx, dbq.LockProjectAvatarParams{OrgID: o.org.ID, ID: *o.project, AllTeams: all, TeamIds: ids})
		return avatarRow{r.AvatarKey, r.AvatarContentType, r.AvatarVersion}, notFound(err, "project")
	}
	r, err := q.GetProjectAvatar(ctx, dbq.GetProjectAvatarParams{OrgID: o.org.ID, ID: *o.project, AllTeams: all, TeamIds: ids})
	return avatarRow{r.AvatarKey, r.AvatarContentType, r.AvatarVersion}, notFound(err, "project")
}

// PrepareAvatar checks a planned avatar upload and returns a presigned PUT.
// project nil: the org's avatar. It writes nothing.
func (s *Service) PrepareAvatar(ctx context.Context, org *auth.Org, project *uuid.UUID, in PrepareAvatarInput) (PreparedUpload, error) {
	o := avatarOwner{org, project}
	if s.store == nil {
		return PreparedUpload{}, errAttachmentsOff
	}
	ct, err := normalizeAvatarType(in.ContentType)
	if err != nil {
		return PreparedUpload{}, err
	}
	if err := checkAvatarSize(in.SizeBytes); err != nil {
		return PreparedUpload{}, err
	}
	if _, err := s.readAvatar(ctx, s.q, o, false); err != nil { // the owner exists in THIS org
		return PreparedUpload{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return PreparedUpload{}, fmt.Errorf("avatar id: %w", err)
	}
	key := o.keyPrefix() + id.String()
	up := s.store.PresignPut(key, ct, in.SizeBytes, uploadTTL)
	return PreparedUpload{
		UploadKey: key, UploadURL: up.URL, Method: up.Method, Headers: up.Headers,
		ContentType: ct, SizeBytes: in.SizeBytes, ExpiresAt: up.Expires,
	}, nil
}

// checkAvatarObject HEADs and reads the uploaded object. A bad object is
// deleted and the answer is 400.
func (s *Service) checkAvatarObject(ctx context.Context, key string) (string, error) {
	info, err := s.store.Head(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return "", httpx.Invalid("no uploaded file for uploadKey: PUT the file to uploadUrl first")
	}
	if err != nil {
		return "", fmt.Errorf("head avatar: %w", err)
	}
	ct, ctErr := normalizeAvatarType(info.ContentType)
	bad := ctErr != nil || checkAvatarSize(info.Size) != nil
	if !bad {
		body, _, err := s.store.Get(ctx, key)
		if err != nil {
			return "", fmt.Errorf("read avatar: %w", err)
		}
		head, err := io.ReadAll(io.LimitReader(body, 16))
		_ = body.Close()
		if err != nil {
			return "", fmt.Errorf("read avatar: %w", err)
		}
		bad = !avatarMagic(ct, head)
	}
	if bad {
		if err := s.store.Delete(ctx, key); err != nil {
			return "", fmt.Errorf("delete rejected avatar: %w", err)
		}
		return "", httpx.Invalid("the uploaded file is not a PNG, JPEG, GIF or WebP image of at most %d bytes; it was deleted", MaxAvatarBytes)
	}
	return ct, nil
}

// SetAvatar confirms an upload and makes it the avatar. The old object is
// deleted after the commit, best effort. Confirming the stored key again
// changes nothing.
func (s *Service) SetAvatar(ctx context.Context, org *auth.Org, p *auth.Principal, project *uuid.UUID, in SetAvatarInput) error {
	o := avatarOwner{org, project}
	if s.store == nil {
		return errAttachmentsOff
	}
	// The owner must exist in THIS org first (a cross-org project is 404), then the key.
	cur, err := s.readAvatar(ctx, s.q, o, false)
	if err != nil {
		return err
	}
	if !avatarKeyOK(o.keyPrefix(), in.UploadKey) {
		return httpx.Invalid("uploadKey is not an avatar upload for this %s", o.kind())
	}
	if cur.key != nil && *cur.key == in.UploadKey {
		return nil
	}
	ct, err := s.checkAvatarObject(ctx, in.UploadKey)
	if err != nil {
		return err
	}
	all, ids := org.Access.Args()
	var old *string
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, err := s.readAvatar(ctx, q, o, true)
		if err != nil {
			return err
		}
		if cur.key != nil && *cur.key == in.UploadKey {
			return nil // a parallel confirm won
		}
		old = cur.key
		if project == nil {
			err = q.SetOrgAvatar(ctx, dbq.SetOrgAvatarParams{ID: org.ID, AvatarKey: &in.UploadKey, AvatarContentType: &ct})
		} else {
			err = q.SetProjectAvatar(ctx, dbq.SetProjectAvatarParams{OrgID: org.ID, ID: *project, AvatarKey: &in.UploadKey, AvatarContentType: &ct, AllTeams: all, TeamIds: ids})
		}
		if err != nil {
			return fmt.Errorf("set avatar: %w", err)
		}
		return s.emitAvatar(ctx, q, o, p)
	})
	if err != nil {
		return err
	}
	s.deleteOldAvatar(ctx, o, old)
	return nil
}

// ClearAvatar removes the avatar. Clearing when none is set changes nothing.
func (s *Service) ClearAvatar(ctx context.Context, org *auth.Org, p *auth.Principal, project *uuid.UUID) error {
	o := avatarOwner{org, project}
	all, ids := org.Access.Args()
	var old *string
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, err := s.readAvatar(ctx, q, o, true)
		if err != nil {
			return err
		}
		if cur.key == nil {
			return nil
		}
		old = cur.key
		if project == nil {
			err = q.ClearOrgAvatar(ctx, org.ID)
		} else {
			err = q.ClearProjectAvatar(ctx, dbq.ClearProjectAvatarParams{OrgID: org.ID, ID: *project, AllTeams: all, TeamIds: ids})
		}
		if err != nil {
			return fmt.Errorf("clear avatar: %w", err)
		}
		return s.emitAvatar(ctx, q, o, p)
	})
	if err != nil {
		return err
	}
	s.deleteOldAvatar(ctx, o, old)
	return nil
}

// emitAvatar is the LAST statement of a set or clear. The payload holds no key and no URL.
func (s *Service) emitAvatar(ctx context.Context, q *dbq.Queries, o avatarOwner, p *auth.Principal) error {
	ev := events.Event{Type: events.OrgUpdated, EntityType: "org", EntityID: o.org.ID, ActorID: &p.UserID,
		Payload: map[string]any{"id": o.org.ID, "slug": o.org.Slug, "changes": []string{"avatar"}}}
	scope := events.ScopeOrg()
	if o.project != nil {
		ev = events.Event{Type: events.ProjectUpdated, EntityType: "project", EntityID: *o.project, ActorID: &p.UserID,
			Payload: map[string]any{"id": *o.project, "changes": []string{"avatar"}}}
		var err error
		if scope, err = projectEventScope(ctx, q, o.org.ID, *o.project); err != nil {
			return err
		}
	}
	_, err := events.Emit(ctx, q, o.org.ID, ev, scope)
	return err
}

// deleteOldAvatar removes the replaced object. A failure leaves an orphan and
// is logged without the key.
func (s *Service) deleteOldAvatar(ctx context.Context, o avatarOwner, key *string) {
	if key == nil || s.store == nil {
		return
	}
	if err := s.store.Delete(context.WithoutCancel(ctx), *key); err != nil {
		slog.Error("avatar: delete of the replaced object failed", "owner", o.kind(), "org_id", o.org.ID, "code", "avatar_delete_failed")
	}
}

// AvatarFileOf reads the stored avatar facts for the GET route. 404 when none.
func (s *Service) AvatarFileOf(ctx context.Context, org *auth.Org, project *uuid.UUID) (AvatarFile, error) {
	cur, err := s.readAvatar(ctx, s.q, avatarOwner{org, project}, false)
	if err != nil {
		return AvatarFile{}, err
	}
	if cur.key == nil || cur.contentType == nil {
		return AvatarFile{}, httpx.NotFound("avatar")
	}
	return AvatarFile{Key: *cur.key, ContentType: *cur.contentType, Version: cur.version}, nil
}

// OpenAvatar opens the object of f. The caller closes the body and bounds its read.
func (s *Service) OpenAvatar(ctx context.Context, f AvatarFile) (io.ReadCloser, int64, error) {
	if s.store == nil {
		return nil, 0, errAttachmentsOff
	}
	body, info, err := s.store.Get(ctx, f.Key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, 0, httpx.NotFound("avatar")
	}
	if err != nil {
		return nil, 0, fmt.Errorf("open avatar: %w", err)
	}
	return body, info.Size, nil
}
