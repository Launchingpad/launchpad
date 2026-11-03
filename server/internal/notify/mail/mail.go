// Package mail builds the notification email (LAUN-34,
// docs/internal/decisions-T13-notifications.md #24): one subject, one plain
// text body and one HTML body from the changes of one issue. It is pure: it
// reads no database and sends nothing, so golden files pin its output.
//
// A mail holds the issue's identifier and title, its team and current
// state, the actor and a link. It holds no description and no comment text
// unless the caller passes an Excerpt (the person's include_excerpt
// setting).
package mail

import (
	"bytes"
	"embed"
	"fmt"
	htmltemplate "html/template"
	"strings"
	texttemplate "text/template"
	"unicode/utf8"
)

//go:embed templates/*
var files embed.FS

// MaxTitle is the longest title in a subject or body, in characters.
const MaxTitle = 100

// MaxExcerpt is the longest excerpt, in characters.
const MaxExcerpt = 200

// The kinds (they equal notify.Event; this package does not import notify).
const (
	KindAssigned    = "assigned"
	KindMention     = "mention"
	KindComment     = "comment"
	KindStateChange = "state_change"
	KindNewIssue    = "new_issue"
)

// Item is one change in the mail.
type Item struct {
	Kind      string
	Actor     string // display name; empty: unknown (GitLab, a system move)
	FromState string // state_change only; empty when unknown
	ToState   string
	Excerpt   string // empty unless the person asked for excerpts
}

// Input is everything a mail needs. Items are in the order to show. Primary
// is the kind that picks the subject and the footer reason (empty: the first
// item's kind).
type Input struct {
	Primary        string
	Identifier     string
	Title          string
	TeamKey        string
	TeamName       string
	StateName      string
	IssueURL       string
	SettingsURL    string
	UnsubscribeURL string // the web page of the one-click link
	Items          []Item
	// NotEmailed is how many updates the rate cap kept out of earlier mails.
	NotEmailed int
}

// Rendered is the result.
type Rendered struct {
	Subject string
	Text    string
	HTML    string
}

type view struct {
	Identifier, Title, TeamKey, StateName string
	IssueURL, SettingsURL, UnsubscribeURL string
	Lines                                 []string
	Excerpts                              []string
	Why                                   string
	More                                  string
}

var (
	textTmpl = texttemplate.Must(texttemplate.ParseFS(files, "templates/mail.txt.tmpl"))
	htmlTmpl = htmltemplate.Must(htmltemplate.ParseFS(files, "templates/mail.html.tmpl"))
)

// OneLine removes control characters (a new line above all) and collapses
// runs of spaces, so a value can never start a new header or a new line of
// the body. It is the notification's `oneLine`.
func OneLine(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == ' ' || r == ' ' {
			r = ' '
		}
		if r == ' ' {
			if space {
				continue
			}
			space = true
		} else {
			space = false
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// Cut shortens s to max characters, with "..." at the end when it cut.
func Cut(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:max-3])) + "..."
}

func shortReason(kind string) string {
	switch kind {
	case KindAssigned:
		return "Assigned to you"
	case KindMention:
		return "You were mentioned"
	case KindComment:
		return "New comment"
	case KindStateChange:
		return "State changed"
	case KindNewIssue:
		return "New issue"
	}
	return "Update"
}

func why(kind, team string) string {
	switch kind {
	case KindAssigned:
		return "this issue was assigned to you"
	case KindMention:
		return "you were mentioned"
	case KindNewIssue:
		return "you follow the team " + team
	default:
		return "you are the creator, the assignee or a commenter of this issue, or you subscribed to it"
	}
}

func withActor(s, actor string) string {
	if actor == "" {
		return s
	}
	return s + " (" + actor + ")"
}

func line(it Item, team string) string {
	actor := OneLine(it.Actor)
	switch it.Kind {
	case KindAssigned:
		if actor == "" {
			return "This issue was assigned to you"
		}
		return actor + " assigned this to you"
	case KindMention:
		if actor == "" {
			return "You were mentioned"
		}
		return actor + " mentioned you"
	case KindComment:
		return withActor("New comment", actor)
	case KindStateChange:
		from, to := OneLine(it.FromState), OneLine(it.ToState)
		s := "State changed"
		if from != "" && to != "" {
			s = "State changed: " + from + " to " + to
		}
		return withActor(s, actor)
	case KindNewIssue:
		return withActor("New issue in team "+team, actor)
	}
	return withActor("Update", actor)
}

