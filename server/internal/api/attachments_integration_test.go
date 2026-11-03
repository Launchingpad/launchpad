package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/dbq"
	"devops/internal/storage"
	"devops/internal/storage/storagetest"
	"devops/internal/tracker"
)

// pngBytes is a tiny valid PNG (1x1). Its content does not matter to the
// server; the size and the signed type do.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
	"\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

// noRedirect is a client that returns the 302 itself.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (e *env) prepare(token, filename, contentType string, size int) tracker.PreparedUpload {
	e.t.Helper()
	var up tracker.PreparedUpload
	e.must(200, e.do("POST", "/api/v1/orgs/orbit/attachments/prepare", token,
		map[string]any{"filename": filename, "contentType": contentType, "sizeBytes": size})).json(e.t, &up)
	return up
}

// putUpload does step 2: the client's PUT straight to the store.
func putUpload(t *testing.T, up tracker.PreparedUpload, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(up.Method, up.UploadURL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range up.Headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	return res.StatusCode
}

func (e *env) download(path, token string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest("GET", e.srv.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.authorize(req, token)
	res, err := noRedirect.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func (e *env) eventsOfType(token, typ string) []tracker.Event {
	e.t.Helper()
	var p page[tracker.Event]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/events?after=0", token, nil)).json(e.t, &p)
	var out []tracker.Event
	for _, ev := range p.Items {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// TestAttachments_FileFlow is the whole AC path against the real silo:
// prepare -> presigned PUT -> create-from-upload -> download 302 ->
// presigned GET, plus the key shape, the event and the reads.
func TestAttachments_FileFlow(t *testing.T) {
	store := storagetest.New(t)
	e := newEnvWith(t, envOpts{store: store, publicBase: "https://tracker.example.com"})
	is := e.createIssue(e.chen, map[string]any{"title": "with a screenshot"})

	up := e.prepare(e.chen, "shot 1.png", "image/png", len(pngBytes))
	prefix := fmt.Sprintf("org/%s/%04d/", e.orgID, time.Now().UTC().Year())
	if !strings.HasPrefix(up.UploadKey, prefix) || !strings.HasSuffix(up.UploadKey, "/shot_1.png") {
		t.Fatalf("uploadKey = %q, want %s{uuid}/shot_1.png", up.UploadKey, prefix)
	}
	keyID := uuid.MustParse(strings.Split(up.UploadKey, "/")[3])
	if up.Method != "PUT" || up.Headers["Content-Type"] != "image/png" || up.SizeBytes != int64(len(pngBytes)) {
		t.Fatalf("prepared = %+v", up)
	}
	if d := time.Until(up.ExpiresAt); d < 14*time.Minute || d > 16*time.Minute {
		t.Fatalf("expiresAt in %v, want about 15m", d)
	}

	create := map[string]any{"kind": "file", "uploadKey": up.UploadKey, "filename": "shot 1.png"}
	t.Run("create before the PUT is 400", func(t *testing.T) {
		r := e.must(400, e.do("POST", "/api/v1/orgs/orbit/issues/"+is.Identifier+"/attachments", e.chen, create))
		if r.errCode(t) != "validation_failed" || !strings.Contains(string(r.body), "PUT the file") {
			t.Fatalf("body %s", r.body)
		}
	})

	if code := putUpload(t, up, pngBytes); code != 200 {
		t.Fatalf("PUT = %d", code)
	}

	var a tracker.Attachment
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/"+is.Identifier+"/attachments", e.chen, create)).json(t, &a)
	wantDL := "https://tracker.example.com/api/v1/orgs/orbit/attachments/" + a.ID.String() + "/download"
	switch {
	case a.ID != keyID, a.IssueID != is.ID, a.Kind != "file", a.Title != "shot 1.png",
		a.Filename == nil || *a.Filename != "shot 1.png",
		a.ContentType == nil || *a.ContentType != "image/png",
		a.SizeBytes == nil || *a.SizeBytes != int64(len(pngBytes)),
		a.DownloadURL == nil || *a.DownloadURL != wantDL,
		a.URL != nil, a.Creator == nil || a.Creator.Username != "chen":
		t.Fatalf("attachment = %+v", a)
	}

	t.Run("same upload attaches once (replay is 200, same row)", func(t *testing.T) {
		var again tracker.Attachment
		e.must(200, e.do("POST", "/api/v1/orgs/orbit/issues/"+is.Identifier+"/attachments", e.chen, create)).json(t, &again)
		if again.ID != a.ID || again.IssueID != is.ID {
			t.Fatalf("replay = %+v, want %s", again, a.ID)
		}
	})
	t.Run("reads", func(t *testing.T) {
		var one tracker.Attachment
		e.must(200, e.do("GET", "/api/v1/orgs/orbit/attachments/"+a.ID.String(), e.claude, nil)).json(t, &one)
		if one.ID != a.ID || one.DownloadURL == nil || *one.DownloadURL != wantDL || one.Creator == nil {
			t.Fatalf("get = %+v", one)
		}
		var list page[tracker.Attachment]
		e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues/"+is.Identifier+"/attachments", e.claude, nil)).json(t, &list)
		if len(list.Items) != 1 || list.Items[0].ID != a.ID {
			t.Fatalf("list = %+v", list.Items)
		}
		var withInc tracker.Issue
		e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues/"+is.Identifier+"?include=attachments", e.claude, nil)).json(t, &withInc)
		if len(withInc.Attachments) != 1 || withInc.Attachments[0].DownloadURL == nil {
			t.Fatalf("include = %+v", withInc.Attachments)
		}
	})
	t.Run("event emitted", func(t *testing.T) {
		evs := e.eventsOfType(e.chen, "attachment.created")
		if len(evs) != 1 || evs[0].EntityID != a.ID || evs[0].EntityType != "attachment" || evs[0].Actor == nil {
			t.Fatalf("events = %+v", evs)
		}
		if !strings.Contains(string(evs[0].Payload), is.ID.String()) || !strings.Contains(string(evs[0].Payload), `"kind":"file"`) {
			t.Fatalf("payload = %s", evs[0].Payload)
		}
	})
	t.Run("download is a 302 to the file", func(t *testing.T) {
		res := e.download("/api/v1/orgs/orbit/attachments/"+a.ID.String()+"/download", e.claude)
		if res.StatusCode != http.StatusFound {
			t.Fatalf("download = %d", res.StatusCode)
		}
		if res.Header.Get("Cache-Control") != "private, no-store" || res.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("headers = %v", res.Header)
		}
		loc := res.Header.Get("Location")
		if !strings.Contains(loc, "X-Amz-Signature=") || !strings.Contains(loc, "X-Amz-Expires=300") {
			t.Fatalf("Location = %s", loc)
		}
		got, err := http.Get(loc)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = got.Body.Close() }()
		b, _ := io.ReadAll(got.Body)
		if got.StatusCode != 200 || !bytes.Equal(b, pngBytes) || got.Header.Get("Content-Type") != "image/png" {
			t.Fatalf("GET Location = %d %s %q", got.StatusCode, got.Header.Get("Content-Type"), b)
		}
		if cd := got.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, `inline; filename="shot 1.png"`) {
			t.Fatalf("Content-Disposition = %q", cd)
		}
	})
	t.Run("download with a session cookie (img src)", func(t *testing.T) {
		session, _ := e.loginAs("chen@example.com")
		res := e.download("/api/v1/orgs/orbit/attachments/"+a.ID.String()+"/download", session)
		if res.StatusCode != http.StatusFound {
			t.Fatalf("download = %d", res.StatusCode)
		}
	})
	t.Run("download needs auth", func(t *testing.T) {
		res := e.download("/api/v1/orgs/orbit/attachments/"+a.ID.String()+"/download", "")
		if res.StatusCode != http.StatusUnauthorized || res.Header.Get("Location") != "" {
			t.Fatalf("download without auth = %d", res.StatusCode)
		}
	})
}

// TestAttachments_TextIsServedAsAttachment: plain text is never rendered
// inline by a browser.
func TestAttachments_TextIsServedAsAttachment(t *testing.T) {
	e := newEnvWith(t, envOpts{store: storagetest.New(t)})
	is := e.createIssue(e.chen, map[string]any{"title": "log"})
	body := []byte("<script>alert(1)</script>")
	up := e.prepare(e.chen, "log.txt", "text/plain; charset=utf-8", len(body))
	if up.ContentType != "text/plain" {
		t.Fatalf("contentType = %q", up.ContentType)
	}
	if code := putUpload(t, up, body); code != 200 {
		t.Fatalf("PUT = %d", code)
	}
	var a tracker.Attachment
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/"+is.Identifier+"/attachments", e.chen,
		map[string]any{"kind": "file", "uploadKey": up.UploadKey})).json(t, &a)
	if a.Filename == nil || *a.Filename != "log.txt" || a.Title != "log.txt" {
		t.Fatalf("default filename/title: %+v", a)
	}
	res := e.download("/api/v1/orgs/orbit/attachments/"+a.ID.String()+"/download", e.chen)
	got, err := http.Get(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	_ = got.Body.Close()
	if cd := got.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if ct := got.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

// TestAttachments_PrepareValidation covers the allowlist, the 100 MiB limit,
// the filename rules, the scope gate and the idempotency refusal.
func TestAttachments_PrepareValidation(t *testing.T) {
	e := newEnvWith(t, envOpts{store: storagetest.New(t)})
	chen := e.user("chen")
	readOnly, _ := e.newToken(chen.ID, tokenOpts{scopes: []string{"read"}})
	path := "/api/v1/orgs/orbit/attachments/prepare"
	for _, tc := range []struct {
		name    string
		token   string
		body    map[string]any
		headers []string
		status  int
		code    string
	}{
		{name: "html refused", body: map[string]any{"filename": "a.html", "contentType": "text/html", "sizeBytes": 10}, status: 400, code: "validation_failed"},
		{name: "svg refused", body: map[string]any{"filename": "a.svg", "contentType": "image/svg+xml", "sizeBytes": 10}, status: 400, code: "validation_failed"},
		{name: "zero bytes", body: map[string]any{"filename": "a.png", "contentType": "image/png", "sizeBytes": 0}, status: 400, code: "validation_failed"},
		{name: "over 100 MiB", body: map[string]any{"filename": "a.mp4", "contentType": "video/mp4", "sizeBytes": tracker.MaxAttachmentBytes + 1}, status: 400, code: "validation_failed"},
		{name: "exactly 100 MiB", body: map[string]any{"filename": "a.mp4", "contentType": "video/mp4", "sizeBytes": tracker.MaxAttachmentBytes}, status: 200},
		{name: "path in filename", body: map[string]any{"filename": "../a.png", "contentType": "image/png", "sizeBytes": 10}, status: 400, code: "validation_failed"},
		{name: "unknown field", body: map[string]any{"filename": "a.png", "contentType": "image/png", "sizeBytes": 10, "key": "x"}, status: 400, code: "bad_request"},
		{name: "read scope", token: readOnly, body: map[string]any{"filename": "a.png", "contentType": "image/png", "sizeBytes": 10}, status: 403, code: "insufficient_scope"},
		{name: "idempotency key refused", headers: []string{"Idempotency-Key", "k1"}, body: map[string]any{"filename": "a.png", "contentType": "image/png", "sizeBytes": 10}, status: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := tc.token
			if tok == "" {
				tok = e.chen
			}
			r := e.do("POST", path, tok, tc.body, tc.headers...)
			if r.status != tc.status {
				t.Fatalf("status = %d, want %d: %s", r.status, tc.status, r.body)
			}
			if tc.code != "" && r.errCode(t) != tc.code {
				t.Fatalf("code = %s, want %s", r.errCode(t), tc.code)
			}
			if r.status == 200 && r.header.Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q", r.header.Get("Cache-Control"))
			}
		})
	}
	t.Run("store refuses a bigger body than prepared", func(t *testing.T) {
		up := e.prepare(e.chen, "a.png", "image/png", 10)
		if code := putUpload(t, up, bytes.Repeat([]byte{1}, 11)); code != http.StatusForbidden {
			t.Fatalf("PUT 11 bytes on a 10-byte upload = %d, want 403", code)
		}
	})
}

