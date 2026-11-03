package linear

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "lin_api_TEST_KEY_must_not_leak"

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, testKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return c, &slept
}

func TestCheckReadOnly(t *testing.T) {
	tests := []struct {
		doc string
		ok  bool
	}{
		{"{ viewer { id } }", true},
		{"query Teams($after: String) { teams { nodes { id } } }", true},
		{"  \n query { issues { nodes { subscribers { nodes { id } } } } }", true},
		{"mutation { issueDelete(id: \"x\") { success } }", false},
		{"query A { a } mutation B { issueDelete(id: \"x\") { success } }", false},
		{"subscription { issueUpdated { id } }", false},
		{"Mutation { x }", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.doc, func(t *testing.T) {
			if err := CheckReadOnly(tt.doc); (err == nil) != tt.ok {
				t.Errorf("CheckReadOnly = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestNewClient_requiresKey(t *testing.T) {
	if _, err := NewClient(Endpoint, "  ", nil); !errors.Is(err, ErrNoKey) {
		t.Errorf("err = %v, want ErrNoKey", err)
	}
	if _, err := NewDownloader("", nil); !errors.Is(err, ErrNoKey) {
		t.Errorf("err = %v, want ErrNoKey", err)
	}
}

func TestQuery_refusesAMutationBeforeAnyRequest(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
	err := c.Query(t.Context(), `mutation { issueArchive(id: "x") { success } }`, nil, nil)
	if !errors.Is(err, ErrNotReadOnly) {
		t.Fatalf("err = %v, want ErrNotReadOnly", err)
	}
	if hits.Load() != 0 || c.Requests() != 0 {
		t.Errorf("a refused mutation sent %d request(s)", hits.Load())
	}
}

func TestQuery_sendsTheKeyAndDecodes(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != testKey ||
			r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var req gqlRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Variables["after"] != "c1" {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"viewer":{"id":"me"}}}`)
	})
	var out struct {
		Viewer struct{ ID string } `json:"viewer"`
	}
	if err := c.Query(t.Context(), "{ viewer { id } }", map[string]any{"after": "c1"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Viewer.ID != "me" {
		t.Errorf("decoded %+v", out)
	}
}

func TestQuery_retriesRateLimits(t *testing.T) {
	tests := []struct {
		name      string
		limited   func(w http.ResponseWriter)
		wantSleep time.Duration
	}{
		{"429 with Retry-After", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		}, 7 * time.Second},
		{"RATELIMITED error code", func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Requests-Reset", strconv.FormatInt(time.Now().Add(30*time.Second).UnixMilli(), 10))
			_, _ = io.WriteString(w, `{"errors":[{"message":"Rate limit exceeded","extensions":{"code":"RATELIMITED"}}]}`)
		}, 30 * time.Second},
		{"a 503", func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) }, 2 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var n atomic.Int32
			c, slept := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if n.Add(1) <= 2 {
					tt.limited(w)
					return
				}
				_, _ = io.WriteString(w, `{"data":{"ok":true}}`)
			})
			var out struct{ OK bool }
			if err := c.Query(t.Context(), "{ ok }", nil, &out); err != nil {
				t.Fatal(err)
			}
			if !out.OK || n.Load() != 3 || len(*slept) != 2 {
				t.Fatalf("ok=%v requests=%d sleeps=%v", out.OK, n.Load(), *slept)
			}
			// The server's wait is honoured (within a second of rounding);
			// without one, the back-off starts at 2s plus jitter.
			if d := (*slept)[0]; d < tt.wantSleep-time.Second || d > tt.wantSleep+time.Second {
				t.Errorf("first sleep %s, want about %s", d, tt.wantSleep)
			}
		})
	}
}

func TestQuery_givesUpAfterMaxRetries(t *testing.T) {
	var n atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c.MaxRetries = 2
	err := c.Query(t.Context(), "{ ok }", nil, nil)
	var api *APIError
	if !errors.As(err, &api) || api.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want the 429 APIError", err)
	}
	if n.Load() != 3 {
		t.Errorf("%d requests, want 3 (one try and two retries)", n.Load())
	}
}

func TestQuery_errorsNeverCarryTheKey(t *testing.T) {
	tests := map[string]http.HandlerFunc{
		"401": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"errors":[{"message":"Authentication required"}]}`)
		},
		"graphql error": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"errors":[{"message":"Cannot query field \"nope\""}]}`)
		},
		"garbage": func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `<html>`) },
	}
	for name, h := range tests {
		t.Run(name, func(t *testing.T) {
			c, _ := newTestClient(t, h)
			err := c.Query(t.Context(), "{ nope }", nil, &struct{}{})
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("the error carries the API key: %v", err)
			}
		})
	}
}

func TestQuery_waitsWhenTheBudgetIsNearlyGone(t *testing.T) {
	reset := time.Now().Add(42 * time.Second)
	var n atomic.Int32
	c, slept := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Requests-Remaining", "1")
			w.Header().Set("X-RateLimit-Requests-Reset", strconv.FormatInt(reset.UnixMilli(), 10))
		}
		_, _ = io.WriteString(w, `{"data":{}}`)
	})
	for range 2 {
		if err := c.Query(t.Context(), "{ ok }", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(*slept) != 1 || (*slept)[0] < 40*time.Second || (*slept)[0] > 43*time.Second {
		t.Errorf("sleeps %v, want one wait of about 42s before the second request", *slept)
	}
}

func TestFetchAll_paginates(t *testing.T) {
	var afters []any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req gqlRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		afters = append(afters, req.Variables["after"])
		if req.Variables["after"] == nil {
			_, _ = io.WriteString(w, `{"data":{"teams":{"nodes":[{"id":"a"},{"id":"b"}],"pageInfo":{"hasNextPage":true,"endCursor":"p2"}}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"teams":{"nodes":[{"id":"c"}],"pageInfo":{"hasNextPage":false,"endCursor":"p3"}}}}`)
	})
	got, err := fetchAll[struct{ ID string }](t.Context(), c, qTeams, "teams")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[2].ID != "c" {
		t.Errorf("got %+v", got)
	}
	if len(afters) != 2 || afters[0] != nil || afters[1] != "p2" {
		t.Errorf("cursors sent %v", afters)
	}
}

func TestFetchIssues_followsTheHistoryCursor(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req gqlRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Query, "IssueHistory") {
			if req.Variables["id"] != "i1" || req.Variables["after"] != "h-cur" {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"issue":{"history":{"nodes":[{"id":"h3","createdAt":"2026-01-03T00:00:00Z"}],"pageInfo":{"hasNextPage":false}}}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[
		  {"id":"i1","number":1,"title":"t","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z",
		   "labelIds":[],"history":{"nodes":[{"id":"h1","createdAt":"2026-01-01T00:00:00Z"},{"id":"h2","createdAt":"2026-01-02T00:00:00Z"}],
		   "pageInfo":{"hasNextPage":true,"endCursor":"h-cur"}}}],
		  "pageInfo":{"hasNextPage":false}}}}`)
	})
	got, err := fetchIssues(t.Context(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].History) != 3 || got[0].History[2].ID != "h3" {
		t.Fatalf("got %+v", got)
	}
}

func TestQueries_areAllReadOnly(t *testing.T) {
	for _, q := range []string{qTeams, qStates, qLabels, qUsers, qProjects, qMilestones, qCycles, qIssues,
		qIssueHistory, qComments, qDocuments, qRelations, qAttachments} {
		if err := CheckReadOnly(q); err != nil {
			t.Errorf("query refused: %.40s", q)
		}
	}
}
