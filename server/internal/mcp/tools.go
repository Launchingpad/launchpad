package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Profile selects which tools a server exposes.
type Profile string

const (
	// ProfileFull exposes every tool.
	ProfileFull Profile = "full"
	// ProfileReviewer exposes only reviewerTools. Its client is read-only too.
	ProfileReviewer Profile = "reviewer"
)

// reviewerTools is the whole reviewer surface (docs/architecture.md, "MCP
// server"). A reviewer reads a ticket, its comments and linked documents,
// and nothing else.
var reviewerTools = []string{"get_issue", "list_comments", "get_document"}

// ParseProfile validates a --profile value.
func ParseProfile(s string) (Profile, error) {
	switch Profile(s) {
	case ProfileFull, "":
		return ProfileFull, nil
	case ProfileReviewer:
		return ProfileReviewer, nil
	}
	return "", fmt.Errorf("unknown profile %q (want full or reviewer)", s)
}

// param is one tool argument. Type is a JSON-schema type: string, integer,
// number, boolean or array (of strings).
type param struct {
	Name     string
	Type     string
	Desc     string
	Required bool
	Nullable bool
	Enum     []string
	// Items is the item schema of an array of objects. Nil: an array of
	// strings.
	Items map[string]any
}

// handler runs one tool call. The args are already checked against the
// tool's params.
type handler func(ctx context.Context, t *Tools, a args) (any, error)

type tool struct {
	Name   string
	Desc   string
	Params []param
	// ReadOnly marks a tool that only GETs and changes nothing, here or there.
	// Every reviewer tool must be. download_attachment only GETs the API but
	// writes a local file, so it is not ReadOnly (decisions-T2 #34).
	ReadOnly bool
	run      handler
}

// inputSchema renders the tool's JSON schema for tools/list.
func (tl *tool) inputSchema() map[string]any {
	props := make(map[string]any, len(tl.Params))
	var req []string
	for _, p := range tl.Params {
		s := map[string]any{"description": p.Desc}
		typ := any(p.Type)
		if p.Nullable {
			typ = []string{p.Type, "null"}
		}
		s["type"] = typ
		if p.Type == "array" {
			if p.Items != nil {
				s["items"] = p.Items
			} else {
				s["items"] = map[string]any{"type": "string"}
			}
		}
		if len(p.Enum) > 0 {
			s["enum"] = p.Enum
		}
		props[p.Name] = s
		if p.Required {
			req = append(req, p.Name)
		}
	}
	out := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(req) > 0 {
		out["required"] = req
	}
	return out
}

// args is a validated argument object. Values have the declared type, or are
// JSON null for a nullable param.
type args struct {
	m map[string]json.RawMessage
}

// parseArgs checks raw against the tool's params: no unknown names, required
// ones present, each value of its declared type.
func (tl *tool) parseArgs(raw json.RawMessage) (args, error) {
	a := args{m: map[string]json.RawMessage{}}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &a.m); err != nil {
			return a, fmt.Errorf("arguments must be a JSON object")
		}
	}
	byName := make(map[string]param, len(tl.Params))
	for _, p := range tl.Params {
		byName[p.Name] = p
	}
	keys := make([]string, 0, len(a.m))
	for k := range a.m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		p, ok := byName[k]
		if !ok {
			return a, fmt.Errorf("unknown argument %q", k)
		}
		v := a.m[k]
		if string(v) == "null" {
			if !p.Nullable {
				return a, fmt.Errorf("argument %q may not be null", k)
			}
			continue
		}
		norm, err := normalise(p, v)
		if err != nil {
			return a, fmt.Errorf("argument %q: %w", k, err)
		}
		a.m[k] = norm
	}
	for _, p := range tl.Params {
		if p.Required {
			v, ok := a.m[p.Name]
			if !ok || string(v) == "null" {
				return a, fmt.Errorf("argument %q is required", p.Name)
			}
		}
	}
	return a, nil
}