// TestAttachments_RejectsBadObject: an object that bypassed the signed PUT
// (here: written with a different signed type) is deleted and refused, so
// the server never trusts the store's contents blindly.
func TestAttachments_RejectsBadObject(t *testing.T) {
	store := storagetest.New(t)
	e := newEnvWith(t, envOpts{store: store})
	is := e.createIssue(e.chen, map[string]any{"title": "x"})
	up := e.prepare(e.chen, "a.png", "image/png", 7)
	body := []byte("<html>")
	body = append(body, '!')
	rogue := store.PresignPut(up.UploadKey, "text/html", int64(len(body)), time.Minute)
	if code := putUpload(t, tracker.PreparedUpload{UploadURL: rogue.URL, Method: rogue.Method, Headers: rogue.Headers}, body); code != 200 {
		t.Fatalf("rogue PUT = %d", code)
	}
	r := e.must(400, e.do("POST", "/api/v1/orgs/orbit/issues/"+is.Identifier+"/attachments", e.chen,
		map[string]any{"kind": "file", "uploadKey": up.UploadKey}))
	if r.errCode(t) != "validation_failed" || !strings.Contains(string(r.body), "deleted") {
		t.Fatalf("body %s", r.body)
	}
	if _, err := store.Head(context.Background(), up.UploadKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("rogue object still there: %v", err)
	}
	if n := len(e.eventsOfType(e.chen, "attachment.created")); n != 0 {
		t.Fatalf("%d attachment events after a refused create", n)
	}
}

