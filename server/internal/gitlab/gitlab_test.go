package gitlab

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"devops/internal/scm"
)

func loadFixture(t *testing.T, name string) MergeRequestEvent {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := Parse(b)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return ev
}

// TestParse_Fixtures reads every fixture: the fields the tracker uses come
// out, and the plan is the documented one. The fixtures are hand-written
// from GitLab's documented payload shape, not recorded deliveries
// (testdata/README.md).
func TestParse_Fixtures(t *testing.T) {
	tests := []struct {
		file    string
		action  string
		state   string
		iid     int64
		draft   bool
		plan    scm.Plan
		comment string
	}{
		{"mr_open.json", "open", "opened", 17, false, scm.Plan{Link: true, Move: scm.MoveToReview, MRState: "opened"}, "open moves to review"},
		{"mr_draft_open.json", "open", "opened", 18, true, scm.Plan{Link: true, Move: scm.MoveNone, MRState: "opened"}, "a draft links but does not move"},
		{"mr_update.json", "update", "opened", 17, false, scm.Plan{Refresh: true}, "a title edit refreshes the links"},
		{"mr_approved.json", "approved", "opened", 17, false, scm.Plan{SetReview: true}, "an approval sets the review and moves nothing"},
		{"mr_unapproved.json", "unapproved", "opened", 17, false, scm.Plan{SetReview: true}, "an unapproval clears the review"},
		{"mr_update_ready.json", "update", "opened", 18, false, scm.Plan{Link: true, Move: scm.MoveToReview, MRState: "opened"}, "marked ready moves to review"},
		{"mr_reopen.json", "reopen", "opened", 17, false, scm.Plan{Link: true, Move: scm.MoveToReview, MRState: "opened", Reopen: true}, "reopen moves to review"},
		{"mr_merge.json", "merge", "merged", 17, false, scm.Plan{Link: true, Move: scm.MoveToTesting, MRState: "merged"}, "merge moves to testing"},
		{"mr_close.json", "close", "closed", 17, false, scm.Plan{CloseLinks: true, MRState: "closed"}, "close does not move"},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			ev := loadFixture(t, tt.file)
			if ev.Project.ID != 42 || ev.Project.PathWithNamespace != "example-org/example-app" {
				t.Errorf("project = %d %q", ev.Project.ID, ev.Project.PathWithNamespace)
			}
			a := ev.Attrs
			if a.Action != tt.action || a.State != tt.state || a.IID != tt.iid {
				t.Errorf("action/state/iid = %q/%q/%d, want %q/%q/%d", a.Action, a.State, a.IID, tt.action, tt.state, tt.iid)
			}
			if ev.IsDraft() != tt.draft {
				t.Errorf("IsDraft = %v, want %v", ev.IsDraft(), tt.draft)
			}
			if !strings.HasPrefix(a.URL, "https://gitlab.example.com/example-org/example-app/-/merge_requests/") {
				t.Errorf("url = %q", a.URL)
			}
			if !strings.Contains(strings.ToLower(a.SourceBranch), "orb-1-") {
				t.Errorf("source branch = %q", a.SourceBranch)
			}
			if got := ev.Plan(); got != tt.plan {
				t.Errorf("%s: plan = %+v, want %+v", tt.comment, got, tt.plan)
			}
		})
	}
}

