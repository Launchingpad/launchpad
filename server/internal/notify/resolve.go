package notify

import (
	"github.com/google/uuid"

	"devops/internal/dbq"
)

// Event is a notification event kind (decisions-T13 #3). A new kind is a new
// CHECK value in a migration and a new entry here.
type Event string

// The v1 events, in the order the API lists them.
const (
	EventAssigned    Event = "assigned"
	EventMention     Event = "mention"
	EventComment     Event = "comment"
	EventStateChange Event = "state_change"
	EventNewIssue    Event = "new_issue"
	// EventProjectUpdateDue (LAUN-196): the lead of a project is reminded to
	// post an update. It is about a project, not an issue, and has no team level.
	EventProjectUpdateDue Event = "project_update_due"
)

// Events lists every event.
var Events = []Event{EventAssigned, EventMention, EventComment, EventStateChange, EventNewIssue, EventProjectUpdateDue}

// ParseEvent returns the event named s.
func ParseEvent(s string) (Event, bool) {
	for _, e := range Events {
		if string(e) == s {
			return e, true
		}
	}
	return "", false
}

// Source says where a resolved value comes from.
type Source string

// The three sources, in resolution order.
const (
	SourceTeam    Source = "team"
	SourceGlobal  Source = "global"
	SourceDefault Source = "default"
)

// Stored values of notification_settings.email / .inbox / .push.
const (
	valueOn  = "on"
	valueOff = "off"
)

// Defaults is the built-in default of one event (decisions-T13 #11). Push
// (LAUN-197) is off for every event: a person turns it on.
type Defaults struct{ Email, Inbox, Push bool }

// DefaultFor returns the built-in default of event. new_issue also needs a
// team follow; that is the consumer's rule, not a setting.
func DefaultFor(e Event) Defaults {
	switch e {
	case EventStateChange, EventNewIssue:
		return Defaults{Email: false, Inbox: true}
	default: // assigned, mention, comment, project_update_due
		return Defaults{Email: true, Inbox: true}
	}
}

// UserPrefs are the master switches of one user (notification_user_settings).
// No row means DefaultUserPrefs.
type UserPrefs struct {
	EmailEnabled   bool
	IncludeExcerpt bool
}

// DefaultUserPrefs is a user with no row.
var DefaultUserPrefs = UserPrefs{EmailEnabled: true, IncludeExcerpt: false}

// Channel is one resolved channel: its value and where it came from. When the
// master email switch is off, the email value is off and Source stays the
// source of the stored or default value (the view shows emailEnabled beside it).
type Channel struct {
	On     bool
	Source Source
}

// Resolved is the result of Resolve for one event.
type Resolved struct {
	Email Channel
	Inbox Channel
	Push  Channel
}

// Resolve decides one event for one user at one team (nil: the global level):
// per channel, the team row when its column is not NULL, else the global row
// when its column is not NULL, else the built-in default. The master switch
// (user.EmailEnabled = false) then turns email off. rows are the user's
// notification_settings rows; rows of other events or other teams are ignored.
// It is pure: the GET view and the consumer both call it (decisions-T13 #33).
func Resolve(user UserPrefs, team *uuid.UUID, event Event, rows []dbq.NotificationSetting) Resolved {
	var global, perTeam *dbq.NotificationSetting
	for i := range rows {
		r := &rows[i]
		if r.Event != string(event) {
			continue
		}
		switch {
		case r.TeamID == nil:
			global = r
		case team != nil && *r.TeamID == *team:
			perTeam = r
		}
	}
	pick := func(col func(*dbq.NotificationSetting) *string, def bool) Channel {
		for _, c := range []struct {
			row *dbq.NotificationSetting
			src Source
		}{{perTeam, SourceTeam}, {global, SourceGlobal}} {
			if c.row == nil {
				continue
			}
			if v := col(c.row); v != nil {
				return Channel{On: *v == valueOn, Source: c.src}
			}
		}
		return Channel{On: def, Source: SourceDefault}
	}
	d := DefaultFor(event)
	out := Resolved{
		Email: pick(func(r *dbq.NotificationSetting) *string { return r.Email }, d.Email),
		Inbox: pick(func(r *dbq.NotificationSetting) *string { return r.Inbox }, d.Inbox),
		Push:  pick(func(r *dbq.NotificationSetting) *string { return r.Push }, d.Push),
	}
	if !user.EmailEnabled {
		out.Email.On = false
	}
	return out
}
