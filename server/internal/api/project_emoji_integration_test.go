package api

import (
	"context"
	"net/http"
	"testing"

	"devops/internal/tracker"
)

// LAUN-232 (docs/internal/decisions-T41-project-emoji.md): a project emoji.

func emojiOf(p tracker.Project) string {
	if p.Emoji == nil {
		return "<nil>"
	}
	return *p.Emoji
}

func TestProjectEmoji_SetChangeClear(t *testing.T) {
	e := newEnv(t)
	// Create with an emoji (surrounding space is dropped); read returns it, the list too.
	p := e.createProject(e.chen, map[string]any{"name": "Rocket", "emoji": " 🚀 "})
	if emojiOf(p) != "🚀" {
		t.Fatalf("created emoji %q", emojiOf(p))
	}
	path := O + "/projects/" + p.ID.String()
	var got tracker.Project
	e.must(200, e.do("GET", path, e.chen, nil)).json(t, &got)
	if emojiOf(got) != "🚀" {
		t.Fatalf("read emoji %q", emojiOf(got))
	}
	if ps := e.listProjectsQ(e.chen, "query=Rocket"); len(ps) != 1 || emojiOf(ps[0]) != "🚀" {
		t.Fatalf("list emoji %+v", ps)
	}
	// No emoji on create is null.
	if plain := e.createProject(e.chen, map[string]any{"name": "Plain"}); plain.Emoji != nil {
		t.Fatalf("plain emoji %q", *plain.Emoji)
	}
	if plain := e.createProject(e.chen, map[string]any{"name": "Plain2", "emoji": ""}); plain.Emoji != nil {
		t.Fatalf("empty emoji %q", *plain.Emoji)
	}

	// Change: one project.updated with "emoji" in changes, the last event, version bumped.
	before := e.eventCount("project.updated")
	var up tracker.Project
	e.must(200, e.do("PATCH", path, e.chen, map[string]any{"emoji": "🧑🏽‍💻"})).json(t, &up)
	if emojiOf(up) != "🧑🏽‍💻" || up.Version != p.Version+1 {
		t.Fatalf("updated %q v%d", emojiOf(up), up.Version)
	}
	if n := e.eventCount("project.updated"); n != before+1 {
		t.Fatalf("project.updated %d, want %d", n, before+1)
	}
	if n := e.countRows(`SELECT count(*) FROM events WHERE type = 'project.updated' AND entity_id = $1
		AND payload->'changes' = '["emoji"]'::jsonb AND seq = (SELECT max(seq) FROM events)`, p.ID); n != 1 {
		t.Fatal("the emoji change is not the last event with changes [\"emoji\"]")
	}
	// The event never carries the value.
	if n := e.countRows(`SELECT count(*) FROM events WHERE payload::text LIKE '%' || $1 || '%'`, "🧑"); n != 0 {
		t.Fatal("an event payload holds the emoji")
	}
	// The same value again writes nothing.
	e.must(200, e.do("PATCH", path, e.chen, map[string]any{"emoji": "🧑🏽‍💻"}))
	if n := e.eventCount("project.updated"); n != before+1 {
		t.Fatalf("a no-op PATCH emitted: %d", n)
	}
	// Clear with "" and with null.
	e.must(200, e.do("PATCH", path, e.chen, map[string]any{"emoji": ""})).json(t, &up)
	if up.Emoji != nil {
		t.Fatalf("cleared with empty: %q", *up.Emoji)
	}
	e.must(200, e.do("PATCH", path, e.chen, map[string]any{"emoji": "🇬🇧"})).json(t, &up)
	e.must(200, e.do("PATCH", path, e.chen, map[string]any{"emoji": nil})).json(t, &up)
	if up.Emoji != nil {
		t.Fatalf("cleared with null: %q", *up.Emoji)
	}
	// Another field leaves the emoji alone.
	e.must(200, e.do("PATCH", path, e.chen, map[string]any{"emoji": "🎉"}))
	e.must(200, e.do("PATCH", path, e.chen, map[string]any{"name": "Rocket 2"})).json(t, &up)
	if emojiOf(up) != "🎉" {
		t.Fatalf("a rename lost the emoji: %q", emojiOf(up))
	}
	// The agent (write scope) sets it too.
	e.must(200, e.do("PATCH", path, e.claude, map[string]any{"emoji": "🤖"})).json(t, &up)
	if emojiOf(up) != "🤖" {
		t.Fatalf("agent emoji %q", emojiOf(up))
	}
}

