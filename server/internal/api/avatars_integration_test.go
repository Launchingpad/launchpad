package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/dbq"
	"devops/internal/storage"
	"devops/internal/storage/storagetest"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// LAUN-22: org and project avatars (decisions-T24).

var (
	jpegBytes = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{1}, 32)...)
	gifBytes  = append([]byte("GIF89a"), bytes.Repeat([]byte{1}, 32)...)
	webpBytes = append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{1}, 32)...)
)

func (e *env) prepareAvatar(token, base, contentType string, size int) tracker.PreparedUpload {
	e.t.Helper()
	var up tracker.PreparedUpload
	e.must(200, e.do("POST", base+"/avatar/prepare", token,
		map[string]any{"contentType": contentType, "sizeBytes": size})).json(e.t, &up)
	return up
}

// uploadAvatar runs prepare, the PUT and the confirm, and returns the confirm response.
func (e *env) uploadAvatar(token, base, contentType string, body []byte) resp {
	e.t.Helper()
	up := e.prepareAvatar(token, base, contentType, len(body))
	if code := putUpload(e.t, up, body); code != 200 {
		e.t.Fatalf("PUT = %d", code)
	}
	return e.do("PUT", base+"/avatar", token, map[string]any{"uploadKey": up.UploadKey})
}

func (e *env) avatarGet(base, token, query string, headers ...string) resp {
	e.t.Helper()
	return e.do("GET", base+"/avatar"+query, token, nil, headers...)
}

