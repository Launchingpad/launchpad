package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/dbq"
	"devops/internal/tracker"
)

// Options configure one import run.
type Options struct {
	Org    string // org slug, e.g. "orbit"
	DryRun bool   // do everything in one transaction, then roll it back
	// Store is the attachment store the exported files are uploaded to.
	// It is required when the plan has files (see Plan.WithoutFiles).
	Store ObjectStore
	// ExportDir is opened as an os.Root to read the files from.
	ExportDir string
	// PublicBaseURL is the API origin download URLs are written on
	// (PUBLIC_BASE_URL); empty writes paths.
	PublicBaseURL string
	Log           *slog.Logger
	// Progress, when set, is told where the run is (LAUN-8, decisions-T17
	// #40): the phase name and done / total in it. The files phase counts
	// bytes; the entity phases count steps (done of total steps). It is
	// called from the run's goroutine and must return quickly; it never sees
	// a record, a title or a file name.
	Progress func(phase string, done, total int64)
}

// UploadCounts are the object store side of a run.
type UploadCounts struct {
	Uploaded int `json:"uploaded"` // PUT in this run
	Present  int `json:"present"`  // already in the store, same size: nothing sent
	Planned  int `json:"planned"`  // a dry run's would-be uploads
	// Keys are the object keys of the planned files, in plan order.
	Keys []string `json:"-"`
}

// Outcome of one record.
type Outcome int

// The outcomes, in increasing order of "what happened". A later step for
// the same record (its labels, its parent) may raise skip to update.
const (
	OutSkip Outcome = iota
	OutUpdate
	OutCreate
)

// Counts is one row of the report.
type Counts struct {
	Export   int `json:"export"`
	Excluded int `json:"excluded"`
	Create   int `json:"create"`
	Update   int `json:"update"`
	Skip     int `json:"skip"`
}

// Report is what a run did (or, with DryRun, would do).
type Report struct {
	DryRun              bool               `json:"dryRun"`
	Counts              map[string]*Counts `json:"counts"`
	Exclusions          []Exclusion        `json:"exclusions"`
	MergedLabels        int                `json:"mergedLabels"`
	MergedRelations     int                `json:"mergedRelations"`
	MissingCycleNumbers map[string][]int32 `json:"missingCycleNumbers"`
	NextNumbers         map[string]int32   `json:"nextIssueNumbers"`
	Cadence             map[string]string  `json:"cadence"`
	PendingUploads      int                `json:"pendingUploads"`
	Uploads             UploadCounts       `json:"uploads"`
	Verify              []VerifyRow        `json:"verify"`
	// Committed is true once the import transaction committed (never in a
	// dry run). A run that returns ErrMismatch with Committed false was
	// rolled back whole; with Committed true its rows are in the database
	// (decisions-T17 #42c).
	Committed bool `json:"committed"`
}

// ErrMismatch is returned when the database does not hold what the export
// says it should. cmd/importer exits non-zero on it.
var ErrMismatch = errors.New("verification failed: database counts differ from the export")

// ErrFilesImported refuses --skip-files on an org whose files an earlier
// import stored: that run would write the uploads.linear.app URLs back
// over the download URLs, and its verify would not look at files.
var ErrFilesImported = errors.New("this org already has imported files: --skip-files would put the uploads.linear.app URLs back; import with the object store (S3_*) instead")

// Run imports ex (planned as p) into the org. The whole import is ONE
// transaction: it is all-or-nothing, a dry run is the same transaction
// rolled back, and the verification inside it sees exactly what would be
// committed. It runs once, at cutover, before anyone uses the org (see
// decisions-T4 for why one long transaction is acceptable here).
func Run(ctx context.Context, pool *pgxpool.Pool, ex *Export, p *Plan, opts Options) (*Report, error) {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	rep := &Report{DryRun: opts.DryRun, Counts: map[string]*Counts{}, Exclusions: p.Exclusions,
		MissingCycleNumbers: p.MissingCycleNumbers, NextNumbers: map[string]int32{}, Cadence: map[string]string{}}
	for _, e := range Entities {
		rep.Counts[e] = &Counts{Export: p.ExportedN[e]}
	}
	for _, x := range p.Exclusions {
		rep.Counts[x.Entity].Excluded++
	}

	var dir *os.Root
	if len(p.Files) > 0 && opts.ExportDir != "" {
		d, err := os.OpenRoot(opts.ExportDir)
		if err != nil {
			return nil, fmt.Errorf("open export dir for files: %w", err)
		}
		defer func() { _ = d.Close() }()
		dir = d
	}

	org, err := dbq.New(pool).GetOrgBySlug(ctx, opts.Org)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("org %q does not exist (first-run setup makes the org: docs/deploy.md 'First-run setup')", opts.Org)
		}
		return nil, fmt.Errorf("org lookup: %w", err)
	}
	r := &run{ctx: ctx, q: dbq.New(pool), org: org.ID, orgSlug: org.Slug, p: p, rep: rep, log: opts.Log,
		out: map[string]map[string]Outcome{}, files: newFileResolver(), store: opts.Store, dir: dir,
		dryRun: opts.DryRun, publicBase: opts.PublicBaseURL, progress: opts.Progress}
	_ = ex // the plan carries everything the run writes

	// The object store first, OUTSIDE the transaction: no row lock is held
	// across a network call (decisions-T3b #6). The ids are seeded, so the
	// keys do not depend on this run committing.
	if err := r.loadMappings(); err != nil {
		return nil, err
	}
	if p.SkipFiles && len(r.maps[EntFile]) > 0 {
		return nil, ErrFilesImported
	}
	start := time.Now()
	if err := r.uploads(); err != nil {
		return nil, fmt.Errorf("import files: %w", err)
	}
	opts.Log.Info("import step done", "step", "uploads", "uploaded", rep.Uploads.Uploaded,
		"present", rep.Uploads.Present, "planned", rep.Uploads.Planned, "took", time.Since(start).Round(time.Millisecond).String())

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	q := dbq.New(tx)
	r.q = q
	if err := q.ImportLockOrg(ctx, org.ID); err != nil {
		return nil, fmt.Errorf("import lock: %w", err)
	}
	if err := r.loadMappings(); err != nil {
		return nil, err
	}
	if err := r.checkStoredIDs(); err != nil {
		return nil, err
	}
	if p.SkipFiles && len(r.maps[EntFile]) > 0 { // stored in between
		return nil, ErrFilesImported
	}
	steps := []struct {
		name string
		fn   func() error
	}{
		{"users", r.users}, {"teams", r.teams}, {"states", r.states}, {"labels", r.labels},
		{"projects", r.projects}, {"milestones", r.milestones}, {"cycles", r.cycles},
		{"snapshots", r.snapshots}, {"cadence", r.cadence}, {"issues", r.issues},
		{"state history", r.stateChanges}, {"documents", r.documents}, {"comments", r.comments},
		{"relations", r.relations}, {"attachments", r.attachments}, {"file rows", r.textFiles},
		{"counters", r.counters},
	}
	for i, s := range steps {
		// A cancel is prompt: the context is checked between entities
		// (decisions-T17 #40), and the driver rolls the transaction back.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("import %s: %w", s.name, err)
		}
		r.report(s.name, int64(i), int64(len(steps)))
		start := time.Now()
		if err := s.fn(); err != nil {
			return nil, fmt.Errorf("import %s: %w", s.name, err)
		}
		opts.Log.Info("import step done", "step", s.name, "took", time.Since(start).Round(time.Millisecond).String())
	}
	r.report("verify", int64(len(steps)), int64(len(steps)))
	r.finishCounts()
	rep.PendingUploads = len(r.files.pending)

	var objects ObjectStore // objects are checked once they are all uploaded
	if !opts.DryRun {
		objects = opts.Store
	}
	rows, err := Verify(ctx, q, org.ID, p, objects)
	if err != nil {
		return nil, err
	}
	rep.Verify = rows
	if len(Mismatches(rows)) > 0 {
		return rep, ErrMismatch // rolled back by the deferred Rollback
	}
	if opts.DryRun {
		return rep, nil // rolled back
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	rep.Committed = true
	// An independent read after the commit: what is actually stored.
	rows, err = Verify(ctx, dbq.New(pool), org.ID, p, objects)
	if err != nil {
		return rep, err
	}
	rep.Verify = rows
	if len(Mismatches(rows)) > 0 {
		return rep, ErrMismatch
	}
	return rep, nil
}

