package tracker

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/dbq"
)

// TestIsForwardMove pins the forward-only rule on the default workflow
// (positions are the board order, as CreateTeamTx makes them).
func TestIsForwardMove(t *testing.T) {
	states := map[string]dbq.WorkflowState{}
	for i, d := range DefaultStates {
		states[d.Name] = dbq.WorkflowState{ID: uuid.New(), Name: d.Name, Type: d.Type, Position: float64(i)}
	}
	archived := states["In Review"]
	now := time.Now()
	archived.ID, archived.ArchivedAt = uuid.New(), &now

	tests := []struct {
		from, to string
		want     bool
	}{
		{"Backlog", "In Review", true},
		{"Triage", "In Review", true}, // LAUN-20: an MR moves a triage issue forward
		{"Triage", "In Testing", true},
		{"In Review", "Triage", false},
		{"Backlog", "Triage", false},
		{"Todo", "In Review", true},
		{"In Progress", "In Review", true},
		{"In Progress", "In Testing", true},
		{"In Review", "In Testing", true},
		{"Backlog", "In Testing", true},
		{"In Testing", "In Review", false}, // a reopen after a merge stays put
		{"In Review", "In Review", false},
		{"In Testing", "In Testing", false},
		{"In Review", "In Progress", false},
		{"In Review", "Todo", false},
		{"Done", "In Review", false},
		{"Done", "In Testing", false},
		{"Canceled", "In Review", false},
		{"Canceled", "In Testing", false},
		{"Duplicate", "In Review", false},
		{"Duplicate", "In Testing", false},
		{"In Testing", "Done", true}, // a testing anchor set to Done still moves forward
	}
	for _, tt := range tests {
		t.Run(tt.from+" to "+tt.to, func(t *testing.T) {
			if got := isForwardMove(states[tt.from], states[tt.to]); got != tt.want {
				t.Fatalf("isForwardMove(%s, %s) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
	t.Run("never into an archived state", func(t *testing.T) {
		if isForwardMove(states["Todo"], archived) {
			t.Fatal("moved into an archived state")
		}
	})
}

func TestCleanText(t *testing.T) {
	tests := []struct {
		name, in string
		max      int
		want     string
	}{
		{"plain", "ORB-1: Fix", 255, "ORB-1: Fix"},
		{"control characters become spaces", "a\tb\nc\x00d", 255, "a b c d"},
		{"trimmed", "  x  ", 255, "x"},
		{"cut by runes", strings.Repeat("é", 10), 4, "éééé"},
		{"invalid utf-8 is replaced", "a\xffb", 255, "a?b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cleanText(tt.in, tt.max)
			if got != tt.want {
				t.Fatalf("cleanText(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if _, err := validateAttachmentText("title", got); err != nil {
				t.Fatalf("cleaned text fails the attachment title check: %v", err)
			}
		})
	}
}