func TestParse_Rejects(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"not json", `{"object_kind":`, "invalid JSON"},
		{"push event", `{"object_kind":"push","project":{"id":1},"object_attributes":{"iid":1}}`, "object_kind"},
		{"no project", `{"object_kind":"merge_request","object_attributes":{"iid":1}}`, "project.id"},
		{"no iid", `{"object_kind":"merge_request","project":{"id":1},"object_attributes":{}}`, "iid"},
		{"iid overflows int32", `{"object_kind":"merge_request","project":{"id":1},"object_attributes":{"iid":2147483648}}`, "iid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestPlan_Actions(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name   string
		action string
		state  string
		draft  *bool
		wip    *bool
		change *BoolChange
		wipChg *BoolChange
		want   scm.Plan
	}{
		{name: "approved sets the review", action: "approved", state: "opened", want: scm.Plan{SetReview: true}},
		{name: "unknown action", action: "", state: "opened", want: scm.Plan{}},
		{name: "reopen of a draft does not move", action: "reopen", state: "opened", draft: &yes, want: scm.Plan{Link: true, MRState: "opened", Reopen: true}},
		{name: "old payload: work_in_progress only", action: "open", state: "opened", wip: &yes, want: scm.Plan{Link: true, MRState: "opened"}},
		{name: "draft field wins over wip", action: "open", state: "opened", draft: &no, wip: &yes, want: scm.Plan{Link: true, MRState: "opened", Move: scm.MoveToReview}},
		{name: "ready via old wip change", action: "update", state: "opened", wipChg: &BoolChange{&yes, &no}, want: scm.Plan{Link: true, MRState: "opened", Move: scm.MoveToReview}},
		{name: "ready to draft does nothing", action: "update", state: "opened", change: &BoolChange{&no, &yes}, want: scm.Plan{Refresh: true}},
		{name: "ready on a closed MR does nothing", action: "update", state: "closed", change: &BoolChange{&yes, &no}, want: scm.Plan{Refresh: true}},
		{name: "half a change does nothing", action: "update", state: "opened", change: &BoolChange{Previous: &yes}, want: scm.Plan{Refresh: true}},
		{name: "merge of a draft still moves", action: "merge", state: "merged", draft: &yes, want: scm.Plan{Link: true, MRState: "merged", Move: scm.MoveToTesting}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ev MergeRequestEvent
			ev.Attrs.Action, ev.Attrs.State, ev.Attrs.Draft, ev.Attrs.WorkInProgress = tt.action, tt.state, tt.draft, tt.wip
			ev.Changes.Draft, ev.Changes.WorkInProgress = tt.change, tt.wipChg
			if got := ev.Plan(); got != tt.want {
				t.Fatalf("plan = %+v, want %+v", got, tt.want)
			}
			if got := ev.Plan().Noop(); got != (!tt.want.Link && !tt.want.CloseLinks && !tt.want.Refresh && !tt.want.SetReview) {
				t.Fatalf("Noop = %v", got)
			}
		})
	}
}

func TestDeliveryKeys(t *testing.T) {
	ev := func(project, iid int64, action string) MergeRequestEvent {
		var e MergeRequestEvent
		e.Project.ID, e.Attrs.IID, e.Attrs.Action = project, iid, action
		return e
	}
	const u = "3f0e1c2a-9b8d-4e6f-a1b2-c3d4e5f6a7b8"
	tests := []struct {
		name string
		ik   string
		ev   MergeRequestEvent
		want []string
	}{
		{"both headers: ik first, then ev", "abc-123", ev(42, 17, "merge"), []string{"gl:ik:abc-123:p42:mr17:merge", "gl:ev:" + u + ":p42:mr17:merge"}},
		{"another mr under the same idempotency key differs", "abc-123", ev(42, 18, "merge"), []string{"gl:ik:abc-123:p42:mr18:merge", "gl:ev:" + u + ":p42:mr18:merge"}},
		{"event uuid only", "", ev(42, 17, "merge"), []string{"gl:ev:" + u + ":p42:mr17:merge"}},
		{"action is cleaned and cut", "", ev(1, 1, "a:b c\n"+strings.Repeat("x", 100)), []string{"gl:ev:" + u + ":p1:mr1:abc" + strings.Repeat("x", 29)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeliveryKeys(tt.ik, u, tt.ev); !slices.Equal(got, tt.want) {
				t.Fatalf("DeliveryKeys = %q, want %q", got, tt.want)
			}
		})
	}
	for _, ik := range []string{"", "k"} {
		a, b := DeliveryKeys(ik, u, ev(42, 17, "merge")), DeliveryKeys(ik, u, ev(42, 18, "merge"))
		for _, ka := range a {
			if slices.Contains(b, ka) {
				t.Fatalf("two MRs of one GitLab request share a key (ik=%q): %q", ik, ka)
			}
		}
	}
}

// The matcher rules are tested in internal/scm; these two read the fixture.
func TestRefs_Fixture(t *testing.T) {
	ev := loadFixture(t, "mr_open.json")
	got := scm.Refs(func(k string) bool { return k == "ORB" }, ev.Attrs.SourceBranch, ev.Attrs.Title, ev.Attrs.Description)
	want := []scm.Ref{{Key: "ORB", Number: 1}, {Key: "ORB", Number: 2}}
	if !slices.Equal(got, want) {
		t.Fatalf("Refs = %v, want %v", got, want)
	}
}

func TestMRRefs_Fixture(t *testing.T) {
	keep := func(k string) bool { return k == "ORB" }
	// ORB-1 in branch and title moves; the "Refs ORB-2" in the description is linked only.
	ev := loadFixture(t, "mr_open.json")
	got := scm.MRRefs(keep, ev.Attrs.SourceBranch, ev.Attrs.Title, ev.Attrs.Description)
	if want := []scm.LinkRef{{Ref: scm.Ref{Key: "ORB", Number: 1}, Move: true}, {Ref: scm.Ref{Key: "ORB", Number: 2}}}; !slices.Equal(got, want) {
		t.Fatalf("fixture MRRefs = %#v, want %#v", got, want)
	}
}

