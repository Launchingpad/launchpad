package importer

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"devops/internal/tracker"
)

// Entity types: the import_mappings.entity_type values, and the rows of
// the import report. "issue" is the value tracker.resolveIssueID reads, so
// GET /issues/{linear-uuid} works.
const (
	EntTeam        = "team"
	EntState       = "workflow_state"
	EntLabel       = "label"
	EntUser        = "user"
	EntProject     = "project"
	EntMilestone   = "milestone"
	EntCycle       = "cycle"
	EntSnapshot    = "cycle_snapshot" // counted, not mapped (keyed by cycle and date)
	EntIssue       = "issue"
	EntStateChange = "issue_state_change"
	EntDocument    = "document"
	EntComment     = "comment"
	EntRelation    = "relation"
	EntAttachment  = "attachment"
	// EntFile is an uploads.linear.app file: the mapping is URL -> the
	// `file` attachment row (T4b).
	EntFile = "file"
)

// Entities is every entity in report order.
var Entities = []string{
	EntTeam, EntState, EntLabel, EntUser, EntProject, EntMilestone, EntCycle, EntSnapshot,
	EntIssue, EntStateChange, EntDocument, EntComment, EntRelation, EntAttachment, EntFile,
}

// Exclusion is an export record the importer deliberately does not write,
// with the reason. Exclusions are part of the expected counts: verify
// expects export minus excluded.
type Exclusion struct {
	Entity   string `json:"entity"`
	SourceID string `json:"sourceId"`
	Reason   string `json:"reason"`
}

// StateChange is one issue_state_changes row planned from an issue's
// history. A nil From is the creation row.
type StateChange struct {
	SourceID string // the IssueHistory id, or "<issue id>:created" when synthesised
	IssueID  string
	At       time.Time
	From     *string
	To       string
	Actor    string // Linear user id, "" for none
}

// Plan is what the import will write, worked out from the export alone
// (no database), so import and verify agree on the expected counts and the
// mapping rules can be unit tested.
type Plan struct {
	Teams       []Team
	States      []WorkflowState
	Labels      []Label // groups first, then by createdAt
	Users       []User
	Projects    []Project
	Milestones  []Milestone
	Cycles      []Cycle
	Issues      []Issue // by createdAt, so a parent is usually written first
	Changes     []StateChange
	Documents   []Document
	Comments    []Comment // by createdAt, so a parent is usually written first
	Relations   []Relation
	Attachments []Attachment
	// Files are the uploads.linear.app files the import stores (T4b), and
	// FileByAttachment the index of the file a Linear attachment is.
	Files            []FilePlan
	FileByAttachment map[string]int
	// SkipFiles: no file is stored and no upload URL is rewritten (the
	// T4 behaviour, for a run with no object store).
	SkipFiles bool

	// LabelCanon maps every Linear label id to the label it is merged into
	// (itself when it is not merged): same-name org-wide labels, a team
	// label named like an org-wide label, and same-name labels of one team.
	LabelCanon map[string]string
	// RelationType is the dev-ops type of each planned relation.
	RelationType map[string]string
	// RelationCanon maps a relation to the one it duplicates (the same
	// unordered pair and type: dev-ops stores one row per pair).
	RelationCanon map[string]string
	// Usernames is the dev-ops username of every Linear user id.
	Usernames map[string]string
	// DocByContent maps a documentContentId to its document id, so a
	// document comment finds its document.
	DocByContent map[string]string
	// Snapshots per Linear cycle id, from its history arrays.
	Snapshots map[string][]Snapshot
	// Cadences per Linear team id.
	Cadences map[string]Cadence
	// MissingCycleNumbers per team key: numbers between the lowest and the
	// highest imported cycle that Linear does not have.
	MissingCycleNumbers map[string][]int32
	// NextNumbers per Linear team id: the issue counter after the import,
	// max(number) + 1, at least the team's floor.
	NextNumbers map[string]int32

	Exclusions []Exclusion
	Expected   map[string]int
	ExportedN  map[string]int
}

