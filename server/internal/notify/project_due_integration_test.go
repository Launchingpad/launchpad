package notify_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/events"
	"devops/internal/notify"
	"devops/internal/tracker"
)

// LAUN-196 part B: the project update reminder, from the job to the inbox and
// the mail. Every time is injected; nothing reads the wall clock.

func utc(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

func itoa(i int) *int       { return &i }
func sptr(s string) *string { return &s }

// project makes a project led by lead (a username, or "") and, when freq is
// not empty, schedules it at clock `at`: weekday 5 (Friday), hour 9.
func (w *world) project(name, lead, freq string, at time.Time) tracker.Project {
	w.t.Helper()
	w.svc.SetClock(func() time.Time { return at })
	p, err := w.svc.CreateProject(context.Background(), w.org, w.user["chen"], tracker.CreateProjectInput{Name: name, Lead: lead})
	if err != nil {
		w.t.Fatalf("CreateProject: %v", err)
	}
	if freq != "" {
		w.schedule(p.ID, freq, 5, 9)
	}
	return p
}

func (w *world) schedule(id uuid.UUID, freq string, weekday, hour int) {
	w.t.Helper()
	_, err := w.svc.UpdateProject(context.Background(), w.org, w.user["chen"], id, tracker.UpdateProjectInput{
		UpdateSchedule: &tracker.UpdateScheduleInput{Frequency: &freq, Weekday: &weekday, Hour: &hour}})
	if err != nil {
		w.t.Fatalf("UpdateProject schedule: %v", err)
	}
}

func (w *world) job(now time.Time) tracker.ReminderRun {
	w.t.Helper()
	r, err := w.svc.RemindProjectUpdates(context.Background(), now)
	if err != nil {
		w.t.Fatalf("RemindProjectUpdates: %v", err)
	}
	return r
}

func (w *world) dueEvents() int {
	w.t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE type = $1`, events.ProjectUpdateDue).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *world) postUpdate(p tracker.Project, at time.Time) {
	w.t.Helper()
	u, err := w.svc.CreateProjectUpdate(context.Background(), w.org, w.user["chen"], p.ID, tracker.CreateProjectUpdateInput{Health: "on_track", Body: "x"})
	if err != nil {
		w.t.Fatal(err)
	}
	w.exec(`UPDATE project_updates SET created_at = $2 WHERE id = $1`, u.ID, at)
}

func TestProjectDue_OneReminderPerDueTime_RestartSafe(t *testing.T) {
	w := newWorld(t)
	// Set on Wed 2026-10-28: the first due time is Fri 10-30 09:00 UTC (the org zone is UTC).
	p := w.project("Launch", "alice", "weekly", utc(2026, 10, 28, 12, 0))

	if r := w.job(utc(2026, 10, 30, 8, 59)); r.Reminded != 0 || w.dueEvents() != 0 {
		t.Fatalf("before the due time: %+v", r)
	}
	if r := w.job(utc(2026, 10, 30, 9, 0)); r.Reminded != 1 || r.Due != 1 {
		t.Fatalf("at the due time: %+v", r)
	}
	// Again, later the same day, and from a fresh Service (a restart or a second instance): nothing.
	if r := w.job(utc(2026, 10, 30, 9, 0)); r.Reminded != 0 {
		t.Fatalf("repeat: %+v", r)
	}
	if r := tracker.New(w.pool); r != nil {
		got, err := r.RemindProjectUpdates(context.Background(), utc(2026, 10, 30, 15, 0))
		if err != nil || got.Reminded != 0 {
			t.Fatalf("restart: %+v %v", got, err)
		}
	}
	if w.dueEvents() != 1 {
		t.Fatalf("events = %d, want 1", w.dueEvents())
	}
	// The consumer makes one notification for the lead: inbox and email by default.
	w.run()
	w.wantRows("alice:project_update_due")
	if rs := w.rows(w.org.ID); !rs[0].Inbox || !rs[0].Email {
		t.Fatalf("channels %+v", rs)
	}
	w.run() // a second consumer pass makes nothing
	w.wantRows("alice:project_update_due")

	// Next week is a new due time.
	if r := w.job(utc(2026, 11, 6, 9, 30)); r.Reminded != 1 {
		t.Fatalf("next week: %+v", r)
	}
	// The body carries ids and the due time only.
	var data string
	if err := w.pool.QueryRow(context.Background(), `SELECT data::text FROM notifications ORDER BY created_at LIMIT 1`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(data, "Launch") || !strings.Contains(data, "2026-10-30T09:00:00Z") {
		t.Fatalf("data %s", data)
	}
	_ = p
}

func TestProjectDue_AnUpdateInTheWindowSilencesTheReminder(t *testing.T) {
	w := newWorld(t)
	p := w.project("Launch", "alice", "weekly", utc(2026, 10, 28, 12, 0))
	w.job(utc(2026, 10, 30, 9, 0)) // reminder 1

	// An update on Tue 11-03 is inside the window before Fri 11-06: no reminder.
	w.postUpdate(p, utc(2026, 11, 3, 10, 0))
	if r := w.job(utc(2026, 11, 6, 9, 0)); r.Reminded != 0 || r.Due != 1 {
		t.Fatalf("with an update: %+v", r)
	}
	// An update BEFORE the previous due time (Fri 11-06) does not count for 11-13.
	if r := w.job(utc(2026, 11, 13, 9, 0)); r.Reminded != 1 {
		t.Fatalf("stale update: %+v", r)
	}
	// An update at exactly the previous due time counts for the next window.
	w.postUpdate(p, utc(2026, 11, 13, 9, 0))
	if r := w.job(utc(2026, 11, 20, 9, 0)); r.Reminded != 0 {
		t.Fatalf("update at the previous due time: %+v", r)
	}
}

func TestProjectDue_LeadChangeAndNoLeadAndAgentAndDeleted(t *testing.T) {
	w := newWorld(t)
	at := utc(2026, 10, 28, 12, 0)
	p := w.project("Led", "alice", "weekly", at)
	noLead := w.project("Lone", "", "weekly", at)
	agent := w.project("Bot", "claude", "weekly", at)
	gone := w.project("Gone", "alice", "weekly", at)
	// A lead change before the due time: the new lead is reminded, the old one is not.
	w.svc.SetClock(func() time.Time { return at })
	if _, err := w.svc.UpdateProject(context.Background(), w.org, w.user["chen"], p.ID, tracker.UpdateProjectInput{Lead: str("bob")}); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.DeleteProject(context.Background(), w.org, w.user["chen"], gone.ID); err != nil {
		t.Fatal(err)
	}
	r := w.job(utc(2026, 10, 30, 9, 0))
	if r.Reminded != 1 {
		t.Fatalf("run %+v, want one reminder (the led project)", r)
	}
	w.run()
	w.wantRows("bob:project_update_due")

	// An agent lead gets nothing even if an event names it (defence in depth).
	ev := events.Event{Type: events.ProjectUpdateDue, EntityType: "project", EntityID: agent.ID,
		Payload: map[string]any{"projectId": agent.ID, "leadId": w.user["claude"].UserID, "dueAt": "2026-10-30T09:00:00Z"}}
	if _, err := events.Emit(context.Background(), w.q, w.org.ID, ev, events.ScopeOrg()); err != nil {
		t.Fatal(err)
	}
	w.run()
	w.wantRows("bob:project_update_due")
	_ = noLead
}

func TestProjectDue_SettingsDecideTheChannels(t *testing.T) {
	w := newWorld(t)
	p := w.project("Launch", "alice", "weekly", utc(2026, 10, 28, 12, 0))
	alice := w.user["alice"].UserID

	// Inbox off, email on: an email-only row.
	w.exec(`INSERT INTO notification_settings (org_id, user_id, event, email, inbox) VALUES ($1, $2, 'project_update_due', 'on', 'off')`, w.org.ID, alice)
	w.job(utc(2026, 10, 30, 9, 0))
	w.run()
	if rs := w.rows(w.org.ID); len(rs) != 1 || rs[0].Inbox || !rs[0].Email {
		t.Fatalf("rows %v", rs)
	}
	// Both off: nothing.
	w.clear()
	w.exec(`UPDATE notification_settings SET email = 'off' WHERE user_id = $1`, alice)
	w.job(utc(2026, 11, 6, 9, 0))
	w.run()
	if rs := w.rows(w.org.ID); len(rs) != 0 {
		t.Fatalf("rows %v, want none", rs)
	}
	// The master email switch: inbox only.
	w.exec(`DELETE FROM notification_settings`)
	w.exec(`INSERT INTO notification_user_settings (org_id, user_id, email_enabled) VALUES ($1, $2, false)`, w.org.ID, alice)
	w.job(utc(2026, 11, 13, 9, 0))
	w.run()
	if rs := w.rows(w.org.ID); len(rs) != 1 || !rs[0].Inbox || rs[0].Email {
		t.Fatalf("rows %v", rs)
	}
	_ = p
}

func TestProjectDue_InboxItemAndMail(t *testing.T) {
	sw := newSWorld(t)
	p := sw.project("Launch <b>", "alice", "weekly", utc(2026, 10, 28, 12, 0))
	sw.job(utc(2026, 10, 30, 9, 0))
	res := sw.send()
	if res.Mails != 1 || res.Sent != 1 {
		t.Fatalf("send %+v", res)
	}
	msgs := sw.m.sent()
	if len(msgs) != 1 || msgs[0].To != "alice@example.test" {
		t.Fatalf("mail %+v", msgs)
	}
	m := msgs[0]
	if m.Subject != "Project update due: Launch <b>" || !strings.Contains(m.Text, testBase+"/"+"orbit"+"/projects/"+p.ID.String()) ||
		strings.Contains(m.HTML, "<b>") || !strings.Contains(m.HTML, "Launch &lt;b&gt;") ||
		!strings.Contains(m.Headers["List-Unsubscribe"], "/api/v1/notifications/unsubscribe/") {
		t.Fatalf("mail %+v", m)
	}
	// The inbox shows a project item, no issue.
	page, err := notify.New(sw.pool).ListInbox(context.Background(), sw.org, sw.user["alice"].UserID, notify.InboxQuery{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("inbox %+v %v", page, err)
	}
	it := page.Items[0]
	if it.Kind != "project_update_due" || it.Issue != nil || it.Project == nil || it.Project.ID != p.ID || it.Project.Name != "Launch <b>" || it.Actor != nil {
		t.Fatalf("item %+v", it)
	}
	// A deleted project removes the notification (cascade): the inbox is empty.
	if err := sw.svc.DeleteProject(context.Background(), sw.org, sw.user["chen"], p.ID); err != nil {
		t.Fatal(err)
	}
	page, err = notify.New(sw.pool).ListInbox(context.Background(), sw.org, sw.user["alice"].UserID, notify.InboxQuery{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("inbox after delete %+v %v", page, err)
	}
}

func TestProjectDue_TimeZoneAndDST(t *testing.T) {
	w := newWorld(t)
	w.exec(`UPDATE orgs SET time_zone = 'America/New_York' WHERE id = $1`, w.org.ID)
	w.project("NY", "alice", "weekly", utc(2026, 10, 28, 12, 0))
	// Fri 09:00 New York: 13:00 UTC while DST runs (until Sun 11-01) ...
	if r := w.job(utc(2026, 10, 30, 12, 59)); r.Reminded != 0 {
		t.Fatalf("one minute early: %+v", r)
	}
	if r := w.job(utc(2026, 10, 30, 13, 0)); r.Reminded != 1 {
		t.Fatalf("EDT due time: %+v", r)
	}
	// ... and 14:00 UTC after the clocks go back: the local hour stays 09:00.
	if r := w.job(utc(2026, 11, 6, 13, 59)); r.Reminded != 0 {
		t.Fatalf("EST one minute early: %+v", r)
	}
	if r := w.job(utc(2026, 11, 6, 14, 0)); r.Reminded != 1 {
		t.Fatalf("EST due time: %+v", r)
	}
}

func TestProjectDue_SchedulingNeverRemindsForThePast(t *testing.T) {
	w := newWorld(t)
	// Scheduled on Sat 10-31: the due time of Fri 10-30 is past and must not fire.
	w.project("Late", "alice", "weekly", utc(2026, 10, 31, 12, 0))
	if r := w.job(utc(2026, 10, 31, 12, 30)); r.Reminded != 0 || w.dueEvents() != 0 {
		t.Fatalf("%+v", r)
	}
	// Turning it off and on again restarts from now.
	var id uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `SELECT id FROM projects`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	w.schedule(id, "none", 5, 9)
	if r := w.job(utc(2026, 11, 6, 9, 0)); r.Projects != 0 {
		t.Fatalf("none is not scheduled: %+v", r)
	}
	// An archived or completed project is not reminded.
	w.schedule(id, "weekly", 5, 9)
	w.exec(`UPDATE projects p SET state = 'completed',
		status_id = (SELECT s.id FROM project_statuses s WHERE s.org_id = p.org_id AND s.kind = 'completed' LIMIT 1)
		WHERE p.id = $1`, id)
	if r := w.job(utc(2026, 11, 13, 9, 0)); r.Reminded != 0 {
		t.Fatalf("completed: %+v", r)
	}
}

func TestProjectDue_NoTeamLevel(t *testing.T) {
	w := newWorld(t)
	st := notify.New(w.pool)
	team := w.teamID()
	_, err := st.UpdateTeam(context.Background(), w.org, w.user["alice"].UserID, team, notify.TeamSettingsInput{
		Events: map[string]notify.EventInput{"project_update_due": {}}})
	if err == nil {
		t.Fatal("a team level for project_update_due was accepted")
	}
	v, err := st.View(context.Background(), w.org, w.user["alice"].UserID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range v.Events {
		if e.Event == "project_update_due" {
			if len(e.Teams) != 0 || e.Default.Email != "on" || e.Default.Inbox != "on" || e.RequiresFollow {
				t.Fatalf("event view %+v", e)
			}
			return
		}
	}
	t.Fatal("project_update_due is not in the settings view")
}

// Four instances running the job at once over many projects: one event per
// project, never more (the FOR UPDATE on the project row).
func TestProjectDue_ConcurrentJobsRemindOncePerProject(t *testing.T) {
	w := newWorld(t)
	const n = 30
	at := utc(2026, 10, 28, 12, 0)
	for i := 0; i < n; i++ {
		w.project(fmt.Sprintf("P%02d", i), "alice", "weekly", at)
	}
	now := utc(2026, 10, 30, 9, 0)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc := tracker.New(w.pool)
			if _, err := svc.RemindProjectUpdates(context.Background(), now); err != nil {
				t.Errorf("job: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := w.dueEvents(); got != n {
		t.Fatalf("events = %d, want %d", got, n)
	}
}

func TestProjectDue_SkipsArchivedDeactivatedAndRemovedLeads(t *testing.T) {
	w := newWorld(t)
	at := utc(2026, 10, 28, 12, 0)
	w.project("Archived", "alice", "weekly", at)
	w.project("Deactivated", "bob", "weekly", at)
	w.project("Removed", "carol", "weekly", at)
	w.project("Fine", "alice", "weekly", at)
	w.exec(`UPDATE projects SET archived_at = now() WHERE name = 'Archived'`)
	w.exec(`UPDATE users SET deactivated_at = now() WHERE username = 'bob'`)
	w.exec(`DELETE FROM org_members WHERE user_id = $1`, w.user["carol"].UserID)
	if r := w.job(utc(2026, 10, 30, 9, 0)); r.Reminded != 1 {
		t.Fatalf("run %+v, want only the fine project", r)
	}
	w.run()
	w.wantRows("alice:project_update_due")
}
