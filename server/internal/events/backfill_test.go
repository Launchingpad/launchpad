package events

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"

	"devops/internal/dbq"
)

// The rules of the backfill job (decisions-T20 #57), without a database: the
// facts are filled in by hand, as the loader would fill them.

type world struct {
	f                                                   *facts
	teamA, teamB, goneTeam                              uuid.UUID
	issueA, issueB, goneIssue                           uuid.UUID
	projA, projAB, projNone, goneProj                   uuid.UUID
	docA, docNone, goneDoc                              uuid.UUID
	commentIssue, commentDoc, goneComment               uuid.UUID
	attachment, relation, cycle, milestone, update      uuid.UUID
	orgLabel, teamLabel, goneLabel, product, noTeamProd uuid.UUID
}

func newWorld() *world {
	w := &world{f: newFacts()}
	ids := []*uuid.UUID{&w.teamA, &w.teamB, &w.goneTeam, &w.issueA, &w.issueB, &w.goneIssue, &w.projA, &w.projAB,
		&w.projNone, &w.goneProj, &w.docA, &w.docNone, &w.goneDoc, &w.commentIssue, &w.commentDoc, &w.goneComment,
		&w.attachment, &w.relation, &w.cycle, &w.milestone, &w.update, &w.orgLabel, &w.teamLabel, &w.goneLabel,
		&w.product, &w.noTeamProd}
	for _, p := range ids {
		*p = uuid.Must(uuid.NewV7())
	}
	f := w.f
	f.team[w.teamA], f.team[w.teamB] = true, true
	f.issueTeam[w.issueA], f.issueTeam[w.issueB] = w.teamA, w.teamB
	f.projectTeams[w.projA] = []uuid.UUID{w.teamA}
	f.projectTeams[w.projAB] = []uuid.UUID{w.teamA, w.teamB}
	f.projectTeams[w.projNone] = []uuid.UUID{}
	f.documentProject[w.docA], f.documentProject[w.docNone] = &w.projA, nil
	f.comment[w.commentIssue] = commentTarget{issue: &w.issueA}
	f.comment[w.commentDoc] = commentTarget{document: &w.docA}
	f.attachmentIssue[w.attachment] = w.issueB
	f.relation[w.relation] = [2]uuid.UUID{w.issueA, w.issueB}
	f.cycleTeam[w.cycle] = w.teamB
	f.milestoneProject[w.milestone] = w.projAB
	f.updateProject[w.update] = w.projNone
	f.labelTeam[w.orgLabel], f.labelTeam[w.teamLabel] = nil, &w.teamB
	f.productTeams[w.product] = []uuid.UUID{w.teamB}
	f.productTeams[w.noTeamProd] = []uuid.UUID{}
	// Every id is "loaded": found or not.
	for t := range numTables {
		for _, p := range ids {
			f.loaded[t][*p] = true
		}
	}
	return w
}

func row(typ string, entity uuid.UUID, p map[string]any) dbq.ListLegacyEventsRow {
	raw, _ := json.Marshal(p)
	if p == nil {
		raw = []byte("{}")
	}
	return dbq.ListLegacyEventsRow{Type: typ, EntityID: entity, Payload: raw}
}

