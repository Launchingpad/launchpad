package mcp

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -update rewrites the golden files: go test ./internal/mcp/ -run Golden -update
var update = flag.Bool("update", false, "rewrite testdata/*.golden.json")

func TestParseProfile(t *testing.T) {
	for in, want := range map[string]Profile{"": ProfileFull, "full": ProfileFull, "reviewer": ProfileReviewer} {
		got, err := ParseProfile(in)
		if err != nil || got != want {
			t.Errorf("ParseProfile(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseProfile("admin"); err == nil {
		t.Error("ParseProfile(admin) accepted")
	}
}

func TestTool_ParseArgs(t *testing.T) {
	tl := &tool{Name: "x", Params: []param{
		{Name: "s", Type: "string", Required: true},
		{Name: "n", Type: "integer", Nullable: true},
		{Name: "b", Type: "boolean"},
		{Name: "l", Type: "array"},
		{Name: "e", Type: "string", Enum: []string{"a", "b"}},
		{Name: "f", Type: "number"},
	}}
	tests := []struct {
		name    string
		raw     string
		wantErr string
		check   func(t *testing.T, a args)
	}{
		{name: "all types", raw: `{"s":"v","n":3,"b":true,"l":["x","y"],"e":"a","f":1.5}`, check: func(t *testing.T, a args) {
			if a.str("s") != "v" || a.integer("n") != 3 || !a.boolean("b") || len(a.list("l")) != 2 || a.str("e") != "a" {
				t.Errorf("args = %v", a.m)
			}
		}},
		{name: "string coercions", raw: `{"s":"v","n":"42","b":"false"}`, check: func(t *testing.T, a args) {
			if a.integer("n") != 42 || a.boolean("b") || !a.has("b") {
				t.Errorf("args = %v", a.m)
			}
		}},
		{name: "null on nullable", raw: `{"s":"v","n":null}`, check: func(t *testing.T, a args) {
			if !a.isNull("n") || a.set("n") || a.integer("n") != 0 {
				t.Errorf("args = %v", a.m)
			}
		}},
		{name: "integral float", raw: `{"s":"v","n":7.0}`, check: func(t *testing.T, a args) {
			if string(a.raw("n")) != "7" {
				t.Errorf("n = %s", a.raw("n"))
			}
		}},
		{name: "missing required", raw: `{}`, wantErr: `"s" is required`},
		{name: "required null", raw: `{"s":null}`, wantErr: "may not be null"},
		{name: "unknown", raw: `{"s":"v","zzz":1}`, wantErr: `unknown argument "zzz"`},
		{name: "fraction", raw: `{"s":"v","n":1.5}`, wantErr: "must be an integer"},
		{name: "bad bool", raw: `{"s":"v","b":"maybe"}`, wantErr: "must be a boolean"},
		{name: "bad list", raw: `{"s":"v","l":[1]}`, wantErr: "array of strings"},
		{name: "enum", raw: `{"s":"v","e":"c"}`, wantErr: "one of a, b"},
		{name: "not an object", raw: `[1]`, wantErr: "JSON object"},
		{name: "number", raw: `{"s":"v","f":"x"}`, wantErr: "must be a number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := tl.parseArgs(json.RawMessage(tt.raw))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, a)
		})
	}
	// No arguments at all is an empty object.
	tl2 := &tool{Name: "y"}
	if _, err := tl2.parseArgs(nil); err != nil {
		t.Errorf("nil args: %v", err)
	}
}

func TestTool_InputSchema(t *testing.T) {
	tl := &tool{Params: []param{
		{Name: "a", Type: "string", Required: true, Desc: "d"},
		{Name: "b", Type: "array", Nullable: true},
		{Name: "c", Type: "string", Enum: []string{"x"}},
	}}
	b, _ := json.Marshal(tl.inputSchema())
	want := `{"additionalProperties":false,"properties":{"a":{"description":"d","type":"string"},` +
		`"b":{"description":"","items":{"type":"string"},"type":["array","null"]},` +
		`"c":{"description":"","enum":["x"],"type":"string"}},"required":["a"],"type":"object"}`
	if string(b) != want {
		t.Errorf("schema =\n%s\nwant\n%s", b, want)
	}
}

// The exact tools/list result of each profile (names, descriptions, input
// schemas, annotations) is pinned in a committed golden file, so any change
// to a tool's contract shows up in review as a diff of that file.
func TestToolsList_Golden(t *testing.T) {
	for _, tt := range []struct {
		profile Profile
		file    string
	}{
		{ProfileFull, "tools_full.golden.json"},
		{ProfileReviewer, "tools_reviewer.golden.json"},
	} {
		t.Run(string(tt.profile), func(t *testing.T) {
			s := newSession(t, tt.profile, newFakeAPI(t, nil))
			_, resp := s.exchange(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
			raw, ok := resp["1"].Result.(json.RawMessage)
			if !ok || resp["1"].Error != nil {
				t.Fatalf("tools/list: %+v", resp["1"])
			}
			var got bytes.Buffer
			if err := json.Indent(&got, raw, "", "  "); err != nil {
				t.Fatal(err)
			}
			got.WriteByte('\n')
			path := filepath.Join("testdata", tt.file)
			if *update {
				if err := os.WriteFile(path, got.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Errorf("tools/list for %s differs from %s; if the change is meant, run with -update and review the diff.\ngot:\n%s",
					tt.profile, path, got.String())
			}
		})
	}
}
