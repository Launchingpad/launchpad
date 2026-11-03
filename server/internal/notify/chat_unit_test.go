package notify_test

import (
	"strings"
	"testing"

	"devops/internal/notify"
	"devops/internal/platform"
)

const goodHook = "https://hooks.slack.com/services/T0001/B0001/abcDEF123"

// The URL check is the first line of the SSRF defence (decisions-T33 #4).
func TestParseSlackWebhook(t *testing.T) {
	if _, err := notify.ParseSlackWebhook(goodHook); err != nil {
		t.Fatalf("good hook refused: %v", err)
	}
	bad := map[string]string{
		"empty":            "",
		"http":             "http://hooks.slack.com/services/T0001/B0001/abc",
		"other host":       "https://evil.example.com/services/T0001/B0001/abc",
		"suffix host":      "https://hooks.slack.com.evil.example.com/services/T0001/B0001/abc",
		"prefix host":      "https://evilhooks.slack.com/services/T0001/B0001/abc",
		"userinfo trick":   "https://hooks.slack.com@evil.example.com/services/T0001/B0001/abc",
		"userinfo":         "https://user:pw@hooks.slack.com/services/T0001/B0001/abc",
		"port":             "https://hooks.slack.com:8443/services/T0001/B0001/abc",
		"trailing dot":     "https://hooks.slack.com./services/T0001/B0001/abc",
		"upper case host":  "https://HOOKS.SLACK.COM/services/T0001/B0001/abc",
		"ip host":          "https://127.0.0.1/services/T0001/B0001/abc",
		"query":            "https://hooks.slack.com/services/T0001/B0001/abc?x=1",
		"empty query":      "https://hooks.slack.com/services/T0001/B0001/abc?",
		"fragment":         "https://hooks.slack.com/services/T0001/B0001/abc#x",
		"dot dot":          "https://hooks.slack.com/services/../B0001/abc",
		"encoded slash":    "https://hooks.slack.com/services/T0001%2fB0001/B0001/abc",
		"encoded path":     "https://hooks.slack.com/services/T0001/B0001/%61bc",
		"too few parts":    "https://hooks.slack.com/services/T0001/B0001",
		"too many parts":   "https://hooks.slack.com/services/T0001/B0001/abc/def",
		"other prefix":     "https://hooks.slack.com/triggers/T0001/B0001/abc",
		"space":            "https://hooks.slack.com/services/T0001/B0001/abc def",
		"newline":          "https://hooks.slack.com/services/T0001/B0001/abc\n",
		"scheme only":      "https://",
		"backslash":        "https://hooks.slack.com\\@evil.example.com/services/T0001/B0001/abc",
		"opaque":           "https:hooks.slack.com/services/T0001/B0001/abc",
		"too long":         "https://hooks.slack.com/services/T0001/B0001/" + strings.Repeat("a", 300),
		"slash after host": "https://hooks.slack.com//services/T0001/B0001/abc",
	}
	for name, u := range bad {
		if _, err := notify.ParseSlackWebhook(u); err == nil {
			t.Errorf("%s: %q was accepted", name, u)
		} else if u != "" && strings.Contains(err.Error(), u) {
			t.Errorf("%s: the error repeats the value: %v", name, err)
		}
	}
}

