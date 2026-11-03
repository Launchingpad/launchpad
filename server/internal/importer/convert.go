package importer

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"devops/internal/tracker"
)

// Length limits of the dev-ops schema (0001 CHECK constraints).
const (
	maxIssueTitle   = 500
	maxNameLen      = 255 // projects, milestones, cycles, documents
	maxLabelName    = 100
	maxStateName    = 100
	maxCommentBody  = 100000
	maxTeamName     = 200
	maxCycleLengthD = 56
)

// stateType maps a Linear workflow state type to the dev-ops type. They
// are the same six types; Linear's newer "duplicate" type is a canceled
// state in dev-ops (decisions-T1 #8: Duplicate is canceled-type).
func stateType(t string) (string, error) {
	switch t {
	case "triage", "backlog", "unstarted", "started", "completed", "canceled":
		return t, nil
	case "duplicate":
		return "canceled", nil
	}
	return "", fmt.Errorf("unknown workflow state type %q", t)
}

// relationType maps a Linear relation type. "similar" is Linear's AI
// suggestion, not a relation a person made, so it is not imported.
func relationType(t string) (string, bool) {
	switch t {
	case "blocks":
		return "blocks", true
	case "duplicate":
		return "duplicate_of", true
	case "related":
		return "related", true
	}
	return "", false
}

// priority is Linear's 0-4 scale (0 none, 1 urgent ... 4 low), which
// dev-ops uses as it is. A value outside it reads as 0.
func priority(p float64) int16 {
	if p < 0 || p > 4 || math.IsNaN(p) {
		return 0
	}
	return int16(math.Round(p))
}

// estimate rounds a Linear estimate to whole points (dev-ops estimates are
// integers). A negative or non-finite value is no estimate.
func estimate(e *float64) *int32 {
	if e == nil || math.IsNaN(*e) || math.IsInf(*e, 0) || *e < 0 || *e > math.MaxInt32 {
		return nil
	}
	v := int32(math.Round(*e))
	return &v
}

// count rounds a history value to a non-negative int32.
func count(v float64) int32 {
	if math.IsNaN(v) || v <= 0 {
		return 0
	}
	if v >= math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(math.Round(v))
}

// points rounds a history value to a non-negative int64.
func points(v float64) int64 {
	if math.IsNaN(v) || v <= 0 {
		return 0
	}
	if v >= math.MaxInt64/2 {
		return math.MaxInt64 / 2
	}
	return int64(math.Round(v))
}

// clip cuts s to at most n runes. A name that is empty after trimming
// becomes fallback (the schema needs at least one character).
func clip(s string, n int, fallback string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// parseDate reads a Linear TimelessDate ("YYYY-MM-DD").
func parseDate(s *string) (*time.Time, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.DateOnly, *s)
	if err != nil {
		return nil, fmt.Errorf("date %q is not YYYY-MM-DD", *s)
	}
	return &t, nil
}

// projectState is the dev-ops project state from Linear's status type,
// which uses the same six names.
func projectState(p Project) string {
	if p.Status != nil {
		switch p.Status.Type {
		case "backlog", "planned", "started", "paused", "completed", "canceled":
			return p.Status.Type
		}
	}
	return "planned"
}

var hexSlugRe = regexp.MustCompile(`^[0-9a-f]{6,32}$`)

// docSlug keeps Linear's document URL shape, slug(title, 50) + "-" +
// slugId. Linear's slugId is 12 hex characters, the same shape dev-ops
// mints (decisions-T3 #25), and it is unique per workspace, so the slug is
// unique per org.
func docSlug(title, slugID string) string {
	suffix := strings.ToLower(slugID)
	if !hexSlugRe.MatchString(suffix) {
		suffix = tracker.Slug(slugID, 32)
	}
	if base := tracker.Slug(title, 50); base != "" {
		return base + "-" + suffix
	}
	return suffix
}

// Cadence is the cycle cadence the importer writes to a team whose cycles
// are still off.
type Cadence struct {
	LengthDays int32
	Weekday    int16 // 0 = Sunday
	Hour, Min  int
	Timezone   string
}

