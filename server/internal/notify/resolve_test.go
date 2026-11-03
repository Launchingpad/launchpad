package notify

import (
	"testing"

	"github.com/google/uuid"

	"devops/internal/dbq"
)

func sp(s string) *string { return &s }

// TestResolve pins the order (team row, global row, built-in default), the
// per-channel NULL fallthrough and the master email switch (decisions-T13 #11).
func TestResolve(t *testing.T) {
	teamA, teamB := uuid.New(), uuid.New()
	global := func(ev Event, email, inbox *string) dbq.NotificationSetting {
		return dbq.NotificationSetting{Event: string(ev), Email: email, Inbox: inbox}
	}
	team := func(id uuid.UUID, ev Event, email, inbox *string) dbq.NotificationSetting {
		return dbq.NotificationSetting{TeamID: &id, Event: string(ev), Email: email, Inbox: inbox}
	}
	on := UserPrefs{EmailEnabled: true}
	off := UserPrefs{EmailEnabled: false}
	cases := []struct {
		name  string
		user  UserPrefs
		team  *uuid.UUID
		event Event
		rows  []dbq.NotificationSetting
		want  Resolved
	}{
		{"default assigned", on, nil, EventAssigned, nil,
			Resolved{Channel{true, SourceDefault}, Channel{true, SourceDefault}, Channel{}}},
		{"default mention", on, &teamA, EventMention, nil,
			Resolved{Channel{true, SourceDefault}, Channel{true, SourceDefault}, Channel{}}},
		{"default comment", on, &teamA, EventComment, nil,
			Resolved{Channel{true, SourceDefault}, Channel{true, SourceDefault}, Channel{}}},
		{"default state_change is inbox only", on, &teamA, EventStateChange, nil,
			Resolved{Channel{false, SourceDefault}, Channel{true, SourceDefault}, Channel{}}},
		{"default new_issue is inbox only", on, &teamA, EventNewIssue, nil,
			Resolved{Channel{false, SourceDefault}, Channel{true, SourceDefault}, Channel{}}},
		{"global row beats default", on, &teamA, EventStateChange,
			[]dbq.NotificationSetting{global(EventStateChange, sp("on"), sp("off"))},
			Resolved{Channel{true, SourceGlobal}, Channel{false, SourceGlobal}, Channel{}}},
		{"team row beats global row", on, &teamA, EventComment,
			[]dbq.NotificationSetting{global(EventComment, sp("on"), sp("on")), team(teamA, EventComment, sp("off"), sp("off"))},
			Resolved{Channel{false, SourceTeam}, Channel{false, SourceTeam}, Channel{}}},
		{"a NULL team column falls to global, per channel", on, &teamA, EventComment,
			[]dbq.NotificationSetting{global(EventComment, nil, sp("off")), team(teamA, EventComment, sp("off"), nil)},
			Resolved{Channel{false, SourceTeam}, Channel{false, SourceGlobal}, Channel{}}},
		{"a NULL global column falls to default", on, nil, EventComment,
			[]dbq.NotificationSetting{global(EventComment, nil, nil)},
			Resolved{Channel{true, SourceDefault}, Channel{true, SourceDefault}, Channel{}}},
		{"another team's row is ignored", on, &teamB, EventComment,
			[]dbq.NotificationSetting{team(teamA, EventComment, sp("off"), sp("off"))},
			Resolved{Channel{true, SourceDefault}, Channel{true, SourceDefault}, Channel{}}},
		{"no team asked: team rows are ignored", on, nil, EventComment,
			[]dbq.NotificationSetting{team(teamA, EventComment, sp("off"), sp("off"))},
			Resolved{Channel{true, SourceDefault}, Channel{true, SourceDefault}, Channel{}}},
		{"another event's row is ignored", on, &teamA, EventComment,
			[]dbq.NotificationSetting{global(EventAssigned, sp("off"), sp("off"))},
			Resolved{Channel{true, SourceDefault}, Channel{true, SourceDefault}, Channel{}}},
		{"master switch wins over a team row", off, &teamA, EventComment,
			[]dbq.NotificationSetting{team(teamA, EventComment, sp("on"), sp("on"))},
			Resolved{Channel{false, SourceTeam}, Channel{true, SourceTeam}, Channel{}}},
		{"master switch wins over the default and leaves the inbox", off, nil, EventAssigned, nil,
			Resolved{Channel{false, SourceDefault}, Channel{true, SourceDefault}, Channel{}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Resolve(c.user, c.team, c.event, c.rows)
			// Push has its own test; with no push column set it is off by default.
			if got.Push != (Channel{false, SourceDefault}) {
				t.Fatalf("push = %+v, want off by default", got.Push)
			}
			got.Push = Channel{}
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestParseEvent(t *testing.T) {
	for _, e := range Events {
		if got, ok := ParseEvent(string(e)); !ok || got != e {
			t.Errorf("ParseEvent(%q) = %q, %v", e, got, ok)
		}
	}
	for _, bad := range []string{"", "Assigned", "digest", "state-change"} {
		if _, ok := ParseEvent(bad); ok {
			t.Errorf("ParseEvent(%q) accepted", bad)
		}
	}
}

// Push (LAUN-197) is off for every event by default, and follows the same
// order as the other channels: team row, global row, default. The master email
// switch does not touch it.
func TestResolve_Push(t *testing.T) {
	teamA := uuid.New()
	g := func(ev Event, push *string) dbq.NotificationSetting {
		return dbq.NotificationSetting{Event: string(ev), Push: push}
	}
	tm := func(ev Event, push *string) dbq.NotificationSetting {
		return dbq.NotificationSetting{TeamID: &teamA, Event: string(ev), Push: push}
	}
	for _, e := range Events {
		if got := Resolve(DefaultUserPrefs, &teamA, e, nil).Push; got != (Channel{false, SourceDefault}) {
			t.Errorf("%s: default push = %+v, want off", e, got)
		}
	}
	rows := []dbq.NotificationSetting{g(EventComment, sp("on"))}
	if got := Resolve(DefaultUserPrefs, &teamA, EventComment, rows).Push; got != (Channel{true, SourceGlobal}) {
		t.Errorf("global on = %+v", got)
	}
	if got := Resolve(DefaultUserPrefs, &teamA, EventAssigned, rows).Push; got.On {
		t.Errorf("another event's row applied: %+v", got)
	}
	rows = append(rows, tm(EventComment, sp("off")))
	if got := Resolve(DefaultUserPrefs, &teamA, EventComment, rows).Push; got != (Channel{false, SourceTeam}) {
		t.Errorf("team off over global on = %+v", got)
	}
	if got := Resolve(UserPrefs{EmailEnabled: false}, nil, EventComment, rows).Push; got != (Channel{true, SourceGlobal}) {
		t.Errorf("the email master switch changed push: %+v", got)
	}
}
