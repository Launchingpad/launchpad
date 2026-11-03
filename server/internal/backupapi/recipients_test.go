package backupapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/backup"
	"devops/internal/httpx"
)

// The recipient rules of decisions-T16 #19 and #20 on the pure planners.

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func newKey(t *testing.T) string {
	t.Helper()
	_, pub, err := backup.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func status(t *testing.T, err error) (int, httpx.Code) {
	t.Helper()
	var e *httpx.Error
	if err == nil {
		return 0, ""
	}
	if !asHTTP(err, &e) {
		t.Fatalf("not an httpx error: %v", err)
	}
	return e.Status, e.Code
}

func asHTTP(err error, e **httpx.Error) bool {
	x, ok := err.(*httpx.Error)
	if ok {
		*e = x
	}
	return ok
}

func mustAdd(t *testing.T, list []storedRecipient, now time.Time, actor *uuid.UUID, ever bool) ([]storedRecipient, storedRecipient) {
	t.Helper()
	out, el, err := planAdd(list, now, newKey(t), actor, ever)
	if err != nil {
		t.Fatal(err)
	}
	return out, el
}

func TestPlanAdd_ImmediateOnlyWhenNeverHadARecipient(t *testing.T) {
	a := uuid.New()
	list, first := mustAdd(t, nil, t0, &a, false)
	if first.EffectiveAt != t0 || first.state(t0) != RecipientActive {
		t.Fatalf("the first add is not immediate: %+v", first)
	}
	// After that, every add is held 24 hours, whatever the set holds.
	_, second := mustAdd(t, list, t0, &a, true)
	if want := t0.Add(24 * time.Hour); !second.EffectiveAt.Equal(want) || second.state(t0) != RecipientPending {
		t.Fatalf("second add effective %v state %s, want %v pending", second.EffectiveAt, second.state(t0), want)
	}
	// An empty set with ever=true (it was emptied) is not immediate either.
	_, again := mustAdd(t, nil, t0, &a, true)
	if again.EffectiveAt.Equal(t0) {
		t.Fatal("an add after the set was emptied was immediate")
	}
	if got := again.view(t0.Add(24*time.Hour - time.Second)).State; got != RecipientPending {
		t.Fatalf("one second before the hold ends the state is %s", got)
	}
	if got := again.view(t0.Add(24 * time.Hour)).State; got != RecipientActive {
		t.Fatalf("at the end of the hold the state is %s", got)
	}
}

func TestPlanAdd_Refusals(t *testing.T) {
	a := uuid.New()
	list, first := mustAdd(t, nil, t0, &a, false)
	cases := []struct {
		name string
		key  string
		list []storedRecipient
		code int
	}{
		{"scrypt-looking text", "scrypt", list, 400},
		{"ssh key", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl", list, 400},
		{"age plugin recipient", "age1yubikey1qtest", list, 400},
		{"garbage with the age prefix", "age1" + strings.Repeat("q", 20), list, 400},
		{"empty", "", list, 400},
		{"the same key", first.PublicKey, list, 409},
	}
	for _, c := range cases {
		_, _, err := planAdd(c.list, t0, c.key, &a, true)
		if st, _ := status(t, err); st != c.code {
			t.Errorf("%s: status %d (%v), want %d", c.name, st, err, c.code)
		}
	}
	// A key whose removal is scheduled: cancel the removal instead.
	rm, _, err := planRemove(list, t0, first.Fingerprint, &a, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := planAdd(rm, t0, first.PublicKey, &a, true); err == nil || !strings.Contains(err.Error(), "cancel the removal") {
		t.Errorf("re-adding a key with a scheduled removal: %v", err)
	}
	// The short fingerprint (12 hex) collides with another recipient's.
	fp := backup.Fingerprint(newKey(t))
	fake := storedRecipient{PublicKey: "x", Fingerprint: fp[:12] + strings.Repeat("0", 52), AddedAt: t0, EffectiveAt: t0}
	// planAdd computes the fingerprint of the new key itself, so craft the collision by
	// storing a recipient that has the new key's short form.
	key := newKey(t)
	fake.Fingerprint = backup.Fingerprint(key)[:12] + strings.Repeat("f", 52)
	if _, _, err := planAdd([]storedRecipient{fake}, t0, key, &a, true); err == nil || !strings.Contains(err.Error(), "short fingerprint") {
		t.Errorf("short fingerprint collision: %v", err)
	}
	// At most 5 live keys.
	full := []storedRecipient{}
	for range backup.MaxRecipients {
		full, _ = mustAdd(t, full, t0, &a, true)
	}
	if _, _, err := planAdd(full, t0, newKey(t), &a, true); err == nil {
		t.Error("a sixth live key was accepted")
	} else if st, code := status(t, err); st != http.StatusConflict || code != httpx.CodeConflict {
		t.Errorf("sixth key: %d %s", st, code)
	}
}

func TestPlanRemove_AndCancel_Rules(t *testing.T) {
	alice, bob := uuid.New(), uuid.New()
	// A active, B pending (added by alice).
	list, a := mustAdd(t, nil, t0, &alice, false)
	list, b := mustAdd(t, list, t0, &alice, true)

	// Remove the active key A: scheduled 24 hours ahead, by alice. It stays active until then.
	rm, kind, err := planRemove(list, t0, a.Fingerprint, &alice, false)
	if err != nil || kind != kindRecipientRemovalScheduled {
		t.Fatalf("remove: %v %s", err, kind)
	}
	ia := find(rm, a.Fingerprint)
	if rm[ia].RemoveAt == nil || !rm[ia].RemoveAt.Equal(t0.Add(Hold)) || *rm[ia].RemoveBy != alice {
		t.Fatalf("removal not recorded: %+v", rm[ia])
	}
	if !rm[ia].activeAt(t0.Add(Hold-time.Second)) || rm[ia].activeAt(t0.Add(Hold)) {
		t.Fatal("a scheduled removal must keep the key active until removeAt, and not after")
	}
	// A second removal of the same key is a conflict.
	if _, _, err := planRemove(rm, t0, a.Fingerprint, &alice, false); err == nil {
		t.Fatal("a second removal was accepted")
	}

	// With a destination enabled, a removal that would leave no key is 409 ...
	solo, only := mustAdd(t, nil, t0, &alice, false)
	if _, _, err := planRemove(solo, t0, only.Fingerprint, &alice, true); err == nil {
		t.Fatal("emptied the set while a destination is enabled")
	} else if st, _ := status(t, err); st != 409 {
		t.Fatalf("status %d", st)
	}
	// ... and allowed when none is enabled.
	if _, _, err := planRemove(solo, t0, only.Fingerprint, &alice, false); err != nil {
		t.Fatalf("remove with no destination enabled: %v", err)
	}
	// A pending key counts as "active after all pending changes": removing A is fine when B is pending.
	if _, _, err := planRemove(list, t0, a.Fingerprint, &alice, true); err != nil {
		t.Fatalf("remove A with B pending: %v", err)
	}

	// Cancelling the pending add B when A has a scheduled removal would leave nothing: 409.
	if _, _, err := planCancel(rm, t0, b.Fingerprint, &alice, true); err == nil {
		t.Fatal("cancelled a pending add and emptied the projected set")
	}
	// Without a destination enabled it is allowed.
	if _, kind, err := planCancel(rm, t0, b.Fingerprint, &alice, false); err != nil || kind != kindRecipientAddCancelled {
		t.Fatalf("cancel add: %v %s", err, kind)
	}
	// The adder can cancel their own add.
	if _, _, err := planCancel(list, t0, b.Fingerprint, &alice, true); err != nil {
		t.Fatalf("the adder cancels their own add: %v", err)
	}
	// DELETE of a pending add is a cancel too.
	if out, kind, err := planRemove(list, t0, b.Fingerprint, &bob, true); err != nil || kind != kindRecipientAddCancelled || find(out, b.Fingerprint) >= 0 {
		t.Fatalf("delete of a pending add: %v %s", err, kind)
	}

	// The admin who scheduled a removal cannot cancel it; another admin can.
	if _, _, err := planCancel(rm, t0, a.Fingerprint, &alice, true); err == nil {
		t.Fatal("the scheduler cancelled their own removal")
	} else if st, code := status(t, err); st != 403 || code != httpx.CodeForbidden {
		t.Fatalf("scheduler cancel: %d %s", st, code)
	}
	out, kind, err := planCancel(rm, t0, a.Fingerprint, &bob, true)
	if err != nil || kind != kindRecipientRemovalCancelled || out[find(out, a.Fingerprint)].RemoveAt != nil {
		t.Fatalf("another admin cancels: %v %s", err, kind)
	}
	// An active key with nothing pending has nothing to cancel.
	if _, _, err := planCancel(list, t0, a.Fingerprint, &alice, true); err == nil {
		t.Fatal("cancelled nothing")
	}
	if _, _, err := planCancel(list, t0, strings.Repeat("a", 64), &alice, true); err == nil {
		t.Fatal("cancelled an unknown key")
	} else if st, _ := status(t, err); st != 404 {
		t.Fatalf("unknown key: %d", st)
	}
}

func TestPlanHost_RemoveAndCancel(t *testing.T) {
	alice := uuid.New()
	list, a := mustAdd(t, nil, t0, &alice, false)
	list, b := mustAdd(t, list, t0, &alice, true) // pending
	// The host removal needs a key that is active NOW to remain while a destination is enabled:
	// removing A leaves only the pending B, so it is refused.
	if _, err := planHostRemove(list, t0, a.Fingerprint, true); err == nil {
		t.Fatal("host removal left no active key")
	}
	// After B is active the same removal passes.
	if out, err := planHostRemove(list, t0.Add(Hold), a.Fingerprint, true); err != nil || find(out, a.Fingerprint) >= 0 {
		t.Fatalf("host removal after the hold: %v", err)
	}
	// With no destination enabled it passes at once.
	if _, err := planHostRemove(list, t0, a.Fingerprint, false); err != nil {
		t.Fatal(err)
	}
	// Host cancel: a pending add, and a pending removal.
	if out, what, err := planHostCancel(list, t0, b.Fingerprint, true); err != nil || what != "add" || find(out, b.Fingerprint) >= 0 {
		t.Fatalf("host cancel of an add: %v %s", err, what)
	}
	rm, _, _ := planRemove(list, t0, a.Fingerprint, &alice, false)
	if out, what, err := planHostCancel(rm, t0, a.Fingerprint, true); err != nil || what != "removal" || out[find(out, a.Fingerprint)].RemoveAt != nil {
		t.Fatalf("host cancel of a removal: %v %s", err, what)
	}
	// The non-empty rule applies to a host cancel of the only pending add too.
	only := []storedRecipient{{PublicKey: "x", Fingerprint: strings.Repeat("b", 64), AddedAt: t0, EffectiveAt: t0.Add(Hold)}}
	if _, _, err := planHostCancel(only, t0, only[0].Fingerprint, true); err == nil {
		t.Fatal("host cancel emptied the set")
	}
	if _, _, err := planHostCancel(list, t0, a.Fingerprint, true); err == nil {
		t.Fatal("host cancel of an active key with nothing pending")
	}
}

func TestParseStored_DropsPassedRemovals(t *testing.T) {
	alice := uuid.New()
	list, a := mustAdd(t, nil, t0, &alice, false)
	rm, _, _ := planRemove(list, t0, a.Fingerprint, &alice, false)
	raw, err := jsonMarshal(rm)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := parseStored(raw, t0.Add(Hold-time.Second)); len(got) != 1 {
		t.Fatalf("before removeAt: %d elements", len(got))
	}
	if got, _ := parseStored(raw, t0.Add(Hold)); len(got) != 0 {
		t.Fatalf("at removeAt the element must be dropped: %d", len(got))
	}
}

func TestCheckFingerprint(t *testing.T) {
	for _, bad := range []string{"", "abc", strings.Repeat("A", 64), strings.Repeat("a", 63), strings.Repeat("g", 64), strings.Repeat("a", 12)} {
		if checkFingerprint(bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if err := checkFingerprint(strings.Repeat("a", 64)); err != nil {
		t.Error(err)
	}
}
