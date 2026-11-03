package notify_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"devops/internal/dbq"
	"devops/internal/tracker"
)

// LAUN-214, decisions-T39: a state-change post names the old and the new state,
// read at post time, org-scoped.

// stateChangePost makes an issue move Todo-ish -> "In Progress" with a channel
// of the given kind, consumes the feed and returns the issue and its row.
func (w *swo) stateChangeRow(t *testing.T, kind string) {
	t.Helper()
	hooks := map[string]string{"slack": hookURL, "discord": discordHook, "fluxer": fluxerHook, "custom": customHook}
	w.putService(w.org.ID, kind, "Chan", hooks[kind], "state_change")
	is := w.createIssue("chen", tracker.CreateIssueInput{})
	w.consume()
	w.exec(`DELETE FROM chat_outbox`)
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")})
	w.consume()
	if r := w.onlyRow(); r.Kind != "state_change" || r.State != "pending" {
		t.Fatalf("row %+v", r)
	}
}

func (w *swo) lastText(t *testing.T) string {
	t.Helper()
	hits := w.slack.got()
	if len(hits) == 0 {
		t.Fatal("no post")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(hits[len(hits)-1].Body), &body); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"text", "content"} {
		if s, ok := body[k].(string); ok {
			return s
		}
	}
	t.Fatalf("no text in %v", body)
	return ""
}

func TestChat_StateChangeNamesBothStates(t *testing.T) {
	for _, kind := range []string{"slack", "discord", "fluxer", "custom"} {
		t.Run(kind, func(t *testing.T) {
			w := newChatWorld(t)
			w.stateChangeRow(t, kind)
			var from string
			if err := w.pool.QueryRow(context.Background(),
				`SELECT fs.name FROM issue_state_changes sc JOIN workflow_states fs ON fs.id = sc.from_state_id`).Scan(&from); err != nil {
				t.Fatal(err)
			}
			if res := w.pass(); res.Sent != 1 {
				t.Fatalf("pass %+v", res)
			}
			text := w.lastText(t)
			if !strings.Contains(text, "Issue changed state: "+from+" → In Progress - ") {
				t.Errorf("text = %q (from %q)", text, from)
			}
			// No title or person name; the logs hold no state name.
			for _, leak := range []string{"chen", "Chen"} {
				if strings.Contains(text, leak) {
					t.Errorf("text holds %q: %q", leak, text)
				}
			}
			if strings.Contains(w.slogs.String(), "In Progress") {
				t.Errorf("a log line holds a state name")
			}
		})
	}
}

func TestChat_StateNameRenamedBeforePostUsesCurrentName(t *testing.T) {
	w := newChatWorld(t)
	w.stateChangeRow(t, "slack")
	w.exec(`UPDATE workflow_states SET name = '@everyone <!channel>' WHERE org_id = $1 AND name = 'In Progress'`, w.org.ID)
	if res := w.pass(); res.Sent != 1 {
		t.Fatalf("pass %+v", res)
	}
	text := w.lastText(t)
	if !strings.Contains(text, "→ @"+"​"+"everyone &lt;!channel&gt; - ") || strings.Contains(text, "<!channel>") {
		t.Errorf("text = %q", text)
	}
}

func TestChat_StateGoneFallsBackToFixedSentence(t *testing.T) {
	w := newChatWorld(t)
	w.stateChangeRow(t, "slack")
	// The event row is gone (pruned): no ids, no names.
	w.exec(`DELETE FROM events WHERE type = 'issue.updated'`)
	if res := w.pass(); res.Sent != 1 {
		t.Fatalf("pass %+v", res)
	}
	if text := w.lastText(t); !strings.Contains(text, "Issue changed state: <") || strings.Contains(text, "→") {
		t.Errorf("text = %q", text)
	}
}

func TestChat_StateDeletedFallsBackToFixedSentence(t *testing.T) {
	w := newChatWorld(t)
	w.stateChangeRow(t, "slack")
	// Point the event at a state id that no longer exists.
	w.exec(`UPDATE events SET payload = jsonb_set(payload, '{toStateId}', to_jsonb(uuidv7()::text)) WHERE type = 'issue.updated'`)
	if res := w.pass(); res.Sent != 1 {
		t.Fatalf("pass %+v", res)
	}
	if text := w.lastText(t); !strings.Contains(text, "Issue changed state: <") || strings.Contains(text, "→") {
		t.Errorf("text = %q", text)
	}
}

func TestChat_StateOfAnotherOrgNeverResolves(t *testing.T) {
	w := newChatWorld(t)
	w.stateChangeRow(t, "slack")
	ctx := context.Background()
	org2, err := w.q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: "elsewhere", Name: "Elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	tm, err := tracker.CreateTeamTx(ctx, w.q, org2.ID, "ELS", "Elsewhere", "")
	if err != nil {
		t.Fatal(err)
	}
	var foreign string
	if err := w.pool.QueryRow(ctx,
		`INSERT INTO workflow_states (org_id, team_id, name, type) VALUES ($1, $2, 'SECRET-OTHER-ORG', 'started') RETURNING id::text`,
		org2.ID, tm.ID).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	w.exec(`UPDATE events SET payload = jsonb_set(payload, '{toStateId}', to_jsonb($1::text)) WHERE type = 'issue.updated'`, foreign)
	if res := w.pass(); res.Sent != 1 {
		t.Fatalf("pass %+v", res)
	}
	text := w.lastText(t)
	if strings.Contains(text, "SECRET-OTHER-ORG") || strings.Contains(text, "→") {
		t.Errorf("a state of another org resolved: %q", text)
	}
}

// A missing from-state alone: no names at all, the old fixed sentence.
func TestChat_MissingFromStateFallsBackToFixedSentence(t *testing.T) {
	w := newChatWorld(t)
	w.stateChangeRow(t, "slack")
	w.exec(`UPDATE events SET payload = payload - 'fromStateId' WHERE type = 'issue.updated'`)
	if res := w.pass(); res.Sent != 1 {
		t.Fatalf("pass %+v", res)
	}
	text := w.lastText(t)
	if !strings.Contains(text, "Issue changed state: <") || strings.Contains(text, "→") || strings.Contains(text, "In Progress") {
		t.Errorf("text = %q", text)
	}
}
