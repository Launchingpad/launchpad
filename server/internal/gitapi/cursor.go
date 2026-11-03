package gitapi

import (
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
)

// A cursor sent to the browser is opaque and holds only the provider's page
// parameter and the page's place in the list; never a URL (T17 #32 rule a).
// The follow-up URL is rebuilt from the API root, a path the client knows and
// that parameter (rule b).

var (
	cursorRE = regexp.MustCompile(`^[A-Za-z0-9_.~-]{1,200}$`)
	paramRE  = regexp.MustCompile(`^[A-Za-z0-9_.~-]{1,100}$`)
)

// ValidParam reports whether s may be a page parameter.
func ValidParam(s string) bool { return paramRE.MatchString(s) }

// EncodeCursor makes the cursor of page number `page` (2 to MaxPages) whose
// provider page parameter is param. It returns "" when the list is at its page
// limit or the parameter is not acceptable (the list ends there).
func EncodeCursor(page int, param string) string {
	if page < 2 || page > MaxPages || !ValidParam(param) {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(page) + "." + param))
}

// DecodeCursor reads a cursor. The empty cursor is page 1 with no parameter.
func DecodeCursor(c string) (page int, param string, err error) {
	if c == "" {
		return 1, "", nil
	}
	if !cursorRE.MatchString(c) {
		return 0, "", ErrInvalidCursor
	}
	b, derr := base64.RawURLEncoding.DecodeString(c)
	if derr != nil {
		return 0, "", ErrInvalidCursor
	}
	n, p, ok := strings.Cut(string(b), ".")
	if !ok {
		return 0, "", ErrInvalidCursor
	}
	page, aerr := strconv.Atoi(n)
	if aerr != nil || page < 2 || page > MaxPages || !ValidParam(p) {
		return 0, "", ErrInvalidCursor
	}
	return page, p, nil
}