func TestAvatars_OrgFlow(t *testing.T) {
	store := storagetest.New(t)
	e := newEnvWith(t, envOpts{store: store, publicBase: "https://tracker.example.com"})
	base := "/api/v1/orgs/orbit"

	t.Run("none: avatarUrl null and GET 404", func(t *testing.T) {
		var o tracker.Org
		e.must(200, e.do("GET", base, e.claude, nil)).json(t, &o)
		if o.AvatarURL != nil {
			t.Fatalf("avatarUrl = %v", *o.AvatarURL)
		}
		expectCode(t, e.avatarGet(base, e.claude, ""), 404, "not_found")
	})

	var o1 tracker.Org
	r := e.uploadAvatar(e.chen, base, "image/png", pngBytes)
	e.must(200, r).json(t, &o1)
	if o1.AvatarURL == nil || *o1.AvatarURL != "https://tracker.example.com/api/v1/orgs/orbit/avatar?v=1" {
		t.Fatalf("avatarUrl = %v", o1.AvatarURL)
	}
	var key1 string
	if err := e.pool.QueryRow(context.Background(), `SELECT avatar_key FROM orgs WHERE id = $1`, e.orgID).Scan(&key1); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key1, "org/"+e.orgID.String()+"/avatars/org/") {
		t.Fatalf("key = %q", key1)
	}

	t.Run("GET headers and bytes", func(t *testing.T) {
		g := e.must(200, e.avatarGet(base, e.claude, "?v=1"))
		h := g.header
		if !bytes.Equal(g.body, pngBytes) {
			t.Fatal("body differs")
		}
		for k, want := range map[string]string{
			"Content-Type": "image/png", "X-Content-Type-Options": "nosniff",
			"Content-Security-Policy": "default-src 'none'; sandbox", "Content-Disposition": "inline",
			"Cache-Control": "private, max-age=31536000, immutable", "ETag": `"av1"`,
		} {
			if got := h.Get(k); got != want {
				t.Errorf("%s = %q, want %q", k, got, want)
			}
		}
		if got := e.avatarGet(base, e.claude, "?v=0").header.Get("Cache-Control"); got != "private, no-cache" {
			t.Errorf("stale v Cache-Control = %q", got)
		}
		if got := e.avatarGet(base, e.claude, "").header.Get("Cache-Control"); got != "private, no-cache" {
			t.Errorf("no v Cache-Control = %q", got)
		}
	})
	t.Run("ETag gives 304", func(t *testing.T) {
		g := e.must(304, e.avatarGet(base, e.claude, "?v=1", "If-None-Match", `"av1"`))
		if len(g.body) != 0 || g.header.Get("ETag") != `"av1"` {
			t.Fatalf("304 = %q %v", g.body, g.header)
		}
		e.must(200, e.avatarGet(base, e.claude, "?v=1", "If-None-Match", `"av0"`))
	})
	t.Run("no login is 401", func(t *testing.T) {
		expectCode(t, e.avatarGet(base, "", ""), 401, "unauthenticated")
	})

	t.Run("replace deletes the old object and bumps the version", func(t *testing.T) {
		var o2 tracker.Org
		e.must(200, e.uploadAvatar(e.chen, base, "image/webp", webpBytes)).json(t, &o2)
		if o2.AvatarURL == nil || !strings.HasSuffix(*o2.AvatarURL, "/avatar?v=2") {
			t.Fatalf("avatarUrl = %v", o2.AvatarURL)
		}
		if _, err := store.Head(context.Background(), key1); err != storage.ErrNotFound {
			t.Fatalf("old object Head err = %v, want ErrNotFound", err)
		}
		g := e.must(200, e.avatarGet(base, e.claude, "?v=2"))
		if g.header.Get("Content-Type") != "image/webp" || !bytes.Equal(g.body, webpBytes) {
			t.Fatalf("replaced avatar = %s", g.header.Get("Content-Type"))
		}
		// An old URL still answers with the current image, revalidated.
		if got := e.must(200, e.avatarGet(base, e.claude, "?v=1")).header.Get("Cache-Control"); got != "private, no-cache" {
			t.Fatalf("old v Cache-Control = %q", got)
		}
	})
	t.Run("confirming the stored key again changes nothing", func(t *testing.T) {
		var k string
		_ = e.pool.QueryRow(context.Background(), `SELECT avatar_key FROM orgs WHERE id = $1`, e.orgID).Scan(&k)
		n := e.eventCount("org.updated")
		e.must(200, e.do("PUT", base+"/avatar", e.chen, map[string]any{"uploadKey": k}))
		if e.eventCount("org.updated") != n {
			t.Fatal("a repeat confirm emitted an event")
		}
	})
	t.Run("event is org.updated avatar and carries no key", func(t *testing.T) {
		evs := e.eventsOfType(e.chen, "org.updated")
		if len(evs) != 2 {
			t.Fatalf("events = %d", len(evs))
		}
		p := string(evs[len(evs)-1].Payload)
		if !strings.Contains(p, `"avatar"`) || strings.Contains(p, "avatars/") {
			t.Fatalf("payload = %s", p)
		}
		var typ string
		if err := e.pool.QueryRow(context.Background(), `SELECT type FROM events WHERE org_id = $1 ORDER BY seq DESC LIMIT 1`, e.orgID).Scan(&typ); err != nil || typ != "org.updated" {
			t.Fatalf("last event = %q %v", typ, err)
		}
	})
	t.Run("clear", func(t *testing.T) {
		var k string
		_ = e.pool.QueryRow(context.Background(), `SELECT avatar_key FROM orgs WHERE id = $1`, e.orgID).Scan(&k)
		var o tracker.Org
		e.must(200, e.do("DELETE", base+"/avatar", e.chen, nil)).json(t, &o)
		if o.AvatarURL != nil {
			t.Fatalf("avatarUrl = %v", *o.AvatarURL)
		}
		expectCode(t, e.avatarGet(base, e.claude, "?v=3"), 404, "not_found")
		if _, err := store.Head(context.Background(), k); err != storage.ErrNotFound {
			t.Fatalf("object Head err = %v", err)
		}
		n := e.eventCount("org.updated")
		e.must(200, e.do("DELETE", base+"/avatar", e.chen, nil))
		if e.eventCount("org.updated") != n {
			t.Fatal("clearing nothing emitted an event")
		}
		// the version never goes back: a set after a clear is a new ETag.
		var o4 tracker.Org
		e.must(200, e.uploadAvatar(e.chen, base, "image/jpeg", jpegBytes)).json(t, &o4)
		if !strings.HasSuffix(*o4.AvatarURL, "?v=4") {
			t.Fatalf("avatarUrl = %v", *o4.AvatarURL)
		}
	})
	t.Run("org list and PATCH carry avatarUrl", func(t *testing.T) {
		var orgs page[tracker.Org]
		e.must(200, e.do("GET", "/api/v1/orgs", e.claude, nil)).json(t, &orgs)
		if len(orgs.Items) != 1 || orgs.Items[0].AvatarURL == nil {
			t.Fatalf("orgs = %+v", orgs.Items)
		}
		var o tracker.Org
		e.must(200, e.do("PATCH", base, e.chen, map[string]any{"name": "Orbit 2"})).json(t, &o)
		if o.AvatarURL == nil {
			t.Fatal("PATCH lost avatarUrl")
		}
	})
}

