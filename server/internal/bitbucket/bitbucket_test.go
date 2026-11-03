package bitbucket

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"devops/internal/gitapi/gitapitest"
	"devops/internal/scm"
)

const (
	cloudKey = "{11111111-2222-3333-4444-555555555555}/{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}"
	headSHA  = "0a1b2c3d4e5f60718293a4b5c6d7e8f901234567"
	headSHA2 = "f0e1d2c3b4a5968778695a4b3c2d1e0f12345678"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func parse(t *testing.T, key, file string) scm.Event {
	t.Helper()
	ev, res, err := Provider{}.Parse(hdr(HeaderEvent, key), fixture(t, file))
	if err != nil || res != scm.ParseOK {
		t.Fatalf("%s: parse = %v, %v", file, res, err)
	}
	return ev
}

func TestEvent_Actions(t *testing.T) {
	tests := []struct {
		key    string
		file   string
		action scm.Action
		kind   scm.Kind
		draft  bool
		fork   bool
		move   scm.Move
		review scm.Review
		sha    string
	}{
		// Cloud
		{EventCloudCreated, "cloud_pr_created.json", scm.ActionOpened, scm.KindChange, false, false, scm.MoveToReview, scm.ReviewNone, headSHA},
		{EventCloudCreated, "cloud_pr_created_draft.json", scm.ActionOpened, scm.KindChange, true, false, scm.MoveNone, scm.ReviewNone, headSHA},
		{EventCloudUpdated, "cloud_pr_updated.json", scm.ActionUpdated, scm.KindChange, false, false, scm.MoveNone, scm.ReviewNone, headSHA2},
		{EventCloudFulfilled, "cloud_pr_fulfilled.json", scm.ActionMerged, scm.KindChange, false, false, scm.MoveToTesting, scm.ReviewNone, headSHA2},
		{EventCloudRejected, "cloud_pr_rejected.json", scm.ActionClosed, scm.KindChange, false, false, scm.MoveNone, scm.ReviewNone, headSHA},
		{EventCloudApproved, "cloud_pr_approved.json", scm.ActionApproved, scm.KindReview, false, false, scm.MoveNone, scm.ReviewApproved, headSHA},
		{EventCloudUnapproved, "cloud_pr_unapproved.json", scm.ActionUnapproved, scm.KindReview, false, false, scm.MoveNone, scm.ReviewNone, headSHA},
		{EventCloudCreated, "cloud_pr_fork_created.json", scm.ActionOpened, scm.KindChange, false, true, scm.MoveToReview, scm.ReviewNone, headSHA},
		{EventCloudCreated, "cloud_pr_deleted_fork_created.json", scm.ActionOpened, scm.KindChange, false, true, scm.MoveToReview, scm.ReviewNone, headSHA},
		// Data Center
		{EventDCOpened, "dc_pr_opened.json", scm.ActionOpened, scm.KindChange, false, false, scm.MoveToReview, scm.ReviewNone, headSHA},
		{EventDCOpened, "dc_pr_opened_draft.json", scm.ActionOpened, scm.KindChange, true, false, scm.MoveNone, scm.ReviewNone, headSHA},
		{EventDCFromRef, "dc_pr_from_ref_updated.json", scm.ActionUpdated, scm.KindChange, false, false, scm.MoveNone, scm.ReviewNone, headSHA2},
		{EventDCModified, "dc_pr_modified.json", scm.ActionUpdated, scm.KindChange, false, false, scm.MoveNone, scm.ReviewNone, headSHA},
		{EventDCMerged, "dc_pr_merged.json", scm.ActionMerged, scm.KindChange, false, false, scm.MoveToTesting, scm.ReviewNone, headSHA2},
		{EventDCDeclined, "dc_pr_declined.json", scm.ActionClosed, scm.KindChange, false, false, scm.MoveNone, scm.ReviewNone, headSHA},
		{EventDCApproved, "dc_pr_reviewer_approved.json", scm.ActionApproved, scm.KindReview, false, false, scm.MoveNone, scm.ReviewApproved, headSHA},
		{EventDCUnapproved, "dc_pr_reviewer_unapproved.json", scm.ActionUnapproved, scm.KindReview, false, false, scm.MoveNone, scm.ReviewNone, headSHA},
		{EventDCNeedsWork, "dc_pr_reviewer_needs_work.json", scm.ActionChangesRequested, scm.KindReview, false, false, scm.MoveNone, scm.ReviewChangesRequested, headSHA},
		{EventDCOpened, "dc_pr_fork_opened.json", scm.ActionOpened, scm.KindChange, false, true, scm.MoveToReview, scm.ReviewNone, headSHA},
		{EventDCOpened, "dc_pr_deleted_fork_opened.json", scm.ActionOpened, scm.KindChange, false, true, scm.MoveToReview, scm.ReviewNone, headSHA},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			ev := parse(t, tt.key, tt.file)
			p := ev.Plan()
			if ev.Action != tt.action || ev.Kind != tt.kind || ev.Draft != tt.draft || ev.Fork != tt.fork || p.Move != tt.move ||
				ev.Review != tt.review || ev.HeadSHA != tt.sha || p.Noop() {
				t.Fatalf("event %+v plan %+v", ev, p)
			}
			if ev.Provider != "bitbucket" || ev.Number != 17 || ev.Author != "example-dev" || ev.OccurredAt.IsZero() ||
				ev.Title != "LAUN-84 fix login redirect" || !strings.Contains(ev.URL, "pull-requests/17") ||
				ev.SourceBranch != "chen/laun-84-fix-login-redirect" {
				t.Fatalf("event fields = %+v", ev)
			}
			if strings.HasPrefix(tt.file, "cloud_") {
				if ev.RepoKey != cloudKey || ev.RepoPath != "example-org/example-app" {
					t.Fatalf("cloud repo = %q %q", ev.RepoKey, ev.RepoPath)
				}
			} else if ev.RepoKey != "4242" || ev.RepoPath != "PROJ/example-app" {
				t.Fatalf("dc repo = %q %q", ev.RepoKey, ev.RepoPath)
			}
		})
	}
}

