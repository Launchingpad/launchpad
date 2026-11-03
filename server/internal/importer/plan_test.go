package importer

import (
	"slices"
	"strings"
	"testing"
)

func reasons(p *Plan) map[string]string {
	out := map[string]string{}
	for _, x := range p.Exclusions {
		out[x.Entity+" "+x.SourceID] = x.Reason
	}
	return out
}

func TestBuildPlan(t *testing.T) {
	p, err := BuildPlan(fixtureExport(), fixtureUserMap(), map[string]int32{"ORB": 3405})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("exclusions", func(t *testing.T) {
		want := map[string]string{
			EntIssue + " " + fxI4:     "trashed in Linear",
			EntDocument + " " + fxD2:  "trashed in Linear",
			EntComment + " lin-cm3":   "archived in Linear on an open issue (deleted)",
			EntComment + " lin-cm5":   "not on an issue or a document (Launchpad has no table for it)",
			EntComment + " lin-cm7":   "empty body",
			EntComment + " lin-cm8":   "issue not imported",
			EntRelation + " lin-r4":   `type "similar" is not imported`,
			EntAttachment + " lin-a3": "archived in Linear on an open issue (removed)",
		}
		got := reasons(p)
		for k, v := range want {
			if got[k] != v {
				t.Errorf("exclusion %s = %q, want %q", k, got[k], v)
			}
		}
		if len(got) != len(want) {
			t.Errorf("got %d exclusions, want %d: %v", len(got), len(want), got)
		}
	})

	t.Run("expected counts", func(t *testing.T) {
		want := map[string]int{
			EntTeam: 1, EntState: 6, EntLabel: 5, EntUser: 2, EntProject: 1, EntMilestone: 1, EntCycle: 3,
			EntSnapshot: 3, EntIssue: 4, EntDocument: 1, EntComment: 4, EntRelation: 4, EntAttachment: 2, EntFile: 2,
			// I1: h1 (its creation row), h2, h3; I2, I3, I5: a synthesised creation row each.
			EntStateChange: 6,
		}
		for e, n := range want {
			if p.Expected[e] != n {
				t.Errorf("expected[%s] = %d, want %d", e, p.Expected[e], n)
			}
		}
		for e, n := range p.Expected {
			if got := len(p.SourceIDs()[e]); e != EntSnapshot && got != n {
				t.Errorf("SourceIDs[%s] has %d ids, Expected says %d", e, got, n)
			}
		}
	})

	t.Run("merges", func(t *testing.T) {
		if p.LabelCanon[fxLBug2] != fxLBug {
			t.Errorf("label bug merges into %q, want Bug", p.LabelCanon[fxLBug2])
		}
		if len(p.Labels) != 4 {
			t.Errorf("%d canonical labels, want 4", len(p.Labels))
		}
		if p.LabelCanon[fxLFlaky] != fxLFlaky {
			t.Errorf("a team label with a name of its own must stay: %q", p.LabelCanon[fxLFlaky])
		}
		if !p.Labels[0].IsGroup {
			t.Error("the label group must be written first")
		}
		if p.RelationCanon["lin-r3"] != "lin-r2" {
			t.Errorf("r3 (the reverse of r2) merges into %q, want lin-r2", p.RelationCanon["lin-r3"])
		}
		if p.RelationType["lin-r5"] != "duplicate_of" {
			t.Errorf("duplicate maps to %q", p.RelationType["lin-r5"])
		}
	})

	t.Run("issues in creation order, parent after child is kept", func(t *testing.T) {
		var ids []string
		for _, is := range p.Issues {
			ids = append(ids, is.ID)
		}
		if !slices.Equal(ids, []string{fxI5, fxI1, fxI2, fxI3}) {
			t.Errorf("issue order %v", ids)
		}
		if RefID(p.Issues[2].Parent) != fxI3 {
			t.Error("I2 must keep its parent I3 for the second pass")
		}
	})

	t.Run("state history", func(t *testing.T) {
		var i1, i2 []StateChange
		for _, c := range p.Changes {
			switch c.IssueID {
			case fxI1:
				i1 = append(i1, c)
			case fxI2:
				i2 = append(i2, c)
			}
		}
		if len(i1) != 3 || i1[0].SourceID != "lin-h1" || i1[0].From != nil || i1[2].To != fxStDone {
			t.Errorf("I1 history %+v", i1)
		}
		if len(i2) != 1 || i2[0].SourceID != fxI2+":created" || i2[0].From != nil || i2[0].To != fxStTodo ||
			!i2[0].At.Equal(ts("2026-10-20T10:00:00Z")) || i2[0].Actor != fxUBot {
			t.Errorf("I2 synthesised creation row %+v", i2)
		}
	})

	t.Run("cycles, cadence, counters", func(t *testing.T) {
		if got := p.MissingCycleNumbers["ORB"]; !slices.Equal(got, []int32{9}) {
			t.Errorf("missing cycle numbers %v, want [9]", got)
		}
		c := p.Cadences[fxTeam]
		if c.LengthDays != 14 || c.Weekday != 0 || c.Hour != 0 || c.Min != 0 || c.Timezone != "Europe/London" {
			t.Errorf("cadence %+v, want 14 days from Sunday 00:00 Europe/London", c)
		}
		if got := p.NextNumbers[fxTeam]; got != 3405 {
			t.Errorf("next number %d, want 3405 (max 3404 + 1, floor 3405)", got)
		}
		if n := len(p.Snapshots[fxC7]); n != 3 {
			t.Errorf("%d snapshots for cycle 7, want 3", n)
		}
	})
}