func TestProjectEmoji_Validation(t *testing.T) {
	e := newEnv(t)
	p := e.createProject(e.chen, map[string]any{"name": "V", "emoji": "✅"})
	path := O + "/projects/" + p.ID.String()
	for name, v := range map[string]string{
		"text":            "ok",
		"letter":          "a",
		"many emoji":      "🚀🎉",
		"emoji and text":  "🚀!",
		"two flags":       "🇬🇧🇺🇸",
		"control char":    "🚀\u0007",
		"nul":             "\u0000",
		"33 bytes":        "❤️‍❤️‍❤️‍❤️",
		"kiss two tones":  "👩🏽‍❤️‍💋‍👨🏿",
		"not an emoji":    "\u2318",
		"braille blank":   "\u2800",
		"replacement":     "\uFFFD",
		"circled chain":   "\u24D0\u200D\u24D1",
		"hidden tag text": "\U0001F3F4\U000E0068\U000E0069\U000E007F",
		"skin on rocket":  "\U0001F680\U0001F3FD",
		"lone skin tone":  "🏽",
		"trailing joiner": "👩‍",
	} {
		t.Run(name, func(t *testing.T) {
			expectCode(t, e.do("PATCH", path, e.chen, map[string]any{"emoji": v}), 400, "validation_failed")
			expectCode(t, e.do("POST", O+"/projects", e.chen, map[string]any{"name": "N", "emoji": v}), 400, "validation_failed")
		})
	}
	// Nothing changed.
	var got tracker.Project
	e.must(200, e.do("GET", path, e.chen, nil)).json(t, &got)
	if emojiOf(got) != "✅" || got.Version != p.Version {
		t.Fatalf("after refusals: %q v%d", emojiOf(got), got.Version)
	}
	if n := e.countRows(`SELECT count(*) FROM projects WHERE name = 'N'`); n != 0 {
		t.Fatalf("a refused create stored %d projects", n)
	}
	// ZWJ, skin tone, keycap and flag sequences are one emoji each.
	// Up to 32 bytes (LAUN-232 follow-up): families, couples with skin tones, the subdivision flags.
	for _, v := range []string{"👩‍💻", "🧑🏽‍💻", "🏳️‍🌈", "❤️‍🔥", "👍🏿", "1️⃣", "🇺🇦", "©️",
		"👨‍👩‍👧", "👨‍👩‍👧‍👦", "👩🏽‍❤️‍👨🏿", "👩‍❤️‍💋‍👨🏿", "🏴󠁧󠁢󠁥󠁮󠁧󠁿", "🏴󠁧󠁢󠁳󠁣󠁴󠁿", "🏴󠁧󠁢󠁷󠁬󠁳󠁿",
		"\u2194\uFE0F", "\u2139\uFE0F", "\u25FB\uFE0F", "\u25FE", "\u2934\uFE0F", "#\uFE0F\u20E3"} {
		var up tracker.Project
		e.must(200, e.do("PATCH", path, e.chen, map[string]any{"emoji": v})).json(t, &up)
		if emojiOf(up) != v {
			t.Fatalf("%q stored as %q", v, emojiOf(up))
		}
	}
	// The CHECK holds the same limit: 33 bytes cannot be stored even past the service.
	if _, err := e.pool.Exec(context.Background(), `UPDATE projects SET emoji = $1 WHERE id = $2`, "❤️‍❤️‍❤️‍❤️", p.ID); err == nil {
		t.Fatal("the CHECK took 33 bytes")
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE projects SET emoji = $1 WHERE id = $2`, "", p.ID); err == nil {
		t.Fatal("the CHECK took an empty string")
	}
}

func TestProjectEmoji_CrossOrgAndTeamAccess(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	hidden := f.orgPath("/projects/" + f.hidden["project"])
	// Another org's project is 404 for an outsider, and nothing changes.
	expectCode(t, e.do("PATCH", hidden, f.outsider, map[string]any{"emoji": "🚀"}), http.StatusNotFound, "not_found")
	// A hidden project is 404 for a restricted member, like a missing one.
	expectCode(t, e.do("PATCH", hidden, f.restricted, map[string]any{"emoji": "🚀"}), http.StatusNotFound, "not_found")
	if n := e.countRows(`SELECT count(*) FROM projects WHERE id = $1 AND emoji IS NOT NULL`, f.hidden["project"]); n != 0 {
		t.Fatal("a refused PATCH set the emoji")
	}
	// The restricted member sets the emoji of a project of their own team.
	var mine tracker.Project
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/projects"), f.restricted,
		map[string]any{"name": "Rita's", "teams": []string{"ORB"}, "emoji": "🌱"})).json(t, &mine)
	if emojiOf(mine) != "🌱" {
		t.Fatalf("restricted create emoji %q", emojiOf(mine))
	}
	// The outsider cannot read it either.
	expectCode(t, e.do("GET", f.orgPath("/projects/"+mine.ID.String()), f.outsider, nil), http.StatusNotFound, "not_found")
}
