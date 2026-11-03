package idempotency

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"devops/internal/httpx"
)

func TestValidKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"abc-123", true},
		{"0b0c6c0e-6ed4-4b39-9a1e-b8d1f0a1c2d3", true},
		{"", false},
		{"has space", false},
		{"tab\t", false},
		{"é", false},
		{strings.Repeat("a", 255), true},
		{strings.Repeat("a", 256), false},
	}
	for _, tt := range tests {
		if got := validKey(tt.key); got != tt.want {
			t.Errorf("validKey(%q) = %v", tt.key, got)
		}
	}
}

func TestRequestHash(t *testing.T) {
	base := requestHash("POST", "/a", []byte(`{"x":1}`))
	if !bytes.Equal(base, requestHash("POST", "/a", []byte(`{"x":1}`))) {
		t.Fatal("hash is not stable")
	}
	for name, h := range map[string][]byte{
		"method": requestHash("PUT", "/a", []byte(`{"x":1}`)),
		"path":   requestHash("POST", "/b", []byte(`{"x":1}`)),
		"body":   requestHash("POST", "/a", []byte(`{"x":2}`)),
		// The separator stops "/a" + "b{" from colliding with "/ab" + "{".
		"boundary": requestHash("POST", "/a"+"{", []byte(`"x":1}`)),
	} {
		if bytes.Equal(base, h) {
			t.Errorf("%s change did not change the hash", name)
		}
	}
}

func TestRecorderKeepsFirstStatus(t *testing.T) {
	r := &recorder{header: map[string][]string{}, status: 200}
	r.WriteHeader(201)
	r.WriteHeader(500)
	_, _ = r.Write([]byte("x"))
	if r.status != 201 || r.body.String() != "x" {
		t.Fatalf("status %d body %q", r.status, r.body.String())
	}
}

func TestStorable(t *testing.T) {
	plain := []byte(`{"id":"x"}`)
	for status, want := range map[int]bool{200: true, 201: true, 400: true, 404: true, 409: true, 401: false, 403: false, 500: false, 503: false} {
		if got := storable(status, plain); got != want {
			t.Errorf("storable(%d) = %v", status, got)
		}
	}
	secret := []byte(`{"secret":"odt_` + strings.Repeat("A", 43) + `"}`)
	if storable(201, secret) {
		t.Error("a body holding a token secret was storable")
	}
}

// LAUN-191: the answer for a deadlock is a 503, which is never stored, so a
// retry with the same key runs the write again.
func TestStorable_DeadlockAnswerIsNotStored(t *testing.T) {
	e := httpx.Errorf(http.StatusServiceUnavailable, httpx.CodeOverloaded, "a concurrent change collided with this one; retry the request")
	body, _ := json.Marshal(httpx.ErrorBody{Error: e.Msg, Code: e.Code})
	if storable(e.Status, body) {
		t.Fatal("a 503 retry answer would be replayed")
	}
	if !storable(http.StatusConflict, []byte(`{}`)) {
		t.Fatal("a plain 409 must still be stored")
	}
}
