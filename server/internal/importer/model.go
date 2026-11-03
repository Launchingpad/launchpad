// Package importer moves a Linear workspace into Launchpad.
//
// The flow has three steps, each a subcommand of cmd/importer:
//
//  1. export: read the Linear GraphQL API (read-only) into JSONL files, one
//     file per entity type, and download the uploads.linear.app files that
//     the markdown refers to (package linear).
//  2. import: write the JSONL into Postgres through the sqlc store (dbq),
//     keeping every original timestamp. import_mappings records which row
//     each Linear id became, so a re-run updates or skips instead of
//     duplicating. --dry-run does the whole import in one transaction and
//     rolls it back, so its create / update / skip counts are exact.
//  3. verify: compare the export counts with the rows the mappings point
//     at, and fail on any difference.
//
// The record types below are the JSONL wire shape. They mirror the GraphQL
// selections in package linear field for field, so a record is written
// exactly as Linear returned it and read back without a translation layer.
package importer

import (
	"encoding/json"
	"time"
)

// Ref is a GraphQL `{ id }` selection. A nil *Ref is a null reference.
type Ref struct {
	ID string `json:"id"`
}

// RefID is the id of r, or "" for a null reference.
func RefID(r *Ref) string {
	if r == nil {
		return ""
	}
	return r.ID
}

// Team is a Linear team with its live cycle cadence.
type Team struct {
	ID                string     `json:"id"`
	Key               string     `json:"key"`
	Name              string     `json:"name"`
	Description       *string    `json:"description"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	ArchivedAt        *time.Time `json:"archivedAt"`
	CyclesEnabled     bool       `json:"cyclesEnabled"`
	CycleStartDay     float64    `json:"cycleStartDay"`
	CycleDuration     float64    `json:"cycleDuration"` // weeks
	CycleCooldownTime float64    `json:"cycleCooldownTime"`
	Timezone          string     `json:"timezone"`
}

// WorkflowState is a Linear issue status.
type WorkflowState struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Type        string     `json:"type"`
	Color       string     `json:"color"`
	Description *string    `json:"description"`
	Position    float64    `json:"position"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	ArchivedAt  *time.Time `json:"archivedAt"`
	Team        *Ref       `json:"team"`
}

// Label is a Linear issue label (workspace or team) or a label group.
type Label struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Color       string     `json:"color"`
	Description *string    `json:"description"`
	IsGroup     bool       `json:"isGroup"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	ArchivedAt  *time.Time `json:"archivedAt"`
	Team        *Ref       `json:"team"`
	Parent      *Ref       `json:"parent"`
}

// User is a Linear user. Email is used only to match the user map file.
type User struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	DisplayName string     `json:"displayName"`
	Email       string     `json:"email"`
	Active      bool       `json:"active"`
	App         bool       `json:"app"`
	Guest       bool       `json:"guest"`
	CreatedAt   time.Time  `json:"createdAt"`
	ArchivedAt  *time.Time `json:"archivedAt"`
}

// ProjectStatus is the `status { type name }` selection of a project.
type ProjectStatus struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// Connection is a GraphQL `{ nodes { id } }` list of references.
type Connection struct {
	Nodes []Ref `json:"nodes"`
}

// Project is a Linear project.
type Project struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Content     *string        `json:"content"`
	SlugID      string         `json:"slugId"`
	Status      *ProjectStatus `json:"status"`
	Lead        *Ref           `json:"lead"`
	StartDate   *string        `json:"startDate"`
	TargetDate  *string        `json:"targetDate"`
	CreatedAt   time.Time      `json:"createdAt"`
	UpdatedAt   time.Time      `json:"updatedAt"`
	StartedAt   *time.Time     `json:"startedAt"`
	CompletedAt *time.Time     `json:"completedAt"`
	CanceledAt  *time.Time     `json:"canceledAt"`
	ArchivedAt  *time.Time     `json:"archivedAt"`
	Teams       Connection     `json:"teams"`
}

// Milestone is a Linear project milestone.
type Milestone struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description *string    `json:"description"`
	TargetDate  *string    `json:"targetDate"`
	SortOrder   float64    `json:"sortOrder"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	ArchivedAt  *time.Time `json:"archivedAt"`
	Project     *Ref       `json:"project"`
}

// Cycle is a Linear cycle with its daily history arrays (index 0 is the
// cycle's first day).
type Cycle struct {
	ID                         string     `json:"id"`
	Number                     float64    `json:"number"`
	Name                       *string    `json:"name"`
	Description                *string    `json:"description"`
	StartsAt                   time.Time  `json:"startsAt"`
	EndsAt                     time.Time  `json:"endsAt"`
	CompletedAt                *time.Time `json:"completedAt"`
	CreatedAt                  time.Time  `json:"createdAt"`
	UpdatedAt                  time.Time  `json:"updatedAt"`
	ArchivedAt                 *time.Time `json:"archivedAt"`
	Team                       *Ref       `json:"team"`
	IssueCountHistory          []float64  `json:"issueCountHistory"`
	CompletedIssueCountHistory []float64  `json:"completedIssueCountHistory"`
	ScopeHistory               []float64  `json:"scopeHistory"`
	CompletedScopeHistory      []float64  `json:"completedScopeHistory"`
	InProgressScopeHistory     []float64  `json:"inProgressScopeHistory"`
}

