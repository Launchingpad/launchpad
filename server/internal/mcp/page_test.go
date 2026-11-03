package mcp

import (
	"encoding/json"
	"net/url"
	"testing"
)

func argsOf(t *testing.T, raw string) args {
	t.Helper()
	tl := &tool{Params: []param{pLimit, pCursor}}
	a, err := tl.parseArgs(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestLimitOf(t *testing.T) {
	for raw, want := range map[string]int{`{}`: 50, `{"limit":1}`: 1, `{"limit":250}`: 250, `{"limit":251}`: 250, `{"limit":100000}`: 250} {
		got, err := limitOf(argsOf(t, raw))
		if err != nil || got != want {
			t.Errorf("limitOf(%s) = %d, %v; want %d", raw, got, err, want)
		}
	}
	if _, err := limitOf(argsOf(t, `{"limit":-3}`)); err == nil {
		t.Error("negative limit accepted")
	}
}

func TestPageQuery(t *testing.T) {
	q := url.Values{}
	if err := pageQuery(argsOf(t, `{"limit":900,"cursor":"k1"}`), q); err != nil {
		t.Fatal(err)
	}
	if q.Get("limit") != "250" || q.Get("cursor") != "k1" {
		t.Errorf("q = %v", q)
	}
}

func TestSlicePage(t *testing.T) {
	all := make([]int, 7)
	for i := range all {
		all[i] = i
	}
	p, err := slicePage(argsOf(t, `{"limit":3}`), all)
	if err != nil || len(p.Items) != 3 || p.NextCursor == nil {
		t.Fatalf("page 1 = %+v, %v", p, err)
	}
	p2, err := slicePage(argsOf(t, `{"limit":3,"cursor":"`+*p.NextCursor+`"}`), all)
	if err != nil || p2.Items[0] != 3 || p2.NextCursor == nil {
		t.Fatalf("page 2 = %+v, %v", p2, err)
	}
	p3, err := slicePage(argsOf(t, `{"limit":3,"cursor":"`+*p2.NextCursor+`"}`), all)
	if err != nil || len(p3.Items) != 1 || p3.NextCursor != nil {
		t.Fatalf("page 3 = %+v, %v", p3, err)
	}
	// Appending to a page must not write into the caller's slice.
	_ = append(p.Items, 99)
	if all[3] != 3 {
		t.Fatal("a page aliases the source slice")
	}
	past, err := slicePage(argsOf(t, `{"cursor":"`+encodeOffset(100)+`"}`), all)
	if err != nil || len(past.Items) != 0 || past.Items == nil || past.NextCursor != nil {
		t.Errorf("past the end = %+v, %v", past, err)
	}
	for _, bad := range []string{"x", encodeOffset(-1)[:3], "bWNwbzE6LTE"} { // last: "mcpo1:-1"
		if _, err := slicePage(argsOf(t, `{"cursor":"`+bad+`"}`), all); err == nil {
			t.Errorf("cursor %q accepted", bad)
		}
	}
}
