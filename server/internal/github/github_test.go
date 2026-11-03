package github

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"devops/internal/scm"
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

func TestEvent_Actions(t *testing.T) {
	tests := []struct {
		file   string
		action scm.Action
		draft  bool
		fork   bool
		move   scm.Move
		noop   bool
	}{
		{"pr_opened.json", scm.ActionOpened, false, false, scm.MoveToReview, false},
		{"pr_opened_draft.json", scm.ActionOpened, true, false, scm.MoveNone, false},
		{"pr_ready_for_review.json", scm.ActionReady, false, false, scm.MoveToReview, false},
		{"pr_converted_to_draft.json", scm.ActionDrafted, true, false, scm.MoveNone, false},
		{"pr_synchronize.json", scm.ActionUpdated, false, false, scm.MoveNone, false},
		{"pr_edited.json", scm.ActionUpdated, false, false, scm.MoveNone, false},
		{"pr_reopened.json", scm.ActionReopened, false, false, scm.MoveToReview, false},
		{"pr_closed_merged.json", scm.ActionMerged, false, false, scm.MoveToTesting, false},
		{"pr_closed_unmerged.json", scm.ActionClosed, false, false, scm.MoveNone, false},
		{"pr_labeled.json", "", false, false, scm.MoveNone, true},
		{"pr_fork_opened.json", scm.ActionOpened, false, true, scm.MoveToReview, false},
		{"pr_fork_closed_merged.json", scm.ActionMerged, false, true, scm.MoveToTesting, false},
		{"pr_deleted_fork_opened.json", scm.ActionOpened, false, true, scm.MoveToReview, false},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			ev, res, err := Provider{}.Parse(hdr(HeaderEvent, EventPullRequest), fixture(t, tt.file))
			if err != nil || res != scm.ParseOK {
				t.Fatalf("parse = %v, %v", res, err)
			}
			p := ev.Plan()
			if ev.Action != tt.action || ev.Draft != tt.draft || ev.Fork != tt.fork || p.Move != tt.move || p.Noop() != tt.noop {
				t.Fatalf("event %+v plan %+v", ev, p)
			}
			if ev.Provider != "github" || ev.Kind != scm.KindChange || ev.RepoKey != "4242" || ev.RepoPath != "example-org/example-app" ||
				ev.Number != 17 || ev.Author != "example-dev" || ev.SourceBranch != "chen/fix-login-redirect" ||
				ev.URL != "https://github.com/example-org/example-app/pull/17" || ev.OccurredAt.IsZero() {
				t.Fatalf("event fields = %+v", ev)
			}
		})
	}
}

// LAUN-83: review deliveries set the review state; nothing moves a ticket.
func TestEvent_Reviews(t *testing.T) {
	tests := []struct {
		file   string
		action scm.Action
		review scm.Review
		noop   bool
	}{
		{"review_approved.json", scm.ActionApproved, scm.ReviewApproved, false},
		{"review_changes_requested.json", scm.ActionChangesRequested, scm.ReviewChangesRequested, false},
		{"review_dismissed.json", scm.ActionUnapproved, scm.ReviewNone, false},
		{"review_commented.json", "", scm.ReviewNone, true},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			ev, res, err := Provider{}.Parse(hdr(HeaderEvent, EventPullRequestReview), fixture(t, tt.file))
			if err != nil || res != scm.ParseOK {
				t.Fatalf("parse = %v, %v", res, err)
			}
			p := ev.Plan()
			if ev.Kind != scm.KindReview || ev.Action != tt.action || ev.Review != tt.review || p.Noop() != tt.noop || p.Move != scm.MoveNone {
				t.Fatalf("event %+v plan %+v", ev, p)
			}
			if ev.Number != 17 || ev.HeadSHA == "" || ev.OccurredAt.IsZero() || ev.Fork {
				t.Fatalf("event fields = %+v", ev)
			}
			if !tt.noop && ev.Plan().SetReview && ev.ReviewValue() != tt.review {
				t.Fatalf("ReviewValue = %q", ev.ReviewValue())
			}
		})
	}
	// The same body under the pull_request header is not a review.
	if _, res, _ := (Provider{}).Parse(hdr(HeaderEvent, "pull_request_review_comment"), fixture(t, "review_approved.json")); res != scm.ParseIgnored {
		t.Fatalf("review comment event = %v, want ignored", res)
	}
}

func TestEvent_ForkKeys(t *testing.T) {
	ev, _, _ := Provider{}.Parse(hdr(HeaderEvent, EventPullRequest), fixture(t, "pr_fork_opened.json"))
	if ev.SourceRepoKey != "7777" {
		t.Fatalf("SourceRepoKey = %q", ev.SourceRepoKey)
	}
	ev, _, _ = Provider{}.Parse(hdr(HeaderEvent, EventPullRequest), fixture(t, "pr_deleted_fork_opened.json"))
	if ev.SourceRepoKey != "" || !ev.Fork {
		t.Fatalf("deleted fork: %+v", ev)
	}
}

