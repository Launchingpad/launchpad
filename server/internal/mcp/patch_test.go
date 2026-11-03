package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestApplyPatch(t *testing.T) {
	const desc = "Intro.\n\n## Acceptance criteria\n\n1. one\n"
	tests := []struct {
		name    string
		ops     []patchOp
		want    string
		wantErr string
	}{
		{name: "append", ops: []patchOp{{Op: "append", Text: "\n2. two\n"}}, want: desc + "\n2. two\n"},
		{name: "prepend", ops: []patchOp{{Op: "prepend", Text: "UPDATE: x\n\n"}}, want: "UPDATE: x\n\n" + desc},
		{name: "insert_before", ops: []patchOp{{Op: "insert_before", Anchor: "## Acceptance criteria", Text: "## Notes\n\n"}},
			want: "Intro.\n\n## Notes\n\n## Acceptance criteria\n\n1. one\n"},
		{name: "insert_after", ops: []patchOp{{Op: "insert_after", Anchor: "1. one", Text: "\n2. two"}},
			want: "Intro.\n\n## Acceptance criteria\n\n1. one\n2. two\n"},
		{name: "replace", ops: []patchOp{{Op: "replace", OldString: "Intro.", NewString: "Intro, edited."}},
			want: "Intro, edited.\n\n## Acceptance criteria\n\n1. one\n"},
		{name: "in order", ops: []patchOp{{Op: "append", Text: "X"}, {Op: "replace", OldString: "X", NewString: "Y"}}, want: desc + "Y"},
		{name: "replace_all", ops: []patchOp{{Op: "replace", OldString: "one", NewString: "1", ReplaceAll: true}, {Op: "append", Text: "one one"}, {Op: "replace", OldString: "one", NewString: "I", ReplaceAll: true}},
			want: "Intro.\n\n## Acceptance criteria\n\n1. 1\nI I"},
		{name: "replace_all none", ops: []patchOp{{Op: "replace", OldString: "zzz", NewString: "y", ReplaceAll: true}}, wantErr: "not found"},
		{name: "replace_range", ops: []patchOp{{Op: "replace_range", From: "Intro.", To: "1. one", NewString: "New intro.\n\n"}},
			want: "New intro.\n\n1. one\n"},
		{name: "replace_range no to", ops: []patchOp{{Op: "replace_range", From: "Intro.", To: "zzz", NewString: "x"}}, wantErr: "to not found"},
		{name: "replace_range to before from", ops: []patchOp{{Op: "replace_range", From: "1. one", To: "Intro.", NewString: "x"}}, wantErr: "to not found after from"},
		{name: "anchor missing", ops: []patchOp{{Op: "insert_before", Anchor: "## Nope", Text: "x"}}, wantErr: "not found"},
		{name: "old_string twice", ops: []patchOp{{Op: "replace", OldString: "n", NewString: "N"}}, wantErr: "occurs"},
		{name: "empty anchor", ops: []patchOp{{Op: "insert_after", Text: "x"}}, wantErr: "anchor is empty"},
		{name: "unknown op", ops: []patchOp{{Op: "delete"}}, wantErr: "unknown op"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applyPatch(desc, tt.ops)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %q, %v\nwant %q", got, err, tt.want)
			}
		})
	}
}

func TestDecodePatch(t *testing.T) {
	if _, err := decodePatch(json.RawMessage(`[{"op":"append","text":"x","bogus":1}]`)); err == nil {
		t.Error("unknown key accepted")
	}
	if _, err := decodePatch(json.RawMessage(`[]`)); err == nil {
		t.Error("empty patch accepted")
	}
	ops, err := decodePatch(json.RawMessage(`[{"op":"replace","old_string":"a","new_string":"b"}]`))
	if err != nil || ops[0].OldString != "a" || ops[0].NewString != "b" {
		t.Errorf("ops = %+v, %v", ops, err)
	}
}