func TestAvatars_Validation(t *testing.T) {
	store := storagetest.New(t)
	e := newEnvWith(t, envOpts{store: store})
	base := "/api/v1/orgs/orbit"
	prep := func(ct string, size int) resp {
		return e.do("POST", base+"/avatar/prepare", e.chen, map[string]any{"contentType": ct, "sizeBytes": size})
	}
	t.Run("prepare refuses svg, other types and sizes", func(t *testing.T) {
		for _, ct := range []string{"image/svg+xml", "text/html", "application/pdf", "video/mp4", ""} {
			expectCode(t, prep(ct, 100), 400, "validation_failed")
		}
		expectCode(t, prep("image/png", 0), 400, "validation_failed")
		expectCode(t, prep("image/png", 2<<20+1), 400, "validation_failed")
		e.must(200, prep("image/png", 2<<20))
		e.must(200, prep("image/gif", 1))
		for _, ct := range []string{"image/gif", "image/webp", "image/jpeg"} {
			e.must(200, prep(ct, 10))
		}
	})
	t.Run("prepare is never idempotent and not cached", func(t *testing.T) {
		r := e.do("POST", base+"/avatar/prepare", e.chen, map[string]any{"contentType": "image/png", "sizeBytes": 10}, "Idempotency-Key", "k1")
		expectCode(t, r, 400, "bad_request")
		if got := prep("image/png", 10).header.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q", got)
		}
	})
	t.Run("a key of another shape is 400", func(t *testing.T) {
		for _, k := range []string{"", "org/x/avatars/org/y", "org/" + e.orgID.String() + "/2026/abc/a.png",
			"org/" + e.orgID.String() + "/avatars/org/not-a-uuid", "org/" + e.orgID.String() + "/avatars/org/../x"} {
			expectCode(t, e.do("PUT", base+"/avatar", e.chen, map[string]any{"uploadKey": k}), 400, "validation_failed")
		}
	})
	t.Run("confirm before the PUT is 400", func(t *testing.T) {
		up := e.prepareAvatar(e.chen, base, "image/png", len(pngBytes))
		expectCode(t, e.do("PUT", base+"/avatar", e.chen, map[string]any{"uploadKey": up.UploadKey}), 400, "validation_failed")
	})
	t.Run("an object that is not the image it says is deleted and refused", func(t *testing.T) {
		up := e.prepareAvatar(e.chen, base, "image/png", 40)
		if code := putUpload(t, up, []byte("<svg xmlns='http://www.w3.org/2000/svg' onload='x()'></svg>")[:40]); code != 200 {
			t.Fatalf("PUT = %d", code)
		}
		expectCode(t, e.do("PUT", base+"/avatar", e.chen, map[string]any{"uploadKey": up.UploadKey}), 400, "validation_failed")
		if _, err := store.Head(context.Background(), up.UploadKey); err != storage.ErrNotFound {
			t.Fatalf("object Head err = %v, want deleted", err)
		}
		expectCode(t, e.avatarGet(base, e.chen, ""), 404, "not_found")
	})
	t.Run("a wrong-size PUT is refused by the store", func(t *testing.T) {
		up := e.prepareAvatar(e.chen, base, "image/png", len(pngBytes))
		if code := putUpload(t, up, append(pngBytes, 0)); code == 200 {
			t.Fatal("store took a body of another size")
		}
	})
	t.Run("unknown fields and non-JSON are refused", func(t *testing.T) {
		expectCode(t, e.do("PUT", base+"/avatar", e.chen, map[string]any{"uploadKey": "x", "svg": true}), 400, "bad_request")
		req, _ := http.NewRequest("PUT", e.srv.URL+base+"/avatar", strings.NewReader("x"))
		e.authorize(req, e.chen)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if res.StatusCode != 415 {
			t.Fatalf("status = %d", res.StatusCode)
		}
	})
}

