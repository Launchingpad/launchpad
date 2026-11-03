package tracker

import (
	"encoding/base64"
	"math"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRankBetween(t *testing.T) {
	if r, ok := rankBetween(1024, 2048); !ok || r != 1536 {
		t.Fatalf("1024..2048 = %v %v", r, ok)
	}
	if r, ok := rankBetween(-2048, -1024); !ok || r != -1536 {
		t.Fatalf("negative = %v %v", r, ok)
	}
	// Equal ranks and a reversed pair have no room.
	for _, p := range [][2]float64{{5, 5}, {6, 5}, {1, math.NaN()}} {
		if _, ok := rankBetween(p[0], p[1]); ok {
			t.Fatalf("%v accepted", p)
		}
	}
	// A gap of 1024 can be halved 21 times (the last one splits a gap of
	// 1/1024) and then asks for a rebalance.
	lo, hi, n := 0.0, 1024.0, 0
	for {
		mid, ok := rankBetween(lo, hi)
		if !ok {
			break
		}
		if !(mid > lo && mid < hi) {
			t.Fatalf("mid %v outside (%v, %v)", mid, lo, hi)
		}
		hi = mid // always drop into the same gap, next to lo
		n++
	}
	if n != 21 {
		t.Fatalf("%d midpoints before a rebalance, want 21", n)
	}
	// Large ranks: the relative guard asks for a rebalance before the float
	// step eats the gap.
	if _, ok := rankBetween(1e11, 1e11+1e-3); ok {
		t.Fatal("a gap near the float step was accepted")
	}
	if _, ok := rankBetween(1e11, 1e11+1024); !ok {
		t.Fatal("a normal gap at a large rank was refused")
	}
}

func TestPlacementValidation(t *testing.T) {
	one, nan, huge := 1.0, math.NaN(), 1e13
	for name, in := range map[string]UpdateIssueInput{
		"two forms":      {SortOrder: &one, PlaceAfter: "ORB-1"},
		"after+before":   {PlaceAfter: "ORB-1", PlaceBefore: "ORB-2"},
		"nan":            {SortOrder: &nan},
		"too large":      {SortOrder: &huge},
		"rank and below": {SortOrder: &one, PlaceBefore: "ORB-1"},
	} {
		if _, err := in.placement(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, in := range map[string]UpdateIssueInput{
		"none": {}, "rank": {SortOrder: &one}, "after": {PlaceAfter: "ORB-1"}, "before": {PlaceBefore: "ORB-1"},
	} {
		if _, err := in.placement(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestParseListFilter_Manual(t *testing.T) {
	now := time.Now()
	for query, want := range map[string]string{
		"sort=manual": "manual", "orderBy=manual": "manual", "sort=manual&orderBy=manual": "manual",
		"": "updatedAt", "orderBy=createdAt": "createdAt",
	} {
		v, _ := url.ParseQuery(query)
		f, err := ParseListFilter(v, now)
		if err != nil || f.OrderBy != want {
			t.Errorf("%q: %q, %v", query, f.OrderBy, err)
		}
	}
	for _, query := range []string{"sort=updatedAt", "sort=manual&orderBy=createdAt", "orderBy=rank"} {
		v, _ := url.ParseQuery(query)
		if _, err := ParseListFilter(v, now); err == nil {
			t.Errorf("%q accepted", query)
		}
	}
	// A cursor made for another order is refused for manual, and back.
	c := encodeCursor(cursor{OrderBy: "updatedAt", At: now, ID: uuid.New()})
	if _, err := ParseListFilter(url.Values{"sort": {"manual"}, "cursor": {c}}, now); err == nil {
		t.Error("updatedAt cursor accepted for manual")
	}
	rank := -1536.0000000000002
	mc := encodeCursor(cursor{OrderBy: "manual", Rank: &rank, ID: uuid.New()})
	f, err := ParseListFilter(url.Values{"sort": {"manual"}, "cursor": {mc}}, now)
	if err != nil || f.Cursor == nil || f.Cursor.Rank == nil || *f.Cursor.Rank != rank {
		t.Fatalf("manual cursor: %+v, %v", f.Cursor, err)
	}
	if _, err := ParseListFilter(url.Values{"cursor": {mc}}, now); err == nil {
		t.Error("manual cursor accepted for updatedAt")
	}
}

func TestDecodeCursor_NeedsATimeUnlessManual(t *testing.T) {
	b := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	id := uuid.New().String()
	for name, raw := range map[string]string{
		"no t":                  `{"o":"createdAt","id":"` + id + `"}`,
		"manual without a rank": `{"o":"manual","id":"` + id + `"}`,
		"rank on another order": `{"o":"createdAt","r":1,"id":"` + id + `"}`,
	} {
		if _, err := decodeCursor(b(raw)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := decodeCursor(b(`{"o":"manual","r":-3,"id":"` + id + `"}`)); err != nil {
		t.Errorf("manual cursor refused: %v", err)
	}
}
