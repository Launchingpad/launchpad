package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"devops/internal/netguard"
)

// The chat services (LAUN-204, docs/internal/decisions-T36-chat-services.md;
// the Slack part is LAUN-198, decisions-T33). This file is the one place that
// talks to a chat service: the webhook URL checks and the POST. The POST goes
// through a *http.Client built by netguard.NewClient (the dial-time guard, no
// redirect, no proxy). Nothing here logs or returns the URL: an error is a
// fixed word (ChatError).

// The kinds of chat service.
const (
	ChatSlack   = "slack"
	ChatDiscord = "discord"
	ChatCustom  = "custom"
	ChatFluxer  = "fluxer"
)

// ChatKinds lists the kinds in the order the admin page shows them.
var ChatKinds = []string{ChatSlack, ChatDiscord, ChatFluxer, ChatCustom}

// SlackHost is the only host a Slack webhook URL may name.
const SlackHost = "hooks.slack.com"

// DiscordHosts are the hosts a Discord webhook URL may name (documented forms
// are discord.com and the older discordapp.com).
var DiscordHosts = []string{"discord.com", "discordapp.com"}

// MaxWebhookBytes bounds the Slack and Discord webhook URL an admin may enter.
const MaxWebhookBytes = 256

// MaxCustomWebhookBytes bounds a custom webhook URL (the sealed blob stays
// under its CHECK of 2048 bytes).
const MaxCustomWebhookBytes = 1024

// MaxWebhookHostBytes bounds the host of a custom or Fluxer webhook (a DNS
// name is at most 253 bytes; the stored host column allows 255).
const MaxWebhookHostBytes = 253

// ChatPostTimeout bounds one post to a chat service.
const ChatPostTimeout = 10 * time.Second

// ChatTestText is the fixed sentence of the test message.
const ChatTestText = "Launchpad test message. This channel is connected."

// The fixed words of a failed post (the CHECK of chat_services.last_error and
// chat_outbox.last_error). They hold no part of the service's reply.
const (
	ChatRejected    = "chat_rejected"     // 4xx other than gone and rate limit: the payload or the token was refused
	ChatGone        = "chat_gone"         // 404, 410: the webhook was removed or the channel archived
	ChatRateLimited = "chat_rate_limited" // 429
	ChatUnavailable = "chat_unavailable"  // 5xx
	ChatUnreachable = "chat_unreachable"  // the connection or the TLS check failed
	ChatBlocked     = "chat_blocked"      // the dial guard, or the URL check, refused the host
	ChatRedirect    = "chat_redirect"     // a 3xx: never followed
	ChatUnreadable  = "chat_unreadable"   // the stored URL does not open with this server's key
)

// ChatError is a failed post. Error is the fixed word and nothing else, so the
// error is safe in a log line and in a response.
type ChatError struct {
	Word       string
	RetryAfter time.Duration // set for chat_rate_limited when the service said how long
}

func (e *ChatError) Error() string { return e.Word }

// permanent reports whether a retry cannot help: the webhook is gone or
// refused, or the host is blocked.
func (e *ChatError) permanent() bool {
	switch e.Word {
	case ChatGone, ChatRejected, ChatBlocked, ChatRedirect:
		return true
	}
	return false
}

var (
	// slackPath is the shape of an Incoming Webhook path: /services/T.../B.../token.
	slackPath = regexp.MustCompile(`^/services/[A-Za-z0-9_-]{1,64}/[A-Za-z0-9_-]{1,64}/[A-Za-z0-9_-]{1,64}$`)
	// discordPath is the documented shape: /api/webhooks/<id>/<token>.
	discordPath = regexp.MustCompile(`^/api/webhooks/[0-9]{1,24}/[A-Za-z0-9_-]{1,128}$`)
	// fluxerPath is the Discord shape, with or without the /v1 segment.
	fluxerPath = regexp.MustCompile(`^/api/(v1/)?webhooks/[0-9]{1,24}/[A-Za-z0-9_-]{1,128}$`)
)

