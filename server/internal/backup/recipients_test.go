package backup

import (
	"testing"
	"time"
)

func TestActiveRecipientKeys(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	raw := `[
 {"publicKey":"a","effectiveAt":"2026-10-01T00:00:00Z","removeAt":null},
 {"publicKey":"pending","effectiveAt":"2026-10-28T00:00:00Z","removeAt":null},
 {"publicKey":"gone","effectiveAt":"2026-09-01T00:00:00Z","removeAt":"2026-10-27T12:00:00Z"},
 {"publicKey":"leaving","effectiveAt":"2026-09-01T00:00:00Z","removeAt":"2026-10-28T00:00:00Z"}]`
	got, err := ActiveRecipientKeys([]byte(raw), now)
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "leaving" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := ActiveRecipientKeys([]byte(`nope`), now); err == nil {
		t.Fatal("bad json accepted")
	}
	if got, _ := ActiveRecipientKeys([]byte(`[]`), now); len(got) != 0 {
		t.Fatal("empty list gave keys")
	}
}
