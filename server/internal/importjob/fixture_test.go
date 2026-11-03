package importjob

import (
	"encoding/json"
	"time"

	"devops/internal/importer"
)

// A small hand-made Linear workspace (every id is made up). It is the shape of
// internal/importer's own fixture without cycles: a team, states, labels, two
// users, a project, issues, comments, a document, a relation, an attachment
// and two uploaded files.

const (
	fxTeam     = "lin-team-orb"
	fxStBack   = "lin-st-backlog"
	fxStTodo   = "lin-st-todo"
	fxStProg   = "lin-st-progress"
	fxStDone   = "lin-st-done"
	fxUChen    = "lin-user-chen"
	fxUBot     = "lin-user-bot"
	fxProject  = "lin-project-1"
	fxI1       = "lin-issue-2557"
	fxI2       = "lin-issue-3404"
	fxI3       = "lin-issue-3403"
	fxUploadA  = "https://uploads.linear.app/org-1/aaaa/bbbb"
	fxUploadC  = "https://uploads.linear.app/org-1/eeee/ffff"
	testKey    = "lin_api_TESTKEY_0123456789abcdefghij"
	fxFilePath = "files/aa/shot.png"
)

var fixtureFiles = map[string]string{fxFilePath: "PNG", "files/cc/notes.csv": "a,b\n1\n"}

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func tp(s string) *time.Time { t := ts(s); return &t }
func sp(s string) *string    { return &s }
func ref(id string) *importer.Ref {
	return &importer.Ref{ID: id}
}