func checkChars(raw string, max int) error {
	if raw == "" || len(raw) > max || !utf8.ValidString(raw) {
		return errors.New("must be a webhook URL of at most " + strconv.Itoa(max) + " bytes")
	}
	for _, r := range raw {
		if r <= ' ' || r == 0x7f {
			return errors.New("must not contain spaces or control characters")
		}
	}
	return nil
}

// ParseSlackWebhook checks a Slack webhook URL and returns it. The error names
// the problem and never the value. The rules: https, the exact host SlackHost,
// no port, no user info, no query or fragment, and a /services/ path of three
// plain segments. An exact allowlist is the first line; the dial guard is the
// second (it checks the resolved address).
func ParseSlackWebhook(raw string) (*url.URL, error) {
	if err := checkChars(raw, MaxWebhookBytes); err != nil {
		return nil, err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("must be a valid URL")
	}
	switch {
	case u.Scheme != "https":
		return nil, errors.New("must start with https://")
	case u.User != nil:
		return nil, errors.New("must not hold a user name or password")
	case u.Host != SlackHost: // exact: no port, no case change, no trailing dot
		return nil, errors.New("must be on " + SlackHost)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "":
		return nil, errors.New("must not have a query or fragment")
	case u.RawPath != "" || !slackPath.MatchString(u.Path):
		return nil, errors.New("must look like https://" + SlackHost + "/services/T.../B.../...")
	}
	return u, nil
}

// ParseDiscordWebhook checks a Discord webhook URL: https, host discord.com or
// discordapp.com exactly, no port, no user info, no query or fragment, and the
// path /api/webhooks/<id>/<token>. The token is in the path, so the whole URL
// is the secret.
func ParseDiscordWebhook(raw string) (*url.URL, error) {
	if err := checkChars(raw, MaxWebhookBytes); err != nil {
		return nil, err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("must be a valid URL")
	}
	hostOK := false
	for _, h := range DiscordHosts {
		hostOK = hostOK || u.Host == h
	}
	switch {
	case u.Scheme != "https":
		return nil, errors.New("must start with https://")
	case u.User != nil:
		return nil, errors.New("must not hold a user name or password")
	case !hostOK:
		return nil, errors.New("must be on discord.com")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "":
		return nil, errors.New("must not have a query or fragment")
	case u.RawPath != "" || !discordPath.MatchString(u.Path):
		return nil, errors.New("must look like https://discord.com/api/webhooks/<id>/<token>")
	}
	return u, nil
}

// ParseCustomWebhook checks a custom webhook URL. The host cannot be on an
// allowlist (the admin types it), so: https only, a host, no user info, no
// fragment, no encoded path; then the same save-time refusals as the push relay
// (CheckRelayHostLiteral: a private, loopback or link-local IP literal, an old
// numeric spelling, a local or internal name). The dial guard is the control
// that matters: a name that resolves to a private address passes here and is
// refused at dial time (chat_blocked). A query is allowed (some services keep
// the token there); the URL is secret as a whole.
func ParseCustomWebhook(raw string) (*url.URL, error) {
	if err := checkChars(raw, MaxCustomWebhookBytes); err != nil {
		return nil, err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("must be a valid URL")
	}
	switch {
	case u.Scheme != "https":
		return nil, errors.New("must start with https://")
	case u.User != nil:
		return nil, errors.New("must not hold a user name or password")
	case u.Hostname() == "":
		return nil, errors.New("must name a host")
	case len(u.Hostname()) > MaxWebhookHostBytes:
		return nil, errors.New("must name a host of at most 253 bytes")
	case u.Fragment != "" || u.Opaque != "":
		return nil, errors.New("must not have a fragment")
	case u.RawPath != "":
		return nil, errors.New("must not have an encoded path")
	}
	if err := CheckRelayHostLiteral(u); err != nil {
		return nil, err
	}
	return u, nil
}

