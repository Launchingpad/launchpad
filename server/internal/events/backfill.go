package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/dbq"
)

// The events backfill job (decisions-T20 #57, ticket 6b, LAUN-222).
//
// Rows made before migration 0038, and rows an older image writes after a
// rollback, have scope 'legacy': a restricted member never reads them (#55).
// This job places them: it derives the scope and the team list from the row's
// entity as it is NOW (the issue's team, the project's teams, ...), and, where
// the entity is gone, from the ids in the payload. What it cannot place stays
// legacy (fail closed). It writes the admin scope for one type only,
// member.access_changed (LAUN-225), which is admin data whatever its entity.
//
// It runs once at every boot (cmd/server), so a roll-forward places the rows
// an older image wrote. A session try advisory lock makes one instance do it.
// It walks the legacy rows in primary-key order, in batches; each UPDATE
// touches one batch's rows only and only rows still 'legacy', so it holds no
// long lock, a crash loses nothing (the next run starts again and finds the
// rows that are still legacy) and a second run changes nothing. It emits no
// event and logs counts only, never a payload.

const (
	// BackfillBatch is the default number of legacy rows one batch reads.
	BackfillBatch = 1000
	// BackfillPause is the default wait between two full batches, so a large
	// history does not make a burst of WAL.
	BackfillPause = 50 * time.Millisecond
	// backfillRounds bounds the lookup rounds of one batch. A row needs at most
	// three (a comment, its document, the document's project).
	backfillRounds = 6
)

// The job's session advisory lock (a try lock; the key is a hash of a fixed name).
const (
	backfillLockSQL   = `SELECT pg_try_advisory_lock(hashtextextended('devops:events:backfill', 0))`
	backfillUnlockSQL = `SELECT pg_advisory_unlock(hashtextextended('devops:events:backfill', 0))`
)

// BackfillOptions tune one run. The zero value takes the defaults.
type BackfillOptions struct {
	BatchSize int           // rows per batch; 0: BackfillBatch
	Pause     time.Duration // between full batches; 0: BackfillPause, <0: none
	// afterBatch, when set, runs after each batch with its number (from 1);
	// an error stops the run (tests: a crash in the middle).
	afterBatch func(n int) error
}

// BackfillResult counts what one run did.
type BackfillResult struct {
	Scanned int64 // legacy rows read
	Placed  int64 // rows given a scope
	Legacy  int64 // rows read and left legacy (no entity and no usable payload)
	Batches int
}

// Backfill places the legacy rows of every org, once, under the job's
// advisory lock. locked is false when another session holds the lock: then
// it does nothing.
func Backfill(ctx context.Context, pool *pgxpool.Pool, o BackfillOptions) (res BackfillResult, locked bool, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return res, false, fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	if err := conn.QueryRow(ctx, backfillLockSQL).Scan(&locked); err != nil {
		return res, false, fmt.Errorf("lock: %w", err)
	}
	if !locked {
		return res, false, nil
	}
	defer func() {
		// The caller's ctx may be done; the unlock must still run, or the
		// connection is destroyed (which also frees a session lock).
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var ok bool
		if uerr := conn.QueryRow(uctx, backfillUnlockSQL).Scan(&ok); uerr != nil || !ok {
			_ = conn.Conn().Close(uctx)
		}
	}()
	res, err = backfill(ctx, dbq.New(conn), o)
	return res, true, err
}

// RunBackfill is the boot job: one locked run, logged with counts only.
func RunBackfill(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) {
	res, locked, err := Backfill(ctx, pool, BackfillOptions{})
	if ctx.Err() != nil {
		return
	}
	switch {
	case err != nil:
		log.ErrorContext(ctx, "events backfill failed; the next boot runs it again", "err", err,
			"scanned", res.Scanned, "placed", res.Placed, "legacy", res.Legacy, "batches", res.Batches)
	case !locked:
		log.InfoContext(ctx, "events backfill: another instance is running it")
	case res.Scanned > 0:
		log.InfoContext(ctx, "events backfill", "scanned", res.Scanned, "placed", res.Placed,
			"legacy", res.Legacy, "batches", res.Batches)
	}
}

