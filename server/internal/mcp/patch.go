package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// patchOp is one edit of save_issue's `patch` argument, the shape Linear's
// save_issue takes for a description edit. Texts are inserted exactly as
// given: callers put their own separators ("\n\n...") in the text.
type patchOp struct {
	Op         string `json:"op"`
	Text       string `json:"text"`
	Anchor     string `json:"anchor"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
	From       string `json:"from"`
	To         string `json:"to"`
}

// patchOps is the JSON schema of one `patch` item.
var patchItemSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"op":          map[string]any{"type": "string", "enum": []string{"append", "prepend", "insert_before", "insert_after", "replace", "replace_range"}},
		"text":        map[string]any{"type": "string", "description": "append, prepend, insert_before, insert_after"},
		"anchor":      map[string]any{"type": "string", "description": "insert_before, insert_after: text that occurs exactly once"},
		"old_string":  map[string]any{"type": "string", "description": "replace: text that occurs exactly once"},
		"new_string":  map[string]any{"type": "string", "description": "replace, replace_range"},
		"replace_all": map[string]any{"type": "boolean", "description": "replace: every occurrence (at least one) instead of exactly one"},
		"from":        map[string]any{"type": "string", "description": "replace_range: start of the range (kept out), occurs exactly once"},
		"to":          map[string]any{"type": "string", "description": "replace_range: text after the range (kept), the first occurrence after from"},
	},
	"required":             []string{"op"},
	"additionalProperties": false,
}

func decodePatch(raw json.RawMessage) ([]patchOp, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var ops []patchOp
	if err := dec.Decode(&ops); err != nil {
		return nil, fmt.Errorf("patch must be an array of {op, text, anchor, old_string, new_string, replace_all, from, to}: %w", err)
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("patch is empty")
	}
	return ops, nil
}

// applyPatch applies ops to desc in order. An anchor or old_string must occur
// exactly once, so an edit never lands in the wrong place.
func applyPatch(desc string, ops []patchOp) (string, error) {
	once := func(i int, what, s string) (int, error) {
		if s == "" {
			return 0, fmt.Errorf("patch %d (%s): %s is empty", i, ops[i].Op, what)
		}
		switch n := strings.Count(desc, s); n {
		case 1:
			return strings.Index(desc, s), nil
		case 0:
			return 0, fmt.Errorf("patch %d (%s): %s not found in the description", i, ops[i].Op, what)
		default:
			return 0, fmt.Errorf("patch %d (%s): %s occurs %d times; give more context", i, ops[i].Op, what, n)
		}
	}
	for i, op := range ops {
		switch op.Op {
		case "append":
			desc += op.Text
		case "prepend":
			desc = op.Text + desc
		case "insert_before":
			at, err := once(i, "anchor", op.Anchor)
			if err != nil {
				return "", err
			}
			desc = desc[:at] + op.Text + desc[at:]
		case "insert_after":
			at, err := once(i, "anchor", op.Anchor)
			if err != nil {
				return "", err
			}
			at += len(op.Anchor)
			desc = desc[:at] + op.Text + desc[at:]
		case "replace":
			if op.ReplaceAll {
				if op.OldString == "" {
					return "", fmt.Errorf("patch %d (replace): old_string is empty", i)
				}
				if !strings.Contains(desc, op.OldString) {
					return "", fmt.Errorf("patch %d (replace): old_string not found in the description", i)
				}
				desc = strings.ReplaceAll(desc, op.OldString, op.NewString)
				continue
			}
			at, err := once(i, "old_string", op.OldString)
			if err != nil {
				return "", err
			}
			desc = desc[:at] + op.NewString + desc[at+len(op.OldString):]
		case "replace_range":
			// [start of from, start of to) becomes new_string; to is kept.
			at, err := once(i, "from", op.From)
			if err != nil {
				return "", err
			}
			if op.To == "" {
				return "", fmt.Errorf("patch %d (replace_range): to is empty", i)
			}
			end := strings.Index(desc[at+len(op.From):], op.To)
			if end < 0 {
				return "", fmt.Errorf("patch %d (replace_range): to not found after from", i)
			}
			end += at + len(op.From)
			desc = desc[:at] + op.NewString + desc[end:]
		default:
			return "", fmt.Errorf("patch %d: unknown op %q (want append, prepend, insert_before, insert_after, replace, replace_range)", i, op.Op)
		}
	}
	return desc, nil
}