// A sealed blob is bound to its org and column (decisions-T33 #3).
func TestSlackKeyring_BoundToOrg(t *testing.T) {
	master := []byte("0123456789abcdef0123456789abcdef")
	k, err := platform.NewSlackKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	blob := k.SealFor(platform.SlackColWebhook, "org-a", []byte(goodHook))
	if strings.Contains(string(blob), "hooks.slack.com") {
		t.Fatal("the blob holds the URL in clear")
	}
	if got, err := k.OpenFor(platform.SlackColWebhook, "org-a", blob); err != nil || string(got) != goodHook {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := k.OpenFor(platform.SlackColWebhook, "org-b", blob); err == nil {
		t.Error("a blob opened for another org")
	}
	if _, err := k.OpenFor("other_col", "org-a", blob); err == nil {
		t.Error("a blob opened for another column")
	}
	other, _ := platform.NewSlackKeyring([]byte("ffffffffffffffffffffffffffffffff"))
	if _, err := other.OpenFor(platform.SlackColWebhook, "org-a", blob); err == nil {
		t.Error("a blob opened under another key")
	}
	// Another scope of the same master key does not open it either.
	git, _ := platform.NewGitKeyring(master)
	if _, err := git.OpenFor("webhook_enc", "org-a", blob); err == nil {
		t.Error("a Slack blob opened as a git blob")
	}
}

func TestChatText_Slack(t *testing.T) {
	got := notify.ChatText(notify.ChatSlack, notify.ChatMessage{Event: notify.EventAssigned, Identifier: "ORB-1", Link: "https://x.example/o/issue/ORB-1"})
	want := "[ORB-1] Issue assigned: <https://x.example/o/issue/ORB-1|open in Launchpad>"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if got := notify.ChatText(notify.ChatSlack, notify.ChatMessage{Event: notify.EventProjectUpdateDue, Link: "https://x.example/o/projects/1"}); got != "Project update due: <https://x.example/o/projects/1|open in Launchpad>" {
		t.Errorf("project text %q", got)
	}
	// Slack's three special characters are escaped in the link.
	if got := notify.ChatText(notify.ChatSlack, notify.ChatMessage{Event: notify.EventComment, Identifier: "A&B", Link: "https://x.example/?a=1&b=<2>"}); !strings.Contains(got, "[A&amp;B]") || !strings.Contains(got, "a=1&amp;b=&lt;2&gt;") {
		t.Errorf("not escaped: %q", got)
	}
}

func TestChatText_DiscordAndCustom(t *testing.T) {
	m := notify.ChatMessage{Event: notify.EventAssigned, Identifier: "ORB-1", Link: "https://x.example/o/issue/ORB-1"}
	// Discord: the link in angle brackets (no embed), no Slack syntax.
	if got, want := notify.ChatText(notify.ChatDiscord, m), "[ORB-1] Issue assigned: <https://x.example/o/issue/ORB-1>"; got != want {
		t.Errorf("discord got %q want %q", got, want)
	}
	// Markdown and mentions in an identifier are defused.
	if got := notify.ChatText(notify.ChatDiscord, notify.ChatMessage{Event: notify.EventNewIssue, Identifier: "@everyone*_x", Link: "https://x.example/"}); strings.Contains(got, "@everyone") || strings.Contains(got, "*_") {
		t.Errorf("discord markdown or mention not defused: %q", got)
	}
	if got, want := notify.ChatText(notify.ChatCustom, m), "[ORB-1] Issue assigned: https://x.example/o/issue/ORB-1"; got != want {
		t.Errorf("custom got %q want %q", got, want)
	}
	for _, k := range notify.ChatKinds {
		if got := notify.ChatText(k, notify.ChatMessage{Event: notify.EventTest}); got != notify.ChatTestText {
			t.Errorf("%s test text %q", k, got)
		}
	}
}

const goodDiscord = "https://discord.com/api/webhooks/123456789012345678/abcDEF_ghi-123"

// Discord's URL check is an exact allowlist (decisions-T36 #4).
func TestParseDiscordWebhook(t *testing.T) {
	if _, err := notify.ParseDiscordWebhook(goodDiscord); err != nil {
		t.Fatalf("good hook refused: %v", err)
	}
	if _, err := notify.ParseDiscordWebhook("https://discordapp.com/api/webhooks/1/abc"); err != nil {
		t.Fatalf("discordapp.com refused: %v", err)
	}
	bad := map[string]string{
		"empty":          "",
		"http":           "http://discord.com/api/webhooks/1/abc",
		"other host":     "https://evil.example.com/api/webhooks/1/abc",
		"suffix host":    "https://discord.com.evil.example.com/api/webhooks/1/abc",
		"sub host":       "https://ptb.discord.com/api/webhooks/1/abc",
		"userinfo trick": "https://discord.com@evil.example.com/api/webhooks/1/abc",
		"port":           "https://discord.com:8443/api/webhooks/1/abc",
		"trailing dot":   "https://discord.com./api/webhooks/1/abc",
		"upper case":     "https://DISCORD.COM/api/webhooks/1/abc",
		"ip host":        "https://127.0.0.1/api/webhooks/1/abc",
		"query":          "https://discord.com/api/webhooks/1/abc?wait=true",
		"fragment":       "https://discord.com/api/webhooks/1/abc#x",
		"non digit id":   "https://discord.com/api/webhooks/abc/abc",
		"no token":       "https://discord.com/api/webhooks/1",
		"too many parts": "https://discord.com/api/webhooks/1/abc/slack",
		"other path":     "https://discord.com/api/users/1/abc",
		"encoded":        "https://discord.com/api/webhooks/1/%61bc",
		"space":          "https://discord.com/api/webhooks/1/abc def",
		"slack url":      goodHook,
		"too long":       "https://discord.com/api/webhooks/1/" + strings.Repeat("a", 300),
	}
	for name, u := range bad {
		if _, err := notify.ParseDiscordWebhook(u); err == nil {
			t.Errorf("%s: %q was accepted", name, u)
		} else if u != "" && strings.Contains(err.Error(), u) {
			t.Errorf("%s: the error repeats the value: %v", name, err)
		}
	}
}

// The custom URL check reuses the push relay refusals (decisions-T36 #5).
func TestParseCustomWebhook(t *testing.T) {
	for _, ok := range []string{
		"https://chat.example.com/hooks/abc",
		"https://chat.example.com:8443/hooks/abc?token=xyz",
		"https://8.8.8.8/hooks/abc",
	} {
		if _, err := notify.ParseCustomWebhook(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	bad := map[string]string{
		"empty":         "",
		"http":          "http://chat.example.com/hooks/abc",
		"userinfo":      "https://u:p@chat.example.com/hooks/abc",
		"fragment":      "https://chat.example.com/hooks/abc#x",
		"no host":       "https:///hooks/abc",
		"loopback":      "https://127.0.0.1/hooks/abc",
		"loopback v6":   "https://[::1]/hooks/abc",
		"private":       "https://10.0.0.5/hooks/abc",
		"private 192":   "https://192.168.1.10/hooks/abc",
		"link local":    "https://169.254.169.254/latest/meta-data",
		"legacy int":    "https://2130706433/hooks/abc",
		"legacy short":  "https://127.1/hooks/abc",
		"legacy hex":    "https://0x7f.0.0.1/hooks/abc",
		"legacy octal":  "https://0177.0.0.1/hooks/abc",
		"localhost":     "https://localhost/hooks/abc",
		"sub localhost": "https://app.localhost/hooks/abc",
		".internal":     "https://hooks.corp.internal/abc",
		".local":        "https://nas.local/hooks/abc",
		".lan":          "https://router.lan/hooks/abc",
		".home.arpa":    "https://box.home.arpa/hooks/abc",
		"single label":  "https://intranet/hooks/abc",
		"encoded path":  "https://chat.example.com/hooks%2Fabc",
		"space":         "https://chat.example.com/hooks/a b",
		"too long":      "https://chat.example.com/" + strings.Repeat("a", 1100),
	}
	for name, u := range bad {
		if _, err := notify.ParseCustomWebhook(u); err == nil {
			t.Errorf("%s: %q was accepted", name, u)
		} else if u != "" && strings.Contains(err.Error(), u) {
			t.Errorf("%s: the error repeats the value: %v", name, err)
		}
	}
}

func TestParseChatWebhook_KindsDoNotCross(t *testing.T) {
	if _, err := notify.ParseChatWebhook(notify.ChatSlack, goodDiscord); err == nil {
		t.Error("a Discord URL passed as Slack")
	}
	if _, err := notify.ParseChatWebhook(notify.ChatDiscord, goodHook); err == nil {
		t.Error("a Slack URL passed as Discord")
	}
	if _, err := notify.ParseChatWebhook("teams", goodHook); err == nil {
		t.Error("an unknown kind passed")
	}
}

// Fluxer: the Discord body, a webhook path on any public https host (LAUN-206).
func TestParseFluxerWebhook(t *testing.T) {
	for _, ok := range []string{
		"https://chat.example.com/api/webhooks/123456789/abcDEF_ghi-123",
		"https://chat.example.com/api/v1/webhooks/123456789/abcDEF_ghi-123",
		"https://chat.example.com:8443/api/webhooks/1/abc",
		"https://discord.com/api/webhooks/1/abc",
	} {
		if _, err := notify.ParseFluxerWebhook(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for name, u := range map[string]string{
		"http":         "http://chat.example.com/api/webhooks/1/abc",
		"userinfo":     "https://u:p@chat.example.com/api/webhooks/1/abc",
		"lan":          "https://fluxer.lan/api/webhooks/1/abc",
		"internal":     "https://fluxer.internal/api/webhooks/1/abc",
		"private ip":   "https://10.0.0.5/api/webhooks/1/abc",
		"loopback":     "https://127.0.0.1/api/webhooks/1/abc",
		"link local":   "https://169.254.169.254/api/webhooks/1/abc",
		"localhost":    "https://localhost/api/webhooks/1/abc",
		"query":        "https://chat.example.com/api/webhooks/1/abc?x=1",
		"fragment":     "https://chat.example.com/api/webhooks/1/abc#x",
		"slack path":   "https://chat.example.com/services/T1/B1/abc",
		"custom path":  "https://chat.example.com/hooks/abc",
		"non digit id": "https://chat.example.com/api/webhooks/abc/abc",
		"no token":     "https://chat.example.com/api/webhooks/1",
		"v2":           "https://chat.example.com/api/v2/webhooks/1/abc",
		"encoded":      "https://chat.example.com/api/webhooks/1/%61bc",
		"empty":        "",
	} {
		if _, err := notify.ParseFluxerWebhook(u); err == nil {
			t.Errorf("%s: %q was accepted", name, u)
		} else if u != "" && strings.Contains(err.Error(), u) {
			t.Errorf("%s: the error repeats the value: %v", name, err)
		}
		if _, err := notify.ParseChatWebhook(notify.ChatFluxer, u); err == nil {
			t.Errorf("%s: ParseChatWebhook accepted %q", name, u)
		}
	}
	// The custom kind keeps its own rules.
	if _, err := notify.ParseChatWebhook(notify.ChatCustom, "https://chat.example.com/hooks/abc"); err != nil {
		t.Errorf("custom refused: %v", err)
	}
}

// The Fluxer post body equals the Discord body, byte for byte, for every event.
func TestFluxerBodyEqualsDiscord(t *testing.T) {
	msgs := []notify.ChatMessage{
		{Event: notify.EventTest},
		{Event: notify.EventNewIssue, Identifier: "ORB-1", Link: "https://x.example/o/issue/ORB-1"},
		{Event: notify.EventNewIssue, Identifier: "@everyone*_x", Link: "https://x.example/"},
		{Event: notify.EventNewIssue, Link: "https://x.example/p"},
	}
	for _, m := range msgs {
		if a, b := notify.ChatText(notify.ChatFluxer, m), notify.ChatText(notify.ChatDiscord, m); a != b {
			t.Errorf("text %q != %q", a, b)
		}
		fb, err := notify.ChatBodyForTest(notify.ChatFluxer, m)
		if err != nil {
			t.Fatal(err)
		}
		db, _ := notify.ChatBodyForTest(notify.ChatDiscord, m)
		if string(fb) != string(db) || !strings.Contains(string(fb), `"allowed_mentions":{"parse":[]}`) {
			t.Errorf("body %s != %s", fb, db)
		}
	}
}

// LAUN-214, decisions-T39: a state change names the old and the new state.
func TestChatText_StateNames(t *testing.T) {
	m := notify.ChatMessage{Event: notify.EventStateChange, Identifier: "LAUN-1", Link: "https://x.example.com/o/issue/LAUN-1",
		FromState: "In Progress", ToState: "In Review"}
	want := map[string]string{
		notify.ChatSlack:   "[LAUN-1] Issue changed state: In Progress → In Review - <https://x.example.com/o/issue/LAUN-1|open in Launchpad>",
		notify.ChatDiscord: "[LAUN-1] Issue changed state: In Progress → In Review - <https://x.example.com/o/issue/LAUN-1>",
		notify.ChatFluxer:  "[LAUN-1] Issue changed state: In Progress → In Review - <https://x.example.com/o/issue/LAUN-1>",
		notify.ChatCustom:  "[LAUN-1] Issue changed state: In Progress → In Review - https://x.example.com/o/issue/LAUN-1",
	}
	for kind, w := range want {
		if got := notify.ChatText(kind, m); got != w {
			t.Errorf("%s: got %q want %q", kind, got, w)
		}
	}
	// No names (or one): the old fixed sentence.
	m.ToState = ""
	if got := notify.ChatText(notify.ChatCustom, m); got != "[LAUN-1] Issue changed state: https://x.example.com/o/issue/LAUN-1" {
		t.Errorf("fallback: %q", got)
	}
	// Names on another kind are ignored.
	m.Event, m.ToState = notify.EventComment, "X"
	if got := notify.ChatText(notify.ChatCustom, m); strings.Contains(got, "In Progress") {
		t.Errorf("a comment post holds a state name: %q", got)
	}
}

func TestChatText_StateNameEscaping(t *testing.T) {
	const link = "https://x.example.com/a"
	const z = "\u200b"
	wrap := func(kind, from string) string {
		head := "[A-1] Issue changed state: " + from + " → Done - "
		switch kind {
		case notify.ChatSlack:
			return head + "<" + link + "|open in Launchpad>"
		case notify.ChatDiscord, notify.ChatFluxer:
			return head + "<" + link + ">"
		}
		return head + link
	}
	// name -> the exact "from" part per kind: slack, discord (and fluxer), custom.
	cases := []struct {
		name                  string
		slack, discord, plain string
	}{
		{"@everyone", "@" + z + "everyone", "@" + z + "everyone", "@" + z + "everyone"},
		{"@here", "@" + z + "here", "@" + z + "here", "@" + z + "here"},
		{"<@U123>", "&lt;@" + z + "U123&gt;", `\<@` + z + `U123\>`, "‹@" + z + "U123›"},
		{"<!channel>", "&lt;!channel&gt;", `\<!channel\>`, "‹!channel›"},
		{"`x`", "x", "\\`x\\`", "`x`"},
		{"*bold* _it_ ~s~", "bold it s", `\*bold\* \_it\_ \~s\~`, "*bold* _it_ ~s~"},
		{"[a](https://evil.example.com)", "[a](https:" + z + "//evil.example.com)", `\[a\](https:` + z + `//evil.example.com)`, "[a](https:" + z + "//evil.example.com)"},
		{"line\nbreak\r\n", "line break", "line break", "line break"},
		{"http:*//evil.example.com/x", "http:" + z + "//evil.example.com/x", `http:\*//evil.example.com/x`, "http:*//evil.example.com/x"},
		{"🚀 Ship it", "🚀 Ship it", "🚀 Ship it", "🚀 Ship it"},
		// Format characters (Cf) are dropped: RTL override, bidi isolate, zero width.
		{"a\u202Eb\u202Ac", "abc", "abc", "abc"},
		{"\u2066x\u2069y", "xy", "xy", "xy"},
		{"in\u200Bvi\u200Dsi\u2060ble\ufeff", "invisible", "invisible", "invisible"},
		{"\u202E@\u200Beveryone", "@" + z + "everyone", "@" + z + "everyone", "@" + z + "everyone"},
	}
	for _, c := range cases {
		for _, kind := range notify.ChatKinds {
			want := c.plain
			switch kind {
			case notify.ChatSlack:
				want = c.slack
			case notify.ChatDiscord, notify.ChatFluxer:
				want = c.discord
			}
			got := notify.ChatText(kind, notify.ChatMessage{Event: notify.EventStateChange, Identifier: "A-1", Link: link,
				FromState: c.name, ToState: "Done"})
			if got != wrap(kind, want) {
				t.Errorf("%s %q:\n got  %q\n want %q", kind, c.name, got, wrap(kind, want))
			}
		}
	}
	long := strings.Repeat("é", 500)
	for _, kind := range notify.ChatKinds {
		got := notify.ChatText(kind, notify.ChatMessage{Event: notify.EventStateChange, Identifier: "A-1", Link: link, FromState: long, ToState: "Done"})
		if want := wrap(kind, strings.Repeat("é", notify.MaxStateNameRunes-1)+"…"); got != want {
			t.Errorf("%s: long name: got %q want %q", kind, got, want)
		}
	}
}

// A name that cleans to nothing counts as missing: the old fixed sentence.
func TestChatText_EmptyAfterCleaningFallsBack(t *testing.T) {
	const link = "https://x.example.com/a"
	for _, c := range []struct{ kind, from, to string }{
		{notify.ChatSlack, "\u202E\u200B", "Done"},
		{notify.ChatSlack, "***", "Done"},
		{notify.ChatSlack, "In Progress", "`~_"},
		{notify.ChatDiscord, "\u2066\u2069", "Done"},
		{notify.ChatFluxer, "A", "\u200D"},
		{notify.ChatCustom, " \n ", "Done"},
		{notify.ChatCustom, "\ufeff", "\u2060"},
	} {
		got := notify.ChatText(c.kind, notify.ChatMessage{Event: notify.EventStateChange, Identifier: "A-1", Link: link, FromState: c.from, ToState: c.to})
		if strings.Contains(got, "→") || !strings.HasPrefix(got, "[A-1] Issue changed state: ") || strings.Contains(got, "state:  ") {
			t.Errorf("%s %q %q: got %q", c.kind, c.from, c.to, got)
		}
	}
	// `*` alone is a real name on Discord (escaped), so it stays.
	if got := notify.ChatText(notify.ChatDiscord, notify.ChatMessage{Event: notify.EventStateChange, Identifier: "A-1", Link: link, FromState: "*", ToState: "Done"}); !strings.Contains(got, `\* → Done`) {
		t.Errorf("discord star: %q", got)
	}
}