// TestAttachments_Links: link attachments need no store.
func TestAttachments_Links(t *testing.T) {
	e := newEnv(t) // no store: links still work
	is := e.createIssue(e.chen, map[string]any{"title": "x"})
	path := "/api/v1/orgs/orbit/issues/" + is.Identifier + "/attachments"
	mr := "https://gitlab.example.com/example-org/dev-ops/-/merge_requests/7"

	var a tracker.Attachment
	e.must(201, e.do("POST", path, e.chen, map[string]any{"kind": "link", "url": mr, "subtitle": "!7"})).json(t, &a)
	if a.Kind != "link" || a.URL == nil || *a.URL != mr || a.Title != mr || a.Subtitle != "!7" ||
		a.DownloadURL != nil || a.Filename != nil || a.Creator == nil {
		t.Fatalf("link = %+v", a)
	}
	var b tracker.Attachment
	e.must(201, e.do("POST", path, e.chen, map[string]any{"kind": "link", "url": mr, "title": "MR !7"})).json(t, &b)
	if b.Title != "MR !7" {
		t.Fatalf("title = %q", b.Title)
	}
	if n := len(e.eventsOfType(e.chen, "attachment.created")); n != 2 {
		t.Fatalf("%d attachment.created events, want 2", n)
	}
	r := e.must(400, e.do("GET", "/api/v1/orgs/orbit/attachments/"+a.ID.String()+"/download", e.chen, nil))
	if r.errCode(t) != "validation_failed" {
		t.Fatalf("download of a link: %s", r.body)
	}
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{name: "javascript url", body: map[string]any{"kind": "link", "url": "javascript:alert(1)"}},
		{name: "relative url", body: map[string]any{"kind": "link", "url": "/x"}},
		{name: "link with uploadKey", body: map[string]any{"kind": "link", "url": mr, "uploadKey": "org/x"}},
		{name: "file with url", body: map[string]any{"kind": "file", "url": mr}},
		{name: "unknown kind", body: map[string]any{"kind": "image", "url": mr}},
		{name: "title too long", body: map[string]any{"kind": "link", "url": mr, "title": strings.Repeat("a", 256)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := e.must(400, e.do("POST", path, e.chen, tc.body)).errCode(t); code != "validation_failed" {
				t.Fatalf("code = %s", code)
			}
		})
	}
	t.Run("unknown issue is 404", func(t *testing.T) {
		e.must(404, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-999/attachments", e.chen, map[string]any{"kind": "link", "url": mr}))
	})
	t.Run("idempotent replay", func(t *testing.T) {
		body := map[string]any{"kind": "link", "url": mr + "#idem"}
		first := e.must(201, e.do("POST", path, e.chen, body, "Idempotency-Key", "att-1"))
		again := e.must(201, e.do("POST", path, e.chen, body, "Idempotency-Key", "att-1"))
		if again.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(first.body, again.body) {
			t.Fatalf("replay: %v %s", again.header, again.body)
		}
	})
}