// Build renders the mail. It returns an error only for an input with no
// items.
func Build(in Input) (Rendered, error) {
	if len(in.Items) == 0 {
		return Rendered{}, fmt.Errorf("mail: no items")
	}
	primary := in.Primary
	if primary == "" {
		primary = in.Items[0].Kind
	}
	title := Cut(OneLine(in.Title), MaxTitle)
	team := OneLine(in.TeamKey)
	v := view{
		Identifier: OneLine(in.Identifier), Title: title, TeamKey: team, StateName: OneLine(in.StateName),
		IssueURL: in.IssueURL, SettingsURL: in.SettingsURL, UnsubscribeURL: in.UnsubscribeURL,
		Why: why(primary, team),
	}
	for _, it := range in.Items {
		v.Lines = append(v.Lines, line(it, team))
		if ex := Cut(OneLine(it.Excerpt), MaxExcerpt); ex != "" {
			v.Excerpts = append(v.Excerpts, ex)
		}
	}
	switch {
	case in.NotEmailed == 1:
		v.More = "1 more update was not emailed. See your inbox."
	case in.NotEmailed > 1:
		v.More = fmt.Sprintf("%d more updates were not emailed. See your inbox.", in.NotEmailed)
	}
	var out Rendered
	out.Subject = "[" + v.Identifier + "] " + shortReason(primary) + ": " + title
	var tb, hb bytes.Buffer
	if err := textTmpl.Execute(&tb, v); err != nil {
		return Rendered{}, fmt.Errorf("mail: text template: %w", err)
	}
	if err := htmlTmpl.Execute(&hb, v); err != nil {
		return Rendered{}, fmt.Errorf("mail: html template: %w", err)
	}
	out.Text, out.HTML = tb.String(), hb.String()
	return out, nil
}

// KindProjectUpdateDue is the reminder to the lead of a project (LAUN-196).
const KindProjectUpdateDue = "project_update_due"

// ProjectDueInput is everything the reminder mail needs. It holds the project
// name and links only: no update text, no address.
type ProjectDueInput struct {
	ProjectName    string
	ProjectURL     string
	SettingsURL    string
	UnsubscribeURL string
	// NotEmailed is how many updates the rate cap kept out of earlier mails.
	NotEmailed int
}

// BuildProjectDue renders the reminder mail of one project. It is plain on
// purpose: one subject, one short text and the same text as HTML.
func BuildProjectDue(in ProjectDueInput) Rendered {
	name := Cut(OneLine(in.ProjectName), MaxTitle)
	more := ""
	switch {
	case in.NotEmailed == 1:
		more = "1 more update was not emailed. See your inbox."
	case in.NotEmailed > 1:
		more = fmt.Sprintf("%d more updates were not emailed. See your inbox.", in.NotEmailed)
	}
	text := "A project update is due for " + name + ".\n\n" +
		"Post an update: " + in.ProjectURL + "\n\n" + moreText(more) +
		"--\nYou get this email because you lead this project and it has an update schedule.\n" +
		"Notification settings: " + in.SettingsURL + "\n" +
		"Stop this kind of email: " + in.UnsubscribeURL + "\n"
	esc := htmltemplate.HTMLEscapeString
	html := "<p>A project update is due for <strong>" + esc(name) + "</strong>.</p>\n" +
		"<p><a href=\"" + esc(in.ProjectURL) + "\">Post an update</a></p>\n" + moreHTML(more) +
		"<hr>\n<p style=\"color: #555; font-size: 12px;\">You get this email because you lead this project and it has an update schedule.<br>\n" +
		"<a href=\"" + esc(in.SettingsURL) + "\">Notification settings</a> &middot; " +
		"<a href=\"" + esc(in.UnsubscribeURL) + "\">Stop this kind of email</a></p>\n"
	return Rendered{Subject: "Project update due: " + name, Text: text, HTML: html}
}

func moreText(m string) string {
	if m == "" {
		return ""
	}
	return m + "\n\n"
}

func moreHTML(m string) string {
	if m == "" {
		return ""
	}
	return "<p>" + htmltemplate.HTMLEscapeString(m) + "</p>\n"
}