func TestAvatars_Permissions(t *testing.T) {
	store := storagetest.New(t)
	e := newEnvWith(t, envOpts{store: store})
	base := "/api/v1/orgs/orbit"
	chen := e.user("chen")
	readTok, _ := e.newToken(chen.ID, tokenOpts{scopes: []string{"read"}})
	prj := e.createProject(e.claude, map[string]any{"name": "Alpha"})
	pbase := base + "/projects/" + prj.ID.String()

	t.Run("org avatar needs admin scope", func(t *testing.T) {
		body := map[string]any{"contentType": "image/png", "sizeBytes": 10}
		expectCode(t, e.do("POST", base+"/avatar/prepare", e.claude, body), 403, "insufficient_scope")
		expectCode(t, e.do("PUT", base+"/avatar", e.claude, map[string]any{"uploadKey": "x"}), 403, "insufficient_scope")
		expectCode(t, e.do("DELETE", base+"/avatar", e.claude, nil), 403, "insufficient_scope")
		expectCode(t, e.do("POST", base+"/avatar/prepare", readTok, body), 403, "insufficient_scope")
	})
	t.Run("project avatar needs write scope", func(t *testing.T) {
		body := map[string]any{"contentType": "image/png", "sizeBytes": 10}
		expectCode(t, e.do("POST", pbase+"/avatar/prepare", readTok, body), 403, "insufficient_scope")
		expectCode(t, e.do("PUT", pbase+"/avatar", readTok, map[string]any{"uploadKey": "x"}), 403, "insufficient_scope")
		expectCode(t, e.do("DELETE", pbase+"/avatar", readTok, nil), 403, "insufficient_scope")
		// a writer (the claude agent) may
		e.must(200, e.uploadAvatar(e.claude, pbase, "image/gif", gifBytes))
	})
	t.Run("a read token may read", func(t *testing.T) {
		e.must(200, e.avatarGet(pbase, readTok, ""))
	})
}