// A missing draft field is not a draft, so a Bitbucket pull request moves to
// In Review when it is opened (T17 open point 3).
func TestMissingDraftFieldIsNotADraft(t *testing.T) {
	for _, c := range []struct{ key, file string }{{EventCloudCreated, "cloud_pr_created.json"}, {EventDCOpened, "dc_pr_opened.json"}} {
		if strings.Contains(string(fixture(t, c.file)), `"draft"`) {
			t.Fatalf("%s must not have a draft field", c.file)
		}
		ev := parse(t, c.key, c.file)
		if ev.Draft || ev.Plan().Move != scm.MoveToReview {
			t.Fatalf("%s: %+v", c.file, ev)
		}
	}
}

func TestEvent_EventTimes(t *testing.T) {
	cl := parse(t, EventCloudCreated, "cloud_pr_created.json")
	if want := time.Date(2026, 10, 26, 10, 15, 30, 123456000, time.UTC); !cl.OccurredAt.Equal(want) {
		t.Fatalf("cloud time = %v, want %v", cl.OccurredAt, want)
	}
	d := parse(t, EventDCOpened, "dc_pr_opened.json")
	if want := time.UnixMilli(1793008530123).UTC(); !d.OccurredAt.Equal(want) {
		t.Fatalf("dc time = %v, want %v", d.OccurredAt, want)
	}
}

func TestChecks_Events(t *testing.T) {
	tests := []struct {
		key   string
		file  string
		state scm.ChecksState
		act   scm.Action
	}{
		{EventCloudStatusNew, "cloud_status_inprogress.json", scm.ChecksPending, scm.ActionChecks},
		{EventCloudStatusUpd, "cloud_status_successful.json", scm.ChecksSuccess, scm.ActionChecks},
		{EventCloudStatusUpd, "cloud_status_failed.json", scm.ChecksFailure, scm.ActionChecks},
		{EventCloudStatusUpd, "cloud_status_stopped.json", scm.ChecksFailure, scm.ActionChecks},
		{EventCloudStatusUpd, "cloud_status_href_only.json", scm.ChecksSuccess, scm.ActionChecks},
		{EventCloudStatusUpd, "cloud_status_unknown_state.json", scm.ChecksNone, ""},
		{EventDCBuildStatus, "dc_build_inprogress.json", scm.ChecksPending, scm.ActionChecks},
		{EventDCBuildStatus, "dc_build_successful.json", scm.ChecksSuccess, scm.ActionChecks},
		{EventDCBuildStatus, "dc_build_failed.json", scm.ChecksFailure, scm.ActionChecks},
		{EventDCBuildStatus, "dc_build_commit_object.json", scm.ChecksSuccess, scm.ActionChecks},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			ev := parse(t, tt.key, tt.file)
			if ev.Kind != scm.KindChecks || ev.Action != tt.act || ev.Checks != tt.state || ev.HeadSHA != headSHA ||
				ev.CheckName != "ci-main" || ev.Number != 0 || ev.OccurredAt.IsZero() || ev.Plan().Checks != (tt.act != "") {
				t.Fatalf("event %+v plan %+v", ev, ev.Plan())
			}
			if strings.HasPrefix(tt.file, "cloud_") != (ev.RepoKey == cloudKey) {
				t.Fatalf("repo key = %q", ev.RepoKey)
			}
		})
	}
}