func TestBackfillPlace(t *testing.T) {
	w := newWorld()
	teams := func(ids ...uuid.UUID) Scope { return ScopeTeams(ids...) }
	legacy := Scope{}
	cases := []struct {
		name string
		row  dbq.ListLegacyEventsRow
		want Scope
	}{
		{"issue", row(IssueUpdated, w.issueA, map[string]any{"teamId": w.teamB}), teams(w.teamA)},
		{"deleted issue: payload team", row(IssueDeleted, w.goneIssue, map[string]any{"teamId": w.teamB}), teams(w.teamB)},
		{"deleted issue: payload team gone", row(IssueDeleted, w.goneIssue, map[string]any{"teamId": w.goneTeam}), legacy},
		{"deleted issue: old shape with parentId", row(IssueDeleted, w.goneIssue, map[string]any{"teamId": w.teamA, "parentId": w.issueB}), legacy},
		{"deleted issue: old shape with null parentId", row(IssueDeleted, w.goneIssue, map[string]any{"teamId": w.teamA, "parentId": nil}), legacy},
		{"deleted issue: old shape with detachedChildren", row(IssueDeleted, w.goneIssue, map[string]any{"teamId": w.teamA, "detachedChildren": 2}), legacy},
		{"updated issue: parentId does not matter", row(IssueUpdated, w.issueA, map[string]any{"parentId": w.issueB}), teams(w.teamA)},
		{"deleted issue: no payload", row(IssueUpdated, w.goneIssue, nil), legacy},
		{"deleted issue: malformed teamId", row(IssueUpdated, w.goneIssue, map[string]any{"teamId": "nope"}), legacy},
		{"attachment", row(AttachmentCreated, w.attachment, nil), teams(w.teamB)},
		{"attachment gone: payload issue", row(AttachmentCreated, uuid.New(), map[string]any{"issueId": w.issueA}), teams(w.teamA)},
		{"attachment gone: issue gone", row(AttachmentCreated, uuid.New(), map[string]any{"issueId": w.goneIssue}), legacy},
		{"issue comment", row(CommentUpdated, w.commentIssue, nil), teams(w.teamA)},
		{"document comment", row(CommentCreated, w.commentDoc, nil), teams(w.teamA)},
		{"deleted comment: payload issue", row(CommentDeleted, w.goneComment, map[string]any{"issueId": w.issueB}), teams(w.teamB)},
		{"deleted comment: payload document", row(CommentDeleted, w.goneComment, map[string]any{"issueId": nil, "documentId": w.docNone}), ScopeOrg()},
		{"deleted comment: document gone", row(CommentDeleted, w.goneComment, map[string]any{"documentId": w.goneDoc}), legacy},
		{"relation", row(RelationCreated, w.relation, nil), ScopeAllTeams(w.teamA, w.teamB)},
		{"deleted relation: payload issues", row(RelationDeleted, uuid.New(), map[string]any{"issueId": w.issueB, "relatedIssueId": w.issueA}), ScopeAllTeams(w.teamA, w.teamB)},
		{"deleted relation: one issue gone", row(RelationDeleted, uuid.New(), map[string]any{"issueId": w.issueA, "relatedIssueId": w.goneIssue}), legacy},
		{"deleted relation: one id missing", row(RelationDeleted, uuid.New(), map[string]any{"issueId": w.issueA}), legacy},
		{"team", row(TeamUpdated, w.teamA, nil), teams(w.teamA)},
		{"deleted team", row(TeamDeleted, w.goneTeam, map[string]any{"id": w.goneTeam}), legacy},
		{"cycle", row(CycleUpdated, w.cycle, nil), teams(w.teamB)},
		{"deleted cycle: payload team", row(CycleDeleted, uuid.New(), map[string]any{"teamId": w.teamA}), teams(w.teamA)},
		{"deleted cycle: no team", row(CycleUpdated, uuid.New(), nil), legacy},
		{"project", row(ProjectUpdated, w.projAB, nil), teams(w.teamA, w.teamB)},
		{"project with no team", row(ProjectCreated, w.projNone, nil), ScopeOrg()},
		{"deleted project", row(ProjectDeleted, w.goneProj, nil), legacy},
		{"update due", row(ProjectUpdateDue, w.projA, map[string]any{"projectId": w.projA}), teams(w.teamA)},
		{"project update", row(ProjectUpdateCreated, w.update, nil), ScopeOrg()},
		{"project update gone: payload project", row(ProjectUpdateCreated, uuid.New(), map[string]any{"projectId": w.projA}), teams(w.teamA)},
		{"milestone", row(MilestoneUpdated, w.milestone, nil), teams(w.teamA, w.teamB)},
		{"milestone gone: payload project", row(MilestoneCreated, uuid.New(), map[string]any{"projectId": w.projA}), teams(w.teamA)},
		{"milestone gone: project gone", row(MilestoneCreated, uuid.New(), map[string]any{"projectId": w.goneProj}), legacy},
		{"document", row(DocumentUpdated, w.docA, nil), teams(w.teamA)},
		{"document with no project", row(DocumentCreated, w.docNone, map[string]any{"projectId": w.projA}), ScopeOrg()},
		{"document gone: payload project", row(DocumentCreated, w.goneDoc, map[string]any{"projectId": w.projAB}), teams(w.teamA, w.teamB)},
		{"document gone: null project", row(DocumentCreated, w.goneDoc, map[string]any{"projectId": nil}), legacy},
		{"org label", row(LabelCreated, w.orgLabel, nil), ScopeOrg()},
		{"team label", row(LabelCreated, w.teamLabel, nil), teams(w.teamB)},
		{"label gone: payload team", row(LabelCreated, w.goneLabel, map[string]any{"teamId": w.teamA}), teams(w.teamA)},
		{"label gone: no team", row(LabelCreated, w.goneLabel, map[string]any{"name": "x"}), legacy},
		{"product updated", row(ProductUpdated, w.product, nil), teams(w.teamB)},
		{"product updated, no team", row(ProductUpdated, w.noTeamProd, nil), ScopeOrg()},
		{"product updated, gone", row(ProductUpdated, uuid.New(), nil), legacy},
		{"product created", row(ProductCreated, w.product, map[string]any{"teamIds": []uuid.UUID{w.teamB, w.teamA}}), ScopeAllTeams(w.teamA, w.teamB)},
		{"product created, no team", row(ProductCreated, w.product, map[string]any{"teamIds": []uuid.UUID{}}), ScopeOrg()},
		{"product deleted, null teams", row(ProductDeleted, uuid.New(), map[string]any{"teamIds": nil}), ScopeOrg()},
		{"product deleted, gone team kept", row(ProductDeleted, uuid.New(), map[string]any{"teamIds": []uuid.UUID{w.goneTeam}}), ScopeAllTeams(w.goneTeam)},
		{"product teams changed", row(ProductTeamsChanged, w.product, map[string]any{"teamIds": []uuid.UUID{w.teamB}, "added": []uuid.UUID{w.teamB}, "removed": []uuid.UUID{w.teamA}}), ScopeAllTeams(w.teamA, w.teamB)},
		{"product: no teamIds", row(ProductCreated, w.product, map[string]any{"slug": "x"}), legacy},
		{"product: malformed removed", row(ProductTeamsChanged, w.product, map[string]any{"teamIds": []uuid.UUID{w.teamB}, "removed": "x"}), legacy},
		{"org guidance", row(AgentGuidanceUpdated, uuid.New(), map[string]any{"team": nil, "cleared": false}), ScopeOrg()},
		{"team guidance", row(AgentGuidanceUpdated, uuid.New(), map[string]any{"team": "ORB", "cleared": false}), legacy},
		{"guidance without team field", row(AgentGuidanceUpdated, uuid.New(), nil), legacy},
		{"chat service", row(ChatServiceUpdated, uuid.New(), map[string]any{"teamId": w.teamA}), teams(w.teamA)},
		{"chat service without team", row(ChatServiceUpdated, uuid.New(), map[string]any{"kind": "slack"}), legacy},
		// LAUN-225: admin data whatever the entity or payload (fail closed).
		{"member access", row(MemberAccessChanged, uuid.New(), map[string]any{"userId": uuid.New(), "teamIds": []uuid.UUID{w.teamA}}), ScopeAdmin()},
		{"member access without payload", row(MemberAccessChanged, w.teamA, nil), ScopeAdmin()},
		{"org kind", row(OrgUpdated, uuid.New(), nil), ScopeOrg()},
		{"legacy slack", row(SlackUpdated, uuid.New(), nil), ScopeOrg()},
		{"unknown type", row("member.made_up", uuid.New(), nil), legacy},
		{"payload not an object", dbq.ListLegacyEventsRow{Type: IssueDeleted, EntityID: w.goneIssue, Payload: []byte(`[1]`)}, legacy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for tb := range numTables { // a made-up entity id is a gone row
				w.f.loaded[tb][c.row.EntityID] = true
			}
			w.f.resetWants()
			got, ok := w.f.place(c.row)
			if w.f.wanting() {
				t.Fatalf("wanted ids that are loaded: %v", w.f.want)
			}
			if !ok {
				got = Scope{}
			}
			if got.kind != c.want.kind || !slices.Equal(got.teams, c.want.teams) {
				t.Fatalf("place = %q %v, want %q %v", got.kind, got.teams, c.want.kind, c.want.teams)
			}
			if ok && checkScope(c.row.Type, got) != nil {
				t.Fatalf("placed scope is not allowed for %s: %v", c.row.Type, checkScope(c.row.Type, got))
			}
		})
	}
}

