package backupnotice

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// clean makes a value safe for a mail subject or line: control characters
// (a CR or LF would end a header) become spaces and the length is capped.
func clean(s string, maxRunes int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= maxRunes {
			break
		}
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			r = ' '
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

func when(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

var (
	codeRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	hex12  = regexp.MustCompile(`^[0-9a-f]{12}$`)
	hex64  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// PageLink is the address of the Backups page: <base>/<org slug>/settings/platform
// (the web route /:org/settings/platform, web/src/App.tsx). It is empty without a
// base URL or with a slug that is not a plain slug, and the mail then has no link (#41).
func PageLink(base, orgSlug string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || !slugRE.MatchString(orgSlug) {
		return ""
	}
	return base + "/" + orgSlug + "/settings/platform"
}

// pageLine is the last line of a mail: the link, or a plain pointer without one.
func pageLine(link string) string {
	if link == "" {
		return "Open Settings, Platform, Backups in Launchpad to look."
	}
	return "Open the Backups page: " + link
}

// RunNotice is the data of one run mail.
type RunNotice struct {
	DestinationName string
	Trigger         string
	Status          string // succeeded | failed
	FinishedAt      time.Time
	Bytes           *int64
	ErrorCode       string
	ErrorText       string
}

// RunMail builds the subject and plain text of a run notice (#41).
func RunMail(n RunNotice, link string) (subject, text string) {
	name := clean(n.DestinationName, 60)
	if name == "" {
		name = "(unnamed)"
	}
	var b strings.Builder
	switch n.Status {
	case "succeeded":
		subject = "Launchpad backup succeeded: " + name
		fmt.Fprintf(&b, "A backup run succeeded.\n\nDestination: %s\nKind of run: %s\nFinished: %s\n", name, clean(n.Trigger, 20), when(n.FinishedAt))
		if n.Bytes != nil {
			fmt.Fprintf(&b, "Size sent: %d bytes\n", *n.Bytes)
		}
	default:
		subject = "Launchpad backup failed: " + name
		fmt.Fprintf(&b, "A backup run failed.\n\nDestination: %s\nKind of run: %s\nFinished: %s\n", name, clean(n.Trigger, 20), when(n.FinishedAt))
		if codeRE.MatchString(n.ErrorCode) {
			fmt.Fprintf(&b, "Code: %s\n", n.ErrorCode)
		}
		if t := clean(n.ErrorText, 200); t != "" {
			fmt.Fprintf(&b, "Detail: %s\n", t)
		}
	}
	b.WriteString("\n" + pageLine(link) + "\n")
	return subject, b.String()
}

// StaleNotice is the data of one stale alert.
type StaleNotice struct {
	DestinationName string
	LastSuccess     *time.Time
	Since           time.Time
	Threshold       time.Duration
	Now             time.Time
}

// StaleMail builds the stale alert (#41).
func StaleMail(n StaleNotice, link string) (subject, text string) {
	name := clean(n.DestinationName, 60)
	if name == "" {
		name = "(unnamed)"
	}
	subject = "Launchpad backup is overdue: " + name
	var b strings.Builder
	fmt.Fprintf(&b, "No backup to this destination has succeeded for longer than %d hours.\n\nDestination: %s\n", int(n.Threshold.Hours()), name)
	if n.LastSuccess != nil {
		fmt.Fprintf(&b, "Last success: %s\n", when(*n.LastSuccess))
	} else {
		fmt.Fprintf(&b, "Last success: none since %s\n", when(n.Since))
	}
	b.WriteString("\nThis alert is sent at most once every 24 hours for each destination.\n")
	b.WriteString("\n" + pageLine(link) + "\n")
	return subject, b.String()
}

// changeText: the sentence of each change-log kind. A kind not in the table gets
// a generic sentence (and its name, when it is a plain identifier).
var changeText = map[string]string{
	"destination_added":             "A backup destination was added.",
	"destination_deleted":           "A backup destination was deleted.",
	"destination_renamed":           "A backup destination was renamed.",
	"destination_config_changed":    "The connection settings of a backup destination changed.",
	"destination_secrets_changed":   "The stored credentials of a backup destination changed.",
	"destination_schedule_changed":  "The schedule of a backup destination changed.",
	"destination_retention_changed": "The retention or verify setting of a backup destination changed.",
	"destination_enabled":           "A backup destination was enabled.",
	"destination_disabled":          "A backup destination was disabled.",
	"destination_pin_set":           "A TLS pin was saved for a backup destination.",
	"destination_host_key_set":      "An SSH host key was saved for a backup destination.",
	"notice_settings_changed":       "The backup notice settings changed.",
	"recipient_added":               "An age recipient key was added. After the first key, a new key is held for 24 hours before it is used.",
	"recipient_removal_scheduled":   "The removal of an age recipient key was scheduled. It takes effect after 24 hours.",
	"recipient_add_cancelled":       "A pending age recipient key was cancelled.",
	"recipient_removal_cancelled":   "A scheduled removal of an age recipient key was cancelled.",
	"recipient_removed_by_host":     "An age recipient key was removed at once by a command on the host.",
	"recipient_cancelled_by_host":   "A pending age recipient change was cancelled by a command on the host.",
	"signing_key_replaced":          "The backup signing key was replaced.",
	"instance_resumed":              "Backups were resumed on this install with a new install id, by a command on the host.",
}

// KnownChangeKind reports whether a change-log kind has its own sentence.
func KnownChangeKind(kind string) bool { _, ok := changeText[kind]; return ok }

// ChangeNotice is the data of one change mail.
type ChangeNotice struct {
	Kind            string
	At              time.Time
	DestinationName string
	Actor           string // username; "" for a command on the host
	Detail          json.RawMessage
}

// ChangeMail builds the notice of a change-log row (#27, #41). Only a whitelist
// of the row's detail is used: a fingerprint (public) and the notice settings.
func ChangeMail(n ChangeNotice, link string) (subject, text string) {
	sentence, ok := changeText[n.Kind]
	if !ok {
		sentence = "A backup setting changed."
		if codeRE.MatchString(n.Kind) {
			sentence = "A backup setting changed (" + n.Kind + ")."
		}
	}
	subject = "Launchpad backup setting changed"
	if name := clean(n.DestinationName, 60); name != "" {
		subject += ": " + name
	}
	var b strings.Builder
	b.WriteString(sentence + "\n\n")
	if name := clean(n.DestinationName, 60); name != "" {
		fmt.Fprintf(&b, "Destination: %s\n", name)
	}
	if a := clean(n.Actor, 80); a != "" {
		fmt.Fprintf(&b, "Changed by: %s\n", a)
	} else {
		b.WriteString("Changed by: a command on the host, or an account that no longer exists\n")
	}
	fmt.Fprintf(&b, "At: %s\n", when(n.At))
	var d map[string]json.RawMessage
	if json.Unmarshal(n.Detail, &d) == nil {
		for _, k := range []string{"fingerprint", "newKeyFingerprint"} {
			var s string
			if json.Unmarshal(d[k], &s) == nil {
				if hex64.MatchString(s) {
					s = s[:12]
				}
				if hex12.MatchString(s) {
					fmt.Fprintf(&b, "Key fingerprint: %s\n", s)
				}
			}
		}
		if n.Kind == "notice_settings_changed" {
			b.WriteString(noticeDiff(d))
		}
	}
	b.WriteString("\nIf you did not expect this change, open the Backups page and review it now.\n")
	b.WriteString(pageLine(link) + "\n")
	return subject, b.String()
}

// noticeDiff prints the old and new notice settings (booleans and a number).
func noticeDiff(d map[string]json.RawMessage) string {
	type vals struct {
		NotifyOnFailure *bool `json:"notifyOnFailure"`
		NotifyOnSuccess *bool `json:"notifyOnSuccess"`
		StaleAlertHours *int  `json:"staleAlertHours"`
	}
	var from, to vals
	if json.Unmarshal(d["from"], &from) != nil || json.Unmarshal(d["to"], &to) != nil {
		return ""
	}
	line := func(label string, a, b any) string { return fmt.Sprintf("%s: %v -> %v\n", label, a, b) }
	var out string
	if from.NotifyOnFailure != nil && to.NotifyOnFailure != nil {
		out += line("Mail on failure", *from.NotifyOnFailure, *to.NotifyOnFailure)
	}
	if from.NotifyOnSuccess != nil && to.NotifyOnSuccess != nil {
		out += line("Mail on success", *from.NotifyOnSuccess, *to.NotifyOnSuccess)
	}
	if from.StaleAlertHours != nil && to.StaleAlertHours != nil {
		out += line("Stale alert after (hours, 0 is off)", *from.StaleAlertHours, *to.StaleAlertHours)
	}
	return out
}
