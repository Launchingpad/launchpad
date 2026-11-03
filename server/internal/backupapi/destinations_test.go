package backupapi

import (
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/platform"
)

func ip(i int) *int { return &i }

func TestSchedule_Validate(t *testing.T) {
	base := schedule{kind: "daily", atUTC: "04:15", keepLast: 14, verify: 7}
	cases := []struct {
		name    string
		mod     func(*schedule)
		confirm bool
		bad     bool
	}{
		{"defaults", func(*schedule) {}, false, false},
		{"weekly with a weekday", func(s *schedule) { s.kind, s.weekday = "weekly", ip(0) }, false, false},
		{"weekly without a weekday", func(s *schedule) { s.kind = "weekly" }, false, true},
		{"weekly with weekday 7", func(s *schedule) { s.kind, s.weekday = "weekly", ip(7) }, false, true},
		{"daily with a weekday", func(s *schedule) { s.weekday = ip(1) }, false, true},
		{"unknown schedule", func(s *schedule) { s.kind = "hourly" }, false, true},
		{"time 24:00", func(s *schedule) { s.atUTC = "24:00" }, false, true},
		{"time 4:15", func(s *schedule) { s.atUTC = "4:15" }, false, true},
		{"time 23:59", func(s *schedule) { s.atUTC = "23:59" }, false, false},
		{"keep 0", func(s *schedule) { s.keepLast = 0 }, true, true},
		{"keep 366", func(s *schedule) { s.keepLast = 366 }, true, true},
		{"keep 365", func(s *schedule) { s.keepLast = 365 }, false, false},
		{"keep 1 unconfirmed", func(s *schedule) { s.keepLast = 1 }, false, true},
		{"keep 1 confirmed", func(s *schedule) { s.keepLast = 1 }, true, false},
		{"verify -1", func(s *schedule) { s.verify = -1 }, false, true},
		{"verify 91", func(s *schedule) { s.verify = 91 }, false, true},
		{"verify 0 and 90", func(s *schedule) { s.verify = 90 }, false, false},
	}
	for _, c := range cases {
		sc := base
		c.mod(&sc)
		err := sc.validate(true, c.confirm)
		if (err != nil) != c.bad {
			t.Errorf("%s: err %v, want bad=%v", c.name, err, c.bad)
		}
	}
	// A PATCH that does not touch keepLast does not ask for the confirmation.
	one := base
	one.keepLast = 1
	if err := one.validate(false, false); err != nil {
		t.Errorf("an unchanged keepLast 1 asked for a confirmation: %v", err)
	}
}

func TestCheckName(t *testing.T) {
	for _, bad := range []string{"", "   ", strings.Repeat("x", 61), "a\nb", "a\x00b"} {
		if _, err := checkName(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if n, err := checkName("  Nextcloud  "); err != nil || n != "Nextcloud" {
		t.Errorf("trim: %q %v", n, err)
	}
	if _, err := checkName(strings.Repeat("é", 60)); err != nil {
		t.Errorf("60 runes: %v", err)
	}
}

func TestSecretSpecs(t *testing.T) {
	// The refresh token is never written by the API.
	if err := checkSecretName(TypeDropbox, "refreshToken", "x", true); err == nil {
		t.Error("the API accepted a refresh token value")
	}
	if err := checkSecretName(TypeDropbox, "refreshToken", "", true); err != nil {
		t.Errorf("clearing a refresh token: %v", err)
	}
	if err := checkSecretName(TypeS3, "password", "x", true); err == nil {
		t.Error("an s3 destination accepted a password")
	}
	if err := checkSecretName(TypeSFTP, "privateKey", strings.Repeat("k", maxSecretLen+1), true); err == nil {
		t.Error("an oversize secret was accepted")
	}
	cases := []struct {
		typ  string
		set  map[string]string
		want int
	}{
		{TypeS3, map[string]string{"accessKey": "a", "secretKey": "b"}, 0},
		{TypeS3, map[string]string{"accessKey": "a"}, 1},
		{TypeWebDAV, map[string]string{}, 0},
		{TypeSFTP, map[string]string{}, 1},
		{TypeSFTP, map[string]string{"privateKey": "k"}, 0},
		{TypeDropbox, map[string]string{"appKey": "a", "appSecret": "b"}, 1},
		{TypeDropbox, map[string]string{"appKey": "a", "appSecret": "b", "refreshToken": "r"}, 0},
	}
	for _, c := range cases {
		if got := missingSecrets(c.typ, c.set); len(got) != c.want {
			t.Errorf("%s %v: missing %v", c.typ, c.set, got)
		}
	}
}

func TestChangedFields(t *testing.T) {
	a := map[string]any{"url": "https://a", "bucket": "b", "nested": map[string]any{"k": 1.0}}
	b := map[string]any{"url": "https://a", "bucket": "c", "region": "eu", "nested": map[string]any{"k": 1.0}}
	if got := changedFields(a, b); !reflect.DeepEqual(got, []string{"bucket", "region"}) {
		t.Errorf("changed %v", got)
	}
	if got := changedFields(a, a); len(got) != 0 {
		t.Errorf("same config reports %v", got)
	}
	if got := changedFields(a, map[string]any{"url": "https://a", "bucket": "b"}); !reflect.DeepEqual(got, []string{"nested"}) {
		t.Errorf("removed key: %v", got)
	}
}

func TestParseConfig(t *testing.T) {
	for _, bad := range []string{"", "null", "[]", `"x"`, "1", "{", strings.Repeat(" ", maxConfigLen+1) + "{}"} {
		if _, _, err := parseConfig(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted config %.20q", bad)
		}
	}
	if _, c, err := parseConfig(json.RawMessage(` { "a" : 1 } `)); err != nil || string(c) != `{"a":1}` {
		t.Errorf("compact: %s %v", c, err)
	}
}

func TestParsePageQuery(t *testing.T) {
	q := func(s string) url.Values { v, _ := url.ParseQuery(s); return v }
	if pq, err := ParsePageQuery(q(""), true); err != nil || pq.Limit != 50 {
		t.Fatalf("defaults: %+v %v", pq, err)
	}
	id := uuid.New()
	if pq, err := ParsePageQuery(q("limit=200&destination="+id.String()), true); err != nil || pq.Limit != 200 || *pq.Destination != id {
		t.Fatalf("full: %+v %v", pq, err)
	}
	for _, bad := range []string{"limit=0", "limit=201", "limit=x", "cursor=!!", "destination=x", "foo=1", "cursor=" + "e30"} {
		if _, err := ParsePageQuery(q(bad), true); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := ParsePageQuery(q("destination="+id.String()), false); err == nil {
		t.Error("changes accepted a destination filter")
	}
	c := cursor{At: t0, ID: id}
	got, err := decodeCursor(c.encode())
	if err != nil || !got.At.Equal(t0) || got.ID != id {
		t.Fatalf("cursor round trip: %+v %v", got, err)
	}
}

// A platform.Secret in a request body prints as [redacted] whatever the verb.
func TestSecretInputsRedact(t *testing.T) {
	in := CreateDestinationInput{Secrets: map[string]platform.Secret{"secretKey": "planted-value"}}
	for _, s := range []string{
		fmtSprint(in), fmtSprintf("%+v", in), fmtSprintf("%#v", in),
	} {
		if strings.Contains(s, "planted-value") {
			t.Errorf("a secret printed: %s", s)
		}
	}
}
