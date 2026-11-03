package tracker

import (
	"net/url"
	"slices"
	"strconv"

	"github.com/google/uuid"

	"devops/internal/httpx"
)

// Shared parsing for the T3 list endpoints. They follow the issue list: an
// unknown parameter is a 400 (decisions-T1 #16), limit is 1-250 (default
// 50), and the cursor is the same opaque keyset value, tagged with the list
// it was made for so a project cursor cannot be replayed on documents.

const (
	defaultListLimit = 50
	maxListLimit     = 250
)

// listQuery is the part every T3 list shares.
type listQuery struct {
	Limit           int
	Cursor          *cursor
	IncludeArchived bool
}

// parseListQuery checks the parameter names against known (plus limit,
// cursor and includeArchived) and reads the shared ones. kind tags the
// cursor.
func parseListQuery(v url.Values, kind string, known ...string) (listQuery, error) {
	for k := range v {
		if k != "limit" && k != "cursor" && k != "includeArchived" && !slices.Contains(known, k) {
			return listQuery{}, httpx.Invalid("unknown query parameter %q", k)
		}
	}
	lq := listQuery{Limit: defaultListLimit}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > maxListLimit {
			return lq, httpx.Invalid("limit must be 1-%d", maxListLimit)
		}
		lq.Limit = n
	}
	var err error
	if lq.IncludeArchived, err = parseBool(v.Get("includeArchived")); err != nil {
		return lq, httpx.Invalid("includeArchived must be true or false")
	}
	if c := v.Get("cursor"); c != "" {
		cur, err := decodeCursor(c)
		if err != nil || cur.OrderBy != kind {
			return lq, httpx.Invalid("malformed cursor")
		}
		lq.Cursor = &cur
	}
	return lq, nil
}

// parseUUIDParam reads an optional id filter. A value that is not a UUID is
// a 400, never "matches nothing".
func parseUUIDParam(v url.Values, name string) (*uuid.UUID, error) {
	s := v.Get(name)
	if s == "" {
		return nil, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil, httpx.Invalid("%s must be an id", name)
	}
	return &id, nil
}