// deriveCadence reads a team's live cadence from Linear. The length is
// cycleDuration (weeks). The weekday and the start time come from the
// newest cycle's start, read in the team's zone: that is what Linear
// actually did, so it cannot disagree with the imported dates. With no
// cycles the weekday is cycleStartDay (0 = Sunday) at 00:00. ok is false
// when the zone is unusable, and then nothing is changed.
func deriveCadence(t Team, cycles []Cycle) (Cadence, bool) {
	loc, err := tracker.LoadZone(t.Timezone)
	if err != nil {
		return Cadence{}, false
	}
	days := int32(math.Round(t.CycleDuration * 7))
	if days < 1 || days > maxCycleLengthD {
		days = 14
	}
	c := Cadence{LengthDays: days, Timezone: t.Timezone}
	var newest *Cycle
	for i := range cycles {
		cy := &cycles[i]
		if RefID(cy.Team) != t.ID {
			continue
		}
		if newest == nil || cy.Number > newest.Number {
			newest = cy
		}
	}
	if newest != nil {
		l := newest.StartsAt.In(loc)
		c.Weekday = int16(l.Weekday())
		c.Hour, c.Min = l.Hour(), l.Minute()
		return c, true
	}
	wd := int(math.Round(t.CycleStartDay))
	if wd < 0 || wd > 6 {
		wd = 0
	}
	c.Weekday = int16(wd)
	return c, true
}

// Snapshot is one burn-up row made from a cycle's history arrays.
type Snapshot struct {
	Date            time.Time // team-local date, midnight UTC (a Postgres date)
	ScopeCount      int32
	CompletedCount  int32
	ScopePoints     int64
	CompletedPoints int64
	StartedPoints   int64
}

// snapshots turns a Linear cycle's daily history arrays into burn-up rows.
// Entry i is the cycle's i-th local day. Linear has no history for the
// started, triage, backlog, unstarted or canceled COUNTS, so those stay 0
// (decisions-T4). At most one row per day of the cycle is made.
func snapshots(cy Cycle, zone string) []Snapshot {
	loc, err := tracker.LoadZone(zone)
	if err != nil {
		loc = time.UTC
	}
	n := len(cy.IssueCountHistory)
	if n == 0 {
		return nil
	}
	l := cy.StartsAt.In(loc)
	maxDays := int(cy.EndsAt.Sub(cy.StartsAt).Hours()/24) + 1
	if maxDays < 1 {
		maxDays = 1
	}
	if n > maxDays {
		n = maxDays
	}
	at := func(a []float64, i int) float64 {
		if i < len(a) {
			return a[i]
		}
		return 0
	}
	out := make([]Snapshot, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Snapshot{
			Date:            time.Date(l.Year(), l.Month(), l.Day()+i, 0, 0, 0, 0, time.UTC),
			ScopeCount:      count(at(cy.IssueCountHistory, i)),
			CompletedCount:  count(at(cy.CompletedIssueCountHistory, i)),
			ScopePoints:     points(at(cy.ScopeHistory, i)),
			CompletedPoints: points(at(cy.CompletedScopeHistory, i)),
			StartedPoints:   points(at(cy.InProgressScopeHistory, i)),
		})
	}
	return out
}

// missingNumbers is every number between the smallest and the largest of
// nums that is not in nums, ascending.
func missingNumbers(nums []int32) []int32 {
	if len(nums) == 0 {
		return nil
	}
	set := map[int32]bool{}
	lo, hi := nums[0], nums[0]
	for _, n := range nums {
		set[n] = true
		lo = min(lo, n)
		hi = max(hi, n)
	}
	var out []int32
	for n := lo; n <= hi; n++ {
		if !set[n] {
			out = append(out, n)
		}
	}
	return out
}

// whole converts a Linear Float that holds an integer (an issue or cycle
// number) to int32, refusing anything else.
func whole(v float64) (int32, error) {
	if v < 1 || v > math.MaxInt32 || v != math.Trunc(v) {
		return 0, fmt.Errorf("number %v is not a positive integer", v)
	}
	return int32(v), nil
}