// ParseFluxerWebhook checks a Fluxer webhook URL. Fluxer is self-hosted, so the
// host rules are those of a custom webhook (any public https host, refused at
// save time and at dial when private); on top of that the path must look like a
// webhook URL (/api/webhooks/<id>/<token> or /api/v1/webhooks/<id>/<token>)
// and there is no query.
func ParseFluxerWebhook(raw string) (*url.URL, error) {
	u, err := ParseCustomWebhook(raw)
	if err != nil {
		return nil, err
	}
	switch {
	case u.RawQuery != "" || u.ForceQuery:
		return nil, errors.New("must not have a query")
	case !fluxerPath.MatchString(u.Path):
		return nil, errors.New("must look like https://<host>/api/webhooks/<id>/<token>")
	}
	return u, nil
}

// ParseChatWebhook checks a webhook URL for a kind.
func ParseChatWebhook(kind, raw string) (*url.URL, error) {
	switch kind {
	case ChatSlack:
		return ParseSlackWebhook(raw)
	case ChatDiscord:
		return ParseDiscordWebhook(raw)
	case ChatFluxer:
		return ParseFluxerWebhook(raw)
	case ChatCustom:
		return ParseCustomWebhook(raw)
	}
	return nil, errors.New("unknown kind")
}

// ChatHost is the host name the admin page shows for a parsed URL (lower case,
// no port). Never the rest of the URL.
func ChatHost(u *url.URL) string { return strings.ToLower(u.Hostname()) }

// ChatPoster posts to a chat service. Build it with NewChatPoster.
type ChatPoster struct {
	client *http.Client
}

// NewChatPoster returns a poster on the guarded client. client must come from
// netguard.NewClient (a test may adjust its transport's dial address, but the
// Dialer stays in the path). A nil client posts nothing and returns
// chat_blocked.
func NewChatPoster(client *http.Client) *ChatPoster { return &ChatPoster{client: client} }

// ChatPolicy is the dial policy of the chat client: public addresses only (the
// zero Policy), the service names the git policy refuses, and the app's own
// subnets when dyn is given. No setting opens a private range here.
func ChatPolicy(dyn netguard.Dynamic) netguard.Policy {
	return netguard.Policy{Dynamic: dyn, Names: append([]string(nil), netguard.GitServiceNames...)}
}

// NewChatClient builds the guarded client of the server.
func NewChatClient(dyn netguard.Dynamic) *http.Client {
	return NewChatClientFor(&netguard.Dialer{Policy: ChatPolicy(dyn)})
}

// NewChatClientFor builds the client on a given dialer: netguard.NewClient (no
// redirect, no proxy, TLS 1.2 or newer). Tests pass a dialer with a fake resolver.
func NewChatClientFor(d *netguard.Dialer) *http.Client {
	return netguard.NewClient(d, ChatPostTimeout)
}

// EventTest is the Event of the test message.
const EventTest Event = "test"

// ChatMessage is what one post holds: an event kind (or EventTest), the issue
// identifier (empty for a project and for the test) and an absolute link. Never
// a title, a name or text of the issue.
//
// FromState and ToState (LAUN-214, decisions-T39) are the raw names of the old
// and the new state of a state_change; both or neither. They are org data an
// admin typed, so ChatText cleans and escapes them for each kind.
type ChatMessage struct {
	Event      Event
	Identifier string
	Link       string
	FromState  string
	ToState    string
	// Personal: the post goes to a member's own webhook (LAUN-229,
	// decisions-T40 #14): the sentence speaks to that member.
	Personal bool
}