// backfill walks the legacy rows after the cursor (org, seq) in batches.
func backfill(ctx context.Context, q *dbq.Queries, o BackfillOptions) (BackfillResult, error) {
	var res BackfillResult
	size := o.BatchSize
	if size <= 0 {
		size = BackfillBatch
	}
	pause := o.Pause
	if pause == 0 {
		pause = BackfillPause
	}
	var afterOrg uuid.UUID
	var afterSeq int64
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		rows, err := q.ListLegacyEvents(ctx, dbq.ListLegacyEventsParams{AfterOrg: afterOrg, AfterSeq: afterSeq, BatchSize: int32(size)})
		if err != nil {
			return res, fmt.Errorf("list legacy events: %w", err)
		}
		if len(rows) == 0 {
			return res, nil
		}
		res.Batches++
		res.Scanned += int64(len(rows))
		for start := 0; start < len(rows); {
			end := start + 1
			for end < len(rows) && rows[end].OrgID == rows[start].OrgID {
				end++
			}
			placed, err := placeOrg(ctx, q, rows[start:end])
			res.Placed += placed
			if err != nil {
				return res, err
			}
			start = end
		}
		res.Legacy = res.Scanned - res.Placed
		last := rows[len(rows)-1]
		afterOrg, afterSeq = last.OrgID, last.Seq
		if o.afterBatch != nil {
			if err := o.afterBatch(res.Batches); err != nil {
				return res, err
			}
		}
		if len(rows) < size {
			return res, nil
		}
		if pause > 0 {
			if !sleepCtx(ctx, pause) {
				return res, ctx.Err()
			}
		}
	}
}

// placeOrg places rows of one org: it resolves their scopes, then writes one
// UPDATE per distinct scope. It returns the number of rows it placed.
func placeOrg(ctx context.Context, q *dbq.Queries, rows []dbq.ListLegacyEventsRow) (int64, error) {
	org := rows[0].OrgID
	f := newFacts()
	scopes := make([]Scope, len(rows))
	for range backfillRounds {
		f.resetWants()
		for i, r := range rows {
			scopes[i] = Scope{}
			if s, ok := f.place(r); ok {
				scopes[i] = s
			}
		}
		if !f.wanting() {
			break
		}
		if err := f.load(ctx, q, org); err != nil {
			return 0, err
		}
	}
	if f.wanting() {
		// Never reached with the rules below (depth three); fail closed.
		return 0, nil
	}
	type group struct {
		scope Scope
		seqs  []int64
	}
	groups := map[string]*group{}
	var order []string
	for i, s := range scopes {
		if s.kind == "" || checkScope(rows[i].Type, s) != nil {
			continue // stays legacy
		}
		key := s.kind + ":" + idsKey(s.teams)
		g, ok := groups[key]
		if !ok {
			g = &group{scope: s}
			groups[key] = g
			order = append(order, key)
		}
		g.seqs = append(g.seqs, rows[i].Seq)
	}
	var placed int64
	for _, key := range order {
		g := groups[key]
		n, err := q.PlaceLegacyEvents(ctx, dbq.PlaceLegacyEventsParams{
			Scope: g.scope.kind, TeamIds: g.scope.TeamIDs(), OrgID: org, Seqs: g.seqs,
		})
		placed += n
		if err != nil {
			return placed, fmt.Errorf("place legacy events: %w", err)
		}
	}
	return placed, nil
}

func idsKey(ids []uuid.UUID) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id.String()
	}
	return strings.Join(parts, ",")
}

// table names one lookup of the job.
type table int

const (
	tIssue table = iota
	tComment
	tAttachment
	tRelation
	tCycle
	tTeam
	tProject
	tMilestone
	tDocument
	tProjectUpdate
	tLabel
	tProduct
	numTables
)

type commentTarget struct{ issue, document *uuid.UUID }

