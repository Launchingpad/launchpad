package mcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Every list tool returns this shape and at most maxLimit items.
const (
	defaultLimit = 50
	maxLimit     = 250
)

// Page is the {items, nextCursor} result of every list tool.
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

// limitOf returns the limit argument, defaulted to 50 and capped at 250.
func limitOf(a args) (int, error) {
	if !a.set("limit") {
		return defaultLimit, nil
	}
	n := a.integer("limit")
	if n < 1 {
		return 0, fmt.Errorf("limit must be at least 1")
	}
	return int(min(n, maxLimit)), nil
}

// pageQuery adds limit and cursor to q for a route the API pages itself.
func pageQuery(a args, q url.Values) error {
	n, err := limitOf(a)
	if err != nil {
		return err
	}
	q.Set("limit", strconv.Itoa(n))
	if c := a.str("cursor"); c != "" {
		q.Set("cursor", c)
	}
	return nil
}

// Several routes (teams, states, labels, users, comments) return every row
// at once. For those the MCP server pages the rows itself, with an offset
// cursor of its own. The prefix keeps it apart from the API's keyset cursors.
const offsetCursorPrefix = "mcpo1:"

func encodeOffset(n int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(offsetCursorPrefix + strconv.Itoa(n)))
}

func decodeOffset(c string) (int, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || !strings.HasPrefix(string(b), offsetCursorPrefix) {
		return 0, fmt.Errorf("malformed cursor")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(string(b), offsetCursorPrefix))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("malformed cursor")
	}
	return n, nil
}

// slicePage cuts one page out of all, per the limit and cursor arguments.
func slicePage[T any](a args, all []T) (Page[T], error) {
	n, err := limitOf(a)
	if err != nil {
		return Page[T]{}, err
	}
	off := 0
	if c := a.str("cursor"); c != "" {
		if off, err = decodeOffset(c); err != nil {
			return Page[T]{}, err
		}
	}
	if off > len(all) {
		off = len(all)
	}
	end := min(off+n, len(all))
	p := Page[T]{Items: all[off:end:end]}
	if p.Items == nil {
		p.Items = []T{}
	}
	if end < len(all) {
		c := encodeOffset(end)
		p.NextCursor = &c
	}
	return p, nil
}

// rawPage is an API page with items kept as raw JSON, so the MCP result
// carries every field the API returns.
type rawPage = Page[json.RawMessage]