func TestParse_Ignored(t *testing.T) {
	for _, key := range []string{"", "diagnostics:ping", "repo:push", "pullrequest:comment_created", "pr:comment:added", "pr:deleted", "repo:refs_changed"} {
		_, res, err := Provider{}.Parse(hdr(HeaderEvent, key), []byte("not even json"))
		if err != nil || res != scm.ParseIgnored {
			t.Fatalf("%q: res = %v err = %v", key, res, err)
		}
	}
}

func TestParse_Errors(t *testing.T) {
	bad := []struct{ key, body string }{
		{EventCloudCreated, `{`},
		{EventCloudCreated, `{"pullrequest":{"id":0},"repository":{}}`},
		{EventCloudCreated, `{"pullrequest":{"id":1},"repository":{"uuid":"{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}"}}`}, // no workspace
		{EventCloudCreated, `{"pullrequest":{"id":1},"repository":{"uuid":"nope","workspace":{"uuid":"{11111111-2222-3333-4444-555555555555}"}}}`},
		{EventCloudCreated, `{"pullrequest":{"id":99999999999},"repository":{"uuid":"{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}","workspace":{"uuid":"{11111111-2222-3333-4444-555555555555}"}}}`},
		{EventDCOpened, `{"pullRequest":{"id":1}}`},
		{EventDCOpened, `{"pullRequest":{"id":1,"toRef":{"repository":{"id":0}}}}`},
		{EventCloudStatusNew, `{"repository":{"uuid":"{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}","workspace":{"uuid":"{11111111-2222-3333-4444-555555555555}"}},"commit_status":{"state":"SUCCESSFUL"}}`},
		{EventDCBuildStatus, `{"repository":{"id":1},"buildStatus":{"state":"SUCCESSFUL"}}`},
		{EventDCBuildStatus, `{"commit":"abcdef1234","buildStatus":{"state":"SUCCESSFUL"}}`},
	}
	for i, b := range bad {
		if _, res, err := (Provider{}).Parse(hdr(HeaderEvent, b.key), []byte(b.body)); err == nil || res != scm.ParseOK {
			t.Fatalf("case %d: res = %v err = %v", i, res, err)
		}
	}
}

func TestCloudKeyFallsBackToOwnerAndIsLowerCase(t *testing.T) {
	body := `{"pullrequest":{"id":3,"state":"OPEN","source":{"repository":{"uuid":"{AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE}"}},
	  "destination":{"repository":{"uuid":"{AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE}"}}},
	  "repository":{"uuid":"{AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE}","owner":{"uuid":"{11111111-2222-3333-4444-555555555555}"}}}`
	ev, _, err := Provider{}.Parse(hdr(HeaderEvent, EventCloudCreated), []byte(body))
	if err != nil || ev.RepoKey != cloudKey || ev.Fork {
		t.Fatalf("ev = %+v err = %v", ev, err)
	}
}

