package tracker

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Wire models. JSON names are camelCase and follow Linear's where one exists,
// so agent briefs and the MCP (T2) keep their meaning. api/openapi.yaml
// documents each of them; keep the two in step.

// UserRef is a user as embedded in another object.
type UserRef struct {
	ID       uuid.UUID `json:"id"`
	Username string    `json:"username"`
	Name     string    `json:"name"`
}

// Member is a user in an org's member list.
type Member struct {
	ID            uuid.UUID  `json:"id"`
	Username      string     `json:"username"`
	Name          string     `json:"name"`
	Email         *string    `json:"email"`
	IsAgent       bool       `json:"isAgent"`
	Role          string     `json:"role"`
	CreatedAt     time.Time  `json:"createdAt"`
	DeactivatedAt *time.Time `json:"deactivatedAt"`
	// TeamAccess ("all" or "selected") and Teams (the team keys of a
	// "selected" member, sorted; empty for "all") are shown to org admins
	// only, and are null for any other caller (decisions-T20 #33). An org
	// admin member is always "all" (#2).
	TeamAccess *string  `json:"teamAccess"`
	Teams      []string `json:"teams"`
}

// Me is the authenticated caller. DisplayName equals Name; Name stays for
// clients written against T1.
type Me struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	Name        string    `json:"name"`
	DisplayName string    `json:"displayName"`
	Email       *string   `json:"email"`
	IsAgent     bool      `json:"isAgent"`
	// IsPlatformAdmin: the user runs this install. The token may
	// still not use the platform routes (RequirePlatformAdmin).
	IsPlatformAdmin bool       `json:"isPlatformAdmin"`
	TokenID         uuid.UUID  `json:"tokenId"`
	TokenKind       string     `json:"tokenKind"`
	Scopes          []string   `json:"scopes"`
	PinnedOrgID     *uuid.UUID `json:"pinnedOrgId"`
}

