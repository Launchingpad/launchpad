package scm

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestRefs(t *testing.T) {
	teams := func(keys ...string) func(string) bool {
		return func(k string) bool { return slices.Contains(keys, k) }
	}
	tests := []struct {
		name  string
		keep  func(string) bool
		texts []string
		want  []string
	}{
		{"branch first, then title, then description", teams("ORB", "WEB"),
			[]string{"chen/orb-3409-dev-ops-t5", "WEB-2: copy", "Also ORB-7 and orb-3409"}, []string{"ORB-3409", "WEB-2", "ORB-7"}},
		{"case-insensitive and upper-cased", teams("ORB"), []string{"Orb-12"}, []string{"ORB-12"}},
		{"only team keys of the org", teams("ORB"), []string{"UTF-8 and SHA-256 and FOO-1 and ORB-5"}, []string{"ORB-5"}},
		{"words in the branch are not identifiers", teams("ORB", "OPS", "DEV"), []string{"chen/orb-1-dev-ops-in-review"}, []string{"ORB-1"}},
		{"underscore after the number is no boundary", teams("ORB"), []string{"feature/ORB-12_fix"}, []string{}},
		{"letters glued before the key are no boundary", teams("ORB"), []string{"xORB-12"}, []string{}},
		{"one-letter prefix never matches", teams("A"), []string{"A-1"}, []string{}},
		{"zero and overflow are skipped", teams("ORB"), []string{"ORB-0 ORB-2147483648 ORB-99999999999999999999 ORB-2147483647"}, []string{"ORB-2147483647"}},
		{"empty texts", teams("ORB"), []string{"", ""}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := []string{}
			for _, r := range Refs(tt.keep, tt.texts...) {
				got = append(got, r.String())
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("Refs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRefs_Cap(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&b, "ORB-%d ", i)
	}
	// Junk that is not a team key must not use up the cap.
	junk := strings.Repeat("UTF-8 ", 500)
	got := Refs(func(k string) bool { return k == "ORB" }, junk, b.String())
	if len(got) != MaxRefs {
		t.Fatalf("got %d refs, want the cap %d", len(got), MaxRefs)
	}
}

// TestMRRefs: only branch and title references may move; a description-only
// reference is linked with Move false; a reference in both moves (once).
func TestMRRefs(t *testing.T) {
	keep := func(k string) bool { return k == "ORB" }
	tests := []struct {
		name                       string
		branch, title, description string
		want                       []LinkRef
	}{
		{"branch moves, description does not", "chen/orb-1-x", "Fix", "Refs ORB-2",
			[]LinkRef{{Ref{"ORB", 1}, true}, {Ref{"ORB", 2}, false}}},
		{"title moves", "feature", "ORB-3: fix", "",
			[]LinkRef{{Ref{"ORB", 3}, true}}},
		{"in title and description: listed once, moves", "feature", "ORB-4: fix", "Closes ORB-4. See ORB-5.",
			[]LinkRef{{Ref{"ORB", 4}, true}, {Ref{"ORB", 5}, false}}},
		{"description only", "feature", "Fix", "ORB-6 and orb-7",
			[]LinkRef{{Ref{"ORB", 6}, false}, {Ref{"ORB", 7}, false}}},
		{"nothing", "feature", "Fix", "UTF-8", []LinkRef{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MRRefs(keep, tt.branch, tt.title, tt.description); !slices.Equal(got, tt.want) {
				t.Fatalf("MRRefs = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestEventPlan(t *testing.T) {
	tests := []struct {
		name string
		ev   Event
		want Plan
	}{
		{"opened moves to review", Event{Action: ActionOpened}, Plan{Link: true, Move: MoveToReview, MRState: "opened"}},
		{"opened draft links only", Event{Action: ActionOpened, Draft: true}, Plan{Link: true, MRState: "opened"}},
		{"reopened", Event{Action: ActionReopened}, Plan{Link: true, Move: MoveToReview, MRState: "opened", Reopen: true}},
		{"ready", Event{Action: ActionReady}, Plan{Link: true, Move: MoveToReview, MRState: "opened"}},
		{"merged draft still moves", Event{Action: ActionMerged, Draft: true}, Plan{Link: true, Move: MoveToTesting, MRState: "merged"}},
		{"closed", Event{Action: ActionClosed}, Plan{CloseLinks: true, MRState: "closed"}},
		{"updated refreshes", Event{Action: ActionUpdated}, Plan{Refresh: true}},
		{"drafted refreshes", Event{Action: ActionDrafted}, Plan{Refresh: true}},
		{"approved sets the review", Event{Action: ActionApproved}, Plan{SetReview: true}},
		{"unapproved sets the review", Event{Action: ActionUnapproved}, Plan{SetReview: true}},
		{"changes requested sets the review", Event{Action: ActionChangesRequested}, Plan{SetReview: true}},
		{"checks record a check", Event{Action: ActionChecks, Kind: KindChecks}, Plan{Checks: true}},
		{"empty", Event{}, Plan{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.ev.Plan()
			if got != tt.want {
				t.Fatalf("plan = %+v, want %+v", got, tt.want)
			}
			if got.Noop() != (!tt.want.Link && !tt.want.CloseLinks && !tt.want.Refresh && !tt.want.SetReview && !tt.want.Checks) {
				t.Fatalf("Noop = %v", got.Noop())
			}
		})
	}
}

func TestMoveString(t *testing.T) {
	if MoveNone.String() != "none" || MoveToReview.String() != "review" || MoveToTesting.String() != "testing" {
		t.Fatal("Move.String")
	}
}

func TestCanonicalRepoKey(t *testing.T) {
	const cloud = "{11111111-2222-3333-4444-555555555555}/{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}"
	good := []struct{ provider, in, want string }{
		{"gitlab", "42", "42"}, {"github", " 0042 ", "42"}, {"bitbucket", "4242", "4242"},
		{"bitbucket", cloud, cloud}, {"bitbucket", strings.ToUpper(cloud), cloud},
	}
	for _, g := range good {
		if got, ok := CanonicalRepoKey(g.provider, g.in); !ok || got != g.want {
			t.Errorf("%s %q -> %q, %v; want %q", g.provider, g.in, got, ok, g.want)
		}
	}
	bad := []struct{ provider, in string }{
		{"gitlab", cloud}, {"github", cloud}, {"gitlab", "0"}, {"github", ""}, {"github", "1234567890123456789"},
		{"bitbucket", "PROJ/slug"}, {"bitbucket", "{x}/{y}"}, {"bitbucket", cloud + "/x"}, {"bitbucket", "0"},
	}
	for _, b := range bad {
		if got, ok := CanonicalRepoKey(b.provider, b.in); ok {
			t.Errorf("%s %q accepted as %q", b.provider, b.in, got)
		}
	}
	if !IsCloudRepoKey(cloud) || IsCloudRepoKey("4242") {
		t.Error("IsCloudRepoKey")
	}
}