// normalise checks v against p's type and returns its canonical JSON. A
// numeric or boolean string ("5", "true") is accepted for an integer or
// boolean param, since some clients send every argument as a string.
func normalise(p param, v json.RawMessage) (json.RawMessage, error) {
	switch p.Type {
	case "string":
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("must be a string")
		}
		if len(p.Enum) > 0 && !slices.Contains(p.Enum, s) {
			return nil, fmt.Errorf("must be one of %s", strings.Join(p.Enum, ", "))
		}
		return v, nil
	case "integer":
		var f float64
		if err := json.Unmarshal(v, &f); err != nil {
			var s string
			if json.Unmarshal(v, &s) != nil {
				return nil, fmt.Errorf("must be an integer")
			}
			n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("must be an integer")
			}
			return json.RawMessage(strconv.FormatInt(n, 10)), nil
		}
		if f != math.Trunc(f) || math.Abs(f) > 1<<53 {
			return nil, fmt.Errorf("must be an integer")
		}
		return json.RawMessage(strconv.FormatInt(int64(f), 10)), nil
	case "number":
		var f float64
		if err := json.Unmarshal(v, &f); err != nil {
			return nil, fmt.Errorf("must be a number")
		}
		return v, nil
	case "boolean":
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			var s string
			if json.Unmarshal(v, &s) != nil {
				return nil, fmt.Errorf("must be a boolean")
			}
			b, err = strconv.ParseBool(s)
			if err != nil {
				return nil, fmt.Errorf("must be a boolean")
			}
		}
		return json.RawMessage(strconv.FormatBool(b)), nil
	case "array":
		// Some clients send an array JSON-encoded as a string
		// ("[\"Bug\"]"); a plain string is a one-item list.
		var str string
		if json.Unmarshal(v, &str) == nil {
			t := strings.TrimSpace(str)
			if strings.HasPrefix(t, "[") {
				v = json.RawMessage(t)
			} else if p.Items == nil && t != "" {
				b, _ := json.Marshal([]string{t})
				return b, nil
			}
		}
		if p.Items != nil {
			var objs []map[string]json.RawMessage
			if err := json.Unmarshal(v, &objs); err != nil {
				return nil, fmt.Errorf("must be an array of objects")
			}
			return v, nil
		}
		var ss []string
		if err := json.Unmarshal(v, &ss); err != nil {
			return nil, fmt.Errorf("must be an array of strings")
		}
		b, _ := json.Marshal(ss)
		return b, nil
	}
	return nil, fmt.Errorf("unsupported type %s", p.Type)
}

func (a args) has(k string) bool { _, ok := a.m[k]; return ok }

// isNull reports whether k was sent as JSON null.
func (a args) isNull(k string) bool { v, ok := a.m[k]; return ok && string(v) == "null" }

// set reports whether k was sent with a non-null value.
func (a args) set(k string) bool { return a.has(k) && !a.isNull(k) }

func (a args) str(k string) string {
	var s string
	if a.set(k) {
		_ = json.Unmarshal(a.m[k], &s) // validated in parseArgs
	}
	return s
}

func (a args) integer(k string) int64 {
	var n int64
	if a.set(k) {
		_ = json.Unmarshal(a.m[k], &n)
	}
	return n
}

func (a args) boolean(k string) bool {
	var b bool
	if a.set(k) {
		_ = json.Unmarshal(a.m[k], &b)
	}
	return b
}

func (a args) list(k string) []string {
	var ss []string
	if a.set(k) {
		_ = json.Unmarshal(a.m[k], &ss)
	}
	return ss
}

// raw returns k's JSON value (null included) for passing through to the API.
func (a args) raw(k string) json.RawMessage { return a.m[k] }

// Tools is the tool set of one server: its catalogue, its profile and the
// API client the handlers call.
type Tools struct {
	c       *Client
	team    string // LAUNCHPAD_TEAM, the default team; may be empty
	profile Profile
	byName  map[string]*tool
	order   []*tool
	now     func() time.Time // list_cycles type; tests pin it
	dir     directory        // cached teams and members (resolve.go)
}

// NewTools builds the tool set for profile. The reviewer profile gets ONLY
// reviewerTools: no write handler is registered at all, so tools/call cannot
// reach one by any name.
func NewTools(c *Client, team string, profile Profile) (*Tools, error) {
	t := &Tools{c: c, team: team, profile: profile, byName: map[string]*tool{}, now: time.Now}
	all := catalogue()
	switch profile {
	case ProfileFull:
		t.order = all
	case ProfileReviewer:
		if !c.readOnly {
			return nil, fmt.Errorf("the reviewer profile needs a read-only client")
		}
		for _, tl := range all {
			if slices.Contains(reviewerTools, tl.Name) {
				if !tl.ReadOnly {
					return nil, fmt.Errorf("reviewer tool %s is not read-only", tl.Name)
				}
				t.order = append(t.order, tl)
			}
		}
	default:
		return nil, fmt.Errorf("unknown profile %q", profile)
	}
	for _, tl := range t.order {
		t.byName[tl.Name] = tl
	}
	return t, nil
}

// Names lists the exposed tool names in catalogue order.
func (t *Tools) Names() []string {
	out := make([]string, 0, len(t.order))
	for _, tl := range t.order {
		out = append(out, tl.Name)
	}
	return out
}

// toolInfo is one tools/list entry.
type toolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

func (t *Tools) list() []toolInfo {
	out := make([]toolInfo, 0, len(t.order))
	for _, tl := range t.order {
		out = append(out, toolInfo{
			Name: tl.Name, Description: tl.Desc, InputSchema: tl.inputSchema(),
			Annotations: map[string]any{"readOnlyHint": tl.ReadOnly},
		})
	}
	return out
}

// lookup returns the named tool if this profile exposes it.
func (t *Tools) lookup(name string) (*tool, bool) {
	tl, ok := t.byName[name]
	return tl, ok
}

// call runs one exposed tool. Bad arguments and API failures are returned
// as the error, which the server sends as the tool's error result.
func (t *Tools) call(ctx context.Context, tl *tool, raw json.RawMessage) (any, error) {
	a, err := tl.parseArgs(raw)
	if err != nil {
		return nil, err
	}
	return tl.run(ctx, t, a)
}
