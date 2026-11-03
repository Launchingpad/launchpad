package tracker

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/scm"
)

// TestGitWorkflow_BuiltinPinned pins the moves of a team with no rule
// (decisions-T44 #1): they are the moves of decisions-T5 #16 and
// decisions-T17 #16, and they agree with scm.Event.Plan's Move.
func TestGitWorkflow_BuiltinPinned(t *testing.T) {
	review, testing_ := uuid.New(), uuid.New()
	team := dbq.Team{ID: uuid.New(), ReviewStateID: &review, TestingStateID: &testing_}
	for _, tc := range []struct {
		action scm.Action
		draft  bool
		want   *uuid.UUID
	}{
		{scm.ActionOpened, false, &review},
		{scm.ActionOpened, true, nil},
		{scm.ActionReopened, false, &review},
		{scm.ActionReopened, true, nil},
		{scm.ActionReady, false, &review},
		{scm.ActionMerged, false, &testing_},
		{scm.ActionMerged, true, &testing_}, // a draft merged still moves
		{scm.ActionClosed, false, nil},
		{scm.ActionUpdated, false, nil},
		{scm.ActionDrafted, true, nil},
		{scm.ActionApproved, false, nil},
		{scm.ActionUnapproved, false, nil},
		{scm.ActionChangesRequested, false, nil},
		{scm.ActionChecks, false, nil},
	} {
		ev := scm.Event{Action: tc.action, Draft: tc.draft}
		mv := resolveGitMove(team, ev, map[string]gitRule{})
		if mv.custom || len(mv.from) != 0 {
			t.Errorf("%s draft=%v: built-in move is %+v", tc.action, tc.draft, mv)
		}
		if (mv.target == nil) != (tc.want == nil) || (mv.target != nil && *mv.target != *tc.want) {
			t.Errorf("%s draft=%v: target %v, want %v", tc.action, tc.draft, mv.target, tc.want)
		}
		// The plan agrees: a review move, a testing move or none.
		var planTarget *uuid.UUID
		switch ev.Plan().Move {
		case scm.MoveToReview:
			planTarget = &review
		case scm.MoveToTesting:
			planTarget = &testing_
		}
		if (planTarget == nil) != (mv.target == nil) || (planTarget != nil && *planTarget != *mv.target) {
			t.Errorf("%s draft=%v: plan move %v, rule target %v", tc.action, tc.draft, ev.Plan().Move, mv.target)
		}
	}
	// A team with no anchor states moves nothing.
	if mv := resolveGitMove(dbq.Team{}, scm.Event{Action: scm.ActionMerged}, nil); mv.target != nil {
		t.Errorf("no anchors: %+v", mv)
	}
}

func TestGitWorkflow_RuleReplacesOneEvent(t *testing.T) {
	review, testing_, done := uuid.New(), uuid.New(), uuid.New()
	team := dbq.Team{ID: uuid.New(), ReviewStateID: &review, TestingStateID: &testing_}
	rules := map[string]gitRule{GitEventMerged: {target: &done}, GitEventOpened: {target: nil}}
	if mv := resolveGitMove(team, scm.Event{Action: scm.ActionMerged}, rules); !mv.custom || mv.target == nil || *mv.target != done {
		t.Fatalf("merged: %+v, want custom to done", mv)
	}
	if mv := resolveGitMove(team, scm.Event{Action: scm.ActionOpened}, rules); !mv.custom || mv.target != nil {
		t.Fatalf("opened: %+v, want custom no move", mv)
	}
	// Events with no rule keep the built-in move.
	if mv := resolveGitMove(team, scm.Event{Action: scm.ActionReady}, rules); mv.custom || mv.target == nil || *mv.target != review {
		t.Fatalf("ready: %+v, want built-in review", mv)
	}
	// A draft open never moves, whatever the rule says.
	rules[GitEventOpened] = gitRule{target: &done}
	if mv := resolveGitMove(team, scm.Event{Action: scm.ActionOpened, Draft: true}, rules); mv.target != nil {
		t.Fatalf("draft open: %+v, want no move", mv)
	}
}