// facts holds the rows the job looked up for one org. A lookup of an id not
// loaded yet records it in want and answers "not found"; placeOrg then loads
// every wanted id and places the rows again, so a result counts only from a
// round that wanted nothing.
type facts struct {
	loaded, want [numTables]map[uuid.UUID]bool

	issueTeam, cycleTeam                             map[uuid.UUID]uuid.UUID
	attachmentIssue, milestoneProject, updateProject map[uuid.UUID]uuid.UUID
	comment                                          map[uuid.UUID]commentTarget
	relation                                         map[uuid.UUID][2]uuid.UUID
	team                                             map[uuid.UUID]bool
	projectTeams, productTeams                       map[uuid.UUID][]uuid.UUID
	documentProject, labelTeam                       map[uuid.UUID]*uuid.UUID
}

func newFacts() *facts {
	f := &facts{
		issueTeam: map[uuid.UUID]uuid.UUID{}, cycleTeam: map[uuid.UUID]uuid.UUID{},
		attachmentIssue: map[uuid.UUID]uuid.UUID{}, milestoneProject: map[uuid.UUID]uuid.UUID{},
		updateProject: map[uuid.UUID]uuid.UUID{}, comment: map[uuid.UUID]commentTarget{},
		relation: map[uuid.UUID][2]uuid.UUID{}, team: map[uuid.UUID]bool{},
		projectTeams: map[uuid.UUID][]uuid.UUID{}, productTeams: map[uuid.UUID][]uuid.UUID{},
		documentProject: map[uuid.UUID]*uuid.UUID{}, labelTeam: map[uuid.UUID]*uuid.UUID{},
	}
	for t := range numTables {
		f.loaded[t] = map[uuid.UUID]bool{}
	}
	f.resetWants()
	return f
}

func (f *facts) resetWants() {
	for t := range numTables {
		f.want[t] = map[uuid.UUID]bool{}
	}
}

func (f *facts) wanting() bool {
	for t := range numTables {
		if len(f.want[t]) > 0 {
			return true
		}
	}
	return false
}

// known reports whether id of table t was loaded; if not, it is wanted.
func (f *facts) known(t table, id uuid.UUID) bool {
	if f.loaded[t][id] {
		return true
	}
	f.want[t][id] = true
	return false
}

func get[V any](f *facts, t table, m map[uuid.UUID]V, id uuid.UUID) (V, bool) {
	var zero V
	if id == uuid.Nil || !f.known(t, id) {
		return zero, false
	}
	v, ok := m[id]
	return v, ok
}