type run struct {
	ctx   context.Context
	q     *dbq.Queries
	org   uuid.UUID
	p     *Plan
	rep   *Report
	log   *slog.Logger
	maps  map[string]map[string]uuid.UUID // entity -> Linear id -> dev-ops id
	out   map[string]map[string]Outcome   // entity -> Linear id -> outcome
	files *fileResolver
	store ObjectStore
	dir   *os.Root
	// stored is the id and key of each planned file (Plan.Files order).
	stored     []storedFile
	orgSlug    string
	publicBase string
	dryRun     bool
	// teamKeys: Linear team id -> key, for the report.
	teamKeys map[string]string
	progress func(phase string, done, total int64)
}

// report tells the progress callback, when there is one.
func (r *run) report(phase string, done, total int64) {
	if r.progress != nil {
		r.progress(phase, done, total)
	}
}

func (r *run) loadMappings() error {
	r.maps = map[string]map[string]uuid.UUID{}
	rows, err := r.q.ImportListMappings(r.ctx, r.org)
	if err != nil {
		return fmt.Errorf("load import mappings: %w", err)
	}
	for _, m := range rows {
		r.setLocal(m.EntityType, m.SourceID, m.TargetID)
	}
	return nil
}

// checkStoredIDs refuses to go on when a file's mapping changed between the
// uploads and the transaction (another importer ran in between).
func (r *run) checkStoredIDs() error {
	for i, fp := range r.p.Files {
		if id, ok := r.fileTarget(fp); ok && id != r.stored[i].id {
			return fmt.Errorf("file %d: its mapping changed while files were uploaded; run the import again", i)
		}
	}
	return nil
}

func (r *run) setLocal(ent, src string, tgt uuid.UUID) {
	if r.maps[ent] == nil {
		r.maps[ent] = map[string]uuid.UUID{}
	}
	r.maps[ent][src] = tgt
}

// target is the mapped dev-ops id of a Linear id.
func (r *run) target(ent, src string) (uuid.UUID, bool) {
	id, ok := r.maps[ent][src]
	return id, ok
}

// ref is target as a nullable column value ("" or unmapped: NULL).
func (r *run) ref(ent, src string) *uuid.UUID {
	if src == "" {
		return nil
	}
	if id, ok := r.target(ent, src); ok {
		return &id
	}
	return nil
}

// mapTo records src -> tgt in import_mappings. It reports whether the
// mapping is new or changed.
func (r *run) mapTo(ent, src string, tgt uuid.UUID) (bool, error) {
	if cur, ok := r.target(ent, src); ok && cur == tgt {
		return false, nil
	}
	if _, err := r.q.ImportUpsertMapping(r.ctx, dbq.ImportUpsertMappingParams{
		OrgID: r.org, EntityType: ent, SourceID: src, TargetID: tgt,
	}); err != nil {
		return false, fmt.Errorf("map %s %s: %w", ent, src, err)
	}
	r.setLocal(ent, src, tgt)
	return true, nil
}

// note records an outcome; a higher outcome wins.
func (r *run) note(ent, src string, o Outcome) {
	if r.out[ent] == nil {
		r.out[ent] = map[string]Outcome{}
	}
	if cur, ok := r.out[ent][src]; !ok || o > cur {
		r.out[ent][src] = o
	}
}