// BuildPlan works out the import of ex. floors are minimum next issue
// numbers by team key (for example ORB 3405). It fails on what no rule can
// handle: an unmapped user with no default, an unknown state type, a
// non-integer issue number.
func BuildPlan(ex *Export, um *UserMap, floors map[string]int32) (*Plan, error) {
	p := &Plan{
		LabelCanon: map[string]string{}, RelationType: map[string]string{}, RelationCanon: map[string]string{},
		Usernames: map[string]string{}, DocByContent: map[string]string{}, Snapshots: map[string][]Snapshot{},
		Cadences: map[string]Cadence{}, MissingCycleNumbers: map[string][]int32{}, NextNumbers: map[string]int32{},
		Expected: map[string]int{}, ExportedN: map[string]int{}, FileByAttachment: map[string]int{},
	}
	exclude := func(ent, id, reason string) {
		p.Exclusions = append(p.Exclusions, Exclusion{Entity: ent, SourceID: id, Reason: reason})
	}

	// Users.
	var unmapped []string
	for _, u := range ex.Users {
		name, ok := um.Resolve(u)
		if !ok {
			unmapped = append(unmapped, u.ID)
			continue
		}
		p.Usernames[u.ID] = name
		p.Users = append(p.Users, u)
	}
	if len(unmapped) > 0 {
		sort.Strings(unmapped)
		return nil, fmt.Errorf("user map: %d Linear user(s) have no mapping and there is no default: %s",
			len(unmapped), strings.Join(unmapped, ", "))
	}

	// Teams and states.
	teamByID := map[string]Team{}
	for _, t := range ex.Teams {
		if !teamKeyOK(t.Key) {
			return nil, fmt.Errorf("team %s: key %q is not a Launchpad team key (A-Z, 0-9, at most 10)", t.ID, t.Key)
		}
		teamByID[t.ID] = t
		p.Teams = append(p.Teams, t)
	}
	stateByID := map[string]WorkflowState{}
	for _, s := range ex.States {
		if _, ok := teamByID[RefID(s.Team)]; !ok {
			exclude(EntState, s.ID, "team not in the export")
			continue
		}
		if _, err := stateType(s.Type); err != nil {
			return nil, fmt.Errorf("state %s: %w", s.ID, err)
		}
		stateByID[s.ID] = s
		p.States = append(p.States, s)
	}

	// Labels (LAUN-54). An org-wide label (no team) is unique by name, ignoring
	// case. A team label keeps its team and is unique by name within the team;
	// the same name on two teams is two labels. A team label whose name an
	// org-wide label already has is merged into that one, as before team
	// labels existed (an issue that carries both keeps one). A label whose
	// team is not in the export is org-wide. Groups are written first.
	labels := append([]Label(nil), ex.Labels...)
	for i := range labels {
		if _, ok := teamByID[RefID(labels[i].Team)]; !ok {
			labels[i].Team = nil
		}
	}
	sort.SliceStable(labels, func(i, j int) bool {
		if labels[i].IsGroup != labels[j].IsGroup {
			return labels[i].IsGroup
		}
		return labels[i].CreatedAt.Before(labels[j].CreatedAt)
	})
	orgByName := map[string]string{}
	teamByName := map[string]string{}          // team id + NUL + lower name -> label id
	for _, pass := range []bool{false, true} { // org-wide labels first, then team labels
		for _, l := range labels {
			if (l.Team != nil) != pass {
				continue
			}
			name := strings.ToLower(clip(l.Name, maxLabelName, "label"))
			if l.Team == nil {
				if c, ok := orgByName[name]; ok {
					p.LabelCanon[l.ID] = c
					continue
				}
				orgByName[name] = l.ID
			} else {
				if c, ok := orgByName[name]; ok {
					p.LabelCanon[l.ID] = c
					continue
				}
				key := RefID(l.Team) + "\x00" + name
				if c, ok := teamByName[key]; ok {
					p.LabelCanon[l.ID] = c
					continue
				}
				teamByName[key] = l.ID
			}
			p.LabelCanon[l.ID] = l.ID
			p.Labels = append(p.Labels, l)
		}
	}
	sort.SliceStable(p.Labels, func(i, j int) bool {
		if p.Labels[i].IsGroup != p.Labels[j].IsGroup {
			return p.Labels[i].IsGroup
		}
		return p.Labels[i].CreatedAt.Before(p.Labels[j].CreatedAt)
	})

	// Projects and milestones.
	projectOK := map[string]bool{}
	for _, pr := range ex.Projects {
		if _, err := parseDate(pr.StartDate); err != nil {
			return nil, fmt.Errorf("project %s: %w", pr.ID, err)
		}
		if _, err := parseDate(pr.TargetDate); err != nil {
			return nil, fmt.Errorf("project %s: %w", pr.ID, err)
		}
		projectOK[pr.ID] = true
		p.Projects = append(p.Projects, pr)
	}
	milestoneProject := map[string]string{}
	for _, m := range ex.Milestones {
		if !projectOK[RefID(m.Project)] {
			exclude(EntMilestone, m.ID, "project not in the export")
			continue
		}
		if _, err := parseDate(m.TargetDate); err != nil {
			return nil, fmt.Errorf("milestone %s: %w", m.ID, err)
		}
		milestoneProject[m.ID] = RefID(m.Project)
		p.Milestones = append(p.Milestones, m)
	}

	// Cycles: numbers, gaps, snapshots, cadence.
	cycleTeam := map[string]string{}
	numbersByTeam := map[string][]int32{}
	for _, cy := range ex.Cycles {
		t, ok := teamByID[RefID(cy.Team)]
		if !ok {
			exclude(EntCycle, cy.ID, "team not in the export")
			continue
		}
		n, err := whole(cy.Number)
		if err != nil {
			return nil, fmt.Errorf("cycle %s: %w", cy.ID, err)
		}
		if !cy.EndsAt.After(cy.StartsAt) {
			return nil, fmt.Errorf("cycle %s: ends at or before it starts", cy.ID)
		}
		cycleTeam[cy.ID] = t.ID
		numbersByTeam[t.Key] = append(numbersByTeam[t.Key], n)
		p.Cycles = append(p.Cycles, cy)
		if rows := snapshots(cy, t.Timezone); len(rows) > 0 {
			p.Snapshots[cy.ID] = rows
		}
	}
	for key, nums := range numbersByTeam {
		if gaps := missingNumbers(nums); len(gaps) > 0 {
			p.MissingCycleNumbers[key] = gaps
		}
	}
	for _, t := range p.Teams {
		if c, ok := deriveCadence(t, p.Cycles); ok {
			p.Cadences[t.ID] = c
		}
	}

	// Issues. Trashed issues were deleted in Linear and are not imported.
	issues := append([]Issue(nil), ex.Issues...)
	sort.SliceStable(issues, func(i, j int) bool { return issues[i].CreatedAt.Before(issues[j].CreatedAt) })
	issueOK := map[string]bool{}
	issueArchived := map[string]bool{}
	maxNumber := map[string]int32{}
	for _, is := range issues {
		if is.Trashed != nil && *is.Trashed {
			exclude(EntIssue, is.ID, "trashed in Linear")
			continue
		}
		tid := RefID(is.Team)
		if _, ok := teamByID[tid]; !ok {
			exclude(EntIssue, is.ID, "team not in the export")
			continue
		}
		st, ok := stateByID[RefID(is.State)]
		if !ok || RefID(st.Team) != tid {
			return nil, fmt.Errorf("issue %s: its state is not a state of its team in the export", is.ID)
		}
		n, err := whole(is.Number)
		if err != nil {
			return nil, fmt.Errorf("issue %s: %w", is.ID, err)
		}
		if _, err := parseDate(is.DueDate); err != nil {
			return nil, fmt.Errorf("issue %s: %w", is.ID, err)
		}
		if c := RefID(is.Cycle); c != "" && cycleTeam[c] != tid {
			// A cycle of another team (or one not exported) cannot be held:
			// cycles_fk is org-wide but the API rule is the issue's team.
			is.Cycle = nil
		}
		if m := RefID(is.ProjectMilestone); m != "" && milestoneProject[m] != RefID(is.Project) {
			is.ProjectMilestone = nil
		}
		if pr := RefID(is.Project); pr != "" && !projectOK[pr] {
			is.Project, is.ProjectMilestone = nil, nil
		}
		issueOK[is.ID] = true
		issueArchived[is.ID] = is.ArchivedAt != nil
		maxNumber[tid] = max(maxNumber[tid], n)
		p.Issues = append(p.Issues, is)
	}
	for i := range p.Issues {
		if par := RefID(p.Issues[i].Parent); par != "" && !issueOK[par] {
			p.Issues[i].Parent = nil
		}
	}
	for _, t := range p.Teams {
		next := maxNumber[t.ID] + 1
		if f, ok := floors[t.Key]; ok && f > next {
			next = f
		}
		p.NextNumbers[t.ID] = next
	}

	// State history.
	for _, is := range p.Issues {
		p.Changes = append(p.Changes, planStateChanges(is, stateByID, func(id string) {
			exclude(EntStateChange, id, "references a state not in the export")
		})...)
	}

	// Documents. Trashed documents were deleted in Linear.
	docOK := map[string]bool{}
	for _, d := range ex.Documents {
		if d.Trashed != nil && *d.Trashed {
			exclude(EntDocument, d.ID, "trashed in Linear")
			continue
		}
		if pr := RefID(d.Project); pr != "" && !projectOK[pr] {
			d.Project = nil
		}
		docOK[d.ID] = true
		if d.DocumentContentID != nil && *d.DocumentContentID != "" {
			p.DocByContent[*d.DocumentContentID] = d.ID
		}
		p.Documents = append(p.Documents, d)
	}

	// Comments.
	comments := append([]Comment(nil), ex.Comments...)
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
	commentOK := map[string]bool{}
	for _, c := range comments {
		switch {
		case c.IssueID != nil && *c.IssueID != "":
			if !issueOK[*c.IssueID] {
				exclude(EntComment, c.ID, "issue not imported")
				continue
			}
			if c.ArchivedAt != nil && !issueArchived[*c.IssueID] {
				exclude(EntComment, c.ID, "archived in Linear on an open issue (deleted)")
				continue
			}
		case c.DocumentContentID != nil && p.DocByContent[*c.DocumentContentID] != "":
			if !docOK[p.DocByContent[*c.DocumentContentID]] {
				exclude(EntComment, c.ID, "document not imported")
				continue
			}
		default:
			exclude(EntComment, c.ID, "not on an issue or a document (Launchpad has no table for it)")
			continue
		}
		if strings.TrimSpace(c.Body) == "" {
			exclude(EntComment, c.ID, "empty body")
			continue
		}
		commentOK[c.ID] = true
		p.Comments = append(p.Comments, c)
	}
	for i := range p.Comments {
		if par := p.Comments[i].ParentID; par != nil && !commentOK[*par] {
			p.Comments[i].ParentID = nil
		}
	}

	// Relations: one row per unordered pair and type.
	pairCanon := map[string]string{}
	for _, r := range ex.Relations {
		a, b := RefID(r.Issue), RefID(r.RelatedIssue)
		typ, ok := relationType(r.Type)
		switch {
		case !ok:
			exclude(EntRelation, r.ID, fmt.Sprintf("type %q is not imported", r.Type))
			continue
		case !issueOK[a] || !issueOK[b]:
			exclude(EntRelation, r.ID, "issue not imported")
			continue
		case a == b:
			exclude(EntRelation, r.ID, "relates an issue to itself")
			continue
		case r.ArchivedAt != nil && !issueArchived[a] && !issueArchived[b]:
			exclude(EntRelation, r.ID, "archived in Linear between open issues (removed)")
			continue
		}
		lo, hi := a, b
		if hi < lo {
			lo, hi = hi, lo
		}
		key := typ + "|" + lo + "|" + hi
		p.RelationType[r.ID] = typ
		if c, ok := pairCanon[key]; ok {
			p.RelationCanon[r.ID] = c
		} else {
			pairCanon[key] = r.ID
			p.RelationCanon[r.ID] = r.ID
		}
		p.Relations = append(p.Relations, r)
	}

	// Attachments.
	for _, a := range ex.Attachments {
		is := RefID(a.Issue)
		switch {
		case !issueOK[is]:
			exclude(EntAttachment, a.ID, "issue not imported")
			continue
		case a.ArchivedAt != nil && !issueArchived[is]:
			exclude(EntAttachment, a.ID, "archived in Linear on an open issue (removed)")
			continue
		case strings.TrimSpace(a.URL) == "":
			exclude(EntAttachment, a.ID, "no url")
			continue
		}
		p.Attachments = append(p.Attachments, a)
	}

	planFiles(p, ex, exclude)

	p.ExportedN = map[string]int{
		EntTeam: len(ex.Teams), EntState: len(ex.States), EntLabel: len(ex.Labels), EntUser: len(ex.Users),
		EntProject: len(ex.Projects), EntMilestone: len(ex.Milestones), EntCycle: len(ex.Cycles),
		EntIssue: len(ex.Issues), EntDocument: len(ex.Documents), EntComment: len(ex.Comments),
		EntRelation: len(ex.Relations), EntAttachment: len(ex.Attachments),
	}
	// A state row has no export record of its own: it is a history entry
	// that changes the state (or a synthesised creation row). Its "export"
	// count is what was planned plus what was excluded.
	scExcluded := 0
	for _, e := range p.Exclusions {
		if e.Entity == EntStateChange {
			scExcluded++
		}
	}
	p.ExportedN[EntStateChange] = len(p.Changes) + scExcluded
	snapN := 0
	for _, rows := range p.Snapshots {
		snapN += len(rows)
	}
	p.ExportedN[EntSnapshot] = snapN
	p.Expected = map[string]int{
		EntTeam: len(p.Teams), EntState: len(p.States), EntLabel: len(ex.Labels), EntUser: len(p.Users),
		EntProject: len(p.Projects), EntMilestone: len(p.Milestones), EntCycle: len(p.Cycles),
		EntSnapshot: snapN, EntIssue: len(p.Issues), EntStateChange: len(p.Changes),
		EntDocument: len(p.Documents), EntComment: len(p.Comments), EntRelation: len(p.Relations),
		EntAttachment: len(p.Attachments), EntFile: len(p.Files),
	}
	p.ExportedN[EntFile] = len(ex.Files)
	return p, nil
}

