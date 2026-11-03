package mail

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func base() Input {
	return Input{
		Identifier: "LAUN-34", Title: "Notifications 4/9: email", TeamKey: "LAUN", StateName: "In Progress",
		IssueURL:       "https://launchpad.example.com/orb/issue/LAUN-34",
		SettingsURL:    "https://launchpad.example.com/orb/settings/notifications",
		UnsubscribeURL: "https://launchpad.example.com/unsubscribe/TOKEN",
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs (run with -update and review the diff)\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestBuild_Golden(t *testing.T) {
	assigned := base()
	assigned.Items = []Item{{Kind: KindAssigned, Actor: "Chen"}}

	multi := base()
	multi.Primary = KindMention
	multi.NotEmailed = 3
	multi.Items = []Item{
		{Kind: KindStateChange, Actor: "Claude", FromState: "Todo", ToState: "In Review"},
		{Kind: KindComment, Actor: "Claude", Excerpt: "Looks good <b>to</b> me\nsecond line"},
		{Kind: KindMention, Actor: "Chen"},
	}

	for name, in := range map[string]Input{"assigned": assigned, "multi": multi} {
		out, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		golden(t, name+".subject.golden", out.Subject+"\n")
		golden(t, name+".txt.golden", out.Text)
		golden(t, name+".html.golden", out.HTML)
	}
}

func TestBuild_NoTextUnlessExcerpt(t *testing.T) {
	in := base()
	in.Items = []Item{{Kind: KindComment, Actor: "Claude"}} // no Excerpt: the caller left it out
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Text, ">") && strings.Contains(out.Text, "\n> ") {
		t.Fatalf("a quote block without an excerpt:\n%s", out.Text)
	}
	if strings.Contains(out.HTML, "<blockquote") {
		t.Fatal("a blockquote without an excerpt")
	}
}

func TestBuild_Escaping(t *testing.T) {
	in := base()
	in.Title = "<script>alert(1)</script>\r\nBcc: x@example.com"
	in.Items = []Item{{Kind: KindComment, Actor: "<img src=x>", Excerpt: "<i>hi</i>"}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.Subject, "\r\n") {
		t.Fatalf("subject has a line break: %q", out.Subject)
	}
	if strings.Contains(out.HTML, "<script>") || strings.Contains(out.HTML, "<img src=x>") || strings.Contains(out.HTML, "<i>hi</i>") {
		t.Fatalf("the html is not escaped:\n%s", out.HTML)
	}
	if strings.Count(out.Text, "\n")+1 < 5 || strings.Contains(out.Text, "\r") {
		t.Fatalf("text %q", out.Text)
	}
}

func TestBuild_LongTitleAndExcerptAreCut(t *testing.T) {
	in := base()
	in.Title = strings.Repeat("t", 500)
	in.Items = []Item{{Kind: KindComment, Excerpt: strings.Repeat("e", 900)}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(strings.TrimPrefix(out.Subject, "[LAUN-34] New comment: "))); n != MaxTitle {
		t.Fatalf("title length %d", n)
	}
	if strings.Contains(out.Text, strings.Repeat("e", MaxExcerpt+1)) {
		t.Fatal("the excerpt is not cut")
	}
}

func TestBuild_NoItems(t *testing.T) {
	if _, err := Build(base()); err == nil {
		t.Fatal("no items built")
	}
}

func TestBuild_NotEmailedLine(t *testing.T) {
	in := base()
	in.Items = []Item{{Kind: KindComment}}
	in.NotEmailed = 1
	out, _ := Build(in)
	if !strings.Contains(out.Text, "1 more update was not emailed. See your inbox.") {
		t.Fatalf("singular line missing:\n%s", out.Text)
	}
	in.NotEmailed = 4
	out, _ = Build(in)
	if !strings.Contains(out.Text, "4 more updates were not emailed. See your inbox.") || !strings.Contains(out.HTML, "4 more updates") {
		t.Fatalf("plural line missing:\n%s", out.Text)
	}
}

func TestBuildProjectDue_NotEmailedLine(t *testing.T) {
	in := ProjectDueInput{ProjectName: "P", ProjectURL: "https://x.example/p", SettingsURL: "https://x.example/s", UnsubscribeURL: "https://x.example/u"}
	if got := BuildProjectDue(in); strings.Contains(got.Text, "not emailed") {
		t.Errorf("no line expected: %s", got.Text)
	}
	in.NotEmailed = 3
	got := BuildProjectDue(in)
	if !strings.Contains(got.Text, "3 more updates were not emailed. See your inbox.") || !strings.Contains(got.HTML, "3 more updates were not emailed") {
		t.Errorf("text %q html %q", got.Text, got.HTML)
	}
}
