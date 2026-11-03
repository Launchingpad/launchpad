package importjob

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"devops/internal/importer"
)

// Session states (decisions-T17 #43).
const (
	StateNew        = "new"
	StateExporting  = "exporting"
	StateExported   = "exported"
	StateUserMap    = "user_map"
	StateDryRunOK   = "dry_run_ok"
	StateImporting  = "importing"
	StateImported   = "imported"
	StateVerified   = "verified"
	StateFailed     = "failed"
	StateClosed     = "closed"
	maxFailedNames  = 20
	maxVerifyRows   = 500
	maxExclusionRep = 200
)

// Job kinds and states.
const (
	KindExport = "export"
	KindDryRun = "dry_run"
	KindImport = "import"
	KindVerify = "verify"

	JobQueued    = "queued"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobCanceled  = "canceled"
)

// Job error codes (not HTTP codes). The text of a job's error is chosen by
// the code from errorTexts: the importer's own errors can name a user email
// or a ticket title and never reach a row or a response (#37).
const (
	CodeVerifyMismatch     = "verify_mismatch"
	CodeInterrupted        = "interrupted"
	CodeCanceled           = "canceled"
	CodeNoSpace            = "no_space"
	CodeLinearUnauthorized = "linear_unauthorized"
	CodeLinearUnreachable  = "linear_unreachable"
	CodeExportFailed       = "export_failed"
	CodeDryRunFailed       = "dry_run_failed"
	CodeImportFailed       = "import_failed"
	CodeVerifyFailed       = "verify_failed"
	CodeExportChanged      = "export_changed"
)

var errorTexts = map[string]string{
	CodeVerifyMismatch:     "The check found a difference between the export and the database. The rows show which.",
	CodeInterrupted:        "The server stopped, or lost this job, before it finished.",
	CodeCanceled:           "The job was canceled.",
	CodeNoSpace:            "The export stopped because the work directory has no space left for the files.",
	CodeLinearUnauthorized: "Linear refused the API key.",
	CodeLinearUnreachable:  "Linear could not be reached.",
	CodeExportFailed:       "The export failed. The server log has the step.",
	CodeDryRunFailed:       "The dry run failed. The server log has the step.",
	CodeImportFailed:       "The import failed and wrote nothing. The server log has the step.",
	CodeVerifyFailed:       "The check could not run. The server log has the step.",
	CodeExportChanged:      "The export on disk is not the one this step was approved for. Run the export and the dry run again.",
}

// FailedFile is one export file that was not downloaded (decisions-T17 #39).
type FailedFile struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// JobCounts is the importer's Report as the job row keeps it: the counts per
// entity, at most maxExclusionRep exclusions (with the total), and the upload
// counts. Uploads.Planned is what a real run would upload.
type JobCounts struct {
	DryRun              bool                       `json:"dryRun"`
	Counts              map[string]importer.Counts `json:"counts"`
	ExclusionsTotal     int                        `json:"exclusionsTotal"`
	Exclusions          []importer.Exclusion       `json:"exclusions"`
	MergedLabels        int                        `json:"mergedLabels"`
	MergedRelations     int                        `json:"mergedRelations"`
	MissingCycleNumbers map[string][]int32         `json:"missingCycleNumbers"`
	NextIssueNumbers    map[string]int32           `json:"nextIssueNumbers"`
	Cadence             map[string]string          `json:"cadence"`
	PendingUploads      int                        `json:"pendingUploads"`
	Uploads             importer.UploadCounts      `json:"uploads"`
}

// JobView is a job on the wire.
type JobView struct {
	ID              uuid.UUID            `json:"id"`
	SessionID       uuid.UUID            `json:"sessionId"`
	Kind            string               `json:"kind"`
	State           string               `json:"state"`
	Phase           string               `json:"phase"`
	Done            int64                `json:"done"`
	Total           int64                `json:"total"`
	Report          *JobCounts           `json:"report"`
	Rows            []importer.VerifyRow `json:"rows"`
	RowsTruncated   bool                 `json:"rowsTruncated"`
	Committed       bool                 `json:"committed"`
	ErrorCode       *string              `json:"errorCode"`
	ErrorText       *string              `json:"errorText"`
	CancelRequested bool                 `json:"cancelRequested"`
	FailedFiles     int                  `json:"failedFiles"`
	FailedFileNames []FailedFile         `json:"failedFileNames"`
	CreatedAt       time.Time            `json:"createdAt"`
	StartedAt       *time.Time           `json:"startedAt"`
	FinishedAt      *time.Time           `json:"finishedAt"`
}

// SessionJobs is the last job of each kind.
type SessionJobs struct {
	Export *JobView `json:"export"`
	DryRun *JobView `json:"dryRun"`
	Import *JobView `json:"import"`
	Verify *JobView `json:"verify"`
}

// SessionView is a session on the wire.
type SessionView struct {
	ID              uuid.UUID      `json:"id"`
	State           string         `json:"state"`
	CreatedBy       *uuid.UUID     `json:"createdBy"`
	ExportCounts    map[string]int `json:"exportCounts"`
	HasUserMap      bool           `json:"hasUserMap"`
	FailedFiles     int            `json:"failedFiles"`
	FailedFileNames []FailedFile   `json:"failedFileNames"`
	Jobs            SessionJobs    `json:"jobs"`
	CreatedAt       time.Time      `json:"createdAt"`
	UpdatedAt       time.Time      `json:"updatedAt"`
	ClosedAt        *time.Time     `json:"closedAt"`
}

// ExportUser is a Linear user of the export with the suggested and the
// current mapping.
type ExportUser struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	DisplayName string  `json:"displayName"`
	Email       string  `json:"email"`
	Active      bool    `json:"active"`
	App         bool    `json:"app"`
	Suggested   *string `json:"suggested"`
	Mapped      *string `json:"mapped"`
}

// MemberRef is an org member a Linear user can be mapped to.
type MemberRef struct {
	Username string `json:"username"`
	Name     string `json:"name"`
	IsAgent  bool   `json:"isAgent"`
}

// UsersView is the answer of GET /import/sessions/{id}/users.
type UsersView struct {
	Users   []ExportUser `json:"users"`
	Members []MemberRef  `json:"members"`
	Map     *UserMapView `json:"map"`
}

// UserMapView is the importer's {default, users} shape.
type UserMapView struct {
	Default string            `json:"default"`
	Users   map[string]string `json:"users"`
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("importjob: marshal: " + err.Error())
	}
	return b
}