func TestParse_Rejects(t *testing.T) {
	for name, body := range map[string]string{
		"not json":     "nope",
		"no action":    `{"number":1,"repository":{"id":1}}`,
		"no repo":      `{"action":"opened","number":1}`,
		"no number":    `{"action":"opened","repository":{"id":1}}`,
		"huge number":  `{"action":"opened","number":99999999999,"repository":{"id":1}}`,
		"negative num": `{"action":"opened","number":-3,"repository":{"id":1}}`,
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParse_OtherEventsAreIgnored(t *testing.T) {
	for _, e := range []string{EventPing, "push", "issues", ""} {
		_, res, err := Provider{}.Parse(hdr(HeaderEvent, e), []byte("not json"))
		if res != scm.ParseIgnored || err != nil {
			t.Errorf("event %q: %v, %v", e, res, err)
		}
	}
}

func TestVerify(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	body := fixture(t, "pr_opened.json")
	good := Sign(secret, body)
	var p Provider
	if !p.Verify(hdr(HeaderSignature, good), body, secret) {
		t.Fatal("a good signature failed")
	}
	bad := map[string]http.Header{
		"wrong secret": hdr(HeaderSignature, Sign([]byte("another-secret-another-secret-123"), body)),
		"wrong body":   hdr(HeaderSignature, Sign(secret, append([]byte(" "), body...))),
		"no header":    hdr(),
		"no prefix":    hdr(HeaderSignature, good[len("sha256="):]),
		"sha1 prefix":  hdr(HeaderSignature, "sha1="+good[len("sha256="):]),
		"short":        hdr(HeaderSignature, good[:len(good)-2]),
		"long":         hdr(HeaderSignature, good+"00"),
		"not hex":      hdr(HeaderSignature, "sha256="+strings.Repeat("zz", 32)),
		"upper prefix": hdr(HeaderSignature, "SHA256="+good[len("sha256="):]),
	}
	for name, h := range bad {
		if p.Verify(h, body, secret) {
			t.Errorf("%s: verified", name)
		}
	}
	if p.Verify(hdr(HeaderSignature, Sign(nil, body)), body, nil) {
		t.Error("an empty secret verified")
	}
	// Upper-case hex is the same signature.
	if !p.Verify(hdr(HeaderSignature, "sha256="+strings.ToUpper(good[len("sha256="):])), body, secret) {
		t.Error("upper-case hex failed")
	}
}

func TestDeliveryKeys(t *testing.T) {
	ev, _, _ := Provider{}.Parse(hdr(HeaderEvent, EventPullRequest), fixture(t, "pr_closed_merged.json"))
	keys, err := Provider{}.DeliveryKeys(hdr(HeaderDelivery, "72d3162e-cc78-11e3-81ab-4c9367dc0958"), ev)
	if err != nil || len(keys) != 1 || keys[0] != "gh:72d3162e-cc78-11e3-81ab-4c9367dc0958:r4242:pr17:merged" {
		t.Fatalf("keys = %v, %v", keys, err)
	}
	// Two PR actions of one delivery GUID (not expected from GitHub, but the key tells them apart).
	ev.Action = "weird:action"
	if keys, _ := (Provider{}).DeliveryKeys(hdr(HeaderDelivery, "g"), ev); keys[0] != "gh:g:r4242:pr17:weirdaction" {
		t.Fatalf("key = %v", keys)
	}
	ev.Action = ""
	if keys, _ := (Provider{}).DeliveryKeys(hdr(HeaderDelivery, "g"), ev); keys[0] != "gh:g:r4242:pr17:x" {
		t.Fatalf("key = %v", keys)
	}
	for _, d := range []string{"", strings.Repeat("a", 101), "has space", "tab\t", "é"} {
		if _, err := (Provider{}).DeliveryKeys(hdr(HeaderDelivery, d), ev); err == nil {
			t.Errorf("delivery %q accepted", d)
		}
	}
	ev.RepoKey = strings.Repeat("9", 201)
	if _, err := (Provider{}).DeliveryKeys(hdr(HeaderDelivery, "g"), ev); err == nil {
		t.Error("a 201-byte repo key was accepted")
	}
}

func TestClientNotWired(t *testing.T) {
	if c, err := (Provider{}).Client(scm.ClientConfig{}); c != nil || err == nil {
		t.Fatalf("Client = %v, %v", c, err)
	}
}

// LAUN-85: check events.
func TestChecks_Fixtures(t *testing.T) {
	for _, c := range []struct {
		file, event, name string
		state             scm.ChecksState
		action            scm.Action
	}{
		{"check_suite_completed_success.json", EventCheckSuite, "example-ci", scm.ChecksSuccess, scm.ActionChecks},
		{"check_suite_completed_failure.json", EventCheckSuite, "example-lint", scm.ChecksFailure, scm.ActionChecks},
		{"check_suite_requested.json", EventCheckSuite, "example-ci", scm.ChecksPending, scm.ActionChecks},
		{"status_success.json", EventStatus, "ci/example-build", scm.ChecksSuccess, scm.ActionChecks},
		{"status_error.json", EventStatus, "ci/example-build", scm.ChecksFailure, scm.ActionChecks},
	} {
		ev, res, err := Provider{}.Parse(hdr(HeaderEvent, c.event), fixture(t, c.file))
		if err != nil || res != scm.ParseOK {
			t.Fatalf("%s: %v %v", c.file, res, err)
		}
		if ev.Kind != scm.KindChecks || ev.Action != c.action || ev.Checks != c.state || ev.CheckName != c.name ||
			ev.RepoKey != "4242" || ev.HeadSHA != "fedcba9876543210fedcba9876543210fedcba98" || ev.OccurredAt.IsZero() || ev.Number != 0 {
			t.Errorf("%s: %+v", c.file, ev)
		}
		if p := ev.Plan(); !p.Checks || p.Link || p.Move != scm.MoveNone || p.Noop() {
			t.Errorf("%s: plan %+v", c.file, p)
		}
	}
}

func TestChecks_States(t *testing.T) {
	for _, c := range []struct {
		action, status, conclusion string
		want                       scm.ChecksState
		ok                         bool
	}{
		{"completed", "completed", "neutral", scm.ChecksSuccess, true},
		{"completed", "completed", "skipped", scm.ChecksSuccess, true},
		{"completed", "completed", "timed_out", scm.ChecksFailure, true},
		{"completed", "completed", "cancelled", scm.ChecksFailure, true},
		{"completed", "completed", "action_required", scm.ChecksFailure, true},
		{"completed", "completed", "stale", scm.ChecksPending, true},
		{"completed", "completed", "surprise", scm.ChecksNone, false},
		{"completed", "completed", "", scm.ChecksNone, false},
		{"rerequested", "queued", "", scm.ChecksPending, true},
		{"requested", "", "", scm.ChecksPending, true},
		{"requested", "in_progress", "", scm.ChecksPending, true},
		{"requested", "weird", "", scm.ChecksNone, false},
	} {
		got, ok := checkSuiteState(c.action, c.status, c.conclusion)
		if got != c.want || ok != c.ok {
			t.Errorf("%+v: %v %v", c, got, ok)
		}
	}
	ev := StatusEvent{SHA: "abc", Context: " ", State: "weird"}
	ev.Repository.ID = 1
	if e := ev.Event(); e.Action != "" || e.CheckName != "check" || e.Plan().Checks {
		t.Errorf("unknown status state: %+v", e)
	}
}

func TestChecks_Rejects(t *testing.T) {
	for _, b := range []string{`{`, `{"action":"completed","check_suite":{"head_sha":"abc"}}`, `{"repository":{"id":1},"check_suite":{}}`} {
		if _, _, err := (Provider{}).Parse(hdr(HeaderEvent, EventCheckSuite), []byte(b)); err == nil {
			t.Errorf("check_suite %s accepted", b)
		}
	}
	for _, b := range []string{`{`, `{"sha":"abc","state":"success"}`, `{"repository":{"id":1},"state":"success"}`} {
		if _, _, err := (Provider{}).Parse(hdr(HeaderEvent, EventStatus), []byte(b)); err == nil {
			t.Errorf("status %s accepted", b)
		}
	}
}

func TestChecks_DeliveryKey(t *testing.T) {
	ev, _, _ := Provider{}.Parse(hdr(HeaderEvent, EventCheckSuite), fixture(t, "check_suite_completed_success.json"))
	keys, err := Provider{}.DeliveryKeys(hdr(HeaderDelivery, "d-1"), ev)
	if err != nil || len(keys) != 1 || keys[0] != "gh:d-1:r4242:shafedcba987654:checks" {
		t.Fatalf("keys = %v, %v", keys, err)
	}
	// No pull request number, and a status of the same commit has the same shape.
	st, _, _ := Provider{}.Parse(hdr(HeaderEvent, EventStatus), fixture(t, "status_success.json"))
	if k, _ := (Provider{}).DeliveryKeys(hdr(HeaderDelivery, "d-2"), st); k[0] != "gh:d-2:r4242:shafedcba987654:checks" {
		t.Fatalf("status key = %v", k)
	}
	st.HeadSHA = "ab:c"
	if k, _ := (Provider{}).DeliveryKeys(hdr(HeaderDelivery, "d-3"), st); k[0] != "gh:d-3:r4242:shaabc:checks" {
		t.Fatalf("short sha key = %v", k)
	}
}