// body makes the JSON body of a post for a kind.
func (m ChatMessage) body(kind string) ([]byte, error) {
	var v map[string]any
	text := ChatText(kind, m)
	switch kind {
	case ChatSlack:
		v = map[string]any{"text": text, "unfurl_links": false, "unfurl_media": false}
	case ChatDiscord, ChatFluxer: // Fluxer takes the Discord body
		// allowed_mentions with an empty parse list: no mention can ping anyone.
		v = map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []string{}}}
	default:
		v = map[string]any{"text": text, "kind": string(m.Event), "id": m.Identifier, "link": m.Link}
	}
	// No HTML escaping: Slack reads < and > in the text (it escapes & itself).
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Post sends m to the webhook. hook is the opened URL; it is checked again
// here for its kind, so a row written another way still cannot reach another
// host. The returned error is nil or a *ChatError.
func (p *ChatPoster) Post(ctx context.Context, kind, hook string, m ChatMessage) error {
	if p == nil || p.client == nil {
		return &ChatError{Word: ChatBlocked}
	}
	u, err := ParseChatWebhook(kind, hook)
	if err != nil {
		return &ChatError{Word: ChatBlocked}
	}
	body, err := m.body(kind)
	if err != nil {
		return &ChatError{Word: ChatRejected}
	}
	ctx, cancel := context.WithTimeout(ctx, ChatPostTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return &ChatError{Word: ChatBlocked}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		// err is a *url.Error that holds the URL: map it to a word, drop it.
		if errors.Is(err, netguard.ErrRefused) {
			return &ChatError{Word: ChatBlocked}
		}
		return &ChatError{Word: ChatUnreachable}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch c := resp.StatusCode; {
	case c >= 200 && c < 300: // Slack: 200. Discord: 204 without ?wait.
		return nil
	case c >= 300 && c < 400:
		return &ChatError{Word: ChatRedirect}
	case c == http.StatusTooManyRequests:
		return &ChatError{Word: ChatRateLimited, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	case c == http.StatusNotFound || c == http.StatusGone:
		return &ChatError{Word: ChatGone}
	case c >= 400 && c < 500:
		return &ChatError{Word: ChatRejected}
	default:
		return &ChatError{Word: ChatUnavailable}
	}
}

// retryAfter reads a Retry-After in seconds (a whole or a decimal number),
// capped at one hour. Anything else is 0.
func retryAfter(v string) time.Duration {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || math.IsNaN(f) || f <= 0 {
		return 0
	}
	// Compare in seconds BEFORE multiplying: a huge value overflows time.Duration.
	if f >= float64(time.Hour/time.Second) {
		return time.Hour
	}
	return time.Duration(math.Ceil(f*1000)) * time.Millisecond
}

// chatSentence is the fixed sentence of each event kind. It names no person
// and no title: a channel is shared, and notify stores no message body.
var chatSentence = map[Event]string{
	EventAssigned:         "Issue assigned",
	EventMention:          "Mention on an issue",
	EventComment:          "New comment on an issue",
	EventStateChange:      "Issue changed state",
	EventNewIssue:         "New issue",
	EventProjectUpdateDue: "Project update due",
}

// personalSentence is the fixed sentence of each post kind of a member's own
// webhook (decisions-T40 #14). It names no person and no title.
var personalSentence = map[Event]string{
	EventAssigned:     "Issue assigned to you",
	EventStateChange:  "Your issue changed state",
	EventComment:      "New comment on your issue",
	EventIssueUpdated: "Your issue was updated",
}

// EventIssueUpdated is the post kind of a member's own webhook for an update
// that is neither an assignment nor a state change (decisions-T40 #10). It is
// not a notification kind.
const EventIssueUpdated Event = "issue_updated"

var (
	slackEscaper        = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	discordStateEscaper = strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "|", `\|`,
		">", `\>`, "[", `\[`, "]", `\]`, "<", `\<`)
	discordEscaper = strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "|", `\|`,
		">", `\>`, "[", `\[`, "]", `\]`, "<", `\<`, "@", "@​")
)

// MaxStateNameRunes is where a state name is cut in a post.
const MaxStateNameRunes = 40

const zwsp = "\u200b"