// load reads every wanted id, one query per table.
func (f *facts) load(ctx context.Context, q *dbq.Queries, org uuid.UUID) error {
	ids := func(t table) []uuid.UUID {
		out := make([]uuid.UUID, 0, len(f.want[t]))
		for id := range f.want[t] {
			out = append(out, id)
			f.loaded[t][id] = true
		}
		return out
	}
	if want := ids(tIssue); len(want) > 0 {
		rows, err := q.BackfillIssueTeams(ctx, dbq.BackfillIssueTeamsParams{OrgID: org, Ids: want})
		if err != nil {
			return fmt.Errorf("backfill: issues: %w", err)
		}
		for _, r := range rows {
			f.issueTeam[r.ID] = r.TeamID
		}
	}
	if want := ids(tComment); len(want) > 0 {
		rows, err := q.BackfillCommentTargets(ctx, dbq.BackfillCommentTargetsParams{OrgID: org, Ids: want})
		if err != nil {
			return fmt.Errorf("backfill: comments: %w", err)
		}
		for _, r := range rows {
			f.comment[r.ID] = commentTarget{issue: r.IssueID, document: r.DocumentID}
		}
	}
	if want := ids(tAttachment); len(want) > 0 {
		rows, err := q.BackfillAttachmentIssues(ctx, dbq.BackfillAttachmentIssuesParams{OrgID: org, Ids: want})
		if err != nil {
			return fmt.Errorf("backfill: attachments: %w", err)
		}
		for _, r := range rows {
			f.attachmentIssue[r.ID] = r.IssueID
		}
	}
	if want := ids(tRelation); len(want) > 0 {
		rows, err := q.BackfillRelationIssues(ctx, dbq.BackfillRelationIssuesParams{OrgID: org, Ids: want})
		if err != nil {
			return fmt.Errorf("backfill: relations: %w", err)
		}
		for _, r := range rows {
			f.relation[r.ID] = [2]uuid.UUID{r.IssueID, r.RelatedIssueID}
		}
	}
	if want := ids(tCycle); len(want) > 0 {
		rows, err := q.BackfillCycleTeams(ctx, dbq.BackfillCycleTeamsParams{OrgID: org, Ids: want})
		if err != nil {
			return fmt.Errorf("backfill: cycles: %w", err)
		}
		for _, r := range rows {
			f.cycleTeam[r.ID] = r.TeamID
		}
	}
	if want := ids(tTeam); len(want) > 0 {
		rows, err := q.BackfillTeams(ctx, dbq.BackfillTeamsParams{OrgID: org, Ids: want})
		if err != nil {
			return fmt.Errorf("backfill: teams: %w", err)
		}
		for _, id := range rows {
			f.team[id] = true
		}
	}
	if want := ids(tProject); len(want) > 0 {
		rows, err := q.BackfillProjectTeams(ctx, dbq.BackfillProjectTeamsParams{OrgID: org, Ids: want})
		if err != nil {
			return fmt.Errorf("backfill: projects: %w", err)
		}
		for _, r := range rows {
			f.projectTeams[r.ID] = r.TeamIds
		}
	}
	if want := ids(tMilestone); len(want) > 0 {
		rows, err := q.BackfillMilestoneProjects(ctx, dbq.BackfillMilestoneProjectsParams{OrgID: org, Ids: want})
		if err != nil {
			return fmt.Errorf("backfill: milestones: %w", err)
		}
		for _, r := range rows {
			f.milestoneProject[r.ID] = r.ProjectID
		}
	}
	if want := ids(tDocument); len(want) > 0 {
		rows, err := q.BackfillDocumentProjects(ctx, dbq.BackfillDocumentProjectsParams{OrgID: org, Ids: want})
		if err != nil {
			return fmt.Errorf("backfill: documents: %w", err)
		}
		for _, r := range rows {
			f.documentProject[r.ID] = r.ProjectID
		}
	}
	if want := ids(tProjectUpdate); len(want) > 0 {
		rows, err := q.BackfillProjectUpdateProjects(ctx, dbq.BackfillProjectUpdateProjectsParams{OrgID: org, Ids: want})
		if err != nil {
			return fmt.Errorf("backfill: project updates: %w", err)
		}
		for _, r := range rows {
			f.updateProject[r.ID] = r.ProjectID
		}
	}
	if want := ids(tLabel); len(want) > 0 {
		rows, err := q.BackfillLabelTeams(ctx, dbq.BackfillLabelTeamsParams{OrgID: org, Ids: want})
		if err != nil {
			return fmt.Errorf("backfill: labels: %w", err)
		}
		for _, r := range rows {
			f.labelTeam[r.ID] = r.TeamID
		}
	}
	if want := ids(tProduct); len(want) > 0 {
		rows, err := q.BackfillProductTeams(ctx, dbq.BackfillProductTeamsParams{OrgID: org, Ids: want})
		if err != nil {
			return fmt.Errorf("backfill: products: %w", err)
		}
		for _, r := range rows {
			f.productTeams[r.ID] = r.TeamIds
		}
	}
	return nil
}

// payload is the decoded payload of a legacy row: only its ids are read.
type payload map[string]json.RawMessage

func decodePayload(raw []byte) payload {
	var p payload
	if json.Unmarshal(raw, &p) != nil {
		return nil // not an object: no payload ids
	}
	return p
}