func TestAvatars_ProjectFlow(t *testing.T) {
	store := storagetest.New(t)
	e := newEnvWith(t, envOpts{store: store, publicBase: ""})
	base := "/api/v1/orgs/orbit"
	prj := e.createProject(e.claude, map[string]any{"name": "Alpha"})
	other := e.createProject(e.claude, map[string]any{"name": "Beta"})
	pbase := base + "/projects/" + prj.ID.String()

	if prj.AvatarURL != nil {
		t.Fatal("new project has an avatar")
	}
	expectCode(t, e.avatarGet(pbase, e.claude, ""), 404, "not_found")

	var p tracker.Project
	e.must(200, e.uploadAvatar(e.claude, pbase, "image/jpeg", jpegBytes)).json(t, &p)
	want := "/api/v1/orgs/orbit/projects/" + prj.ID.String() + "/avatar?v=1"
	if p.AvatarURL == nil || *p.AvatarURL != want {
		t.Fatalf("avatarUrl = %v, want %s", p.AvatarURL, want)
	}
	if p.Version != prj.Version {
		t.Fatalf("project version moved %d -> %d", prj.Version, p.Version)
	}
	g := e.must(200, e.avatarGet(pbase, e.claude, "?v=1"))
	if g.header.Get("Content-Type") != "image/jpeg" || !bytes.Equal(g.body, jpegBytes) {
		t.Fatal("wrong image")
	}
	// the other project and the org are untouched
	expectCode(t, e.avatarGet(base+"/projects/"+other.ID.String(), e.claude, ""), 404, "not_found")
	expectCode(t, e.avatarGet(base, e.claude, ""), 404, "not_found")
	// the key of the project is not accepted for the other one
	var key string
	_ = e.pool.QueryRow(context.Background(), `SELECT avatar_key FROM projects WHERE id = $1`, prj.ID).Scan(&key)
	expectCode(t, e.do("PUT", base+"/projects/"+other.ID.String()+"/avatar", e.claude, map[string]any{"uploadKey": key}), 400, "validation_failed")
	expectCode(t, e.do("PUT", base+"/avatar", e.chen, map[string]any{"uploadKey": key}), 400, "validation_failed")

	t.Run("list and get carry avatarUrl", func(t *testing.T) {
		var list page[tracker.Project]
		e.must(200, e.do("GET", base+"/projects", e.claude, nil)).json(t, &list)
		seen := 0
		for _, it := range list.Items {
			if it.ID == prj.ID {
				seen++
				if it.AvatarURL == nil {
					t.Fatal("list lost avatarUrl")
				}
			} else if it.AvatarURL != nil {
				t.Fatal("other project has an avatarUrl")
			}
		}
		if seen != 1 {
			t.Fatal("project not listed")
		}
		var one tracker.Project
		e.must(200, e.do("PATCH", pbase, e.claude, map[string]any{"description": "d"})).json(t, &one)
		if one.AvatarURL == nil {
			t.Fatal("PATCH lost avatarUrl")
		}
	})
	t.Run("event is project.updated avatar", func(t *testing.T) {
		evs := e.eventsOfType(e.claude, "project.updated")
		last := string(evs[len(evs)-1].Payload)
		_ = last
		var typ string
		if err := e.pool.QueryRow(context.Background(), `SELECT type FROM events WHERE org_id = $1 AND type = 'project.updated' ORDER BY seq ASC LIMIT 1`, e.orgID).Scan(&typ); err != nil {
			t.Fatal(err)
		}
		first := evs[0]
		if !strings.Contains(string(first.Payload), `"avatar"`) {
			t.Fatalf("payload = %s", first.Payload)
		}
	})
	t.Run("clear", func(t *testing.T) {
		var c tracker.Project
		e.must(200, e.do("DELETE", pbase+"/avatar", e.claude, nil)).json(t, &c)
		if c.AvatarURL != nil {
			t.Fatal("avatarUrl after clear")
		}
		expectCode(t, e.avatarGet(pbase, e.claude, ""), 404, "not_found")
		if _, err := store.Head(context.Background(), key); err != storage.ErrNotFound {
			t.Fatalf("object Head err = %v", err)
		}
	})
	t.Run("unknown project is 404", func(t *testing.T) {
		missing := base + "/projects/0192f0aa-0000-7000-8000-000000000000"
		expectCode(t, e.avatarGet(missing, e.claude, ""), 404, "not_found")
		expectCode(t, e.do("POST", missing+"/avatar/prepare", e.claude, map[string]any{"contentType": "image/png", "sizeBytes": 10}), 404, "not_found")
		expectCode(t, e.do("DELETE", missing+"/avatar", e.claude, nil), 404, "not_found")
		expectCode(t, e.avatarGet(base+"/projects/nope", e.claude, ""), 404, "not_found")
	})
}