func TestCanonicalRepoKey(t *testing.T) {
	good := map[string]string{"4242": "4242", "0042": "42", cloudKey: cloudKey,
		strings.ToUpper(cloudKey): cloudKey}
	for in, want := range good {
		if got, ok := CanonicalRepoKey(in); !ok || got != want {
			t.Fatalf("%q -> %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "0", "-1", "12345678901234567890", "abc", "{x}/{y}", cloudKey + "/x", "PROJ/slug", "4242#1"} {
		if _, ok := CanonicalRepoKey(in); ok {
			t.Fatalf("%q must not be a repo key", in)
		}
	}
}

func TestVerify_SignatureKinds(t *testing.T) {
	body := fixture(t, "cloud_pr_created.json")
	secret := []byte("s3cret-current")
	pending := []byte("s3cret-pending")
	p := Provider{}
	for name, sec := range map[string][]byte{"current": secret, "pending": pending} {
		h := hdr(HeaderSignature, Sign(sec, body))
		if !p.Verify(h, body, sec) {
			t.Fatalf("%s secret: good signature refused", name)
		}
	}
	h := hdr(HeaderSignature, Sign(secret, body))
	if p.Verify(h, body, pending) {
		t.Fatal("the pending secret accepted the current secret's signature")
	}
	if p.Verify(h, append(body, ' '), secret) {
		t.Fatal("a changed body was accepted")
	}
	if p.Verify(h, body, nil) || p.Verify(h, body, []byte{}) {
		t.Fatal("an empty secret matched")
	}
	for _, v := range []string{"", "sha256=", "sha1=" + strings.Repeat("a", 40), "sha256=" + strings.Repeat("z", 64), Sign(secret, body)[:len(Sign(secret, body))-1]} {
		if p.Verify(hdr(HeaderSignature, v), body, secret) {
			t.Fatalf("malformed signature %q accepted", v)
		}
		if _, ok := ParseSignature(hdr(HeaderSignature, v)); ok {
			t.Fatalf("malformed signature %q parsed", v)
		}
	}
	if p.Verify(http.Header{}, body, secret) {
		t.Fatal("a missing header was accepted")
	}
}

func TestDeliveryKeys(t *testing.T) {
	p := Provider{}
	ev := parse(t, EventCloudCreated, "cloud_pr_created.json")
	keys, err := p.DeliveryKeys(hdr(HeaderRequestUUID, "{req-uuid-1}"), ev)
	want := "bb:{req-uuid-1}:r{11111111-2222-3333-4444-555555555555}_{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}:pr17:opened"
	if err != nil || len(keys) != 1 || keys[0] != want {
		t.Fatalf("keys = %v err = %v", keys, err)
	}
	// Data Center: X-Request-Id; the hook UUID is never part of a key.
	dcEv := parse(t, EventDCMerged, "dc_pr_merged.json")
	keys, err = p.DeliveryKeys(hdr(HeaderRequestID, "req-9", "X-Hook-UUID", "{hook}"), dcEv)
	if err != nil || len(keys) != 1 || keys[0] != "bb:req-9:r4242:pr17:merged" {
		t.Fatalf("keys = %v err = %v", keys, err)
	}
	// A check event has the sha form and no pull request number.
	ck := parse(t, EventCloudStatusNew, "cloud_status_inprogress.json")
	keys, err = p.DeliveryKeys(hdr(HeaderRequestUUID, "d1"), ck)
	if err != nil || keys[0] != "bb:d1:r{11111111-2222-3333-4444-555555555555}_{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}:sha0a1b2c3d4e5f:checks" {
		t.Fatalf("check keys = %v err = %v", keys, err)
	}
	// The key differs per action and per delivery.
	a, _ := p.DeliveryKeys(hdr(HeaderRequestID, "r1"), dcEv)
	b, _ := p.DeliveryKeys(hdr(HeaderRequestID, "r2"), dcEv)
	if a[0] == b[0] {
		t.Fatal("two deliveries share a key")
	}
	for _, bad := range []string{"", strings.Repeat("a", 101), "has space", "tab\there", "nonascii-é"} {
		if _, err := p.DeliveryKeys(hdr(HeaderRequestUUID, bad), ev); err == nil {
			t.Fatalf("delivery %q accepted", bad)
		}
	}
	long := ev
	long.RepoKey = strings.Repeat("1", 201)
	if _, err := p.DeliveryKeys(hdr(HeaderRequestUUID, "ok"), long); err == nil {
		t.Fatal("a 201-byte repo key was accepted")
	}
	if _, err := p.DeliveryKeys(hdr(), ev); err == nil {
		t.Fatal("no delivery header was accepted")
	}
}

func TestClient_IsTheGuardedGitapiClient(t *testing.T) {
	// With no guarded HTTP client there is no API client: nothing can dial around netguard.
	if _, err := (Provider{}).Client(scm.ClientConfig{BaseURL: "https://bitbucket.org", Token: "t"}); err == nil {
		t.Fatal("a client was built with no guarded HTTP client")
	}
	p := Provider{Options: gitapitest.Options(nil)}
	for _, base := range []string{"https://bitbucket.org", "https://bitbucket.example.test"} {
		if _, err := p.Client(scm.ClientConfig{BaseURL: base, Token: "t"}); err != nil {
			t.Fatal(base, err)
		}
	}
}