func fixtureExport() *importer.Export {
	team := ref(fxTeam)
	state := func(id, name, typ string, pos float64) importer.WorkflowState {
		return importer.WorkflowState{ID: id, Name: name, Type: typ, Color: "#aaaaaa", Position: pos,
			CreatedAt: ts("2025-01-01T10:00:00Z"), UpdatedAt: ts("2025-01-01T10:00:00Z"), Team: team}
	}
	return &importer.Export{
		Teams: []importer.Team{{ID: fxTeam, Key: "ORB", Name: "Orbit", CreatedAt: ts("2025-01-01T09:00:00Z"),
			UpdatedAt: ts("2025-01-01T09:00:00Z"), Timezone: "Europe/London"}},
		States: []importer.WorkflowState{
			state(fxStBack, "Backlog", "backlog", 0), state(fxStTodo, "Todo", "unstarted", 1),
			state(fxStProg, "In Progress", "started", 2), state(fxStDone, "Done", "completed", 3),
		},
		Labels: []importer.Label{{ID: "lin-label-bug", Name: "Bug", Color: "#eb5757", CreatedAt: ts("2025-01-02T00:00:00Z")}},
		Users: []importer.User{
			{ID: fxUChen, Name: "Chen", DisplayName: "chen", Email: "Chen@Example.com", Active: true},
			{ID: fxUBot, Name: "Claude", DisplayName: "claude", Email: "bot@example.com", Active: true, App: true},
		},
		Projects: []importer.Project{{ID: fxProject, Name: "dev-ops", Description: "Replace Linear",
			Content: sp("Plan ![shot](" + fxUploadA + ")"), SlugID: "abc123", Status: &importer.ProjectStatus{Type: "started"},
			Lead: ref(fxUChen), CreatedAt: ts("2026-10-01T08:00:00Z"), UpdatedAt: ts("2026-10-20T08:00:00Z"),
			StartedAt: tp("2026-10-02T08:00:00Z"), Teams: importer.Connection{Nodes: []importer.Ref{{ID: fxTeam}}}}},
		Issues: []importer.Issue{
			{ID: fxI1, Identifier: "ORB-2557", Number: 2557, Title: "Compose project name",
				Description: sp("Needs `-p`. See ![x](" + fxUploadA + ")."), Priority: 2, SortOrder: -12.5,
				CreatedAt: ts("2026-08-31T10:00:00.123Z"), UpdatedAt: ts("2026-09-02T10:00:00Z"),
				StartedAt: tp("2026-09-01T10:00:00Z"), CompletedAt: tp("2026-09-02T09:00:00Z"),
				LabelIDs: []string{"lin-label-bug"}, Team: team, State: ref(fxStDone), Assignee: ref(fxUChen),
				Creator: ref(fxUChen), Project: ref(fxProject),
				History: []importer.HistoryEntry{
					{ID: "lin-h1", CreatedAt: ts("2026-08-31T10:00:00.123Z"), ToStateID: sp(fxStBack), ActorID: sp(fxUChen)},
					{ID: "lin-h2", CreatedAt: ts("2026-09-01T10:00:00Z"), FromStateID: sp(fxStBack), ToStateID: sp(fxStProg), ActorID: sp(fxUBot)},
					{ID: "lin-h3", CreatedAt: ts("2026-09-02T09:00:00Z"), FromStateID: sp(fxStProg), ToStateID: sp(fxStDone)},
				}},
			{ID: fxI2, Identifier: "ORB-3404", Number: 3404, Title: "Child",
				CreatedAt: ts("2026-10-20T10:00:00Z"), UpdatedAt: ts("2026-10-20T10:00:00Z"),
				Team: team, State: ref(fxStTodo), Assignee: ref(fxUBot), Creator: ref(fxUBot), Parent: ref(fxI3),
				History: []importer.HistoryEntry{}},
			{ID: fxI3, Identifier: "ORB-3403", Number: 3403, Title: "Parent", Priority: 1,
				Description: sp("Data: [notes](" + fxUploadC + ")"),
				CreatedAt:   ts("2026-10-21T10:00:00Z"), UpdatedAt: ts("2026-10-21T10:00:00Z"),
				Team: team, State: ref(fxStBack), Creator: ref(fxUChen), History: []importer.HistoryEntry{}},
		},
		Comments: []importer.Comment{
			{ID: "lin-cm1", Body: "First", CreatedAt: ts("2026-09-01T11:00:00Z"), UpdatedAt: ts("2026-09-01T11:00:00Z"),
				IssueID: sp(fxI1), User: ref(fxUChen)},
			{ID: "lin-cm2", Body: "Reply with ![y](" + fxUploadA + ")", CreatedAt: ts("2026-09-01T12:00:00Z"),
				UpdatedAt: ts("2026-09-01T12:30:00Z"), IssueID: sp(fxI1), ParentID: sp("lin-cm1"), User: ref(fxUBot)},
		},
		Documents: []importer.Document{{ID: "lin-doc-1", Title: "Cutover plan", Content: sp("Steps"), SlugID: "a1b2c3d4e5f6",
			CreatedAt: ts("2026-10-04T10:00:00Z"), UpdatedAt: ts("2026-10-04T11:00:00Z"),
			DocumentContentID: sp("lin-dc1"), Project: ref(fxProject), Creator: ref(fxUChen)}},
		Relations: []importer.Relation{{ID: "lin-r1", Type: "blocks", CreatedAt: ts("2026-10-20T11:00:00Z"),
			Issue: ref(fxI1), RelatedIssue: ref(fxI2)}},
		Attachments: []importer.Attachment{
			{ID: "lin-a1", Title: "MR !42", Subtitle: sp("merged"), URL: "https://gitlab.example/x/-/merge_requests/42",
				Metadata: json.RawMessage(`{"state":"merged"}`), CreatedAt: ts("2026-09-02T08:00:00Z"),
				UpdatedAt: ts("2026-09-02T08:00:00Z"), Creator: ref(fxUBot), Issue: ref(fxI1)},
			{ID: "lin-a2", Title: "shot.png", URL: fxUploadA, Metadata: json.RawMessage(`{}`), CreatedAt: ts("2026-09-02T08:10:00Z"),
				UpdatedAt: ts("2026-09-02T08:10:00Z"), Issue: ref(fxI1)},
		},
	}
}

// fixtureUploads are the files the fixture's texts refer to, as the export
// manifest would list them after a download.
func fixtureUploads() []importer.File {
	return []importer.File{
		{URL: fxUploadA, Path: fxFilePath, Filename: "shot.png", ContentType: "image/png", SizeBytes: 3},
		{URL: fxUploadC, Path: "files/cc/notes.csv", Filename: "notes.csv", ContentType: "text/csv", SizeBytes: 6},
	}
}
