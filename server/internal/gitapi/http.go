package gitapi

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"devops/internal/netguard"
)

// NewHTTPClient builds the one HTTP client the provider clients use: the
// netguard dialer (always-refused ranges, the git policy), no proxy, no
// redirect. The caller builds the Dialer from netguard.GitPolicy.
func NewHTTPClient(d *netguard.Dialer) *http.Client {
	return netguard.NewClient(d, RequestTimeout)
}

// SafeSegment reports whether s may be one path segment: it is not "." or "..".
// The segment regexes allow dots, and a dot segment would climb the path.
func SafeSegment(s string) bool { return s != "." && s != ".." }

var numRE = regexp.MustCompile(`^[0-9]{1,18}$`)

// IsNumeric reports whether s is a plain decimal id (an id this package made).
func IsNumeric(s string) bool { return numRE.MatchString(s) }

// PageNumber returns the page a cursor asks for and its provider parameter
// when that parameter is a page number.
func PageNumber(cursor string) (page int, param string, err error) {
	page, param, err = DecodeCursor(cursor)
	if err != nil {
		return 0, "", err
	}
	if param != "" {
		if _, aerr := strconv.Atoi(param); aerr != nil {
			return 0, "", ErrInvalidCursor
		}
	}
	return page, param, nil
}

// NextLink returns the URL of rel="next" in a Link header, or "". A URL that
// holds a comma is not supported (the hosts do not send one).
func NextLink(h http.Header) string {
	for _, v := range h.Values("Link") {
		for _, item := range strings.Split(v, ",") {
			parts := strings.Split(item, ";")
			u := strings.TrimSpace(parts[0])
			if !strings.HasPrefix(u, "<") || !strings.HasSuffix(u, ">") {
				continue
			}
			for _, a := range parts[1:] {
				a = strings.ReplaceAll(strings.TrimSpace(a), " ", "")
				if a == `rel="next"` || a == "rel=next" {
					return u[1 : len(u)-1]
				}
			}
		}
	}
	return ""
}
