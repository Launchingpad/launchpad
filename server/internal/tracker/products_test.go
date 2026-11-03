package tracker

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSlugFromName(t *testing.T) {
	for in, want := range map[string]string{
		"Launchpad":               "launchpad",
		"Café Ünit":               "cafe-unit",
		"  A  b--c  ":             "a-b-c",
		"ＦＵＬＬ width":              "full-width",
		"3D Tools":                "3d-tools",
		"日本語":                     "",
		"":                        "",
		strings.Repeat("ab ", 30): strings.TrimRight(strings.Repeat("ab-", 14)+"ab", "-"),
	} {
		got := slugFromName(in)
		if len(got) > 40 || strings.HasSuffix(got, "-") {
			t.Errorf("%q: %q is not trimmed to 40", in, got)
		}
		if in != strings.Repeat("ab ", 30) && got != want {
			t.Errorf("slugFromName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseProductRef(t *testing.T) {
	id := uuid.New()
	for _, c := range []struct {
		ref  string
		isID bool
	}{
		{id.String(), true},
		{strings.ToUpper(id.String()), true},
		{strings.ReplaceAll(id.String(), "-", ""), false},
		{"{" + id.String() + "}", false},
		{"urn:uuid:" + id.String(), false},
		{"launchpad", false},
		{"", false},
	} {
		if _, ok := parseProductRef(c.ref); ok != c.isID {
			t.Errorf("parseProductRef(%q) = %v, want %v", c.ref, ok, c.isID)
		}
	}
	if got, _ := parseProductRef(strings.ToUpper(id.String())); got != id {
		t.Error("upper-case id parsed to another id")
	}
}
