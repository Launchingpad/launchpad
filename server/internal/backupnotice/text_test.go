package backupnotice

import (
	"encoding/json"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestClean_StripsControlCharacters(t *testing.T) {
	got := clean("a\r\nBcc: evil@example.com\x00\x1b[31m", 60)
	if strings.ContainsAny(got, "\r\n\x00\x1b") {
		t.Fatalf("control characters left: %q", got)
	}
	if long := clean(strings.Repeat("x", 500), 60); len(long) != 60 {
		t.Fatalf("not capped: %d", len(long))
	}
}

func TestRunMail_NoHeaderInjectionAndFixedFields(t *testing.T) {
	n := RunNotice{DestinationName: "nas\r\nBcc: evil@example.com", Trigger: "schedule", Status: "failed",
		FinishedAt: t0, ErrorCode: "auth_failed", ErrorText: "the remote refused the credentials\nsecond line"}
	subj, text := RunMail(n, PageLink("https://launchpad.example.com/", "orbit"))
	if strings.ContainsAny(subj, "\r\n") {
		t.Fatalf("subject: %q", subj)
	}
	for _, want := range []string{"Destination: nas", "Code: auth_failed", "https://launchpad.example.com/orbit/settings/platform"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "com//") {
		t.Errorf("double slash in the link:\n%s", text)
	}
	// A code that is not a plain identifier is left out.
	n.ErrorCode = "Bad Code\n"
	if _, text = RunMail(n, ""); strings.Contains(text, "Bad Code") || strings.Contains(text, "https://") {
		t.Errorf("odd code kept or a link made without a base URL:\n%s", text)
	}
}

func TestChangeMail_UsesOnlyAWhitelistOfDetail(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	detail, _ := json.Marshal(map[string]any{
		"fingerprint": fp, "identity": map[string]any{"host": "secret-host.internal.example"},
		"password": "planted-detail-secret", "url": "https://user:pw@host.example/x",
	})
	subj, text := ChangeMail(ChangeNotice{Kind: "destination_pin_set", At: t0, DestinationName: "nas", Actor: "chen", Detail: detail}, "")
	all := subj + text
	for _, bad := range []string{"secret-host", "planted-detail-secret", "user:pw", fp} {
		if strings.Contains(all, bad) {
			t.Errorf("mail holds %q:\n%s", bad, all)
		}
	}
	for _, want := range []string{"Changed by: chen", "Key fingerprint: " + fp[:12], "Destination: nas"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

func TestChangeMail_UnknownKindAndHostAction(t *testing.T) {
	_, text := ChangeMail(ChangeNotice{Kind: "Weird Kind!", At: t0, Detail: json.RawMessage(`{}`)}, "")
	if strings.Contains(text, "Weird") || !strings.Contains(text, "a command on the host") {
		t.Errorf("text:\n%s", text)
	}
	_, text = ChangeMail(ChangeNotice{Kind: "future_kind", At: t0, Detail: json.RawMessage(`{}`)}, "")
	if !strings.Contains(text, "(future_kind)") {
		t.Errorf("a plain identifier should be named:\n%s", text)
	}
}

func TestNoticeSettingsDiff(t *testing.T) {
	d := json.RawMessage(`{"from":{"notifyOnFailure":true,"notifyOnSuccess":false,"staleAlertHours":48},"to":{"notifyOnFailure":false,"notifyOnSuccess":true,"staleAlertHours":0}}`)
	_, text := ChangeMail(ChangeNotice{Kind: "notice_settings_changed", At: t0, Actor: "chen", Detail: d}, "")
	for _, want := range []string{"Mail on failure: true -> false", "Mail on success: false -> true", "(hours, 0 is off): 48 -> 0"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

func TestStaleMail(t *testing.T) {
	_, text := StaleMail(StaleNotice{DestinationName: "nas", LastSuccess: ago(100), Since: *ago(100), Threshold: 48 * 3600e9, Now: t0}, "")
	if !strings.Contains(text, "longer than 48 hours") || !strings.Contains(text, "Last success: 2026-10-23") {
		t.Errorf("text:\n%s", text)
	}
	_, text = StaleMail(StaleNotice{DestinationName: "nas", Since: *ago(100), Threshold: 48 * 3600e9, Now: t0}, "")
	if !strings.Contains(text, "Last success: none since") {
		t.Errorf("text:\n%s", text)
	}
}

func TestPageLink_ShapeMatchesTheWebRoute(t *testing.T) {
	link := PageLink("https://launchpad.example.com/", "orbit")
	if link != "https://launchpad.example.com/orbit/settings/platform" {
		t.Fatalf("link = %q", link)
	}
	u, err := url.Parse(link)
	if err != nil || !regexp.MustCompile(`^/[^/]+/settings/platform$`).MatchString(u.Path) {
		t.Fatalf("path %q", u.Path)
	}
	for _, c := range [][2]string{{"", "orbit"}, {"https://x.example", ""}, {"https://x.example", "a b"}, {"https://x.example", "../x"}} {
		if PageLink(c[0], c[1]) != "" {
			t.Errorf("PageLink(%q, %q) should be empty", c[0], c[1])
		}
	}
	// The route lives in web/src/App.tsx: /:org with a child "settings" and its child "platform".
	// Checked when the web tree is next to the server (it is, in this repository).
	b, err := os.ReadFile("../../../web/src/App.tsx")
	if err != nil {
		t.Skip("web/src/App.tsx not found")
	}
	app := string(b)
	i, j, k := strings.Index(app, `path="/:org"`), strings.Index(app, `path="settings"`), strings.Index(app, `path="platform"`)
	if i < 0 || j < i || k < j {
		t.Fatalf("web/src/App.tsx no longer nests settings/platform under /:org (indexes %d %d %d)", i, j, k)
	}
}

func TestChangeMail_InstanceResumedHasItsOwnSentence(t *testing.T) {
	if !KnownChangeKind("instance_resumed") {
		t.Fatal("instance_resumed has no sentence")
	}
}
