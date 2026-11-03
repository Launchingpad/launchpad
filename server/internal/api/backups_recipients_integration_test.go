//go:build backuptest

package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"devops/internal/backupapi"
)

func (bk *bkEnv) del(token, fp string) resp {
	return bk.do("DELETE", bkp+"/recipients/"+fp, token, nil)
}

func (bk *bkEnv) cancel(token, fp string) resp {
	return bk.do("POST", bkp+"/recipients/"+fp+"/cancel", token, nil)
}

func stateOf(l backupapi.RecipientList, fp string) string {
	for _, r := range l.Items {
		if r.Fingerprint == fp {
			return r.State
		}
	}
	return "absent"
}

// TestBackups_Recipients_Hold: the first add is immediate; every later add is
// held 24 hours by the injected clock, with or without a destination.
func TestBackups_Recipients_Hold(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	first := bk.addRecipient(bk.admin)
	if first.State != "active" || first.AddedBy == nil || *first.AddedBy != bk.chen.ID || len(first.Fingerprint) != 64 ||
		first.ShortFingerprint != first.Fingerprint[:12] {
		t.Fatalf("first add: %+v", first)
	}
	if l := bk.recipients(); !l.EverHadRecipient || l.Limit != 5 {
		t.Fatalf("list: %+v", l)
	}
	// No destination is enabled and the second add is held all the same.
	second := bk.addRecipient(bk.admin)
	if second.State != "pending" || !second.EffectiveAt.Equal(bk.clock.Now().Add(24*time.Hour)) {
		t.Fatalf("second add: %+v (clock %v)", second, bk.clock.Now())
	}
	bk.clock.Advance(24*time.Hour - time.Second)
	if got := stateOf(bk.recipients(), second.Fingerprint); got != "pending" {
		t.Fatalf("one second before the hold ends: %s", got)
	}
	if bk.status().ActiveRecipients != 1 {
		t.Fatal("a pending key counted as active")
	}
	bk.clock.Advance(time.Second)
	if got := stateOf(bk.recipients(), second.Fingerprint); got != "active" {
		t.Fatalf("at the end of the hold: %s", got)
	}
	if bk.status().ActiveRecipients != 2 {
		t.Fatal("the key is not active after the hold")
	}
	if got := strings.Join(bk.kinds(), ","); got != "recipient_added,recipient_added" {
		t.Fatalf("change kinds %s", got)
	}
}

