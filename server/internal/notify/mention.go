package notify

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxMentions bounds the names one text can name, so a hostile text cannot
// make a large lookup.
const maxMentions = 50

// mentionRe finds "@name" candidates. A name is the existing
// users_username_format: [a-z][a-z0-9_-]{0,38}, matched without case. The
// text before the "@" is checked by the caller (see ParseMentions).
var mentionRe = regexp.MustCompile(`(?i)@([a-z][a-z0-9_-]*)`)

// ParseMentions returns the distinct usernames a text mentions, lowercase, in
// order of first appearance (decisions-T13 #17). It is pure: it does not know
// who exists.
//
// A mention is "@name" where name is 1 to 39 characters of the username
// format, not preceded by a letter (any script), digit, "_", "." or "@" (so "a@example.com" is
// not one), and not inside an inline code span, a fenced code block (``` or
// ~~~) or a quoted line (">"). A name longer than 39 characters is not a
// username and makes no mention.
func ParseMentions(text string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] && len(out) < maxMentions {
			seen[n] = true
			out = append(out, n)
		}
	}
	var fence string // the opening fence marker, while inside a fenced block
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if m := fenceOpen(trimmed); m != "" && len(line)-len(trimmed) < 4 {
			fence = m
			continue
		}
		if strings.HasPrefix(trimmed, ">") && len(line)-len(trimmed) < 4 {
			continue
		}
		line = blankCodeSpans(line)
		for _, loc := range mentionRe.FindAllStringSubmatchIndex(line, -1) {
			start, nameStart, nameEnd := loc[0], loc[2], loc[3]
			if start > 0 {
				r, _ := utf8.DecodeLastRuneInString(line[:start])
				if isMentionBlocker(r) {
					continue
				}
			}
			name := line[nameStart:nameEnd]
			if len(name) > 39 {
				continue
			}
			add(strings.ToLower(name))
			// "@chen-" or "@chen_": the dash or underscore may be text
			// after a name. Offer the trimmed name too; the lookup decides.
			if t := strings.TrimRight(name, "-_"); t != name && t != "" {
				add(strings.ToLower(t))
			}
		}
	}
	return out
}

// fenceOpen returns the fence marker ("```" or "~~~", at least three
// characters) that a line opens, or "".
func fenceOpen(trimmed string) string {
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(trimmed) && trimmed[n] == c {
			n++
		}
		if n >= 3 {
			return strings.Repeat(string(c), n)
		}
	}
	return ""
}

func isMentionBlocker(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '.' || r == '@'
}

// blankCodeSpans replaces every inline code span of a line, backticks
// included, with spaces. A run of n backticks closes at the next run of
// exactly n; an unclosed run is plain text.
func blankCodeSpans(line string) string {
	if !strings.Contains(line, "`") {
		return line
	}
	b := []byte(line)
	i := 0
	for i < len(b) {
		if b[i] != '`' {
			i++
			continue
		}
		n := 0
		for i+n < len(b) && b[i+n] == '`' {
			n++
		}
		j := i + n
		closed := -1
		for j < len(b) {
			if b[j] != '`' {
				j++
				continue
			}
			m := 0
			for j+m < len(b) && b[j+m] == '`' {
				m++
			}
			if m == n {
				closed = j + m
				break
			}
			j += m
		}
		if closed < 0 {
			i += n
			continue
		}
		for k := i; k < closed; k++ {
			b[k] = ' '
		}
		i = closed
	}
	return string(b)
}
