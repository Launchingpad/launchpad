package notify

import (
	"strings"
	"testing"
)

// decisions-T40 #10: assigned first, then the change kind when the member owns
// the issue and ticked my_issues; nothing otherwise.
func TestPersonalPostKind(t *testing.T) {
	both := []string{PersonalAssigned, PersonalMyIssues}
	cases := []struct {
		events   []string
		assigned bool
		change   Event
		want     Event
		ok       bool
	}{
		{both, true, EventIssueUpdated, EventAssigned, true},
		{both, false, EventComment, EventComment, true},
		{both, false, "", "", false},
		{[]string{PersonalAssigned}, false, EventStateChange, "", false},
		{[]string{PersonalMyIssues}, true, EventIssueUpdated, EventIssueUpdated, true},
		{[]string{PersonalMyIssues}, true, "", "", false},
		{[]string{}, true, EventComment, "", false},
	}
	for i, c := range cases {
		got, ok := personalPostKind(c.events, c.assigned, c.change)
		if got != c.want || ok != c.ok {
			t.Errorf("case %d: got %q %v, want %q %v", i, got, ok, c.want, c.ok)
		}
		if ok && personalChoice(got) == PersonalAssigned != (got == EventAssigned) {
			t.Errorf("case %d: choice of %q", i, got)
		}
	}
}

func TestParsePersonalEvents(t *testing.T) {
	got, err := parsePersonalEvents([]string{PersonalMyIssues, PersonalAssigned, PersonalMyIssues})
	if err != nil || strings.Join(got, ",") != "assigned,my_issues" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := parsePersonalEvents([]string{"mention"}); err == nil {
		t.Fatal("mention accepted")
	}
	if got, err := parsePersonalEvents([]string{}); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
}

// The personal sentences speak to the member and carry no title or name; the
// state names follow T39 for every kind.
func TestPersonalChatText(t *testing.T) {
	link := "https://launchpad.example.com/orbit/issue/ORB-1"
	for ev, want := range map[Event]string{
		EventAssigned:     "[ORB-1] Issue assigned to you: " + link,
		EventComment:      "[ORB-1] New comment on your issue: " + link,
		EventIssueUpdated: "[ORB-1] Your issue was updated: " + link,
		EventStateChange:  "[ORB-1] Your issue changed state: " + link,
	} {
		if got := ChatText(ChatCustom, ChatMessage{Event: ev, Identifier: "ORB-1", Link: link, Personal: true}); got != want {
			t.Errorf("%s: %q, want %q", ev, got, want)
		}
	}
	got := ChatText(ChatSlack, ChatMessage{Event: EventStateChange, Identifier: "ORB-1", Link: link, Personal: true, FromState: "Todo", ToState: "Done"})
	if got != "[ORB-1] Your issue changed state: Todo → Done - <"+link+"|open in Launchpad>" {
		t.Errorf("slack state: %q", got)
	}
	if got := ChatText(ChatSlack, ChatMessage{Event: EventTest, Personal: true}); got != ChatTestText {
		t.Errorf("test: %q", got)
	}
	// The chat sentences are unchanged.
	if got := ChatText(ChatCustom, ChatMessage{Event: EventAssigned, Identifier: "ORB-1", Link: link}); got != "[ORB-1] Issue assigned: "+link {
		t.Errorf("chat: %q", got)
	}
}

func TestPersonalScope(t *testing.T) {
	a := personalScope([16]byte{1}, [16]byte{2}, [16]byte{3})
	if !strings.HasPrefix(a, "pwh.") || a == sealScope([16]byte{1}, [16]byte{3}) {
		t.Fatalf("scope %q", a)
	}
}

// A host longer than a DNS name is refused at save (400), never stored (the
// host column allows 255): custom and Fluxer share the check.
func TestParseCustomWebhookLongHost(t *testing.T) {
	long := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 60) + ".example.com"
	if len(long) <= MaxWebhookHostBytes {
		t.Fatalf("host too short for the test: %d", len(long))
	}
	for _, raw := range []string{"https://" + long + "/x", "https://" + long + "/api/webhooks/1/tok"} {
		for _, kind := range []string{ChatCustom, ChatFluxer} {
			if _, err := ParseChatWebhook(kind, raw); err == nil || strings.Contains(err.Error(), long) {
				t.Errorf("%s %d-byte host: err %v", kind, len(long), err)
			}
		}
	}
	ok := strings.Repeat("a", 60) + ".example.com"
	if _, err := ParseChatWebhook(ChatCustom, "https://"+ok+"/x"); err != nil {
		t.Errorf("a normal host is refused: %v", err)
	}
}