// Org is an org the caller belongs to.
type Org struct {
	ID        uuid.UUID `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
	// TimeZone is an IANA name (LAUN-56); DefaultTeam is a team key or null.
	TimeZone    string  `json:"timeZone"`
	DefaultTeam *string `json:"defaultTeam"`
	// AvatarURL is the logo's URL on the API, or null (LAUN-22).
	AvatarURL *string `json:"avatarUrl"`
}

// TeamRef is a team as embedded in an issue.
type TeamRef struct {
	ID   uuid.UUID `json:"id"`
	Key  string    `json:"key"`
	Name string    `json:"name"`
}

// Team is a team.
type Team struct {
	ID             uuid.UUID  `json:"id"`
	Key            string     `json:"key"`
	Name           string     `json:"name"`
	Description    string     `json:"description"`
	DefaultStateID *uuid.UUID `json:"defaultStateId"`
	ReviewStateID  *uuid.UUID `json:"reviewStateId"`
	TestingStateID *uuid.UUID `json:"testingStateId"`
	CyclesEnabled  bool       `json:"cyclesEnabled"`
	// The cycle cadence (T3): length, the local weekday (0 = Sunday) and
	// HH:MM a cycle starts at, the IANA zone those are read in, and whether
	// open issues roll over to the next cycle.
	CycleLengthDays   int32  `json:"cycleLengthDays"`
	CycleStartWeekday int16  `json:"cycleStartWeekday"`
	CycleStartTime    string `json:"cycleStartTime"`
	CycleTimezone     string `json:"cycleTimezone"`
	CycleRollover     bool   `json:"cycleRollover"`
	// ProductIDs are the team's NON-archived products (T18 #9); an empty
	// array, never null, when there are none.
	ProductIDs []uuid.UUID `json:"productIds"`
	CreatedAt  time.Time   `json:"createdAt"`
	UpdatedAt  time.Time   `json:"updatedAt"`
}

// StateRef is a workflow state as embedded in an issue.
type StateRef struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Type  string    `json:"type"`
	Color string    `json:"color"`
}

// WorkflowState is one state of a team's workflow.
type WorkflowState struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Color       string    `json:"color"`
	Description string    `json:"description"`
	Position    float64   `json:"position"`
	TeamID      uuid.UUID `json:"teamId"`
}

// LabelRef is a label as embedded in an issue.
type LabelRef struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Color string    `json:"color"`
}

// Label is an org label or a team label (TeamID set). IssueCount is the
// number of non-archived issues that carry it (decisions-T21-team-labels #5);
// it is 0 on a label just made.
type Label struct {
	ID          uuid.UUID  `json:"id"`
	Name        string     `json:"name"`
	Color       string     `json:"color"`
	Description string     `json:"description"`
	ParentID    *uuid.UUID `json:"parentId"`
	TeamID      *uuid.UUID `json:"teamId"`
	TeamKey     *string    `json:"teamKey"`
	IssueCount  int32      `json:"issueCount"`
	CreatedAt   time.Time  `json:"createdAt"`
}

// IssueRef is a compact pointer to another issue.
type IssueRef struct {
	ID         uuid.UUID `json:"id"`
	Identifier string    `json:"identifier"`
	Title      string    `json:"title"`
	StateName  string    `json:"stateName,omitempty"`
	StateType  string    `json:"stateType,omitempty"`
}

// Issue is the issue resource. The include= fields are omitted unless asked
// for; when asked for they are always arrays (possibly empty).
type Issue struct {
	ID                   uuid.UUID  `json:"id"`
	Identifier           string     `json:"identifier"`
	Number               int32      `json:"number"`
	Title                string     `json:"title"`
	Description          string     `json:"description"`
	DescriptionTruncated bool       `json:"descriptionTruncated"`
	Priority             int16      `json:"priority"`
	PriorityLabel        string     `json:"priorityLabel"`
	Estimate             *int32     `json:"estimate"`
	Team                 TeamRef    `json:"team"`
	State                StateRef   `json:"state"`
	Assignee             *UserRef   `json:"assignee"`
	Creator              *UserRef   `json:"creator"`
	Labels               []LabelRef `json:"labels"`
	Parent               *IssueRef  `json:"parent"`
	ProjectID            *uuid.UUID `json:"projectId"`
	MilestoneID          *uuid.UUID `json:"milestoneId"`
	CycleID              *uuid.UUID `json:"cycleId"`
	DueDate              *string    `json:"dueDate"`
	SortOrder            float64    `json:"sortOrder"`
	// Rebalanced is only on a PATCH response: the placement renumbered the
	// destination column, so the other cards' sortOrder changed.
	Rebalanced    bool       `json:"rebalanced,omitempty"`
	GitBranchName string     `json:"gitBranchName"`
	Version       int32      `json:"version"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	StartedAt     *time.Time `json:"startedAt"`
	CompletedAt   *time.Time `json:"completedAt"`
	CanceledAt    *time.Time `json:"canceledAt"`
	ArchivedAt    *time.Time `json:"archivedAt"`

	Relations    *Relations    `json:"relations,omitempty"`
	StateHistory []StateChange `json:"stateHistory,omitzero"`
	Comments     []Comment     `json:"comments,omitzero"`
	Attachments  []Attachment  `json:"attachments,omitzero"`
	// ChangeRequests are the merge and pull requests linked to the issue
	// (only with include=changeRequests, and only when there is one).
	ChangeRequests []ChangeRequest `json:"changeRequests,omitzero"`
}

// Relation is one edge, seen from the issue it is listed under.
type Relation struct {
	ID    uuid.UUID `json:"id"`
	Type  string    `json:"type"` // blocks | blockedBy | related | duplicateOf | duplicates
	Issue IssueRef  `json:"issue"`
	// CreatedAt and CreatedBy say when and by whom the relation was added
	// (CreatedBy is null for an imported relation or a deleted user).
	CreatedAt time.Time `json:"createdAt"`
	CreatedBy *UserRef  `json:"createdBy"`
}