// cleanStateName makes a state name safe for any chat kind before the kind's
// own escaping: control characters and line breaks become one space, runs of
// space collapse, format characters (Cf) are dropped, "@" and "://" are broken with a zero-width space so no
// mention and no link forms, and the name is cut at MaxStateNameRunes.
func cleanStateName(name string) string {
	name = strings.ToValidUTF8(name, "")
	// Unicode format characters (Cf: bidi overrides and isolates, zero-width
	// marks) are dropped first; the zero-width spaces below are added after.
	name = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name)
	name = strings.Join(strings.FieldsFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }), " ")
	if utf8.RuneCountInString(name) > MaxStateNameRunes {
		name = string([]rune(name)[:MaxStateNameRunes-1]) + "…"
	}
	name = strings.ReplaceAll(name, "@", "@"+zwsp)
	return strings.ReplaceAll(name, "://", ":"+zwsp+"//")
}

// slackStateName escapes for Slack mrkdwn: & < > as entities (so <!channel>,
// <@U1> and <http://x|y> stay text) and no emphasis or code marks.
func slackStateName(name string) string {
	// Delete the marks FIRST: deleting them after the "://" break would join
	// "http:*//" into a live link.
	name = strings.NewReplacer("*", "", "_", "", "~", "", "`", "").Replace(name)
	return slackEscaper.Replace(cleanStateName(name))
}

// discordStateName escapes for Discord markdown. cleanStateName already broke
// "@", so the "@" rule of discordEscaper is not applied a second time.
func discordStateName(name string) string {
	return discordStateEscaper.Replace(cleanStateName(name))
}

// customStateName: plain text for a receiver we do not know; angle brackets
// become look-alike marks so no <!channel> or <@id> form is left.
func customStateName(name string) string {
	return strings.NewReplacer("<", "‹", ">", "›").Replace(cleanStateName(name))
}

// stateArrow returns "from → to" for a kind, or "" when the message has no
// pair of names. Both names are cleaned first; a name that cleans to nothing
// (only format characters, or only marks on Slack) counts as missing, and the
// post keeps its fixed sentence (decisions-T39 rule 3).
func stateArrow(kind string, m ChatMessage) string {
	if m.Event != EventStateChange {
		return ""
	}
	esc := customStateName
	switch kind {
	case ChatSlack:
		esc = slackStateName
	case ChatDiscord, ChatFluxer:
		esc = discordStateName
	}
	from, to := esc(m.FromState), esc(m.ToState)
	if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
		return ""
	}
	return from + " → " + to
}

// ChatText makes the text of one post for a kind: "[LAUN-12] Issue assigned:
// <link>" (Slack: <link|open in Launchpad>; Discord: <link>, so no embed opens;
// custom: the plain link). The identifier is empty for a project. link is an
// absolute URL the server built. The test message is one fixed sentence.
func ChatText(kind string, m ChatMessage) string {
	if m.Event == EventTest {
		return ChatTestText
	}
	sentence := chatSentence[m.Event]
	if m.Personal {
		sentence = personalSentence[m.Event]
	}
	if sentence == "" {
		sentence = "Update"
	}
	sep, link := ": ", m.Link
	if arrow := stateArrow(kind, m); arrow != "" {
		sentence += ": " + arrow // "Issue changed state: A → B - <link>"
		sep = " - "
	}
	var b strings.Builder
	switch kind {
	case ChatSlack:
		if m.Identifier != "" {
			b.WriteString("[" + slackEscaper.Replace(m.Identifier) + "] ")
		}
		b.WriteString(sentence + sep + "<" + slackEscaper.Replace(link) + "|open in Launchpad>")
	case ChatDiscord, ChatFluxer:
		if m.Identifier != "" {
			b.WriteString("[" + discordEscaper.Replace(m.Identifier) + "] ")
		}
		b.WriteString(sentence + sep + "<" + link + ">")
	default:
		if m.Identifier != "" {
			b.WriteString("[" + m.Identifier + "] ")
		}
		b.WriteString(sentence + sep + link)
	}
	return b.String()
}
