package importer

import (
	"bytes"
	"errors"
	"testing"
	"testing/iotest"
	"time"
)

func TestNewV7At(t *testing.T) {
	at := ts("2026-08-31T10:00:00.123Z")
	u, err := NewV7At(at)
	if err != nil {
		t.Fatal(err)
	}
	if u.Version() != 7 {
		t.Errorf("version %d, want 7", u.Version())
	}
	if u.Variant().String() != "RFC4122" {
		t.Errorf("variant %s", u.Variant())
	}
	if got := V7Time(u); !got.Equal(at) {
		t.Errorf("timestamp %s, want %s", got, at)
	}
	v, _ := NewV7At(at)
	if u == v {
		t.Error("two ids at the same time must differ in the random bits")
	}
	later, _ := NewV7At(at.Add(time.Millisecond))
	if bytes.Compare(later[:6], u[:6]) <= 0 {
		t.Error("a later time must sort later")
	}
}

func TestNewV7At_errors(t *testing.T) {
	if _, err := NewV7At(time.Unix(-1, 0)); err == nil {
		t.Error("a time before 1970 must be refused")
	}
	boom := errors.New("no entropy")
	if _, err := newV7At(time.Now(), iotest.ErrReader(boom)); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the reader's error", err)
	}
}
