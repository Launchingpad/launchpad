package tracker

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestParseIdentifier(t *testing.T) {
	tests := []struct {
		in     string
		key    string
		number int32
		ok     bool
	}{
		{in: "ORB-123", key: "ORB", number: 123, ok: true},
		{in: "orb-7", key: "ORB", number: 7, ok: true},
		{in: "A1-1", key: "A1", number: 1, ok: true},
		{in: "ORB-0", ok: false},
		{in: "ORB-", ok: false},
		{in: "1RB-2", ok: false},
		{in: "ORB-99999999999", ok: false},
		{in: "01a16d1e-fd0c-797c-8e74-679e26a7276f", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			key, n, ok := ParseIdentifier(tt.in)
			if ok != tt.ok || key != tt.key || n != tt.number {
				t.Fatalf("got (%q, %d, %v), want (%q, %d, %v)", key, n, ok, tt.key, tt.number, tt.ok)
			}
		})
	}
}

func TestSlugAndBranchName(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{name: "simple", title: "Fix the login bug", want: "chen/orb-12-fix-the-login-bug"},
		{name: "punctuation", title: "  ORB: Café -- crash!! ", want: "chen/orb-12-orb-caf-crash"},
		{name: "cut at 40", title: "aaaaaaaaaa bbbbbbbbbb cccccccccc ddddddd eeee", want: "chen/orb-12-aaaaaaaaaa-bbbbbbbbbb-cccccccccc-ddddddd"},
		{name: "cut lands on a dash", title: "aaaaaaaaaa bbbbbbbbbb cccccccccc dddddd eeee", want: "chen/orb-12-aaaaaaaaaa-bbbbbbbbbb-cccccccccc-dddddd"},
		{name: "empty slug", title: "!!!", want: "chen/orb-12"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GitBranchName("chen", "ORB", 12, tt.title)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			slug := strings.TrimPrefix(strings.TrimPrefix(got, "chen/orb-12"), "-")
			if len(slug) > 40 {
				t.Fatalf("slug %q longer than 40", slug)
			}
		})
	}
}

func TestParseTimeFilter(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in      string
		want    time.Time
		wantErr bool
	}{
		{in: "-P1D", want: now.AddDate(0, 0, -1)},
		{in: "P1D", want: now.AddDate(0, 0, -1)},
		{in: "-P2W", want: now.AddDate(0, 0, -14)},
		{in: "-PT3H", want: now.Add(-3 * time.Hour)},
		{in: "-P1M", want: now.AddDate(0, -1, 0)},
		{in: "-P1DT1H30M", want: now.AddDate(0, 0, -1).Add(-90 * time.Minute)},
		{in: "2026-10-01T00:00:00Z", want: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		{in: "2026-10-01", want: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		{in: "P", wantErr: true},
		{in: "-PT", wantErr: true},
		{in: "yesterday", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseTimeFilter(tt.in, now)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}
				return
			}
			if err != nil || !got.Equal(tt.want) {
				t.Fatalf("got %v (%v), want %v", got, err, tt.want)
			}
		})
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := cursor{OrderBy: "updatedAt", At: time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC), ID: uuid.New()}
	got, err := decodeCursor(encodeCursor(c))
	if err != nil || got.ID != c.ID || !got.At.Equal(c.At) || got.OrderBy != c.OrderBy {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for _, bad := range []string{"", "!!", "e30", "bm90IGpzb24"} {
		if _, err := decodeCursor(bad); err == nil {
			t.Fatalf("decodeCursor(%q) succeeded", bad)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	s, cut := truncateRunes("héllo", 3)
	if s != "hél" || !cut {
		t.Fatalf("got %q %v", s, cut)
	}
	s, cut = truncateRunes("abc", 3)
	if s != "abc" || cut {
		t.Fatalf("got %q %v", s, cut)
	}
}

func TestPriorityLabel(t *testing.T) {
	want := []string{"No priority", "Urgent", "High", "Medium", "Low"}
	for p, w := range want {
		if got := PriorityLabel(int16(p)); got != w {
			t.Fatalf("PriorityLabel(%d) = %q, want %q", p, got, w)
		}
	}
}

func TestErrorMappersKeepNil(t *testing.T) {
	if err := notFound(nil, "issue"); err != nil {
		t.Fatalf("notFound(nil) = %v", err)
	}
	if err := invalidRef(nil, "x"); err != nil {
		t.Fatalf("invalidRef(nil) = %v", err)
	}
}

func TestParseTicketRef(t *testing.T) {
	for _, tt := range []struct {
		in  string
		key string
		n   int32
		ok  bool
	}{
		{"LAUN-113", "LAUN", 113, true},
		{"laun-113", "LAUN", 113, true},
		{" #laun-113 ", "LAUN", 113, true},
		{"LAUN - 113", "LAUN", 113, true},
		{"113", "", 113, true},
		{"#113", "", 113, true},
		{" # 113 ", "", 113, true},
		{"#", "", 0, false},
		{"LAUN-", "", 0, false},
		{"\u0663", "", 0, false},                  // Arabic-indic digit
		{"LAUN-\uff11\uff11\uff13", "", 0, false}, // full-width digits
		{"LAUN-113abc", "", 0, false},
		{"A-B-1", "", 0, false},
		{"2147483647", "", 2147483647, true},
		{"LAUN-2147483647", "LAUN", 2147483647, true},
		{"2147483648", "", 0, false},
		{"0", "", 0, false},
		{"LAUN-0", "", 0, false},
		{"113 bugs", "", 0, false},
		{"99999999999", "", 0, false},
		{"", "", 0, false},
	} {
		k, n, ok := ParseTicketRef(tt.in)
		if k != tt.key || n != tt.n || ok != tt.ok {
			t.Errorf("%q: got (%q,%d,%v)", tt.in, k, n, ok)
		}
	}
}
