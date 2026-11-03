package tracker

import (
	"strings"
	"testing"
	"time"

	"devops/internal/scm"
)

func TestChangeRequestState(t *testing.T) {
	tests := []struct {
		state  string
		draft  bool
		review string
		want   string
	}{
		{"opened", false, "none", CRopen},
		{"opened", true, "none", CRDraft},
		{"opened", true, "approved", CRDraft},
		{"opened", false, "approved", CRApproved},
		{"opened", false, "changes_requested", CRopen},
		{"merged", true, "none", CRMerged},
		{"merged", false, "approved", CRMerged},
		{"closed", false, "approved", CRClosed},
	}
	for _, tt := range tests {
		if got := changeRequestState(tt.state, tt.draft, tt.review); got != tt.want {
			t.Errorf("%+v = %s, want %s", tt, got, tt.want)
		}
	}
}

func at(min int) *time.Time {
	t := time.Date(2026, 10, 26, 12, min, 0, 0, time.UTC)
	return &t
}

// The decisions of a link-plan event on a link that exists (decisions-T5 #25,
// decisions-T17 #18).
func TestNextLinkForLinkPlan(t *testing.T) {
	open := scm.Event{Action: scm.ActionOpened, URL: "https://h/x/1", Title: "new", OccurredAt: *at(10)}
	merge := scm.Event{Action: scm.ActionMerged, URL: "https://h/x/1", Title: "new", OccurredAt: *at(10)}
	tests := []struct {
		name      string
		cur       mrLinkRow
		ev        scm.Event
		wantState string // "" : nothing is written
		stale     bool
		title     string
	}{
		{"open on open refreshes", mrLinkRow{State: "opened", Title: "old", ProviderUpdatedAt: at(5)}, open, "opened", false, "new"},
		{"older open is stale", mrLinkRow{State: "opened", Title: "old", ProviderUpdatedAt: at(20)}, open, "", true, ""},
		{"equal time is accepted", mrLinkRow{State: "opened", Title: "old", ProviderUpdatedAt: at(10)}, open, "opened", false, "new"},
		{"no stored time is accepted", mrLinkRow{State: "opened", Title: "old"}, open, "opened", false, "new"},
		{"open on closed is stale", mrLinkRow{State: "closed", ProviderUpdatedAt: at(5)}, open, "", true, ""},
		{"reopen on closed opens", mrLinkRow{State: "closed", ProviderUpdatedAt: at(5)}, scm.Event{Action: scm.ActionReopened, URL: "https://h/x/1", OccurredAt: *at(10)}, "opened", false, ""},
		{"older reopen is stale", mrLinkRow{State: "closed", ProviderUpdatedAt: at(20)}, scm.Event{Action: scm.ActionReopened, URL: "https://h/x/1", OccurredAt: *at(10)}, "", true, ""},
		{"open on merged is stale", mrLinkRow{State: "merged"}, open, "", true, ""},
		{"merge on merged writes nothing and is not stale", mrLinkRow{State: "merged"}, merge, "", false, ""},
		{"merge on open merges", mrLinkRow{State: "opened", Title: "old", ProviderUpdatedAt: at(5)}, merge, "merged", false, "new"},
		{"merge on closed merges", mrLinkRow{State: "closed", Title: "old", ProviderUpdatedAt: at(5)}, merge, "merged", false, "new"},
		{"an older merge still merges, state only", mrLinkRow{State: "opened", Title: "newer", ProviderUpdatedAt: at(20)}, merge, "merged", false, "newer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := tt.ev.Plan()
			next, _, stale := nextLinkForLinkPlan(tt.cur, tt.ev, plan)
			if stale != tt.stale {
				t.Fatalf("stale = %v, want %v", stale, tt.stale)
			}
			if tt.wantState == "" {
				if next != nil {
					t.Fatalf("wrote %+v, want nothing", next)
				}
				return
			}
			if next == nil || next.State != tt.wantState || (tt.title != "" && next.Title != tt.title) {
				t.Fatalf("next = %+v, want state %s title %q", next, tt.wantState, tt.title)
			}
		})
	}
}

func TestWithEventFields(t *testing.T) {
	cur := mrLinkRow{HeadSHA: "aaa", Author: "dev", RepoPath: "o/r", ProviderUpdatedAt: at(5)}
	long := strings.Repeat("é", 400)
	next, reset := withEventFields(cur, scm.Event{HeadSHA: "bbb", Author: long, RepoPath: long, Title: "t\x00x", OccurredAt: *at(10)})
	if !reset || next.HeadSHA != "bbb" || len([]rune(next.Author)) != maxMRAuthorRunes || len([]rune(next.RepoPath)) != maxMRRepoPathRunes ||
		next.Title != "t x" || !next.ProviderUpdatedAt.Equal(*at(10)) {
		t.Fatalf("next = %+v reset=%v", next, reset)
	}
	// An event with none of those fields keeps what is stored and resets nothing.
	next, reset = withEventFields(cur, scm.Event{})
	if reset || next.HeadSHA != "aaa" || next.Author != "dev" || next.RepoPath != "o/r" || !next.ProviderUpdatedAt.Equal(*at(5)) {
		t.Fatalf("empty event: %+v reset=%v", next, reset)
	}
	// An older host time never moves the stored time back.
	next, _ = withEventFields(cur, scm.Event{OccurredAt: *at(1)})
	if !next.ProviderUpdatedAt.Equal(*at(5)) {
		t.Fatalf("stored time moved back: %v", next.ProviderUpdatedAt)
	}
}