// safeExtRe is an extension the fallback display name may keep.
var safeExtRe = regexp.MustCompile(`^\.[A-Za-z0-9]{1,10}$`)

// WithoutFiles is the plan of a run that stores no file: every upload URL
// stays as it is, as in T4 (cmd/importer --skip-files).
func (p *Plan) WithoutFiles() *Plan {
	p.SkipFiles = true
	p.Files = nil
	p.FileByAttachment = map[string]int{}
	p.Expected[EntFile] = 0
	return p
}

// planFiles decides, for every exported file, which row holds it and which
// issue that row is on (decisions-T4b). Owners in order: an imported Linear
// attachment whose URL is the file; else the first imported issue whose
// description refers to it; else the first imported comment on an issue
// that does. A file only a document, a project or a milestone refers to has
// no issue for a row (attachments.issue_id is NOT NULL), so it is excluded,
// its URL stays, and verify fails on it: a typical export has none.
func planFiles(p *Plan, ex *Export, exclude func(ent, id, reason string)) {
	type owner struct {
		issue, attachment, creator string
		at                         time.Time
	}
	owners := map[string]owner{}
	claim := func(u string, o owner) {
		if _, ok := owners[u]; !ok {
			owners[u] = o
		}
	}
	atts := append([]Attachment(nil), p.Attachments...)
	sort.SliceStable(atts, func(i, j int) bool { return atts[i].CreatedAt.Before(atts[j].CreatedAt) })
	for _, a := range atts {
		if IsUploadURL(a.URL) {
			claim(CanonicalUploadURL(a.URL), owner{issue: RefID(a.Issue), attachment: a.ID, creator: RefID(a.Creator), at: a.CreatedAt})
		}
	}
	for _, is := range p.Issues { // createdAt order
		for _, u := range UploadURLs(deref(is.Description)) {
			claim(u, owner{issue: is.ID, creator: RefID(is.Creator), at: is.CreatedAt})
		}
	}
	for _, c := range p.Comments { // createdAt order
		if c.IssueID == nil || *c.IssueID == "" {
			continue
		}
		for _, u := range UploadURLs(c.Body) {
			claim(u, owner{issue: *c.IssueID, creator: RefID(c.User), at: c.CreatedAt})
		}
	}
	elsewhere := map[string]bool{}
	for _, c := range p.Comments { // a comment on a document is imported too
		if c.IssueID == nil || *c.IssueID == "" {
			for _, u := range UploadURLs(c.Body) {
				elsewhere[u] = true
			}
		}
	}
	for _, d := range p.Documents {
		for _, u := range UploadURLs(deref(d.Content)) {
			elsewhere[u] = true
		}
	}
	for _, pr := range p.Projects {
		for _, u := range UploadURLs(pr.Description, deref(pr.Content)) {
			elsewhere[u] = true
		}
	}
	for _, m := range p.Milestones {
		for _, u := range UploadURLs(deref(m.Description)) {
			elsewhere[u] = true
		}
	}

	for _, f := range ex.Files {
		f.URL = CanonicalUploadURL(f.URL)
		o, owned := owners[f.URL]
		ct, typeOK := fileContentType(f)
		switch {
		case f.Path == "" || f.Error != "":
			exclude(EntFile, f.URL, "not downloaded by the export")
			continue
		case !owned && elsewhere[f.URL]:
			exclude(EntFile, f.URL, "only a document (or a comment on one), a project or a milestone refers to it (no issue for its row)")
			continue
		case !owned:
			exclude(EntFile, f.URL, "no imported record refers to it")
			continue
		case !typeOK:
			exclude(EntFile, f.URL, fmt.Sprintf("type %q is not an attachment type", f.ContentType))
			continue
		case f.SizeBytes < 1 || f.SizeBytes > tracker.MaxAttachmentBytes:
			exclude(EntFile, f.URL, "size is outside 1 byte to 100 MiB")
			continue
		}
		name := f.Filename
		if !tracker.ValidAttachmentFilename(name) {
			name = "file"
			if ext := path.Ext(f.Filename); safeExtRe.MatchString(ext) {
				name += ext
			}
		}
		if o.attachment != "" {
			p.FileByAttachment[o.attachment] = len(p.Files)
		}
		p.Files = append(p.Files, FilePlan{URL: f.URL, File: f, ContentType: ct, Filename: name,
			IssueID: o.issue, AttachmentID: o.attachment, At: o.at, Creator: o.creator})
	}
}