// TestAttachments_Unavailable: with no store the file routes are 503
// attachments_unavailable, and nothing else breaks.
func TestAttachments_Unavailable(t *testing.T) {
	e := newEnv(t)
	is := e.createIssue(e.chen, map[string]any{"title": "x"})
	r := e.must(503, e.do("POST", "/api/v1/orgs/orbit/attachments/prepare", e.chen,
		map[string]any{"filename": "a.png", "contentType": "image/png", "sizeBytes": 10}))
	if r.errCode(t) != "attachments_unavailable" {
		t.Fatalf("prepare: %s", r.body)
	}
	key := fmt.Sprintf("org/%s/2026/%s/a.png", e.orgID, uuid.Must(uuid.NewV7()))
	r = e.must(503, e.do("POST", "/api/v1/orgs/orbit/issues/"+is.Identifier+"/attachments", e.chen,
		map[string]any{"kind": "file", "uploadKey": key}))
	if r.errCode(t) != "attachments_unavailable" {
		t.Fatalf("create: %s", r.body)
	}
	// A file row that exists (written while a store was on) still reads,
	// but its download is 503.
	fn, ct, size := "a.png", "image/png", int64(10)
	row, err := e.q.InsertAttachment(context.Background(), dbq.InsertAttachmentParams{
		OrgID: e.orgID, IssueID: is.ID, Kind: "file", Title: fn, ObjectKey: &key,
		Filename: &fn, ContentType: &ct, SizeBytes: &size,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/attachments/"+row.ID.String(), e.chen, nil))
	r = e.must(503, e.do("GET", "/api/v1/orgs/orbit/attachments/"+row.ID.String()+"/download", e.chen, nil))
	if r.errCode(t) != "attachments_unavailable" {
		t.Fatalf("download: %s", r.body)
	}
}

// TestAttachments_CrossOrg: chen is a member of both orgs with an unpinned
// token, so only the org in the path stops him. Another org's attachment is
// 404 on every route; another org's upload key in a body is 400.
func TestAttachments_CrossOrg(t *testing.T) {
	store := storagetest.New(t)
	e := newEnvWith(t, envOpts{store: store})
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	tok, _ := e.newToken(chen.ID, tokenOpts{})

	orb := e.createIssue(tok, map[string]any{"title": "orbit"})
	up := e.prepare(tok, "secret.png", "image/png", len(pngBytes))
	if code := putUpload(t, up, pngBytes); code != 200 {
		t.Fatalf("PUT = %d", code)
	}
	var a tracker.Attachment
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-1/attachments", tok,
		map[string]any{"kind": "file", "uploadKey": up.UploadKey})).json(t, &a)
	// An orbit upload that is not attached yet.
	spare := e.prepare(tok, "spare.png", "image/png", len(pngBytes))
	if code := putUpload(t, spare, pngBytes); code != 200 {
		t.Fatalf("PUT = %d", code)
	}
	e.must(201, e.do("POST", "/api/v1/orgs/acme/issues", tok, map[string]any{"team": "ACM", "title": "acme"}))

	A := "/api/v1/orgs/acme"
	expect := func(t *testing.T, status int, method, path string, body any) {
		t.Helper()
		r := e.do(method, path, tok, body)
		if r.status != status {
			t.Fatalf("%s %s: status %d, want %d: %s", method, path, r.status, status, r.body)
		}
		r.errCode(t)
	}
	t.Run("another org's attachment is 404", func(t *testing.T) {
		expect(t, 404, "GET", A+"/attachments/"+a.ID.String(), nil)
		expect(t, 404, "GET", A+"/attachments/"+a.ID.String()+"/download", nil)
		expect(t, 404, "GET", A+"/issues/"+orb.ID.String()+"/attachments", nil)
		expect(t, 404, "POST", A+"/issues/"+orb.ID.String()+"/attachments", map[string]any{"kind": "link", "url": "https://x.example"})
	})
	t.Run("another org's upload key is 400", func(t *testing.T) {
		expect(t, 400, "POST", A+"/issues/ACM-1/attachments", map[string]any{"kind": "file", "uploadKey": spare.UploadKey})
		expect(t, 400, "POST", A+"/issues/ACM-1/attachments", map[string]any{"kind": "file", "uploadKey": up.UploadKey})
	})
	t.Run("acme lists nothing", func(t *testing.T) {
		var p page[tracker.Attachment]
		e.must(200, e.do("GET", A+"/issues/ACM-1/attachments", tok, nil)).json(t, &p)
		if len(p.Items) != 0 {
			t.Fatalf("acme sees %+v", p.Items)
		}
	})
	t.Run("prepare in acme keys under acme", func(t *testing.T) {
		var p tracker.PreparedUpload
		e.must(200, e.do("POST", A+"/attachments/prepare", tok,
			map[string]any{"filename": "a.png", "contentType": "image/png", "sizeBytes": 3})).json(t, &p)
		if !strings.HasPrefix(p.UploadKey, "org/"+acmeID.String()+"/") {
			t.Fatalf("acme key = %s", p.UploadKey)
		}
	})
}

