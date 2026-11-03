package importer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"devops/internal/dbq"
	"devops/internal/storage"
)

// VerifyRow compares one entity: what the export says should exist
// (export minus deliberate exclusions) and what the database holds.
type VerifyRow struct {
	Entity   string `json:"entity"`
	Expected int    `json:"expected"`
	Actual   int    `json:"actual"`
	// Rows is, for a row that names records, the first maxDetailRows of
	// them as "entity id", so an operator can fix the source.
	Rows []string `json:"rows,omitempty"`
}

// maxDetailRows caps VerifyRow.Rows.
const maxDetailRows = 20

// leftoverUploadURLs counts the imported rows whose text still holds an
// uploads.linear.app URL by the importer's one definition (markdown.go),
// and names the first of them. The SQL only narrows the candidates.
func leftoverUploadURLs(ctx context.Context, q *dbq.Queries, orgID uuid.UUID) (VerifyRow, error) {
	rows, err := q.ImportListLinearUploadCandidates(ctx, orgID)
	if err != nil {
		return VerifyRow{}, fmt.Errorf("verify upload urls: %w", err)
	}
	v := VerifyRow{Entity: VerifyLinearURLs}
	for _, r := range rows {
		if len(UploadURLs(r.Body)) == 0 {
			continue // names the host, holds no URL
		}
		v.Actual++
		if len(v.Rows) < maxDetailRows {
			v.Rows = append(v.Rows, r.EntityType+" "+r.ID.String())
		}
	}
	return v, nil
}

// Verify counts, per entity, the Linear ids of the plan whose mapping
// points at a row that exists, and compares that with the plan. A mapping
// whose row was deleted does not count, so a lost row is a mismatch; a
// mapping left from an older export that this export no longer has is not
// counted either. Burn-up rows are checked as (cycle, date) pairs.
//
// With files (T4b) two more rows: VerifyLinearURLs, the texts that still
// point at uploads.linear.app (expected 0), and, when store is not nil,
// VerifyObjects, the file rows whose object is in the store with its size.
func Verify(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, p *Plan, store ObjectStore) ([]VerifyRow, error) {
	present, err := q.ImportVerifyMapped(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("verify counts: %w", err)
	}
	want := p.SourceIDs()
	actual := map[string]int{}
	for _, r := range present {
		if want[r.EntityType][r.SourceID] {
			actual[r.EntityType]++
		}
	}

	maps, err := q.ImportListMappings(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("verify mappings: %w", err)
	}
	cycleTarget := map[string]uuid.UUID{}
	for _, m := range maps {
		if m.EntityType == EntCycle {
			cycleTarget[m.SourceID] = m.TargetID
		}
	}
	keys := []string{}
	for _, c := range p.Cycles {
		cid, ok := cycleTarget[c.ID]
		if !ok {
			continue // its rows cannot be found: they count as missing
		}
		for _, s := range p.Snapshots[c.ID] {
			keys = append(keys, cid.String()+"|"+s.Date.Format(time.DateOnly))
		}
	}
	snaps, err := q.ImportVerifySnapshots(ctx, dbq.ImportVerifySnapshotsParams{OrgID: orgID, Keys: keys})
	if err != nil {
		return nil, fmt.Errorf("verify snapshots: %w", err)
	}
	actual[EntSnapshot] = int(snaps)

	out := make([]VerifyRow, 0, len(Entities)+2)
	for _, e := range Entities {
		out = append(out, VerifyRow{Entity: e, Expected: p.Expected[e], Actual: actual[e]})
	}
	if p.SkipFiles {
		n, err := q.ImportCountFileMappings(ctx, orgID)
		if err != nil {
			return nil, fmt.Errorf("verify files: %w", err)
		}
		if n > 0 {
			return nil, ErrFilesImported // a skip-files verify would not look at them
		}
		return out, nil
	}
	left, err := leftoverUploadURLs(ctx, q, orgID)
	if err != nil {
		return nil, err
	}
	out = append(out, left)
	if store != nil {
		n, err := verifyObjects(ctx, q, orgID, p, store)
		if err != nil {
			return nil, err
		}
		out = append(out, VerifyRow{Entity: VerifyObjects, Expected: len(p.Files), Actual: n})
	}
	return out, nil
}

// The verify rows that are not import_mappings entities.
const (
	VerifyLinearURLs = "linear_upload_url" // texts still pointing at uploads.linear.app
	VerifyObjects    = "file_object"       // file rows whose object is in the store
)

// verifyObjects HEADs the object of every planned file's row and counts
// those the store holds with the row's size.
func verifyObjects(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, p *Plan, store ObjectStore) (int, error) {
	rows, err := q.ImportListFileObjects(ctx, orgID)
	if err != nil {
		return 0, fmt.Errorf("verify file objects: %w", err)
	}
	want := map[string]bool{}
	for _, f := range p.Files {
		want[f.URL] = true
	}
	n := 0
	for _, r := range rows {
		if !want[r.SourceID] || r.ObjectKey == nil || r.SizeBytes == nil {
			continue
		}
		info, err := store.Head(ctx, *r.ObjectKey)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("verify file objects: %w", err)
		}
		if info.Size == *r.SizeBytes {
			n++
		}
	}
	return n, nil
}

// Mismatches are the rows whose counts differ.
func Mismatches(rows []VerifyRow) []VerifyRow {
	var bad []VerifyRow
	for _, r := range rows {
		if r.Expected != r.Actual {
			bad = append(bad, r)
		}
	}
	return bad
}

// SourceIDs is, per mapped entity, the Linear ids the plan writes or maps.
// Its sizes are the Expected counts.
func (p *Plan) SourceIDs() map[string]map[string]bool {
	out := map[string]map[string]bool{}
	add := func(ent, id string) {
		if out[ent] == nil {
			out[ent] = map[string]bool{}
		}
		out[ent][id] = true
	}
	for _, x := range p.Teams {
		add(EntTeam, x.ID)
	}
	for _, x := range p.States {
		add(EntState, x.ID)
	}
	for id := range p.LabelCanon {
		add(EntLabel, id)
	}
	for _, x := range p.Users {
		add(EntUser, x.ID)
	}
	for _, x := range p.Projects {
		add(EntProject, x.ID)
	}
	for _, x := range p.Milestones {
		add(EntMilestone, x.ID)
	}
	for _, x := range p.Cycles {
		add(EntCycle, x.ID)
	}
	for _, x := range p.Issues {
		add(EntIssue, x.ID)
	}
	for _, x := range p.Changes {
		add(EntStateChange, x.SourceID)
	}
	for _, x := range p.Documents {
		add(EntDocument, x.ID)
	}
	for _, x := range p.Comments {
		add(EntComment, x.ID)
	}
	for _, x := range p.Relations {
		add(EntRelation, x.ID)
	}
	for _, x := range p.Attachments {
		add(EntAttachment, x.ID)
	}
	for _, x := range p.Files {
		add(EntFile, x.URL)
	}
	return out
}