// Relations groups an issue's edges by direction.
type Relations struct {
	Blocks      []Relation `json:"blocks"`
	BlockedBy   []Relation `json:"blockedBy"`
	Related     []Relation `json:"related"`
	DuplicateOf []Relation `json:"duplicateOf"`
	Duplicates  []Relation `json:"duplicates"`
}

// StateChange is one row of an issue's state history.
type StateChange struct {
	ID            uuid.UUID  `json:"id"`
	FromStateID   *uuid.UUID `json:"fromStateId"`
	FromStateName *string    `json:"fromStateName"`
	ToStateID     uuid.UUID  `json:"toStateId"`
	ToStateName   string     `json:"toStateName"`
	Actor         *UserRef   `json:"actor"`
	Source        string     `json:"source"`
	CreatedAt     time.Time  `json:"createdAt"`
}

// Comment is a comment on an issue or on a document: exactly one of IssueID
// and DocumentID is set.
type Comment struct {
	ID         uuid.UUID  `json:"id"`
	IssueID    *uuid.UUID `json:"issueId"`
	DocumentID *uuid.UUID `json:"documentId"`
	ParentID   *uuid.UUID `json:"parentId"`
	Body       string     `json:"body"`
	Author     *UserRef   `json:"author"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	EditedAt   *time.Time `json:"editedAt"`
}

// Attachment is a link or a file on an issue. URL is set for a
// link; Filename, ContentType, SizeBytes and DownloadURL for a file.
// DownloadURL is the API route that redirects to the store; it is the URL
// Markdown should use.
type Attachment struct {
	ID          uuid.UUID `json:"id"`
	IssueID     uuid.UUID `json:"issueId"`
	Kind        string    `json:"kind"`
	Title       string    `json:"title"`
	Subtitle    string    `json:"subtitle"`
	URL         *string   `json:"url"`
	Filename    *string   `json:"filename"`
	ContentType *string   `json:"contentType"`
	SizeBytes   *int64    `json:"sizeBytes"`
	DownloadURL *string   `json:"downloadUrl"`
	Creator     *UserRef  `json:"creator"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Event is one change-feed entry.
type Event struct {
	Seq        int64           `json:"seq"`
	Type       string          `json:"type"`
	EntityType string          `json:"entityType"`
	EntityID   uuid.UUID       `json:"entityId"`
	Actor      *UserRef        `json:"actor"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"createdAt"`
}

// Token is an API token as listed. The secret is never in it.
type Token struct {
	ID         uuid.UUID  `json:"id"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	User       UserRef    `json:"user"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  *time.Time `json:"expiresAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
}

// CreatedToken is returned once, at creation. Secret is the only copy.
type CreatedToken struct {
	Token  Token  `json:"token"`
	Secret string `json:"secret"`
}

// --- T3 planning objects ----------------------------------------------------

// IssueCounts are the issue counts of a project (LAUN-170): total counts every
// non-archived issue (sub-issues too); canceled ones are out of scope, so
// progress is done / scope. An empty project is all zeros.
type IssueCounts struct {
	Total    int32 `json:"total"`
	Scope    int32 `json:"scope"`
	Done     int32 `json:"done"`
	Started  int32 `json:"started"`
	Canceled int32 `json:"canceled"`
}

// Project is a project. A project belongs to zero or more teams.
type Project struct {
	ID               uuid.UUID `json:"id"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	Content          string    `json:"content"`
	ContentTruncated bool      `json:"contentTruncated"`
	// State is the KIND of Status (backlog, planned, started, paused, completed, canceled), as before LAUN-195.
	State      string           `json:"state"`
	Status     ProjectStatusRef `json:"status"`
	Lead       *UserRef         `json:"lead"`
	StartDate  *string          `json:"startDate"`
	TargetDate *string          `json:"targetDate"`
	Teams      []TeamRef        `json:"teams"`
	// Labels are the project labels on the project (LAUN-194), by name.
	Labels      []ProjectLabelRef `json:"labels"`
	Version     int32             `json:"version"`
	CreatedAt   time.Time         `json:"createdAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
	StartedAt   *time.Time        `json:"startedAt"`
	CompletedAt *time.Time        `json:"completedAt"`
	CanceledAt  *time.Time        `json:"canceledAt"`
	ArchivedAt  *time.Time        `json:"archivedAt"`
	// UpdateSchedule is when the lead is reminded to post an update (LAUN-196).
	UpdateSchedule UpdateSchedule `json:"updateSchedule"`
	// AvatarURL is the logo's URL on the API, or null (LAUN-22).
	AvatarURL *string `json:"avatarUrl"`
	// Emoji is one emoji as the icon, or null (LAUN-232). The web shows the
	// logo first, then the emoji, then a letter tile.
	Emoji         *string     `json:"emoji"`
	IssueCounts   IssueCounts `json:"issueCounts"`
	avatarVersion int32
	hasAvatar     bool
}

// Milestone is a project milestone.
type Milestone struct {
	ID          uuid.UUID  `json:"id"`
	ProjectID   uuid.UUID  `json:"projectId"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	TargetDate  *string    `json:"targetDate"`
	SortOrder   float64    `json:"sortOrder"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	ArchivedAt  *time.Time `json:"archivedAt"`
}

// Cycle is one team cycle. The server creates cycles itself (the cycle job);
// the API can only rename them.
type Cycle struct {
	ID          uuid.UUID  `json:"id"`
	Team        TeamRef    `json:"team"`
	Number      int32      `json:"number"`
	Name        *string    `json:"name"`
	Description string     `json:"description"`
	StartsAt    time.Time  `json:"startsAt"`
	EndsAt      time.Time  `json:"endsAt"`
	CompletedAt *time.Time `json:"completedAt"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	ArchivedAt  *time.Time `json:"archivedAt"`
}

// CycleSnapshot is one day of a cycle's burn-up (docs/internal/decisions-T3.md).
type CycleSnapshot struct {
	CycleID         uuid.UUID `json:"cycleId"`
	Date            string    `json:"date"`
	ScopeCount      int32     `json:"scopeCount"`
	TriageCount     int32     `json:"triageCount"`
	BacklogCount    int32     `json:"backlogCount"`
	UnstartedCount  int32     `json:"unstartedCount"`
	StartedCount    int32     `json:"startedCount"`
	CompletedCount  int32     `json:"completedCount"`
	CanceledCount   int32     `json:"canceledCount"`
	ScopePoints     int64     `json:"scopePoints"`
	StartedPoints   int64     `json:"startedPoints"`
	CompletedPoints int64     `json:"completedPoints"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// Document is a markdown document, optionally in a project.
type Document struct {
	ID               uuid.UUID  `json:"id"`
	Title            string     `json:"title"`
	Content          string     `json:"content"`
	ContentTruncated bool       `json:"contentTruncated"`
	Slug             string     `json:"slug"`
	ProjectID        *uuid.UUID `json:"projectId"`
	Creator          *UserRef   `json:"creator"`
	Version          int32      `json:"version"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	ArchivedAt       *time.Time `json:"archivedAt"`
}

// ProjectLabelRef is a project label as it shows on a project (LAUN-194).
type ProjectLabelRef struct {
	ID       uuid.UUID  `json:"id"`
	Name     string     `json:"name"`
	Color    string     `json:"color"`
	ParentID *uuid.UUID `json:"parentId"`
}

// ProjectLabel is an org-wide project label, with the number of projects that carry it.
type ProjectLabel struct {
	ID           uuid.UUID  `json:"id"`
	Name         string     `json:"name"`
	Color        string     `json:"color"`
	ParentID     *uuid.UUID `json:"parentId"`
	ProjectCount int        `json:"projectCount"`
	CreatedAt    time.Time  `json:"createdAt"`
}