// HistoryEntry is one IssueHistory row. Only the state fields are kept:
// they feed issue_state_changes.
type HistoryEntry struct {
	ID          string    `json:"id"`
	CreatedAt   time.Time `json:"createdAt"`
	FromStateID *string   `json:"fromStateId"`
	ToStateID   *string   `json:"toStateId"`
	ActorID     *string   `json:"actorId"`
}

// Issue is a Linear issue. History holds every page of its history (the
// exporter follows the nested connection's cursor).
type Issue struct {
	ID               string         `json:"id"`
	Identifier       string         `json:"identifier"`
	Number           float64        `json:"number"`
	Title            string         `json:"title"`
	Description      *string        `json:"description"`
	Priority         float64        `json:"priority"`
	Estimate         *float64       `json:"estimate"`
	SortOrder        float64        `json:"sortOrder"`
	CreatedAt        time.Time      `json:"createdAt"`
	UpdatedAt        time.Time      `json:"updatedAt"`
	StartedAt        *time.Time     `json:"startedAt"`
	CompletedAt      *time.Time     `json:"completedAt"`
	CanceledAt       *time.Time     `json:"canceledAt"`
	ArchivedAt       *time.Time     `json:"archivedAt"`
	Trashed          *bool          `json:"trashed"`
	DueDate          *string        `json:"dueDate"`
	LabelIDs         []string       `json:"labelIds"`
	Team             *Ref           `json:"team"`
	State            *Ref           `json:"state"`
	Assignee         *Ref           `json:"assignee"`
	Creator          *Ref           `json:"creator"`
	Parent           *Ref           `json:"parent"`
	Project          *Ref           `json:"project"`
	ProjectMilestone *Ref           `json:"projectMilestone"`
	Cycle            *Ref           `json:"cycle"`
	History          []HistoryEntry `json:"history"`
}

// Comment is a Linear comment on an issue, a document (documentContentId)
// or something dev-ops has no table for (a project update).
type Comment struct {
	ID                string     `json:"id"`
	Body              string     `json:"body"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	EditedAt          *time.Time `json:"editedAt"`
	ArchivedAt        *time.Time `json:"archivedAt"`
	IssueID           *string    `json:"issueId"`
	DocumentContentID *string    `json:"documentContentId"`
	ProjectUpdateID   *string    `json:"projectUpdateId"`
	ParentID          *string    `json:"parentId"`
	User              *Ref       `json:"user"`
}

// Document is a Linear document.
type Document struct {
	ID                string     `json:"id"`
	Title             string     `json:"title"`
	Content           *string    `json:"content"`
	SlugID            string     `json:"slugId"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	ArchivedAt        *time.Time `json:"archivedAt"`
	Trashed           *bool      `json:"trashed"`
	DocumentContentID *string    `json:"documentContentId"`
	Project           *Ref       `json:"project"`
	Creator           *Ref       `json:"creator"`
	Issue             *Ref       `json:"issue"`
}

// Relation is a Linear issue relation (blocks, duplicate, related, similar).
type Relation struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	ArchivedAt   *time.Time `json:"archivedAt"`
	Issue        *Ref       `json:"issue"`
	RelatedIssue *Ref       `json:"relatedIssue"`
}

// Attachment is a Linear issue attachment (a link: a GitLab MR, a URL, or
// an uploaded file under uploads.linear.app).
type Attachment struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	Subtitle   *string         `json:"subtitle"`
	URL        string          `json:"url"`
	SourceType *string         `json:"sourceType"`
	Metadata   json.RawMessage `json:"metadata"`
	CreatedAt  time.Time       `json:"createdAt"`
	UpdatedAt  time.Time       `json:"updatedAt"`
	ArchivedAt *time.Time      `json:"archivedAt"`
	Creator    *Ref            `json:"creator"`
	Issue      *Ref            `json:"issue"`
}

// File is one downloaded uploads.linear.app object (files.jsonl). Path is
// relative to the export directory. Error is set when the download failed;
// such a file has no Path.
type File struct {
	URL         string `json:"url"`
	Path        string `json:"path,omitempty"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	SizeBytes   int64  `json:"sizeBytes,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Manifest is manifest.json: when the export ran and how many records each
// file holds. Import refuses an export whose files do not match it.
type Manifest struct {
	Format     int            `json:"format"`
	ExportedAt time.Time      `json:"exportedAt"`
	Counts     map[string]int `json:"counts"`
}

// ManifestFormat is the current export layout version.
const ManifestFormat = 1

// The export files, by entity. The key is also the Manifest.Counts key.
const (
	FileTeams       = "teams"
	FileStates      = "workflow_states"
	FileLabels      = "labels"
	FileUsers       = "users"
	FileProjects    = "projects"
	FileMilestones  = "milestones"
	FileCycles      = "cycles"
	FileIssues      = "issues"
	FileComments    = "comments"
	FileDocuments   = "documents"
	FileRelations   = "relations"
	FileAttachments = "attachments"
	FileFiles       = "files"
)

// EntityFiles is every JSONL file of an export, in import order.
var EntityFiles = []string{
	FileTeams, FileStates, FileLabels, FileUsers, FileProjects, FileMilestones, FileCycles,
	FileIssues, FileComments, FileDocuments, FileRelations, FileAttachments, FileFiles,
}

// Export is a whole export read into memory (a typical one is a few MB).
type Export struct {
	Manifest    Manifest
	Teams       []Team
	States      []WorkflowState
	Labels      []Label
	Users       []User
	Projects    []Project
	Milestones  []Milestone
	Cycles      []Cycle
	Issues      []Issue
	Comments    []Comment
	Documents   []Document
	Relations   []Relation
	Attachments []Attachment
	Files       []File
}
