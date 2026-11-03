package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/scm"
)

// MR checks from provider events (LAUN-85, decisions-T17 #19). A check event
// names a commit. The links with that head sha keep a map from a check name to
// {state, at}; the link's checks is the worst entry. The columns checks,
// checks_detail and checks_updated_at belong to these events only, so a newer
// MR update never hides an older check and the reverse. A new head sha clears
// them (withEventFields, ApplyMRLink).

const (
	// maxCheckNames is the most check names one link keeps.
	maxCheckNames = 50
	// maxCheckNameRunes cuts a check name (host text).
	maxCheckNameRunes = 100
)

// checkEntry is one check of a link.
type checkEntry struct {
	State string    `json:"state"`
	At    time.Time `json:"at"`
}

func checkSeverity(state string) int {
	switch state {
	case string(scm.ChecksFailure):
		return 3
	case string(scm.ChecksPending):
		return 2
	case string(scm.ChecksSuccess):
		return 1
	}
	return 0
}

// worstCheck is the state of a link: failure over pending over success; none
// for an empty map.
func worstCheck(m map[string]checkEntry) string {
	worst := string(scm.ChecksNone)
	for _, e := range m {
		if checkSeverity(e.State) > checkSeverity(worst) {
			worst = e.State
		}
	}
	return worst
}

// mergeCheck puts one result into the map. A result older than the stored one
// of THAT name is dropped (an equal time replaces: hosts give second
// resolution). A name beyond maxCheckNames evicts the entry with the oldest
// time. It reports whether the map changed.
func mergeCheck(m map[string]checkEntry, name, state string, at time.Time) bool {
	at = at.UTC()
	if old, ok := m[name]; ok {
		if at.Before(old.At) || (old.State == state && old.At.Equal(at)) {
			return false
		}
	} else if len(m) >= maxCheckNames {
		oldest, first := "", true
		for n, e := range m {
			if first || e.At.Before(m[oldest].At) || (e.At.Equal(m[oldest].At) && n < oldest) {
				oldest, first = n, false
			}
		}
		delete(m, oldest)
	}
	m[name] = checkEntry{State: state, At: at}
	return true
}

// maxCheckSkew is how far ahead of the server clock a host time may be before
// it is not believed (LAUN-97).
const maxCheckSkew = 5 * time.Minute

// clampCheckTime gives a check result its time. No usable host time takes the
// server time. A host time more than maxCheckSkew ahead of the server (a wrong
// clock, or a sender that wants its word to be the last) takes the server time
// too, so it cannot block later results of that name.
func clampCheckTime(at, now time.Time) time.Time {
	if at.IsZero() || at.After(now.Add(maxCheckSkew)) {
		return now
	}
	return at
}

func newestCheck(m map[string]checkEntry) *time.Time {
	var t *time.Time
	for _, e := range m {
		if t == nil || e.At.After(*t) {
			at := e.At
			t = &at
		}
	}
	return t
}

// applyChecks applies one check event to the (locked) links with its head
// sha. It returns the events to emit and whether any link changed. It writes
// no timeline row and moves nothing.
func (s *Service) applyChecks(ctx context.Context, q *dbq.Queries, hook GitHook, repoKey string, ev scm.Event, rows []dbq.LockMRLinksByHeadSHARow) ([]events.Scoped, bool, error) {
	state := string(ev.Checks)
	name := cleanText(ev.CheckName, maxCheckNameRunes)
	if checkSeverity(state) == 0 || name == "" {
		return nil, false, nil
	}
	at := clampCheckTime(ev.OccurredAt, s.now())
	var out []events.Scoped
	changed := false
	for _, r := range rows {
		detail := map[string]checkEntry{}
		if len(r.ChecksDetail) > 0 {
			if err := json.Unmarshal(r.ChecksDetail, &detail); err != nil || detail == nil {
				detail = map[string]checkEntry{} // an unreadable map starts again; the entry below is the newest word
			}
		}
		if !mergeCheck(detail, name, state, at) {
			continue
		}
		raw, err := json.Marshal(detail)
		if err != nil {
			return nil, false, fmt.Errorf("checks detail: %w", err)
		}
		next := worstCheck(detail)
		if err := q.ApplyMRChecks(ctx, dbq.ApplyMRChecksParams{
			OrgID: hook.OrgID, ID: r.ID, Checks: next, ChecksDetail: raw, ChecksUpdatedAt: newestCheck(detail),
		}); err != nil {
			return nil, false, fmt.Errorf("update mr checks: %w", err)
		}
		changed = true
		if next != r.Checks {
			marker := ev
			marker.Number, marker.URL = r.Number, r.Url
			out = append(out, mrEvent(r.IssueID, Identifier(r.TeamKey, r.IssueNumber), ev.Provider, r.TeamID, r.AssigneeID,
				mrMarker(marker, repoKey)))
		}
	}
	return out, changed, nil
}