// Every event type has a backfill rule or is named here as never placed.
func TestBackfillCoversEveryType(t *testing.T) {
	w := newWorld()
	neverPlaced := map[string]bool{} // none: every type can be placed in some case
	for typ := range kindScopes {
		placedSome := false
		for _, entity := range []uuid.UUID{w.issueA, w.commentIssue, w.attachment, w.relation, w.teamA, w.cycle, w.projA,
			w.update, w.milestone, w.docA, w.orgLabel, w.product} {
			p := map[string]any{"teamId": w.teamA, "teamIds": []uuid.UUID{w.teamA}, "team": nil}
			w.f.resetWants()
			if _, ok := w.f.place(row(typ, entity, p)); ok {
				placedSome = true
			}
		}
		if !placedSome && !neverPlaced[typ] {
			t.Errorf("event type %s has no backfill rule (internal/events/backfill.go place)", typ)
		}
	}
}

// A lookup of an id that is not loaded yet is wanted, never a "not found".
func TestBackfillWantsBeforeDeciding(t *testing.T) {
	f := newFacts()
	comment, issue := uuid.New(), uuid.New()
	r := row(CommentCreated, comment, map[string]any{"issueId": issue})
	if _, _ = f.place(r); !f.want[tComment][comment] {
		t.Fatalf("the comment was not wanted: %v", f.want)
	}
	f.loaded[tComment][comment] = true // the comment is gone
	f.resetWants()
	if _, _ = f.place(r); !f.want[tIssue][issue] {
		t.Fatalf("the payload issue was not wanted: %v", f.want)
	}
}