func TestProvider(t *testing.T) {
	var p Provider
	if p.Name() != "gitlab" {
		t.Fatal(p.Name())
	}
	h := http.Header{}
	h.Set(HeaderToken, "s3cret")
	if !p.Verify(h, nil, []byte("s3cret")) || p.Verify(h, nil, []byte("other")) || p.Verify(http.Header{}, nil, []byte("")) || p.Verify(http.Header{}, nil, nil) {
		t.Fatal("Verify")
	}
	if _, err := p.Client(scm.ClientConfig{}); err == nil {
		t.Fatal("Client should report no API client yet")
	}
	// Another event type is ignored and its body is not read.
	h.Set(HeaderEvent, "Push Hook")
	if _, res, err := p.Parse(h, []byte("not json")); err != nil || res != scm.ParseIgnored {
		t.Fatalf("push hook: %v %v", res, err)
	}
	h.Set(HeaderEvent, MergeRequestHook)
	if _, _, err := p.Parse(h, []byte("not json")); err == nil {
		t.Fatal("bad body must fail")
	}
}

// TestProvider_Fixtures: Parse gives the neutral event, and DeliveryKeys of
// it equals the keys of the raw payload (so keys stored before the refactor
// still match).
func TestProvider_Fixtures(t *testing.T) {
	const u = "3f0e1c2a-9b8d-4e6f-a1b2-c3d4e5f6a7b8"
	tests := []struct {
		file   string
		action scm.Action
	}{
		{"mr_open.json", scm.ActionOpened}, {"mr_draft_open.json", scm.ActionOpened},
		{"mr_update.json", scm.ActionUpdated}, {"mr_update_ready.json", scm.ActionReady},
		{"mr_reopen.json", scm.ActionReopened}, {"mr_merge.json", scm.ActionMerged}, {"mr_close.json", scm.ActionClosed},
	}
	var p Provider
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", tt.file))
			if err != nil {
				t.Fatal(err)
			}
			h := http.Header{}
			h.Set(HeaderEvent, MergeRequestHook)
			h.Set(HeaderEventUUID, u)
			h.Set(HeaderIdempotencyKey, "idem-1")
			ev, res, err := p.Parse(h, b)
			if err != nil || res != scm.ParseOK {
				t.Fatalf("parse: %v %v", res, err)
			}
			raw := loadFixture(t, tt.file)
			if ev.Action != tt.action || ev.Provider != "gitlab" || ev.Kind != scm.KindChange ||
				ev.RepoKey != "42" || ev.RepoPath != "example-org/example-app" || ev.Number != raw.Attrs.IID ||
				ev.URL != raw.Attrs.URL || ev.Title != raw.Attrs.Title || ev.SourceBranch != raw.Attrs.SourceBranch ||
				ev.Draft != raw.IsDraft() {
				t.Fatalf("event = %+v", ev)
			}
			if ev.Plan() != raw.Plan() {
				t.Fatalf("plan differs")
			}
			got, err := p.DeliveryKeys(h, ev)
			if err != nil || !slices.Equal(got, DeliveryKeys("idem-1", u, raw)) {
				t.Fatalf("keys = %q (%v), want %q", got, err, DeliveryKeys("idem-1", u, raw))
			}
		})
	}
	if _, err := p.DeliveryKeys(http.Header{}, scm.Event{}); err == nil {
		t.Fatal("missing Event UUID must fail")
	}
}

// An unknown GitLab action never becomes a real scm action, and its plan is empty.
func TestEvent_UnknownAction(t *testing.T) {
	for _, a := range []string{"", "approval", "unapproval", "merged", "opened", "ready"} {
		var mr MergeRequestEvent
		mr.Attrs.Action, mr.Attrs.State = a, "opened"
		ev := mr.Event()
		if ev.Action != "" || !ev.Plan().Noop() || rawAction(ev.Action) != "" {
			t.Fatalf("action %q: event action %q", a, ev.Action)
		}
	}
	for _, raw := range []string{"open", "reopen", "update", "merge", "close", "approved", "unapproved"} {
		var mr MergeRequestEvent
		mr.Attrs.Action, mr.Attrs.State = raw, "opened"
		if got := rawAction(mr.Event().Action); got != raw {
			t.Fatalf("rawAction round trip: %q -> %q", raw, got)
		}
	}
}