// TestAttachments_PrepareNonMember: a user who is not a member of the org
// gets 404 on prepare (RequireOrgMember runs before the handler), not a
// presigned URL.
func TestAttachments_PrepareNonMember(t *testing.T) {
	e := newEnvWith(t, envOpts{store: storagetest.New(t)})
	stranger := e.newUser("stranger")
	tok, _ := e.newToken(stranger.ID, tokenOpts{})
	r := e.do("POST", "/api/v1/orgs/orbit/attachments/prepare", tok,
		map[string]any{"filename": "a.png", "contentType": "image/png", "sizeBytes": 3})
	if r.status != 404 || r.errCode(t) != "not_found" {
		t.Fatalf("prepare by a non-member: %d %s", r.status, r.body)
	}
	if strings.Contains(string(r.body), "X-Amz-Signature") {
		t.Fatal("a non-member got a presigned URL")
	}
}

// attachRows counts the attachment rows of an org's upload key.
func (e *env) attachRows(key string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM attachments WHERE object_key = $1`, key).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// TestAttachments_ReplayIdempotent (LAUN-106): a second attach of one
// uploadKey to the same issue answers the same row with 200 and writes no
// row and no event; another issue is 409; two parallel attaches leave one
// row and one event.
func TestAttachments_ReplayIdempotent(t *testing.T) {
	store := storagetest.New(t)
	e := newEnvWith(t, envOpts{store: store})
	one := e.createIssue(e.chen, map[string]any{"title": "one"})
	two := e.createIssue(e.chen, map[string]any{"title": "two"})
	path := func(i tracker.Issue) string { return "/api/v1/orgs/orbit/issues/" + i.Identifier + "/attachments" }

	up := e.prepare(e.chen, "a.png", "image/png", len(pngBytes))
	if code := putUpload(t, up, pngBytes); code != 200 {
		t.Fatalf("PUT = %d", code)
	}
	body := map[string]any{"kind": "file", "uploadKey": up.UploadKey}
	var first, second tracker.Attachment
	e.must(201, e.do("POST", path(one), e.chen, body)).json(t, &first)
	e.must(200, e.do("POST", path(one), e.chen, body)).json(t, &second)
	if first.ID != second.ID || !first.CreatedAt.Equal(second.CreatedAt) {
		t.Fatalf("replay %+v != %+v", second, first)
	}
	if n := e.attachRows(up.UploadKey); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	if evs := e.eventsOfType(e.chen, "attachment.created"); len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	t.Run("another issue is 409", func(t *testing.T) {
		r := e.must(409, e.do("POST", path(two), e.chen, body))
		if r.errCode(t) != "conflict" {
			t.Fatalf("body %s", r.body)
		}
	})

	t.Run("two parallel attaches leave one row and one event", func(t *testing.T) {
		up := e.prepare(e.chen, "b.png", "image/png", len(pngBytes))
		if code := putUpload(t, up, pngBytes); code != 200 {
			t.Fatalf("PUT = %d", code)
		}
		body := map[string]any{"kind": "file", "uploadKey": up.UploadKey}
		const n = 6
		statuses := make(chan int, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			go func() {
				<-start
				statuses <- e.doNoFatal("POST", path(one), e.chen, body).status
			}()
		}
		close(start)
		created := 0
		for i := 0; i < n; i++ {
			switch st := <-statuses; st {
			case 201:
				created++
			case 200:
			default:
				t.Errorf("status %d, want 200 or 201", st)
			}
		}
		if created != 1 {
			t.Fatalf("201 count = %d, want 1", created)
		}
		if rows := e.attachRows(up.UploadKey); rows != 1 {
			t.Fatalf("rows = %d, want 1", rows)
		}
		if evs := e.eventsOfType(e.chen, "attachment.created"); len(evs) != 2 {
			t.Fatalf("events = %d, want 2 (a.png and b.png)", len(evs))
		}
	})
}