func TestAvatars_CrossOrg(t *testing.T) {
	store := storagetest.New(t)
	e := newEnvWith(t, envOpts{store: store})
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	tok, _ := e.newToken(chen.ID, tokenOpts{})
	prj := e.createProject(tok, map[string]any{"name": "Orbit project"})
	pbase := "/api/v1/orgs/orbit/projects/" + prj.ID.String()
	e.must(200, e.uploadAvatar(tok, "/api/v1/orgs/orbit", "image/png", pngBytes))
	e.must(200, e.uploadAvatar(tok, pbase, "image/png", pngBytes))
	var orgKey, prjKey string
	_ = e.pool.QueryRow(context.Background(), `SELECT avatar_key FROM orgs WHERE id = $1`, e.orgID).Scan(&orgKey)
	_ = e.pool.QueryRow(context.Background(), `SELECT avatar_key FROM projects WHERE id = $1`, prj.ID).Scan(&prjKey)

	A := "/api/v1/orgs/acme"
	apbase := A + "/projects/" + prj.ID.String()
	t.Run("another org's project avatar is 404 on every route", func(t *testing.T) {
		expectCode(t, e.avatarGet(apbase, tok, ""), 404, "not_found")
		expectCode(t, e.do("POST", apbase+"/avatar/prepare", tok, map[string]any{"contentType": "image/png", "sizeBytes": 10}), 404, "not_found")
		expectCode(t, e.do("PUT", apbase+"/avatar", tok, map[string]any{"uploadKey": prjKey}), 404, "not_found")
		expectCode(t, e.do("DELETE", apbase+"/avatar", tok, nil), 404, "not_found")
		// and the orbit avatar is still there
		e.must(200, e.avatarGet(pbase, tok, ""))
	})
	t.Run("acme's own avatar is not orbit's", func(t *testing.T) {
		expectCode(t, e.avatarGet(A, tok, ""), 404, "not_found")
	})
	t.Run("an orbit key in acme is 400", func(t *testing.T) {
		expectCode(t, e.do("PUT", A+"/avatar", tok, map[string]any{"uploadKey": orgKey}), 400, "validation_failed")
		acmeProject := e.do("POST", A+"/projects", tok, map[string]any{"name": "acme project"})
		var ap tracker.Project
		e.must(201, acmeProject).json(t, &ap)
		expectCode(t, e.do("PUT", A+"/projects/"+ap.ID.String()+"/avatar", tok, map[string]any{"uploadKey": prjKey}), 400, "validation_failed")
	})
	t.Run("a stranger's token cannot read", func(t *testing.T) {
		stranger := e.newUser("stranger")
		strTok, _ := e.newToken(stranger.ID, tokenOpts{})
		expectCode(t, e.avatarGet("/api/v1/orgs/orbit", strTok, ""), 404, "not_found")
		expectCode(t, e.avatarGet(pbase, strTok, ""), 404, "not_found")
	})
}

func TestAvatars_StoreOff(t *testing.T) {
	e := newEnv(t)
	base := "/api/v1/orgs/orbit"
	expectCode(t, e.do("POST", base+"/avatar/prepare", e.chen, map[string]any{"contentType": "image/png", "sizeBytes": 10}), 503, "attachments_unavailable")
	expectCode(t, e.do("PUT", base+"/avatar", e.chen, map[string]any{"uploadKey": "x"}), 503, "attachments_unavailable")
	expectCode(t, e.avatarGet(base, e.chen, ""), 404, "not_found")
	e.must(200, e.do("DELETE", base+"/avatar", e.chen, nil))
}

// TestAvatars_NoSecretsInLogs: the upload URL and the object key never reach a log.
func TestAvatars_NoSecretsInLogs(t *testing.T) {
	store := storagetest.New(t)
	e := newEnvWith(t, envOpts{store: store, logLevel: -4})
	base := "/api/v1/orgs/orbit"
	up := e.prepareAvatar(e.chen, base, "image/png", len(pngBytes))
	putUpload(t, up, pngBytes)
	e.must(200, e.do("PUT", base+"/avatar", e.chen, map[string]any{"uploadKey": up.UploadKey}))
	e.must(200, e.uploadAvatar(e.chen, base, "image/gif", gifBytes))
	e.must(200, e.avatarGet(base, e.chen, ""))
	e.must(200, e.do("DELETE", base+"/avatar", e.chen, nil))
	logs := e.logs.String()
	uuidPart := up.UploadKey[strings.LastIndex(up.UploadKey, "/")+1:]
	for _, bad := range []string{up.UploadURL, up.UploadKey, uuidPart, "X-Amz-Signature", "avatars/"} {
		if strings.Contains(logs, bad) {
			t.Fatalf("log holds %q", bad)
		}
	}
	// the event payloads and the idempotency table hold no key either
	if n := e.countRows(`SELECT count(*) FROM events WHERE payload::text LIKE '%avatars/%'`); n != 0 {
		t.Fatalf("%d events hold a key", n)
	}
}