// TestBackups_Recipients_Refusals: bad keys, duplicates, the cap of 5.
func TestBackups_Recipients_Refusals(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	for name, key := range map[string]string{
		"ssh":    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl",
		"scrypt": "scrypt",
		"junk":   "age1notakey",
		"empty":  "  ",
	} {
		r := bk.do("POST", bkp+"/recipients", bk.admin, map[string]any{"publicKey": key})
		if r.status != 400 {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	if r := bk.do("POST", bkp+"/recipients", bk.admin, map[string]any{}); r.status != 400 {
		t.Errorf("no key: %d", r.status)
	}
	key := bk.newKey()
	bk.must(201, bk.do("POST", bkp+"/recipients", bk.admin, map[string]any{"publicKey": key}))
	if r := bk.do("POST", bkp+"/recipients", bk.admin, map[string]any{"publicKey": " " + key + "\n"}); r.status != 409 {
		t.Errorf("the same key again: %d %s", r.status, r.body)
	}
	for i := 0; i < 4; i++ {
		bk.addRecipient(bk.admin)
	}
	if r := bk.do("POST", bkp+"/recipients", bk.admin, map[string]any{"publicKey": bk.newKey()}); r.status != 409 {
		t.Errorf("a sixth live key: %d %s", r.status, r.body)
	}
	// A cancelled pending add frees a place.
	l := bk.recipients()
	var pending string
	for _, r := range l.Items {
		if r.State == "pending" {
			pending = r.Fingerprint
		}
	}
	bk.must(200, bk.del(bk.admin, pending))
	bk.addRecipient(bk.admin)
	if n := len(bk.changes()); n != 7 { // 5 adds + 1 cancel + 1 add
		t.Errorf("%d change rows", n)
	}
}

// TestBackups_Recipients_RemovalAndCancel: DELETE on an active key schedules a
// removal; DELETE on a pending add cancels it; a removal is cancelled only by a
// different admin; an add by anyone, the adder included.
func TestBackups_Recipients_RemovalAndCancel(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	a := bk.addRecipient(bk.admin) // active
	b := bk.addRecipient(bk.admin) // pending
	// DELETE on a pending add cancels it.
	var l backupapi.RecipientList
	bk.must(200, bk.del(bk.admin, b.Fingerprint)).json(t, &l)
	if stateOf(l, b.Fingerprint) != "absent" {
		t.Fatalf("pending add not cancelled: %+v", l)
	}
	// DELETE on an active key schedules its removal 24 hours ahead.
	bk.must(200, bk.del(bk.admin, a.Fingerprint)).json(t, &l)
	var got backupapi.Recipient
	for _, r := range l.Items {
		if r.Fingerprint == a.Fingerprint {
			got = r
		}
	}
	if got.State != "removing" || got.RemoveAt == nil || !got.RemoveAt.Equal(bk.clock.Now().Add(24*time.Hour)) || got.RemoveBy == nil || *got.RemoveBy != bk.chen.ID {
		t.Fatalf("scheduled removal: %+v", got)
	}
	if r := bk.del(bk.admin, a.Fingerprint); r.status != 409 {
		t.Errorf("a second removal: %d", r.status)
	}
	// The admin who scheduled it cannot cancel it; the web path says 403.
	if r := bk.cancel(bk.admin, a.Fingerprint); r.status != 403 || r.errCode(t) != "forbidden" {
		t.Fatalf("the scheduler cancelled: %d %s", r.status, r.body)
	}
	if r := bk.cancel(bk.session, a.Fingerprint); r.status != 403 {
		t.Fatalf("the scheduler's session cancelled: %d %s", r.status, r.body)
	}
	// Another admin can.
	bk.must(200, bk.cancel(bk.admin2, a.Fingerprint)).json(t, &l)
	if stateOf(l, a.Fingerprint) != "active" {
		t.Fatalf("not cancelled: %+v", l)
	}
	// The key stays active through the removal's would-be time.
	bk.clock.Advance(48 * time.Hour)
	if stateOf(bk.recipients(), a.Fingerprint) != "active" {
		t.Fatal("a cancelled removal still landed")
	}
	// A scheduled removal lands: the key is gone after removeAt, and the next write drops it.
	bk.must(200, bk.del(bk.admin2, a.Fingerprint))
	bk.clock.Advance(24*time.Hour - time.Second)
	if stateOf(bk.recipients(), a.Fingerprint) != "removing" {
		t.Fatal("the key left before removeAt")
	}
	bk.clock.Advance(time.Second)
	if stateOf(bk.recipients(), a.Fingerprint) != "absent" {
		t.Fatal("the key stayed after removeAt")
	}
	// An add can be cancelled by its adder; and the empty set does not make the next add immediate.
	c := bk.addRecipient(bk.admin)
	if c.State != "pending" {
		t.Fatalf("add after the set was emptied: %+v", c)
	}
	bk.must(200, bk.cancel(bk.admin, c.Fingerprint))
	// Unknown key, a short fingerprint, nothing to cancel.
	if r := bk.cancel(bk.admin, strings.Repeat("a", 64)); r.status != 404 {
		t.Errorf("unknown key: %d", r.status)
	}
	if r := bk.del(bk.admin, strings.Repeat("a", 12)); r.status != 400 {
		t.Errorf("short fingerprint: %d", r.status)
	}
	d := bk.addRecipient(bk.admin)
	bk.clock.Advance(24 * time.Hour)
	if r := bk.cancel(bk.admin, d.Fingerprint); r.status != 409 {
		t.Errorf("cancel on an active key with nothing pending: %d", r.status)
	}
	want := "recipient_added,recipient_add_cancelled,recipient_removal_scheduled,recipient_removal_cancelled,recipient_removal_scheduled,recipient_added,recipient_add_cancelled,recipient_added"
	// the first add, its cancel ... : check as a multiset-free ordered list
	if got := strings.Join(bk.kinds(), ","); got != "recipient_added,recipient_added,recipient_add_cancelled,recipient_removal_scheduled,"+
		"recipient_removal_cancelled,recipient_removal_scheduled,recipient_added,recipient_add_cancelled,recipient_added" {
		t.Errorf("change kinds %s\n(want like %s)", got, want)
	}
}

// TestBackups_Recipients_NonEmpty: while a destination is enabled the keys that
// will be active after every pending change are never empty.
func TestBackups_Recipients_NonEmpty(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	a := bk.addRecipient(bk.admin)
	d := bk.dest("one", "s3")
	// Removing the only key with no destination enabled is allowed ... and undone.
	bk.must(200, bk.del(bk.admin, a.Fingerprint))
	bk.must(200, bk.cancel(bk.admin2, a.Fingerprint))
	bk.forceEnable(d.ID)
	if r := bk.del(bk.admin, a.Fingerprint); r.status != 409 {
		t.Fatalf("removed the last key under an enabled destination: %d %s", r.status, r.body)
	}
	// With a pending key B, A can go; then cancelling B would empty the set: 409 on the cancel too.
	b := bk.addRecipient(bk.admin)
	bk.must(200, bk.del(bk.admin, a.Fingerprint))
	if r := bk.cancel(bk.admin2, b.Fingerprint); r.status != 409 {
		t.Fatalf("cancelling a pending add emptied the set: %d %s", r.status, r.body)
	}
	if r := bk.del(bk.admin2, b.Fingerprint); r.status != 409 {
		t.Fatalf("DELETE of a pending add emptied the set: %d %s", r.status, r.body)
	}
	// Cancelling the removal of A is fine.
	bk.must(200, bk.cancel(bk.admin2, a.Fingerprint))
}

// TestBackups_Recipients_HostCommands: the host path removes at once and
// cancels at once, with a change-log row that has no actor.
func TestBackups_Recipients_HostCommands(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	ctx := context.Background()
	a := bk.addRecipient(bk.admin)
	b := bk.addRecipient(bk.admin)
	d := bk.dest("one", "s3")
	bk.forceEnable(d.ID)
	// B is pending: removing A would leave no ACTIVE key.
	if err := bk.svc.HostRemoveRecipient(ctx, a.Fingerprint); err == nil {
		t.Fatal("the host removal left no active key while a destination is enabled")
	}
	bk.clock.Advance(24 * time.Hour)
	if err := bk.svc.HostRemoveRecipient(ctx, a.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if stateOf(bk.recipients(), a.Fingerprint) != "absent" {
		t.Fatal("the key is still listed")
	}
	// Now B is the last key: refused.
	if err := bk.svc.HostRemoveRecipient(ctx, b.Fingerprint); err == nil {
		t.Fatal("the host removed the last key under an enabled destination")
	}
	// Without an enabled destination it works.
	if _, err := bk.pool.Exec(ctx, `UPDATE backup_destinations SET enabled = false`); err != nil {
		t.Fatal(err)
	}
	c := bk.addRecipient(bk.admin) // pending
	what, err := bk.svc.HostCancelRecipient(ctx, c.Fingerprint)
	if err != nil || what != "add" {
		t.Fatalf("host cancel of a pending add: %v %s", err, what)
	}
	bk.must(200, bk.del(bk.admin, b.Fingerprint)) // removal scheduled by chen
	what, err = bk.svc.HostCancelRecipient(ctx, b.Fingerprint)
	if err != nil || what != "removal" {
		t.Fatalf("host cancel of a removal: %v %s", err, what)
	}
	if err := bk.svc.HostRemoveRecipient(ctx, b.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if err := bk.svc.HostRemoveRecipient(ctx, b.Fingerprint); err == nil {
		t.Fatal("removed a key twice")
	}
	if err := bk.svc.HostRemoveRecipient(ctx, "abc"); err == nil {
		t.Fatal("a short fingerprint was accepted")
	}
	// Host rows have no actor.
	items := bk.changes()
	var host int
	for _, c := range items {
		if strings.HasSuffix(c.Kind, "_by_host") {
			host++
			if c.ActorID != nil {
				t.Errorf("host row %s has an actor", c.Kind)
			}
		}
	}
	if host != 4 {
		t.Errorf("%d host rows, want 4 (%v)", host, bk.kinds())
	}
}