// upserted turns an upsert result into an outcome. A row came back: it was
// inserted (create, when nothing was mapped before) or changed (update).
// pgx.ErrNoRows: the row exists and is unchanged (skip), unless the mapping
// itself is new (an adopted row: update).
func (r *run) upserted(ent, src string, wasMapped, newMapping bool, err error) error {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if newMapping {
			r.note(ent, src, OutUpdate)
		} else {
			r.note(ent, src, OutSkip)
		}
		return nil
	case err != nil:
		return fmt.Errorf("write %s %s: %w", ent, src, err)
	case !wasMapped:
		r.note(ent, src, OutCreate)
	default:
		r.note(ent, src, OutUpdate)
	}
	return nil
}

func (r *run) finishCounts() {
	for ent, m := range r.out {
		c := r.rep.Counts[ent]
		for _, o := range m {
			switch o {
			case OutCreate:
				c.Create++
			case OutUpdate:
				c.Update++
			default:
				c.Skip++
			}
		}
	}
}

func newID(t time.Time) (uuid.UUID, error) { return NewV7At(t) }

// idFor is the mapped id of src, or a new UUIDv7 at createdAt.
func (r *run) idFor(ent, src string, createdAt time.Time) (uuid.UUID, bool, error) {
	if id, ok := r.target(ent, src); ok {
		return id, true, nil
	}
	id, err := newID(createdAt)
	return id, false, err
}

// --- users and teams --------------------------------------------------------

func (r *run) users() error {
	byName := map[string]uuid.UUID{}
	for _, u := range r.p.Users {
		name := r.p.Usernames[u.ID]
		id, ok := byName[name]
		if !ok {
			m, err := r.q.GetOrgMemberByUsername(r.ctx, dbq.GetOrgMemberByUsernameParams{OrgID: r.org, Username: name})
			if errors.Is(err, pgx.ErrNoRows) {
				return missingUserError(r.orgSlug, name)
			}
			if err != nil {
				return fmt.Errorf("member %s: %w", name, err)
			}
			id = m.ID
			byName[name] = id
		}
		_, had := r.target(EntUser, u.ID)
		changed, err := r.mapTo(EntUser, u.ID, id)
		if err != nil {
			return err
		}
		switch {
		case !had:
			r.note(EntUser, u.ID, OutCreate)
		case changed:
			r.note(EntUser, u.ID, OutUpdate)
		default:
			r.note(EntUser, u.ID, OutSkip)
		}
	}
	return nil
}

// missingUserError is the import's stop when the user map names a username
// that is not an active member of the org. It names the fix: an agent such
// as claude comes from POST /orgs/{org}/agents, a human from
// first-run setup.
func missingUserError(orgSlug, name string) error {
	return fmt.Errorf("user map names %q, which is not a member of the org; create it first: "+
		"an agent (such as claude) with POST /api/v1/orgs/%s/agents {\"username\":%q,\"name\":...} and an admin token, "+
		"a human through first-run setup (docs/deploy.md \"Agent users and their tokens\")", name, orgSlug, name)
}

// user is the dev-ops user of a Linear user id (NULL when unknown).
func (r *run) user(linearID string) *uuid.UUID { return r.ref(EntUser, linearID) }

func (r *run) teams() error {
	r.teamKeys = map[string]string{}
	for _, t := range r.p.Teams {
		r.teamKeys[t.ID] = t.Key
		row, err := r.q.GetTeamByKeySystem(r.ctx, dbq.GetTeamByKeySystemParams{OrgID: r.org, Key: t.Key})
		created := false
		if errors.Is(err, pgx.ErrNoRows) {
			row, err = tracker.CreateTeamTx(r.ctx, r.q, r.org, t.Key, clip(t.Name, maxTeamName, t.Key), deref(t.Description))
			created = true
		}
		if err != nil {
			return fmt.Errorf("team %s: %w", t.Key, err)
		}
		changed, err := r.mapTo(EntTeam, t.ID, row.ID)
		if err != nil {
			return err
		}
		switch {
		case created:
			r.note(EntTeam, t.ID, OutCreate)
		case changed:
			r.note(EntTeam, t.ID, OutUpdate) // an existing team (setup's first team, ORB) adopted
		default:
			r.note(EntTeam, t.ID, OutSkip)
		}
	}
	return nil
}

// --- workflow states and labels --------------------------------------------

func (r *run) states() error {
	byTeam := map[string][]WorkflowState{}
	for _, s := range r.p.States {
		byTeam[RefID(s.Team)] = append(byTeam[RefID(s.Team)], s)
	}
	for _, t := range r.p.Teams {
		teamID, _ := r.target(EntTeam, t.ID)
		existing, err := r.q.ImportListTeamStates(r.ctx, dbq.ImportListTeamStatesParams{OrgID: r.org, TeamID: teamID})
		if err != nil {
			return fmt.Errorf("list states of %s: %w", t.Key, err)
		}
		byName := map[string]uuid.UUID{}
		nameOf := map[uuid.UUID]string{}
		for _, s := range existing {
			byName[strings.ToLower(s.Name)] = s.ID
			nameOf[s.ID] = strings.ToLower(s.Name)
		}
		claimed := map[uuid.UUID]bool{}
		for _, id := range r.maps[EntState] {
			claimed[id] = true
		}
		states := byTeam[t.ID]
		sort.SliceStable(states, func(i, j int) bool { return states[i].Position < states[j].Position })
		for _, s := range states {
			name := clip(s.Name, maxStateName, "State")
			id, mapped := r.target(EntState, s.ID)
			adopted := false
			if !mapped {
				if ex, ok := byName[strings.ToLower(name)]; ok && !claimed[ex] {
					id, adopted = ex, true // adopt the default state of that name
				} else {
					if id, err = newID(s.CreatedAt); err != nil {
						return err
					}
					name = freeName(name, maxStateName, func(n string) bool {
						_, taken := byName[strings.ToLower(n)]
						return taken
					})
				}
			}
			claimed[id] = true
			byName[strings.ToLower(name)] = id
			typ, err := stateType(s.Type)
			if err != nil {
				return err
			}
			_, err = r.q.ImportUpsertState(r.ctx, dbq.ImportUpsertStateParams{
				ID: id, OrgID: r.org, TeamID: teamID, Name: name, Type: typ, Color: color(s.Color),
				Description: deref(s.Description), Position: s.Position, CreatedAt: s.CreatedAt, ArchivedAt: s.ArchivedAt,
			})
			newMap, merr := r.mapTo(EntState, s.ID, id)
			if merr != nil {
				return merr
			}
			if err := r.upserted(EntState, s.ID, mapped || adopted, newMap, err); err != nil {
				return err
			}
		}
	}
	return nil
}

