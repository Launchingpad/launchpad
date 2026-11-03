package tracker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Identifier is team.key || '-' || number. It is computed on read only.
func Identifier(teamKey string, number int32) string {
	return teamKey + "-" + strconv.Itoa(int(number))
}

var identifierRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]{0,9})-([1-9][0-9]{0,9})$`)

// ParseIdentifier splits "ORB-123" (any case) into ("ORB", 123).
func ParseIdentifier(s string) (key string, number int32, ok bool) {
	m := identifierRe.FindStringSubmatch(s)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.ParseInt(m[2], 10, 32)
	if err != nil {
		return "", 0, false
	}
	return strings.ToUpper(m[1]), int32(n), true
}

var ticketRefRe = regexp.MustCompile(`^#?\s*(?:([A-Za-z][A-Za-z0-9]{0,9})\s*-\s*)?([1-9][0-9]{0,9})$`)

// ParseTicketRef reads a search query that names a ticket: "LAUN-113" (any
// case, spaces around the dash allowed), "#LAUN-113", "113" or "#113". For a
// bare number key is "". Surrounding space is ignored.
func ParseTicketRef(s string) (key string, number int32, ok bool) {
	m := ticketRefRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.ParseInt(m[2], 10, 32)
	if err != nil {
		return "", 0, false
	}
	return strings.ToUpper(m[1]), int32(n), true
}

// Slug lowercases s, turns every run of non [a-z0-9] into one '-', and cuts
// the result to at most max bytes without a trailing '-'.
func Slug(s string, max int) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			continue
		}
		dash = true
	}
	out := b.String()
	if len(out) > max {
		out = out[:max]
	}
	return strings.TrimRight(out, "-")
}

// GitBranchName is {username}/{team-key}-{number}-{slug(title, 40)}, all
// lowercase except the username (already lowercase by constraint). GitLab
// matching uses only the "orb-<n>" token.
func GitBranchName(username, teamKey string, number int32, title string) string {
	base := strings.ToLower(Identifier(teamKey, number))
	if slug := Slug(title, 40); slug != "" {
		base += "-" + slug
	}
	if username == "" {
		return base
	}
	return username + "/" + base
}

// PriorityLabel is Linear's name for a priority value.
func PriorityLabel(p int16) string {
	switch p {
	case 1:
		return "Urgent"
	case 2:
		return "High"
	case 3:
		return "Medium"
	case 4:
		return "Low"
	default:
		return "No priority"
	}
}

var isoDurationRe = regexp.MustCompile(`^(-)?P(?:(\d+)Y)?(?:(\d+)M)?(?:(\d+)W)?(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// ParseTimeFilter reads a createdAt/updatedAt filter: an ISO-8601 duration
// ("-P1D" and "P1D" both mean "since one day before now", as in Linear) or an
// RFC 3339 timestamp / YYYY-MM-DD date. It returns the lower bound.
func ParseTimeFilter(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	m := isoDurationRe.FindStringSubmatch(s)
	if m == nil || s == "P" || s == "-P" || strings.HasSuffix(s, "T") {
		return time.Time{}, fmt.Errorf("%q is not an ISO-8601 duration or a timestamp", s)
	}
	part := func(i int) int {
		if m[i] == "" {
			return 0
		}
		n, _ := strconv.Atoi(m[i])
		return n
	}
	t := now.AddDate(-part(2), -part(3), -(7*part(4) + part(5)))
	t = t.Add(-(time.Duration(part(6))*time.Hour + time.Duration(part(7))*time.Minute + time.Duration(part(8))*time.Second))
	return t, nil
}

// cursor is the opaque keyset position. It is base64url JSON so its shape can
// change later without breaking clients that treat it as opaque.
type cursor struct {
	OrderBy string    `json:"o"`
	At      time.Time `json:"t"`
	ID      uuid.UUID `json:"id"`
	// Rank is the sort_order of the last item of a sort=manual page. A
	// float64 round-trips exactly through JSON, so the keyset is exact.
	Rank *float64 `json:"r,omitempty"`
	// Match is set when the last item of a ticket-number search page was the
	// ticket the query names (LAUN-133): those rows sort before the rest.
	Match bool `json:"m,omitempty"`
}

func encodeCursor(c cursor) string {
	b, _ := json.Marshal(c) // a struct of plain fields cannot fail to marshal
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, fmt.Errorf("malformed cursor")
	}
	var c cursor
	if err := json.Unmarshal(b, &c); err != nil || c.ID == uuid.Nil || (c.At.IsZero() && !(c.OrderBy == "manual" && c.Rank != nil)) {
		return cursor{}, fmt.Errorf("malformed cursor")
	}
	return c, nil
}

// truncateRunes cuts s to n runes and reports whether it cut anything.
func truncateRunes(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos], true
		}
		i++
	}
	return s, false
}