func TestBuildPlan_counterFloorAboveMax(t *testing.T) {
	p, err := BuildPlan(fixtureExport(), fixtureUserMap(), map[string]int32{"ORB": 5000})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.NextNumbers[fxTeam]; got != 5000 {
		t.Errorf("next number %d, want the floor 5000", got)
	}
}

func TestBuildPlan_errors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(ex *Export, um *UserMap)
		want   string
	}{
		{"unmapped user and no default", func(ex *Export, um *UserMap) { um.Default = "" },
			"1 Linear user(s) have no mapping"},
		{"unknown state type", func(ex *Export, um *UserMap) { ex.States[0].Type = "weird" },
			`unknown workflow state type "weird"`},
		{"fractional issue number", func(ex *Export, um *UserMap) { ex.Issues[0].Number = 1.5 },
			"is not a positive integer"},
		{"bad team key", func(ex *Export, um *UserMap) { ex.Teams[0].Key = "orb" },
			"is not a Launchpad team key"},
		{"issue state of another team", func(ex *Export, um *UserMap) { ex.Issues[0].State = ref("nope") },
			"its state is not a state of its team"},
		{"bad due date", func(ex *Export, um *UserMap) { ex.Issues[0].DueDate = sp("05/09/2026") },
			"is not YYYY-MM-DD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ex, um := fixtureExport(), fixtureUserMap()
			tt.mutate(ex, um)
			_, err := BuildPlan(ex, um, nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestPlanStateChanges_skipsUnknownStates(t *testing.T) {
	states := map[string]WorkflowState{"a": {ID: "a"}, "b": {ID: "b"}}
	is := Issue{ID: "i", CreatedAt: ts("2026-01-01T00:00:00Z"), State: ref("b"), History: []HistoryEntry{
		{ID: "h2", CreatedAt: ts("2026-01-03T00:00:00Z"), FromStateID: sp("a"), ToStateID: sp("gone")},
		{ID: "h1", CreatedAt: ts("2026-01-02T00:00:00Z"), FromStateID: sp("a"), ToStateID: sp("b")},
		{ID: "h0", CreatedAt: ts("2026-01-02T00:00:00Z"), FromStateID: sp("a"), ToStateID: sp("a")},
	}}
	var skipped []string
	got := planStateChanges(is, states, func(id string) { skipped = append(skipped, id) })
	if !slices.Equal(skipped, []string{"h2"}) {
		t.Errorf("skipped %v, want [h2]", skipped)
	}
	// The creation row goes into the state of the first change's FROM.
	if len(got) != 2 || got[0].SourceID != "i:created" || got[0].To != "a" || got[1].SourceID != "h1" {
		t.Errorf("changes %+v", got)
	}
}

// LAUN-54: labels are merged by scope. The same name on two teams is two
// labels; a team label named like an org-wide label merges into it; a label
// whose team is not in the export is org-wide.
func TestPlan_labelScopes(t *testing.T) {
	ex := fixtureExport()
	ex.Teams = append(ex.Teams, Team{ID: "lin-team-web", Key: "WEB", Name: "Web", CreatedAt: ts("2025-01-01T09:00:00Z"),
		UpdatedAt: ts("2025-01-01T09:00:00Z")})
	orb, web := ref(fxTeam), ref("lin-team-web")
	ex.Labels = append(ex.Labels,
		Label{ID: "l-orb-ui", Name: "UI", CreatedAt: ts("2025-04-01T00:00:00Z"), Team: orb},
		Label{ID: "l-web-ui", Name: "ui", CreatedAt: ts("2025-04-02T00:00:00Z"), Team: web},
		Label{ID: "l-web-ui2", Name: "UI", CreatedAt: ts("2025-04-03T00:00:00Z"), Team: web},
		Label{ID: "l-gone", Name: "Orphan", CreatedAt: ts("2025-04-04T00:00:00Z"), Team: ref("lin-team-gone")},
		// created BEFORE the org-wide label of its name: still merged into it
		Label{ID: "l-early", Name: "Late", CreatedAt: ts("2024-01-01T00:00:00Z"), Team: orb},
		Label{ID: "l-late", Name: "late", CreatedAt: ts("2025-05-01T00:00:00Z")},
	)
	p, err := BuildPlan(ex, fixtureUserMap(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.LabelCanon["l-orb-ui"] != "l-orb-ui" || p.LabelCanon["l-web-ui"] != "l-web-ui" {
		t.Errorf("the same name on two teams must stay two labels: %v", p.LabelCanon)
	}
	if p.LabelCanon["l-web-ui2"] != "l-web-ui" {
		t.Errorf("same name on one team merges: %q", p.LabelCanon["l-web-ui2"])
	}
	if p.LabelCanon["l-early"] != "l-late" {
		t.Errorf("a team label named like an org label merges into it: %q", p.LabelCanon["l-early"])
	}
	for _, l := range p.Labels {
		if l.ID == "l-gone" && l.Team != nil {
			t.Error("a label of a team outside the export must be org-wide")
		}
	}
}
