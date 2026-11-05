package move

import (
	"time"

	"github.com/google/uuid"
)

// Manifest is the first frame of a bundle. It holds counts and versions only:
// no secret, no row value.
type Manifest struct {
	Format           int        `json:"format"`
	AppVersion       string     `json:"appVersion"`
	MigrationVersion uint       `json:"migrationVersion"`
	ExportedBy       *uuid.UUID `json:"exportedBy,omitempty"` // nil: run from the host by an operator
	ExportedAt       time.Time  `json:"exportedAt"`
	// Tables lists the copied tables in bundle order, with their row counts.
	Tables []TableCount `json:"tables"`
	// Sealed lists the sealed columns that hold a value (how many), so an
	// import can refuse early when it lacks a key.
	Sealed              []SealedCount `json:"sealed"`
	AttachmentsIncluded bool          `json:"attachmentsIncluded"`
	ObjectCount         int64         `json:"objectCount"`
	Bytes               int64         `json:"bytes"` // sum of the object sizes
}

// TableCount is one table of the manifest.
type TableCount struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
}

// SealedCount is one sealed column with values.
type SealedCount struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Ring   string `json:"ring"`
	Count  int64  `json:"count"`
}

// TotalRows is the sum of the table row counts.
func (m *Manifest) TotalRows() int64 {
	var n int64
	for _, t := range m.Tables {
		n += t.Rows
	}
	return n
}
