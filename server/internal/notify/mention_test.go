package notify

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseMentions pins the rules of decisions-T13 #17.
func TestParseMentions(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"plain", "hello @chen", []string{"chen"}},
		{"start of text", "@chen please look", []string{"chen"}},
		{"case folded", "@Chen and @CHEN", []string{"chen"}},
		{"two names in order", "@bob then @alice then @bob", []string{"bob", "alice"}},
		{"email is not a mention", "mail a@example.com or x@chen", nil},
		{"after a word character", "foo@chen", nil},
		{"after a digit", "9@chen", nil},
		{"after underscore", "_@chen", nil},
		{"after a dot", "see.@chen", nil},
		{"double at", "@@chen", nil},
		{"after punctuation", "(@chen), \"@bob\" and [@carol]", []string{"chen", "bob", "carol"}},
		{"dot ends the name", "thanks @chen.", []string{"chen"}},
		{"dash and underscore inside", "@a-b_c1", []string{"a-b_c1"}},
		{"trailing dash also offers the trimmed name", "@chen-", []string{"chen-", "chen"}},
		{"must start with a letter", "@1abc @_abc @-abc", nil},
		{"too long is not a username", "@" + strings.Repeat("a", 40), nil},
		{"39 characters is the longest", "@" + strings.Repeat("a", 39), []string{strings.Repeat("a", 39)}},
		{"inline code", "run `@chen` here", nil},
		{"inline code then text", "`@chen` @bob", []string{"bob"}},
		{"double backtick span", "``a ` @chen`` @bob", []string{"bob"}},
		{"unclosed backtick is text", "a ` @chen", []string{"chen"}},
		{"fenced block", "before\n```\n@chen\n```\n@bob", []string{"bob"}},
		{"fenced block with language", "```go\n@chen\n```", nil},
		{"tilde fence", "~~~\n@chen\n~~~\n@bob", []string{"bob"}},
		{"unclosed fence swallows the rest", "```\n@chen\n@bob", nil},
		{"short fence does not close a long one", "````\n```\n@chen\n````\n@bob", []string{"bob"}},
		{"quoted line", "> @chen said\n@bob", []string{"bob"}},
		{"quoted line with indent", "  > @chen", nil},
		{"indented code is not special", "    @chen", []string{"chen"}},
		{"crlf", "a\r\n> @chen\r\n@bob", []string{"bob"}},
		{"empty", "", nil},
		{"unicode letter before", "é@chen", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseMentions(c.text)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("ParseMentions(%q) = %q, want %q", c.text, got, c.want)
			}
		})
	}
}

func TestParseMentions_Bounded(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		b.WriteString(" @u")
		b.WriteString(strings.Repeat("a", i%30))
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(strings.Repeat("b", i/26))
	}
	if got := ParseMentions(b.String()); len(got) > maxMentions {
		t.Errorf("got %d names, want at most %d", len(got), maxMentions)
	}
}