// T20 (LAUN-217, a 2a review ask): prepare and confirm on a project of a team
// the caller cannot see answer as for an unknown project, and change nothing.
func TestAvatars_HiddenProject(t *testing.T) {
	store := storagetest.New(t)
	e := newEnvWith(t, envOpts{store: store})
	ctx := context.Background()
	e.must(http.StatusCreated, e.do("POST", O+"/teams", e.chen, map[string]any{"key": "HID", "name": "Hidden"}))
	prj := e.createProject(e.chen, map[string]any{"name": "Hidden plan", "teams": []string{"HID"}})
	pbase := O + "/projects/" + prj.ID.String()
	e.must(200, e.uploadAvatar(e.chen, pbase, "image/png", pngBytes))
	var key string
	var version int32
	_ = e.pool.QueryRow(ctx, `SELECT avatar_key, avatar_version FROM projects WHERE id = $1`, prj.ID).Scan(&key, &version)

	orb, err := e.q.GetTeamByKeySystem(ctx, dbq.GetTeamByKeySystemParams{OrgID: e.orgID, Key: testdb.TeamKey})
	if err != nil {
		t.Fatal(err)
	}
	rid := testdb.AddMember(t, e.pool, e.orgID, "rita", "member")
	testdb.MakeRestricted(t, e.pool, e.orgID, rid, orb.ID)
	rita, _ := e.newToken(rid, tokenOpts{scopes: []string{"read", "write"}})
	unknown := O + "/projects/" + uuid.NewString()

	same := func(what string, hidden, missing resp) {
		t.Helper()
		if hidden.status != http.StatusNotFound || !bytes.Equal(hidden.body, missing.body) || hidden.status != missing.status {
			t.Fatalf("%s: hidden %d %s, unknown %d %s", what, hidden.status, hidden.body, missing.status, missing.body)
		}
	}
	prep := map[string]any{"contentType": "image/png", "sizeBytes": len(pngBytes)}
	same("prepare", e.do("POST", pbase+"/avatar/prepare", rita, prep), e.do("POST", unknown+"/avatar/prepare", rita, prep))

	// A real, uploaded key of this project (prepared by the admin, not confirmed).
	up := e.prepareAvatar(e.chen, pbase, "image/jpeg", len(jpegBytes))
	if code := putUpload(t, up, jpegBytes); code != 200 {
		t.Fatalf("PUT = %d", code)
	}
	body := map[string]any{"uploadKey": up.UploadKey}
	same("confirm", e.do("PUT", pbase+"/avatar", rita, body), e.do("PUT", unknown+"/avatar", rita, body))
	same("read", e.avatarGet(pbase, rita, ""), e.avatarGet(unknown, rita, ""))

	var key2 string
	var version2 int32
	_ = e.pool.QueryRow(ctx, `SELECT avatar_key, avatar_version FROM projects WHERE id = $1`, prj.ID).Scan(&key2, &version2)
	if key2 != key || version2 != version {
		t.Fatalf("a hidden project's avatar changed: %s v%d -> %s v%d", key, version, key2, version2)
	}
	if _, err := store.Head(ctx, up.UploadKey); err != nil {
		t.Fatalf("the refused confirm touched the uploaded object: %v", err)
	}
	e.must(200, e.avatarGet(pbase, e.chen, ""))
}
