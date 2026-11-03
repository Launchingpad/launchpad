package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusConflict, CodeVersionConflict, "stale")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "stale" || body["code"] != "version_conflict" || len(body) != 2 {
		t.Fatalf("body = %v", body)
	}
}

func TestWriteErr(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		status   int
		code     string
		internal bool
	}{
		{name: "typed", err: NotFound("issue"), status: 404, code: "not_found"},
		{name: "wrapped typed", err: errors.Join(errors.New("x"), Invalid("bad")), status: 400, code: "validation_failed"},
		{name: "plain", err: errors.New("db down"), status: 500, code: "internal_error", internal: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			internal := WriteErr(rec, tt.err)
			if internal != tt.internal || rec.Code != tt.status {
				t.Fatalf("internal=%v status=%d", internal, rec.Code)
			}
			var body ErrorBody
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if string(body.Code) != tt.code {
				t.Fatalf("code = %q", body.Code)
			}
			if tt.internal && strings.Contains(rec.Body.String(), "db down") {
				t.Fatal("internal detail leaked to the client")
			}
		})
	}
}

func TestDecodeJSON(t *testing.T) {
	type dst struct {
		Title string `json:"title"`
	}
	tests := []struct {
		name   string
		body   string
		ct     string
		status int // 0 = success
	}{
		{name: "ok", body: `{"title":"x"}`, ct: "application/json"},
		{name: "ok charset", body: `{"title":"x"}`, ct: "application/json; charset=utf-8"},
		{name: "unknown field", body: `{"titel":"x"}`, status: 400},
		{name: "empty", body: ``, status: 400},
		{name: "two objects", body: `{"title":"x"}{"title":"y"}`, status: 400},
		{name: "wrong type", body: `{"title":1}`, status: 400},
		{name: "media type", body: `{}`, ct: "text/plain", status: 415},
		{name: "too large", body: `{"title":"` + strings.Repeat("a", MaxBodyBytes) + `"}`, status: 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			if tt.ct != "" {
				r.Header.Set("Content-Type", tt.ct)
			}
			var d dst
			err := DecodeJSON(httptest.NewRecorder(), r, &d)
			if tt.status == 0 {
				if err != nil || d.Title != "x" {
					t.Fatalf("err=%v d=%+v", err, d)
				}
				return
			}
			var apiErr *Error
			if !errors.As(err, &apiErr) || apiErr.Status != tt.status {
				t.Fatalf("err = %v, want status %d", err, tt.status)
			}
		})
	}
}

func TestOpt(t *testing.T) {
	type body struct {
		A Opt[string]  `json:"a"`
		B Opt[*string] `json:"b"`
		C Opt[int]     `json:"c"`
	}
	tests := []struct {
		name string
		in   string
		want func(b body) bool
	}{
		{name: "absent", in: `{}`, want: func(b body) bool { return !b.A.Set && !b.B.Set && !b.C.Set }},
		{name: "null", in: `{"a":null,"b":null}`, want: func(b body) bool { return b.A.Set && b.A.Null && b.B.Set && b.B.Null }},
		{name: "value", in: `{"a":"x","c":3}`, want: func(b body) bool { return b.A.Set && !b.A.Null && b.A.V == "x" && b.C.V == 3 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b body
			if err := json.Unmarshal([]byte(tt.in), &b); err != nil {
				t.Fatal(err)
			}
			if !tt.want(b) {
				t.Fatalf("got %+v", b)
			}
		})
	}
}