func TestGitWorkflow_MayMoveTo(t *testing.T) {
	now := time.Now()
	st := func(typ string, pos float64) dbq.WorkflowState {
		return dbq.WorkflowState{ID: uuid.New(), Type: typ, Position: pos}
	}
	todo, prog, review, testing_, done, canceled := st("unstarted", 2), st("started", 3), st("started", 4), st("started", 5), st("completed", 6), st("canceled", 7)
	archived := st("completed", 8)
	archived.ArchivedAt = &now
	fwd := gitMove{target: &done.ID}
	for _, tc := range []struct {
		name     string
		mv       gitMove
		from, to dbq.WorkflowState
		want     bool
	}{
		{"forward to done", fwd, testing_, done, true},
		{"forward within started", fwd, prog, review, true},
		{"backward refused", fwd, testing_, review, false},
		{"out of done refused", fwd, done, review, false},
		{"out of canceled refused", fwd, canceled, done, false},
		{"into archived refused", fwd, todo, archived, false},
		{"same state", fwd, done, done, false},
		{"from set allows backward", gitMove{from: []uuid.UUID{testing_.ID}}, testing_, review, true},
		{"from set refuses other states", gitMove{from: []uuid.UUID{testing_.ID}}, prog, done, false},
		{"from set allows out of done when listed", gitMove{from: []uuid.UUID{done.ID}}, done, review, true},
		{"from set never into archived", gitMove{from: []uuid.UUID{todo.ID}}, todo, archived, false},
	} {
		if got := mayMoveTo(tc.mv, tc.from, tc.to); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func to(id uuid.UUID) httpx.Opt[uuid.UUID] { return httpx.Opt[uuid.UUID]{Set: true, V: id} }

var noMove = httpx.Opt[uuid.UUID]{Set: true, Null: true}

func TestGitWorkflow_Validate(t *testing.T) {
	a, b, c, other := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	live := map[uuid.UUID]bool{a: true, b: true, c: true}
	ok, err := validateGitRules([]GitWorkflowRuleInput{
		{Event: "merged", StateID: to(a), FromStateIDs: []uuid.UUID{c, b}},
		{Event: "closed", StateID: noMove},
	}, live)
	if err != nil || len(ok) != 2 || ok["closed"].target != nil || len(ok["merged"].from) != 2 {
		t.Fatalf("valid rules: %+v, %v", ok, err)
	}
	same, _ := validateGitRules([]GitWorkflowRuleInput{
		{Event: "closed", StateID: noMove}, {Event: "merged", StateID: to(a), FromStateIDs: []uuid.UUID{b, c}},
	}, live)
	if !sameGitRules(ok, same) {
		t.Fatal("the same rules in another order differ")
	}
	if sameGitRules(ok, map[string]gitRule{}) {
		t.Fatal("rules equal the empty set")
	}
	for name, in := range map[string][]GitWorkflowRuleInput{
		"unknown event":     {{Event: "approved", StateID: to(a)}},
		"repeated event":    {{Event: "merged", StateID: to(a)}, {Event: "merged", StateID: to(b)}},
		"other state":       {{Event: "merged", StateID: to(other)}},
		"other from state":  {{Event: "merged", StateID: to(a), FromStateIDs: []uuid.UUID{other}}},
		"repeated from":     {{Event: "merged", StateID: to(a), FromStateIDs: []uuid.UUID{b, b}}},
		"no move with from": {{Event: "merged", StateID: noMove, FromStateIDs: []uuid.UUID{b}}},
		"missing stateId":   {{Event: "merged"}},
	} {
		_, err := validateGitRules(in, live)
		var he *httpx.Error
		if !errors.As(err, &he) || he.Status != http.StatusBadRequest || he.Code != httpx.CodeValidationFailed {
			t.Errorf("%s: err = %v, want 400 validation_failed", name, err)
		}
	}
}