// id reads a uuid string field; null, missing, malformed or the nil id is false.
func (p payload) id(key string) (uuid.UUID, bool) {
	var s string
	if raw, ok := p[key]; !ok || json.Unmarshal(raw, &s) != nil {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(s)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}

// has reports a field that is present, whatever its value.
func (p payload) has(key string) bool {
	_, ok := p[key]
	return ok
}

// isNull reports a field that is present and JSON null.
func (p payload) isNull(key string) bool {
	raw, ok := p[key]
	return ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// ids reads a list of uuid strings. null counts as an empty list (the
// writers marshal an empty Go slice that way); a missing field or any
// malformed element is false.
func (p payload) ids(key string) ([]uuid.UUID, bool) {
	raw, ok := p[key]
	if !ok {
		return nil, false
	}
	var ss []string
	if json.Unmarshal(raw, &ss) != nil {
		return nil, false
	}
	out := make([]uuid.UUID, 0, len(ss))
	for _, s := range ss {
		id, err := uuid.Parse(s)
		if err != nil || id == uuid.Nil {
			return nil, false
		}
		out = append(out, id)
	}
	return out, true
}

// orgKinds are the event types that are always org-level (#7, build notes 6a).
var orgKinds = []string{
	ProjectStatusCreated, ProjectStatusUpdated, ProjectStatusDeleted, ProjectStatusesReordered,
	ProjectLabelCreated, ProjectLabelUpdated, ProjectLabelDeleted,
	OrgUpdated, OnboardingStepChanged, GitIdentityRemoved, SlackUpdated, PushUpdated,
}

// place is the rule of #57 for one row: the scope Emit gives the type (#54,
// build notes 6a), from the entity now, else from the payload ids. false:
// the row stays legacy.
func (f *facts) place(r dbq.ListLegacyEventsRow) (Scope, bool) {
	p := decodePayload(r.Payload)
	id := r.EntityID
	switch r.Type {
	case IssueCreated, IssueUpdated, IssueDeleted:
		if r.Type == IssueDeleted && (p.has("parentId") || p.has("detachedChildren")) {
			// The pre-6a shape names the parent and counts the children,
			// which may be in a team the reader cannot see (#42, LAUN-221
			// review fixes): legacy (fail closed).
			return Scope{}, false
		}
		if s, ok := f.issueScope(id); ok {
			return s, true
		}
		return f.teamScope(p) // a deleted issue: the payload's teamId
	case AttachmentCreated:
		if iss, ok := get(f, tAttachment, f.attachmentIssue, id); ok {
			return f.issueScope(iss)
		}
		if iss, ok := p.id("issueId"); ok {
			return f.issueScope(iss)
		}
	case CommentCreated, CommentUpdated, CommentDeleted:
		var issue, doc *uuid.UUID
		if c, ok := get(f, tComment, f.comment, id); ok {
			issue, doc = c.issue, c.document
		} else {
			if v, ok := p.id("issueId"); ok {
				issue = &v
			}
			if v, ok := p.id("documentId"); ok {
				doc = &v
			}
		}
		switch {
		case issue != nil:
			return f.issueScope(*issue)
		case doc != nil:
			return f.documentScope(*doc)
		}
	case RelationCreated, RelationDeleted:
		pair, ok := get(f, tRelation, f.relation, id)
		if !ok {
			a, okA := p.id("issueId")
			b, okB := p.id("relatedIssueId")
			if !okA || !okB {
				return Scope{}, false
			}
			pair = [2]uuid.UUID{a, b}
		}
		// Both issues' teams, all of them (#54). With one side gone its team is
		// unknown, so the row stays legacy.
		ta, okA := get(f, tIssue, f.issueTeam, pair[0])
		tb, okB := get(f, tIssue, f.issueTeam, pair[1])
		if okA && okB {
			return ScopeAllTeams(ta, tb), true
		}
	case TeamCreated, TeamUpdated, TeamDeleted:
		if _, ok := get(f, tTeam, f.team, id); ok {
			return ScopeTeams(id), true
		}
	case CycleCreated, CycleUpdated, CycleCompleted, CycleDeleted:
		if team, ok := get(f, tCycle, f.cycleTeam, id); ok {
			return ScopeTeams(team), true
		}
		return f.teamScope(p)
	case ProjectCreated, ProjectUpdated, ProjectDeleted, ProjectUpdateDue:
		return f.projectScope(id)
	case ProjectUpdateCreated:
		if proj, ok := get(f, tProjectUpdate, f.updateProject, id); ok {
			return f.projectScope(proj)
		}
		if proj, ok := p.id("projectId"); ok {
			return f.projectScope(proj)
		}
	case MilestoneCreated, MilestoneUpdated:
		if proj, ok := get(f, tMilestone, f.milestoneProject, id); ok {
			return f.projectScope(proj)
		}
		if proj, ok := p.id("projectId"); ok {
			return f.projectScope(proj)
		}
	case DocumentCreated, DocumentUpdated:
		if _, ok := get(f, tDocument, f.documentProject, id); ok {
			return f.documentScope(id)
		}
		// A deleted document: only a named project places it. A null
		// projectId said "no project" at create time, not now: legacy.
		if proj, ok := p.id("projectId"); ok {
			return f.projectScope(proj)
		}
	case LabelCreated:
		if team, ok := get(f, tLabel, f.labelTeam, id); ok {
			if team == nil {
				return ScopeOrg(), true // an org label (#8)
			}
			return ScopeTeams(*team), true
		}
		return f.teamScope(p)
	case ProductUpdated:
		if teams, ok := get(f, tProduct, f.productTeams, id); ok {
			if len(teams) == 0 {
				return ScopeOrg(), true
			}
			return ScopeTeams(teams...), true
		}
	case ProductCreated, ProductDeleted, ProductTeamsChanged:
		// The payload names the teams, so every team it names is in the
		// all-of scope (#9, build notes 6a): a deleted team makes it stricter.
		teams, ok := p.ids("teamIds")
		if !ok {
			return Scope{}, false
		}
		for _, key := range []string{"added", "removed"} {
			if more, ok := p.ids(key); ok {
				teams = append(teams, more...)
			} else if _, present := p[key]; present {
				return Scope{}, false
			}
		}
		if len(teams) == 0 {
			return ScopeOrg(), true
		}
		return ScopeAllTeams(teams...), true
	case AgentGuidanceUpdated:
		// The org entry is org-wide. A team entry names a team KEY, not an id,
		// and a key can belong to another team later: legacy.
		if p.isNull("team") {
			return ScopeOrg(), true
		}
	case ChatServiceUpdated:
		return f.teamScope(p)
	case MemberAccessChanged:
		// Who sees which team is admin data (#33, #48). A legacy row of this
		// type exists only after a migration down past 0038 and up again, or
		// an older image on a newer database: admin, by type (fail closed).
		return ScopeAdmin(), true
	default:
		if slices.Contains(orgKinds, r.Type) {
			return ScopeOrg(), true
		}
	}
	return Scope{}, false
}

// issueScope: the issue's team, when the issue exists.
func (f *facts) issueScope(issue uuid.UUID) (Scope, bool) {
	if team, ok := get(f, tIssue, f.issueTeam, issue); ok {
		return ScopeTeams(team), true
	}
	return Scope{}, false
}

// teamScope: the payload's teamId, when that team of the org exists.
func (f *facts) teamScope(p payload) (Scope, bool) {
	team, ok := p.id("teamId")
	if !ok {
		return Scope{}, false
	}
	if _, ok := get(f, tTeam, f.team, team); ok {
		return ScopeTeams(team), true
	}
	return Scope{}, false
}

// projectScope: the project's teams now; org when it has none (#36). A
// project that is gone is not org-level: its teams are unknown.
func (f *facts) projectScope(project uuid.UUID) (Scope, bool) {
	if teams, ok := get(f, tProject, f.projectTeams, project); ok {
		return ScopeProject(teams), true
	}
	return Scope{}, false
}

// documentScope: the document's project; org when it has none (#38).
func (f *facts) documentScope(doc uuid.UUID) (Scope, bool) {
	proj, ok := get(f, tDocument, f.documentProject, doc)
	if !ok {
		return Scope{}, false
	}
	if proj == nil {
		return ScopeOrg(), true
	}
	return f.projectScope(*proj)
}
