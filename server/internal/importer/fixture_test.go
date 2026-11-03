package importer

import (
	"encoding/json"
	"time"
)

// A small hand-made Linear workspace. Every id is made up; no record comes
// from Orbit's real Linear. It exercises each mapping rule the importer
// has: adoption of seeded rows, merges, exclusions, a parent written after
// its child, a synthesised creation row, a cycle number gap and the
// uploads.linear.app URLs.

const (
	fxTeam = "lin-team-orb"

	fxStBacklog = "lin-st-backlog"
	fxStTodo    = "lin-st-todo"
	fxStProg    = "lin-st-progress"
	fxStDone    = "lin-st-done"
	fxStTriage  = "lin-st-triage" // not seeded: created
	fxStDup     = "lin-st-dup"    // Linear's "duplicate" type

	fxLBug   = "lin-label-bug"   // adopts the seed's Bug
	fxLBug2  = "lin-label-bug2"  // "bug" on a team: merged into Bug
	fxLArea  = "lin-label-area"  // a group
	fxLBack  = "lin-label-back"  // child of the group
	fxLFlaky = "lin-label-flaky" // a label of the team ORB (LAUN-54): kept as a team label
	fxUChen  = "lin-user-chen"
	fxUBot   = "lin-user-bot"

	fxProject   = "lin-project-1"
	fxMilestone = "lin-milestone-1"

	fxC7  = "lin-cycle-7"
	fxC8  = "lin-cycle-8"
	fxC10 = "lin-cycle-10" // 9 is missing

	fxI1 = "lin-issue-2557"
	fxI2 = "lin-issue-3404" // child of I3, created BEFORE I3
	fxI3 = "lin-issue-3403"
	fxI4 = "lin-issue-100" // trashed
	fxI5 = "lin-issue-50"  // archived

	fxD1 = "lin-doc-1"
	fxD2 = "lin-doc-2" // trashed

	fxUploadA = "https://uploads.linear.app/org-1/aaaa/bbbb"
	fxUploadB = "https://uploads.linear.app/org-1/cccc/dddd" // only the project refers to it (see tests)
	fxUploadC = "https://uploads.linear.app/org-1/eeee/ffff" // only I3's description refers to it
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func tp(s string) *time.Time { t := ts(s); return &t }
func sp(s string) *string    { return &s }
func bp(b bool) *bool        { return &b }
func fp(f float64) *float64  { return &f }
func ref(id string) *Ref     { return &Ref{ID: id} }

func fixtureExport() *Export {
	team := ref(fxTeam)
	state := func(id, name, typ string, pos float64) WorkflowState {
		return WorkflowState{ID: id, Name: name, Type: typ, Color: "#aaaaaa", Position: pos,
			CreatedAt: ts("2025-01-01T10:00:00Z"), UpdatedAt: ts("2025-01-01T10:00:00Z"), Team: team}
	}
	cycle := func(id string, n float64, start string) Cycle {
		s := ts(start)
		return Cycle{ID: id, Number: n, StartsAt: s, EndsAt: s.AddDate(0, 0, 14),
			CreatedAt: s.Add(-time.Hour), UpdatedAt: s, Team: team}
	}
	c7 := cycle(fxC7, 7, "2026-08-29T23:00:00Z") // Sunday 30 Aug 00:00 BST
	c7.CompletedAt = tp("2026-09-12T23:00:00Z")
	c7.IssueCountHistory = []float64{2, 2, 3}
	c7.CompletedIssueCountHistory = []float64{0, 1, 1}
	c7.ScopeHistory = []float64{3, 3, 5}
	c7.CompletedScopeHistory = []float64{0, 1, 1}
	c7.InProgressScopeHistory = []float64{1, 1, 2}
	c8 := cycle(fxC8, 8, "2026-09-12T23:00:00Z")
	c8.CompletedAt = tp("2026-09-26T23:00:00Z")
	c10 := cycle(fxC10, 10, "2026-10-10T23:00:00Z")

	return &Export{
		Teams: []Team{{ID: fxTeam, Key: "ORB", Name: "Orbit", CreatedAt: ts("2025-01-01T09:00:00Z"),
			UpdatedAt: ts("2025-01-01T09:00:00Z"), CyclesEnabled: true, CycleStartDay: 0, CycleDuration: 2,
			Timezone: "Europe/London"}},
		States: []WorkflowState{
			state(fxStBacklog, "Backlog", "backlog", 0), state(fxStTodo, "Todo", "unstarted", 1),
			state(fxStProg, "In Progress", "started", 2), state(fxStDone, "Done", "completed", 3),
			state(fxStTriage, "Triage", "triage", -1), state(fxStDup, "Duplicate", "duplicate", 5),
		},
		Labels: []Label{
			{ID: fxLBug, Name: "Bug", Color: "#eb5757", CreatedAt: ts("2025-01-02T00:00:00Z")},
			{ID: fxLBug2, Name: "bug", Color: "#ff0000", CreatedAt: ts("2025-02-02T00:00:00Z"), Team: team},
			{ID: fxLArea, Name: "Area", Color: "#000000", IsGroup: true, CreatedAt: ts("2025-03-01T00:00:00Z")},
			{ID: fxLBack, Name: "Backend", Color: "#00ff00", CreatedAt: ts("2025-03-02T00:00:00Z"), Parent: ref(fxLArea)},
			{ID: fxLFlaky, Name: "Flaky", Color: "#ffaa00", CreatedAt: ts("2025-03-03T00:00:00Z"), Team: team},
		},
		Users: []User{
			{ID: fxUChen, Name: "Chen", DisplayName: "chen", Email: "Chen@Example.com", Active: true},
			{ID: fxUBot, Name: "Claude", DisplayName: "claude", Email: "bot@example.com", Active: true, App: true},
		},
		Projects: []Project{{ID: fxProject, Name: "dev-ops", Description: "Replace Linear",
			Content: sp("Plan ![shot](" + fxUploadA + ")"), SlugID: "abc123", Status: &ProjectStatus{Type: "started"},
			Lead: ref(fxUChen), StartDate: sp("2026-10-01"), TargetDate: sp("2026-11-30"),
			CreatedAt: ts("2026-10-01T08:00:00Z"), UpdatedAt: ts("2026-10-20T08:00:00Z"),
			StartedAt: tp("2026-10-02T08:00:00Z"), Teams: Connection{Nodes: []Ref{{ID: fxTeam}}}}},
		Milestones: []Milestone{{ID: fxMilestone, Name: "Importer", Description: sp("T4"), TargetDate: sp("2026-10-31"),
			SortOrder: 1.5, CreatedAt: ts("2026-10-02T08:00:00Z"), UpdatedAt: ts("2026-10-03T08:00:00Z"), Project: ref(fxProject)}},
		Cycles: []Cycle{c7, c8, c10},
		Issues: []Issue{
			{ID: fxI1, Identifier: "ORB-2557", Number: 2557, Title: "Compose project name",
				Description: sp("Needs `-p`. See ![x](" + fxUploadA + ")."), Priority: 2, Estimate: fp(3), SortOrder: -12.5,
				CreatedAt: ts("2026-08-31T10:00:00.123Z"), UpdatedAt: ts("2026-09-02T10:00:00Z"),
				StartedAt: tp("2026-09-01T10:00:00Z"), CompletedAt: tp("2026-09-02T09:00:00Z"),
				DueDate: sp("2026-09-05"), LabelIDs: []string{fxLBug, fxLBug2, fxLBack},
				Team: team, State: ref(fxStDone), Assignee: ref(fxUChen), Creator: ref(fxUChen),
				Project: ref(fxProject), ProjectMilestone: ref(fxMilestone), Cycle: ref(fxC7),
				History: []HistoryEntry{
					{ID: "lin-h1", CreatedAt: ts("2026-08-31T10:00:00.123Z"), ToStateID: sp(fxStBacklog), ActorID: sp(fxUChen)},
					{ID: "lin-h2", CreatedAt: ts("2026-09-01T10:00:00Z"), FromStateID: sp(fxStBacklog), ToStateID: sp(fxStProg), ActorID: sp(fxUBot)},
					{ID: "lin-h3", CreatedAt: ts("2026-09-02T09:00:00Z"), FromStateID: sp(fxStProg), ToStateID: sp(fxStDone)},
					{ID: "lin-h4", CreatedAt: ts("2026-09-02T09:30:00Z")}, // a title edit: no state change
				}},
			{ID: fxI2, Identifier: "ORB-3404", Number: 3404, Title: "Child", Priority: 0,
				CreatedAt: ts("2026-10-20T10:00:00Z"), UpdatedAt: ts("2026-10-20T10:00:00Z"),
				Team: team, State: ref(fxStTodo), Assignee: ref(fxUBot), Creator: ref(fxUBot), Parent: ref(fxI3),
				LabelIDs: []string{fxLFlaky},
				Cycle:    ref(fxC10)},
			{ID: fxI3, Identifier: "ORB-3403", Number: 3403, Title: "Parent", Priority: 1, Estimate: fp(2.6),
				Description: sp("Data: [notes](" + fxUploadC + ")"),
				CreatedAt:   ts("2026-10-21T10:00:00Z"), UpdatedAt: ts("2026-10-21T10:00:00Z"),
				Team: team, State: ref(fxStTriage), Creator: ref(fxUChen)},
			{ID: fxI4, Identifier: "ORB-100", Number: 100, Title: "Trashed", Trashed: bp(true),
				CreatedAt: ts("2025-05-01T10:00:00Z"), UpdatedAt: ts("2025-05-01T10:00:00Z"), Team: team, State: ref(fxStBacklog)},
			{ID: fxI5, Identifier: "ORB-50", Number: 50, Title: "Archived", ArchivedAt: tp("2025-06-01T00:00:00Z"),
				CreatedAt: ts("2025-04-01T10:00:00Z"), UpdatedAt: ts("2025-06-01T00:00:00Z"), Team: team, State: ref(fxStDup)},
		},
		Comments: []Comment{
			{ID: "lin-cm1", Body: "First", CreatedAt: ts("2026-09-01T11:00:00Z"), UpdatedAt: ts("2026-09-01T11:00:00Z"),
				IssueID: sp(fxI1), User: ref(fxUChen)},
			{ID: "lin-cm2", Body: "Reply with ![y](" + fxUploadA + ")", CreatedAt: ts("2026-09-01T12:00:00Z"),
				UpdatedAt: ts("2026-09-01T12:30:00Z"), EditedAt: tp("2026-09-01T12:30:00Z"),
				IssueID: sp(fxI1), ParentID: sp("lin-cm1"), User: ref(fxUBot)},
			{ID: "lin-cm3", Body: "Deleted", CreatedAt: ts("2026-09-01T13:00:00Z"), UpdatedAt: ts("2026-09-01T13:00:00Z"),
				ArchivedAt: tp("2026-09-01T14:00:00Z"), IssueID: sp(fxI1), User: ref(fxUChen)},
			{ID: "lin-cm4", Body: "On an archived issue", CreatedAt: ts("2025-04-02T10:00:00Z"), UpdatedAt: ts("2025-04-02T10:00:00Z"),
				ArchivedAt: tp("2025-06-01T00:00:00Z"), IssueID: sp(fxI5), User: ref(fxUChen)},
			{ID: "lin-cm5", Body: "On a project update", CreatedAt: ts("2026-10-05T10:00:00Z"), UpdatedAt: ts("2026-10-05T10:00:00Z"),
				ProjectUpdateID: sp("lin-pu1"), User: ref(fxUChen)},
			{ID: "lin-cm6", Body: "On the doc", CreatedAt: ts("2026-10-06T10:00:00Z"), UpdatedAt: ts("2026-10-06T10:00:00Z"),
				DocumentContentID: sp("lin-dc1"), User: ref(fxUChen)},
			{ID: "lin-cm7", Body: "   ", CreatedAt: ts("2026-10-07T10:00:00Z"), UpdatedAt: ts("2026-10-07T10:00:00Z"),
				IssueID: sp(fxI1), User: ref(fxUChen)},
			{ID: "lin-cm8", Body: "On trash", CreatedAt: ts("2025-05-02T10:00:00Z"), UpdatedAt: ts("2025-05-02T10:00:00Z"),
				IssueID: sp(fxI4), User: ref(fxUChen)},
		},
		Documents: []Document{
			{ID: fxD1, Title: "Cutover plan", Content: sp("Steps"), SlugID: "a1b2c3d4e5f6",
				CreatedAt: ts("2026-10-04T10:00:00Z"), UpdatedAt: ts("2026-10-04T11:00:00Z"),
				DocumentContentID: sp("lin-dc1"), Project: ref(fxProject), Creator: ref(fxUChen)},
			{ID: fxD2, Title: "Old", SlugID: "ffffffffffff", Trashed: bp(true),
				CreatedAt: ts("2026-10-04T10:00:00Z"), UpdatedAt: ts("2026-10-04T11:00:00Z")},
		},
		Relations: []Relation{
			{ID: "lin-r1", Type: "blocks", CreatedAt: ts("2026-10-20T11:00:00Z"), Issue: ref(fxI1), RelatedIssue: ref(fxI2)},
			{ID: "lin-r2", Type: "related", CreatedAt: ts("2026-10-21T11:00:00Z"), Issue: ref(fxI2), RelatedIssue: ref(fxI3)},
			{ID: "lin-r3", Type: "related", CreatedAt: ts("2026-10-21T12:00:00Z"), Issue: ref(fxI3), RelatedIssue: ref(fxI2)},
			{ID: "lin-r4", Type: "similar", CreatedAt: ts("2026-10-21T13:00:00Z"), Issue: ref(fxI3), RelatedIssue: ref(fxI1)},
			{ID: "lin-r5", Type: "duplicate", CreatedAt: ts("2026-10-21T14:00:00Z"), Issue: ref(fxI3), RelatedIssue: ref(fxI1)},
		},
		Attachments: []Attachment{
			{ID: "lin-a1", Title: "MR !42", Subtitle: sp("merged"), URL: "https://gitlab.example/x/-/merge_requests/42",
				Metadata: json.RawMessage(`{"state":"merged"}`), CreatedAt: ts("2026-09-02T08:00:00Z"),
				UpdatedAt: ts("2026-09-02T08:00:00Z"), Creator: ref(fxUBot), Issue: ref(fxI1)},
			{ID: "lin-a2", Title: "shot.png", URL: fxUploadA, Metadata: json.RawMessage(`{}`), CreatedAt: ts("2026-09-02T08:10:00Z"),
				UpdatedAt: ts("2026-09-02T08:10:00Z"), Issue: ref(fxI1)},
			{ID: "lin-a3", Title: "gone", URL: "https://example.com/gone", Metadata: json.RawMessage(`{}`), ArchivedAt: tp("2026-09-03T00:00:00Z"),
				CreatedAt: ts("2026-09-02T08:20:00Z"), UpdatedAt: ts("2026-09-03T00:00:00Z"), Issue: ref(fxI1)},
		},
		Files: []File{
			{URL: fxUploadA, Path: "files/aa/shot.png", Filename: "shot.png", ContentType: "image/png", SizeBytes: 3},
			{URL: fxUploadC, Path: "files/cc/notes.csv", Filename: "notes.csv", ContentType: "text/csv", SizeBytes: 6},
		},
	}
}

func fixtureUserMap() *UserMap {
	return &UserMap{Default: "chen", Users: map[string]string{fxUBot: "claude"}}
}

// fixtureFiles are the bytes of the fixture's files, by export path.
var fixtureFiles = map[string]string{"files/aa/shot.png": "PNG", "files/cc/notes.csv": "a,b\n1\n"}