// planStateChanges turns an issue's Linear history into state rows. Only
// entries that change the state count. Linear's first entry for an issue
// made in a state has no fromState; when the history has no such entry,
// a creation row (from NULL) is synthesised at the issue's createdAt, into
// the state of the first change (or the current state), so the burn-up
// sees the starting state as dev-ops' own creates do (decisions-T1 #10).
func planStateChanges(is Issue, states map[string]WorkflowState, skip func(id string)) []StateChange {
	hist := append([]HistoryEntry(nil), is.History...)
	sort.SliceStable(hist, func(i, j int) bool { return hist[i].CreatedAt.Before(hist[j].CreatedAt) })
	var out []StateChange
	created := false
	for _, h := range hist {
		if h.ToStateID == nil || *h.ToStateID == "" {
			continue
		}
		if h.FromStateID != nil && *h.FromStateID == *h.ToStateID {
			continue
		}
		if _, ok := states[*h.ToStateID]; !ok {
			skip(h.ID)
			continue
		}
		var from *string
		if h.FromStateID != nil && *h.FromStateID != "" {
			if _, ok := states[*h.FromStateID]; !ok {
				skip(h.ID)
				continue
			}
			f := *h.FromStateID
			from = &f
		} else {
			if created {
				// A second "from nothing" entry would be a second creation.
				skip(h.ID)
				continue
			}
			created = true
		}
		out = append(out, StateChange{SourceID: h.ID, IssueID: is.ID, At: h.CreatedAt, From: from,
			To: *h.ToStateID, Actor: deref(h.ActorID)})
	}
	if !created {
		to := RefID(is.State)
		if len(out) > 0 && out[0].From != nil {
			to = *out[0].From
		}
		first := StateChange{SourceID: is.ID + ":created", IssueID: is.ID, At: is.CreatedAt, To: to, Actor: RefID(is.Creator)}
		out = append([]StateChange{first}, out...)
	}
	return out
}

func teamKeyOK(k string) bool {
	if k == "" || len(k) > 10 || k[0] < 'A' || k[0] > 'Z' {
		return false
	}
	for i := 1; i < len(k); i++ {
		c := k[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