// freeName is name, or name with " (2)", " (3)" ... until taken says no.
func freeName(name string, maxLen int, taken func(string) bool) string {
	if !taken(name) {
		return name
	}
	for i := 2; ; i++ {
		suffix := fmt.Sprintf(" (%d)", i)
		n := clip(name, maxLen-len(suffix), "x") + suffix
		if !taken(n) {
			return n
		}
	}
}

func color(c string) string {
	if c == "" {
		return "#95a2b3"
	}
	return c
}

func (r *run) labels() error {
	existing, err := r.q.ImportListLabels(r.ctx, r.org)
	if err != nil {
		return fmt.Errorf("list labels: %w", err)
	}
	// A label is found by scope and name: "" is the org scope, else the
	// team id (LAUN-54).
	scopeKey := func(team *uuid.UUID, name string) string {
		k := ""
		if team != nil {
			k = team.String()
		}
		return k + "\x00" + strings.ToLower(name)
	}
	byName := map[string]uuid.UUID{}
	for _, l := range existing {
		byName[scopeKey(l.TeamID, l.Name)] = l.ID
	}
	for _, l := range r.p.Labels { // canonical labels, groups first
		name := clip(l.Name, maxLabelName, "label")
		var team *uuid.UUID
		if l.Team != nil {
			tid, ok := r.target(EntTeam, RefID(l.Team))
			if !ok {
				return fmt.Errorf("label %s: team %s was not written", l.ID, RefID(l.Team))
			}
			team = &tid
		}
		id, mapped := r.target(EntLabel, l.ID)
		adopted := false
		if !mapped {
			if ex, ok := byName[scopeKey(team, name)]; ok {
				// The default label of that name (Bug, Improvement, Feature),
				// or one an earlier export mapped from a label since renamed.
				id, adopted = ex, true
			} else if id, err = newID(l.CreatedAt); err != nil {
				return err
			}
		}
		byName[scopeKey(team, name)] = id
		if mapped || adopted {
			// The row exists (LAUN-54). A row that an earlier import merged
			// from several labels (another source label is mapped to it) is
			// left exactly as it is: no write, no team, nothing moved. Any
			// other row may take its team only while no issue of another
			// team carries it.
			if r.labelRowShared(id, l.ID) {
				r.note(EntLabel, l.ID, OutSkip)
				continue
			}
			if team != nil {
				n, err := r.q.ImportLabelUsedOutsideTeam(r.ctx, dbq.ImportLabelUsedOutsideTeamParams{OrgID: r.org, LabelID: id, TeamID: *team})
				if err != nil {
					return fmt.Errorf("label %s use: %w", l.ID, err)
				}
				if n > 0 {
					team = nil
				}
			}
		}
		var parent *uuid.UUID
		if pr := RefID(l.Parent); pr != "" {
			if pid, ok := r.target(EntLabel, r.p.LabelCanon[pr]); ok && pid != id {
				parent = &pid
			}
		}
		_, err = r.q.ImportUpsertLabel(r.ctx, dbq.ImportUpsertLabelParams{
			ID: id, OrgID: r.org, Name: name, Color: color(l.Color), Description: deref(l.Description),
			ParentID: parent, CreatedAt: l.CreatedAt, ArchivedAt: l.ArchivedAt, TeamID: team,
		})
		newMap, merr := r.mapTo(EntLabel, l.ID, id)
		if merr != nil {
			return merr
		}
		if err := r.upserted(EntLabel, l.ID, mapped || adopted, newMap, err); err != nil {
			return err
		}
	}
	// Labels merged into another of the same name: a mapping only.
	ids := make([]string, 0, len(r.p.LabelCanon))
	for id := range r.p.LabelCanon {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, lid := range ids {
		canon := r.p.LabelCanon[lid]
		if canon == lid {
			continue
		}
		r.rep.MergedLabels++
		tgt, ok := r.target(EntLabel, canon)
		if !ok {
			return fmt.Errorf("label %s: merged into %s, which was not written", lid, canon)
		}
		changed, err := r.mapTo(EntLabel, lid, tgt)
		if err != nil {
			return err
		}
		if changed {
			r.note(EntLabel, lid, OutUpdate)
		} else {
			r.note(EntLabel, lid, OutSkip)
		}
	}
	return nil
}

// labelRowShared reports whether another CANONICAL source label (one the plan
// keeps apart, not one merged into src) is mapped to the label row id.
func (r *run) labelRowShared(id uuid.UUID, src string) bool {
	for other, tgt := range r.maps[EntLabel] {
		if tgt == id && other != src && r.p.LabelCanon[other] == other {
			return true
		}
	}
	return false
}

// --- projects, milestones, cycles -------------------------------------------

func (r *run) projects() error {
	for _, pr := range r.p.Projects {
		id, mapped, err := r.idFor(EntProject, pr.ID, pr.CreatedAt)
		if err != nil {
			return err
		}
		start, _ := parseDate(pr.StartDate) // checked by BuildPlan
		target, _ := parseDate(pr.TargetDate)
		own := Owner{Entity: EntProject, ID: id}
		_, err = r.q.ImportUpsertProject(r.ctx, dbq.ImportUpsertProjectParams{
			ID: id, OrgID: r.org, Name: clip(pr.Name, maxNameLen, "Project"),
			Description: r.files.rewrite(r.ctx, pr.Description, own),
			Content:     r.files.rewrite(r.ctx, deref(pr.Content), own),
			State:       projectState(pr), LeadID: r.user(RefID(pr.Lead)), StartDate: start, TargetDate: target,
			CreatedAt: pr.CreatedAt, UpdatedAt: pr.UpdatedAt, StartedAt: pr.StartedAt,
			CompletedAt: pr.CompletedAt, CanceledAt: pr.CanceledAt, ArchivedAt: pr.ArchivedAt,
		})
		newMap, merr := r.mapTo(EntProject, pr.ID, id)
		if merr != nil {
			return merr
		}
		if err := r.upserted(EntProject, pr.ID, mapped, newMap, err); err != nil {
			return err
		}
		keep := []uuid.UUID{}
		for _, t := range pr.Teams.Nodes {
			tid, ok := r.target(EntTeam, t.ID)
			if !ok {
				continue
			}
			keep = append(keep, tid)
			n, err := r.q.ImportAddProjectTeam(r.ctx, dbq.ImportAddProjectTeamParams{OrgID: r.org, ProjectID: id, TeamID: tid})
			if err != nil {
				return fmt.Errorf("project %s team: %w", pr.ID, err)
			}
			if n > 0 && mapped {
				r.note(EntProject, pr.ID, OutUpdate)
			}
		}
		n, err := r.q.ImportPruneProjectTeams(r.ctx, dbq.ImportPruneProjectTeamsParams{OrgID: r.org, ProjectID: id, Keep: keep})
		if err != nil {
			return fmt.Errorf("project %s teams: %w", pr.ID, err)
		}
		if n > 0 {
			r.note(EntProject, pr.ID, OutUpdate)
		}
	}
	return nil
}

func (r *run) milestones() error {
	for _, m := range r.p.Milestones {
		id, mapped, err := r.idFor(EntMilestone, m.ID, m.CreatedAt)
		if err != nil {
			return err
		}
		pid, ok := r.target(EntProject, RefID(m.Project))
		if !ok {
			return fmt.Errorf("milestone %s: project not written", m.ID)
		}
		target, _ := parseDate(m.TargetDate)
		_, err = r.q.ImportUpsertMilestone(r.ctx, dbq.ImportUpsertMilestoneParams{
			ID: id, OrgID: r.org, ProjectID: pid, Name: clip(m.Name, maxNameLen, "Milestone"),
			Description: r.files.rewrite(r.ctx, deref(m.Description), Owner{Entity: EntMilestone, ID: id}),
			TargetDate:  target, SortOrder: m.SortOrder, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
			ArchivedAt: m.ArchivedAt,
		})
		newMap, merr := r.mapTo(EntMilestone, m.ID, id)
		if merr != nil {
			return merr
		}
		if err := r.upserted(EntMilestone, m.ID, mapped, newMap, err); err != nil {
			return err
		}
	}
	return nil
}

func (r *run) cycles() error {
	byTeam := map[string][]Cycle{}
	for _, c := range r.p.Cycles {
		byTeam[RefID(c.Team)] = append(byTeam[RefID(c.Team)], c)
	}
	for _, t := range r.p.Teams {
		teamID, _ := r.target(EntTeam, t.ID)
		if err := r.q.ImportLockTeamCycles(r.ctx, teamID); err != nil {
			return fmt.Errorf("lock cycles of %s: %w", t.Key, err)
		}
		held, err := r.q.ImportTeamCycleNumbers(r.ctx, dbq.ImportTeamCycleNumbersParams{OrgID: r.org, TeamID: teamID})
		if err != nil {
			return fmt.Errorf("cycle numbers of %s: %w", t.Key, err)
		}
		byNumber := map[int32]uuid.UUID{}
		for _, h := range held {
			byNumber[h.Number] = h.ID
		}
		type planned struct {
			c      Cycle
			id     uuid.UUID
			mapped bool
			n      int32
		}
		var todo []planned
		var clashes []string
		for _, c := range byTeam[t.ID] {
			id, mapped, err := r.idFor(EntCycle, c.ID, c.CreatedAt)
			if err != nil {
				return err
			}
			n, _ := whole(c.Number) // checked by BuildPlan
			if other, ok := byNumber[n]; ok && other != id {
				clashes = append(clashes, fmt.Sprintf("%s cycle %d", t.Key, n))
			}
			todo = append(todo, planned{c: c, id: id, mapped: mapped, n: n})
		}
		if len(clashes) > 0 {
			return fmt.Errorf("%d imported cycle number(s) are already used by cycles the importer did not make "+
				"(were cycles turned on before the import? decisions-T3 #35): %s", len(clashes), strings.Join(clashes, ", "))
		}
		for _, pc := range todo {
			var name *string
			if pc.c.Name != nil && strings.TrimSpace(*pc.c.Name) != "" {
				n := clip(*pc.c.Name, maxNameLen, "Cycle")
				name = &n
			}
			_, err := r.q.ImportUpsertCycle(r.ctx, dbq.ImportUpsertCycleParams{
				ID: pc.id, OrgID: r.org, TeamID: teamID, Number: pc.n, Name: name,
				Description: deref(pc.c.Description), StartsAt: pc.c.StartsAt, EndsAt: pc.c.EndsAt,
				CompletedAt: pc.c.CompletedAt, CreatedAt: pc.c.CreatedAt, UpdatedAt: pc.c.UpdatedAt,
				ArchivedAt: pc.c.ArchivedAt,
			})
			newMap, merr := r.mapTo(EntCycle, pc.c.ID, pc.id)
			if merr != nil {
				return merr
			}
			if err := r.upserted(EntCycle, pc.c.ID, pc.mapped, newMap, err); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *run) snapshots() error {
	keys, err := r.q.ImportListSnapshotKeys(r.ctx, r.org)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	have := map[string]bool{}
	for _, k := range keys {
		have[k.CycleID.String()+k.SnapshotDate.Format(time.DateOnly)] = true
	}
	for _, c := range r.p.Cycles {
		cid, ok := r.target(EntCycle, c.ID)
		if !ok {
			return fmt.Errorf("cycle %s not written", c.ID)
		}
		for _, s := range r.p.Snapshots[c.ID] {
			key := cid.String() + s.Date.Format(time.DateOnly)
			id, err := newID(s.Date)
			if err != nil {
				return err
			}
			_, err = r.q.ImportUpsertSnapshot(r.ctx, dbq.ImportUpsertSnapshotParams{
				ID: id, OrgID: r.org, CycleID: cid, SnapshotDate: s.Date, ScopeCount: s.ScopeCount,
				CompletedCount: s.CompletedCount, ScopePoints: s.ScopePoints, CompletedPoints: s.CompletedPoints,
				StartedPoints: s.StartedPoints, CreatedAt: s.Date,
			})
			src := c.ID + "@" + s.Date.Format(time.DateOnly)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				r.note(EntSnapshot, src, OutSkip)
			case err != nil:
				return fmt.Errorf("snapshot %s: %w", src, err)
			case have[key]:
				r.note(EntSnapshot, src, OutUpdate)
			default:
				r.note(EntSnapshot, src, OutCreate)
				have[key] = true
			}
		}
	}
	return nil
}

func (r *run) cadence() error {
	for _, t := range r.p.Teams {
		teamID, _ := r.target(EntTeam, t.ID)
		c, ok := r.p.Cadences[t.ID]
		if !ok {
			r.rep.Cadence[t.Key] = "unchanged: Linear's timezone is not usable"
			continue
		}
		start := pgtype.Time{Microseconds: int64(c.Hour)*3600e6 + int64(c.Min)*60e6, Valid: true}
		n, err := r.q.ImportSetTeamCadence(r.ctx, dbq.ImportSetTeamCadenceParams{
			LengthDays: c.LengthDays, Weekday: c.Weekday, StartTime: start, Timezone: c.Timezone,
			OrgID: r.org, ID: teamID,
		})
		if err != nil {
			return fmt.Errorf("cadence of %s: %w", t.Key, err)
		}
		row, err := r.q.GetTeamByIDSystem(r.ctx, dbq.GetTeamByIDSystemParams{OrgID: r.org, ID: teamID})
		if err != nil {
			return fmt.Errorf("team %s: %w", t.Key, err)
		}
		desc := fmt.Sprintf("%d days from %s %02d:%02d %s", c.LengthDays, time.Weekday(c.Weekday), c.Hour, c.Min, c.Timezone)
		switch {
		case row.CyclesEnabled:
			r.rep.Cadence[t.Key] = "unchanged: cycles are already on (the admin's cadence stands)"
		case n > 0:
			r.rep.Cadence[t.Key] = "set to " + desc
		default:
			r.rep.Cadence[t.Key] = "already " + desc
		}
	}
	return nil
}

// --- issues -----------------------------------------------------------------

func (r *run) issues() error {
	type planned struct {
		is     Issue
		id     uuid.UUID
		mapped bool
		n      int32
		team   uuid.UUID
	}
	held := map[uuid.UUID]map[int32]uuid.UUID{}
	var todo []planned
	var clashes []string
	for _, is := range r.p.Issues {
		teamID, ok := r.target(EntTeam, RefID(is.Team))
		if !ok {
			return fmt.Errorf("issue %s: team not written", is.ID)
		}
		if held[teamID] == nil {
			rows, err := r.q.ImportTeamIssueNumbers(r.ctx, dbq.ImportTeamIssueNumbersParams{OrgID: r.org, TeamID: teamID})
			if err != nil {
				return fmt.Errorf("issue numbers: %w", err)
			}
			held[teamID] = map[int32]uuid.UUID{}
			for _, h := range rows {
				held[teamID][h.Number] = h.ID
			}
		}
		id, mapped, err := r.idFor(EntIssue, is.ID, is.CreatedAt)
		if err != nil {
			return err
		}
		n, _ := whole(is.Number)
		if other, ok := held[teamID][n]; ok && other != id {
			clashes = append(clashes, fmt.Sprintf("%s-%d", r.teamKeys[RefID(is.Team)], n))
		}
		todo = append(todo, planned{is: is, id: id, mapped: mapped, n: n, team: teamID})
	}
	if len(clashes) > 0 {
		sort.Strings(clashes)
		if len(clashes) > 20 {
			clashes = append(clashes[:20], "...")
		}
		return fmt.Errorf("issue numbers already used by issues the importer did not make: %s", strings.Join(clashes, ", "))
	}

	for _, pi := range todo {
		is := pi.is
		state, ok := r.target(EntState, RefID(is.State))
		if !ok {
			return fmt.Errorf("issue %s: state not written", is.ID)
		}
		due, _ := parseDate(is.DueDate)
		own := Owner{Entity: EntIssue, ID: pi.id, IssueID: &pi.id}
		_, err := r.q.ImportUpsertIssue(r.ctx, dbq.ImportUpsertIssueParams{
			ID: pi.id, OrgID: r.org, TeamID: pi.team, Number: pi.n, Title: clip(is.Title, maxIssueTitle, "(untitled)"),
			Description: r.files.rewrite(r.ctx, deref(is.Description), own), StateID: state,
			Priority: priority(is.Priority), Estimate: estimate(is.Estimate),
			AssigneeID: r.user(RefID(is.Assignee)), CreatorID: r.user(RefID(is.Creator)),
			ParentID: r.ref(EntIssue, RefID(is.Parent)), ProjectID: r.ref(EntProject, RefID(is.Project)),
			MilestoneID: r.ref(EntMilestone, RefID(is.ProjectMilestone)), CycleID: r.ref(EntCycle, RefID(is.Cycle)),
			DueDate: due, SortOrder: is.SortOrder, CreatedAt: is.CreatedAt, UpdatedAt: is.UpdatedAt,
			StartedAt: is.StartedAt, CompletedAt: is.CompletedAt, CanceledAt: is.CanceledAt, ArchivedAt: is.ArchivedAt,
		})
		newMap, merr := r.mapTo(EntIssue, is.ID, pi.id)
		if merr != nil {
			return merr
		}
		if err := r.upserted(EntIssue, is.ID, pi.mapped, newMap, err); err != nil {
			return err
		}
		if err := r.issueLabels(is, pi.id, pi.mapped); err != nil {
			return err
		}
	}
	// Second pass: parents written after their child.
	for _, pi := range todo {
		par := RefID(pi.is.Parent)
		if par == "" {
			continue
		}
		pid, ok := r.target(EntIssue, par)
		if !ok {
			continue
		}
		n, err := r.q.ImportSetIssueParent(r.ctx, dbq.ImportSetIssueParentParams{OrgID: r.org, ID: pi.id, ParentID: &pid})
		if err != nil {
			return fmt.Errorf("issue %s parent: %w", pi.is.ID, err)
		}
		if n > 0 && pi.mapped {
			r.note(EntIssue, pi.is.ID, OutUpdate)
		}
	}
	return nil
}

func (r *run) issueLabels(is Issue, id uuid.UUID, mapped bool) error {
	want := []uuid.UUID{}
	seen := map[uuid.UUID]bool{}
	for _, lid := range is.LabelIDs {
		canon, ok := r.p.LabelCanon[lid]
		if !ok {
			continue // a label not in the export (deleted)
		}
		tid, ok := r.target(EntLabel, canon)
		if !ok || seen[tid] {
			continue
		}
		seen[tid] = true
		want = append(want, tid)
	}
	changed := false
	for _, tid := range want {
		n, err := r.q.ImportAddIssueLabel(r.ctx, dbq.ImportAddIssueLabelParams{OrgID: r.org, IssueID: id, LabelID: tid, CreatedAt: is.CreatedAt})
		if err != nil {
			return fmt.Errorf("issue %s label: %w", is.ID, err)
		}
		changed = changed || n > 0
	}
	n, err := r.q.ImportPruneIssueLabels(r.ctx, dbq.ImportPruneIssueLabelsParams{OrgID: r.org, IssueID: id, Keep: want})
	if err != nil {
		return fmt.Errorf("issue %s labels: %w", is.ID, err)
	}
	if (changed || n > 0) && mapped {
		r.note(EntIssue, is.ID, OutUpdate)
	}
	return nil
}

func (r *run) stateChanges() error {
	for _, sc := range r.p.Changes {
		id, mapped, err := r.idFor(EntStateChange, sc.SourceID, sc.At)
		if err != nil {
			return err
		}
		issue, ok := r.target(EntIssue, sc.IssueID)
		if !ok {
			return fmt.Errorf("state change %s: issue not written", sc.SourceID)
		}
		to, ok := r.target(EntState, sc.To)
		if !ok {
			return fmt.Errorf("state change %s: state not written", sc.SourceID)
		}
		var from *uuid.UUID
		if sc.From != nil {
			f, ok := r.target(EntState, *sc.From)
			if !ok {
				return fmt.Errorf("state change %s: state not written", sc.SourceID)
			}
			from = &f
		}
		_, err = r.q.ImportUpsertStateChange(r.ctx, dbq.ImportUpsertStateChangeParams{
			ID: id, OrgID: r.org, IssueID: issue, FromStateID: from, ToStateID: to, ActorID: r.user(sc.Actor), CreatedAt: sc.At,
		})
		newMap, merr := r.mapTo(EntStateChange, sc.SourceID, id)
		if merr != nil {
			return merr
		}
		if err := r.upserted(EntStateChange, sc.SourceID, mapped, newMap, err); err != nil {
			return err
		}
	}
	return nil
}

// --- documents, comments, relations, attachments ----------------------------

func (r *run) documents() error {
	for _, d := range r.p.Documents {
		id, mapped, err := r.idFor(EntDocument, d.ID, d.CreatedAt)
		if err != nil {
			return err
		}
		slug := docSlug(d.Title, d.SlugID)
		owner, err := r.q.ImportDocumentSlugOwner(r.ctx, dbq.ImportDocumentSlugOwnerParams{OrgID: r.org, Slug: slug})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("document %s slug: %w", d.ID, err)
		case owner != id:
			slug += "-" + strings.ReplaceAll(id.String(), "-", "")[20:]
		}
		_, err = r.q.ImportUpsertDocument(r.ctx, dbq.ImportUpsertDocumentParams{
			ID: id, OrgID: r.org, ProjectID: r.ref(EntProject, RefID(d.Project)), Title: clip(d.Title, maxNameLen, "Untitled"),
			Content: r.files.rewrite(r.ctx, deref(d.Content), Owner{Entity: EntDocument, ID: id}), Slug: slug,
			CreatorID: r.user(RefID(d.Creator)), CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, ArchivedAt: d.ArchivedAt,
		})
		newMap, merr := r.mapTo(EntDocument, d.ID, id)
		if merr != nil {
			return merr
		}
		if err := r.upserted(EntDocument, d.ID, mapped, newMap, err); err != nil {
			return err
		}
	}
	return nil
}

func (r *run) comments() error {
	type planned struct {
		c      Comment
		id     uuid.UUID
		mapped bool
	}
	var todo []planned
	for _, c := range r.p.Comments {
		id, mapped, err := r.idFor(EntComment, c.ID, c.CreatedAt)
		if err != nil {
			return err
		}
		var issue, doc *uuid.UUID
		owner := Owner{Entity: EntComment, ID: id}
		if c.IssueID != nil && *c.IssueID != "" {
			issue = r.ref(EntIssue, *c.IssueID)
			owner.IssueID = issue
		} else if c.DocumentContentID != nil {
			doc = r.ref(EntDocument, r.p.DocByContent[*c.DocumentContentID])
		}
		if issue == nil && doc == nil {
			return fmt.Errorf("comment %s: its issue or document was not written", c.ID)
		}
		var parent *uuid.UUID
		if c.ParentID != nil {
			parent = r.ref(EntComment, *c.ParentID)
		}
		_, err = r.q.ImportUpsertComment(r.ctx, dbq.ImportUpsertCommentParams{
			ID: id, OrgID: r.org, IssueID: issue, DocumentID: doc, ParentID: parent,
			AuthorID: r.user(RefID(c.User)), Body: clip(r.files.rewrite(r.ctx, c.Body, owner), maxCommentBody, "(empty)"),
			CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, EditedAt: c.EditedAt,
		})
		newMap, merr := r.mapTo(EntComment, c.ID, id)
		if merr != nil {
			return merr
		}
		if err := r.upserted(EntComment, c.ID, mapped, newMap, err); err != nil {
			return err
		}
		todo = append(todo, planned{c: c, id: id, mapped: mapped})
	}
	for _, pc := range todo {
		if pc.c.ParentID == nil {
			continue
		}
		pid, ok := r.target(EntComment, *pc.c.ParentID)
		if !ok {
			continue
		}
		n, err := r.q.ImportSetCommentParent(r.ctx, dbq.ImportSetCommentParentParams{OrgID: r.org, ID: pc.id, ParentID: &pid})
		if err != nil {
			return fmt.Errorf("comment %s parent: %w", pc.c.ID, err)
		}
		if n > 0 && pc.mapped {
			r.note(EntComment, pc.c.ID, OutUpdate)
		}
	}
	return nil
}

func (r *run) relations() error {
	var merged []Relation
	for _, rel := range r.p.Relations {
		if r.p.RelationCanon[rel.ID] != rel.ID {
			merged = append(merged, rel)
			continue
		}
		typ := r.p.RelationType[rel.ID]
		a, okA := r.target(EntIssue, RefID(rel.Issue))
		b, okB := r.target(EntIssue, RefID(rel.RelatedIssue))
		if !okA || !okB {
			return fmt.Errorf("relation %s: issue not written", rel.ID)
		}
		id, mapped := r.target(EntRelation, rel.ID)
		adopted := false
		if !mapped {
			ex, err := r.q.ImportFindRelationByPair(r.ctx, dbq.ImportFindRelationByPairParams{OrgID: r.org, Type: typ, A: a, B: b})
			switch {
			case err == nil:
				id, adopted = ex, true // the pair exists already (made through the API): adopt it
			case errors.Is(err, pgx.ErrNoRows):
				if id, err = newID(rel.CreatedAt); err != nil {
					return err
				}
			default:
				return fmt.Errorf("relation %s: %w", rel.ID, err)
			}
		}
		_, err := r.q.ImportUpsertRelation(r.ctx, dbq.ImportUpsertRelationParams{
			ID: id, OrgID: r.org, Type: typ, IssueID: a, RelatedIssueID: b, CreatedAt: rel.CreatedAt,
		})
		newMap, merr := r.mapTo(EntRelation, rel.ID, id)
		if merr != nil {
			return merr
		}
		if err := r.upserted(EntRelation, rel.ID, mapped || adopted, newMap, err); err != nil {
			return err
		}
	}
	for _, rel := range merged {
		r.rep.MergedRelations++
		tgt, ok := r.target(EntRelation, r.p.RelationCanon[rel.ID])
		if !ok {
			return fmt.Errorf("relation %s: merged into one that was not written", rel.ID)
		}
		changed, err := r.mapTo(EntRelation, rel.ID, tgt)
		if err != nil {
			return err
		}
		if changed {
			r.note(EntRelation, rel.ID, OutUpdate)
		} else {
			r.note(EntRelation, rel.ID, OutSkip)
		}
	}
	return nil
}

func (r *run) attachments() error {
	for _, a := range r.p.Attachments {
		meta := a.Metadata
		if len(meta) == 0 || string(meta) == "null" || !json.Valid(meta) {
			meta = json.RawMessage(`{}`)
		}
		if i, ok := r.p.FileByAttachment[a.ID]; ok && r.stored != nil {
			// An uploaded Linear file: this attachment is the file row.
			if err := r.fileRow(i, a.Title, deref(a.Subtitle), meta, a.UpdatedAt); err != nil {
				return err
			}
			continue
		}
		id, mapped, err := r.idFor(EntAttachment, a.ID, a.CreatedAt)
		if err != nil {
			return err
		}
		issue, ok := r.target(EntIssue, RefID(a.Issue))
		if !ok {
			return fmt.Errorf("attachment %s: issue not written", a.ID)
		}
		url := a.URL
		if IsUploadURL(url) {
			url = r.files.rewrite(r.ctx, url, Owner{Entity: EntAttachment, ID: id, IssueID: &issue})
		}
		_, err = r.q.ImportUpsertLinkAttachment(r.ctx, dbq.ImportUpsertLinkAttachmentParams{
			ID: id, OrgID: r.org, IssueID: issue, Title: a.Title, Subtitle: deref(a.Subtitle), Url: &url,
			Metadata: meta, CreatorID: r.user(RefID(a.Creator)), CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
		})
		newMap, merr := r.mapTo(EntAttachment, a.ID, id)
		if merr != nil {
			return merr
		}
		if err := r.upserted(EntAttachment, a.ID, mapped, newMap, err); err != nil {
			return err
		}
	}
	return nil
}

func (r *run) counters() error {
	for _, t := range r.p.Teams {
		teamID, _ := r.target(EntTeam, t.ID)
		if _, err := r.q.ImportRaiseIssueCounter(r.ctx, dbq.ImportRaiseIssueCounterParams{
			NextNumber: r.p.NextNumbers[t.ID], OrgID: r.org, TeamID: teamID,
		}); err != nil {
			return fmt.Errorf("counter of %s: %w", t.Key, err)
		}
		next, err := r.q.ImportIssueCounter(r.ctx, dbq.ImportIssueCounterParams{OrgID: r.org, TeamID: teamID})
		if err != nil {
			return fmt.Errorf("counter of %s: %w", t.Key, err)
		}
		r.rep.NextNumbers[t.Key] = next
	}
	return nil
}