// LAUN-83: the event carries the head sha, the host time and, for an open,
// the author; an approval carries no author.
func TestEvent_StateFields(t *testing.T) {
	open := loadFixture(t, "mr_open.json").Event()
	if open.HeadSHA != "3f2a9c1e5b7d4a6c8e0f1a2b3c4d5e6f7a8b9c0d" || open.Author != "exampledev" ||
		!open.OccurredAt.Equal(time.Date(2026, 10, 24, 9, 12, 3, 0, time.UTC)) || open.RepoPath != "example-org/example-app" {
		t.Fatalf("open event = %+v", open)
	}
	appr := loadFixture(t, "mr_approved.json").Event()
	if appr.Action != scm.ActionApproved || appr.Author != "" || appr.ReviewValue() != scm.ReviewApproved ||
		!appr.OccurredAt.Equal(time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("approved event = %+v", appr)
	}
	if loadFixture(t, "mr_unapproved.json").Event().ReviewValue() != scm.ReviewNone {
		t.Fatal("an unapproval must clear the review")
	}
	for _, in := range []string{"", "yesterday", "2026-13-40 99:00:00 UTC"} {
		if !parseTime(in).IsZero() {
			t.Fatalf("parseTime(%q) is not the zero time", in)
		}
	}
	if got := parseTime("2026-10-24T09:12:03Z"); got.IsZero() {
		t.Fatal("the ISO form was not read")
	}
}

// LAUN-85: pipeline events.
func TestPipeline_Fixtures(t *testing.T) {
	const sha = "3f2a9c1e5b7d4a6c8e0f1a2b3c4d5e6f7a8b9c0d"
	for _, c := range []struct {
		file, name string
		state      scm.ChecksState
		at         time.Time
	}{
		{"pipeline_success.json", "Build and test", scm.ChecksSuccess, time.Date(2026, 10, 24, 10, 5, 0, 0, time.UTC)},
		{"pipeline_failed.json", "Deploy review", scm.ChecksFailure, time.Date(2026, 10, 24, 10, 5, 0, 0, time.UTC)},
		{"pipeline_running.json", "pipeline", scm.ChecksPending, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)},
	} {
		b, _ := os.ReadFile(filepath.Join("testdata", c.file))
		ev, res, err := Provider{}.Parse(hdr(HeaderEvent, PipelineHook), b)
		if err != nil || res != scm.ParseOK {
			t.Fatalf("%s: %v %v", c.file, res, err)
		}
		if ev.Kind != scm.KindChecks || ev.Action != scm.ActionChecks || ev.Checks != c.state || ev.CheckName != c.name ||
			ev.RepoKey != "42" || ev.HeadSHA != sha || !ev.OccurredAt.Equal(c.at) || ev.Number != 0 {
			t.Errorf("%s: %+v", c.file, ev)
		}
		if p := ev.Plan(); !p.Checks || p.Noop() {
			t.Errorf("%s: plan %+v", c.file, p)
		}
	}
}

func TestPipeline_StatesAndRejects(t *testing.T) {
	for st, want := range map[string]scm.ChecksState{"success": scm.ChecksSuccess, "skipped": scm.ChecksSuccess, "failed": scm.ChecksFailure,
		"canceled": scm.ChecksFailure, "created": scm.ChecksPending, "running": scm.ChecksPending, "manual": scm.ChecksPending, "canceling": scm.ChecksPending,
		"waiting_for_resource": scm.ChecksPending, "preparing": scm.ChecksPending, "pending": scm.ChecksPending, "scheduled": scm.ChecksPending} {
		if got, ok := pipelineState(st); !ok || got != want {
			t.Errorf("%s = %v %v", st, got, ok)
		}
	}
	if _, ok := pipelineState("surprise"); ok {
		t.Error("unknown status accepted")
	}
	for _, b := range []string{`{`, `{"object_kind":"push","project":{"id":1},"object_attributes":{"sha":"a"}}`,
		`{"object_kind":"pipeline","object_attributes":{"sha":"a"}}`, `{"object_kind":"pipeline","project":{"id":1},"object_attributes":{}}`} {
		if _, _, err := (Provider{}).Parse(hdr(HeaderEvent, PipelineHook), []byte(b)); err == nil {
			t.Errorf("%s accepted", b)
		}
	}
}

func TestPipeline_DeliveryKeys(t *testing.T) {
	b, _ := os.ReadFile(filepath.Join("testdata", "pipeline_success.json"))
	ev, _, _ := Provider{}.Parse(hdr(HeaderEvent, PipelineHook), b)
	keys, err := Provider{}.DeliveryKeys(hdr(HeaderEventUUID, "u-1", HeaderIdempotencyKey, "ik-1"), ev)
	want := []string{"gl:ik:ik-1:p42:sha3f2a9c1e5b7d:checks", "gl:ev:u-1:p42:sha3f2a9c1e5b7d:checks"}
	if err != nil || !slices.Equal(keys, want) {
		t.Fatalf("keys = %v, %v", keys, err)
	}
}

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}
