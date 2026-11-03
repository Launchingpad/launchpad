package importjob

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"

	"devops/internal/importer"
	"devops/internal/storage"
)

// syncBuf is a log sink that tests can read while the manager writes.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// memStore is an in-memory importer.ObjectStore that counts what it is asked.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    atomic.Int32
	heads   atomic.Int32
	// failHead, when set, makes every Head fail with it.
	failHead error
	// failHeadFrom, when > 0, makes the Nth and later Head calls answer not
	// found (an object that vanishes after the upload).
	failHeadFrom atomic.Int32
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

func (s *memStore) Head(_ context.Context, key string) (storage.ObjectInfo, error) {
	n := s.heads.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if from := s.failHeadFrom.Load(); from > 0 && n >= from {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	if s.failHead != nil {
		return storage.ObjectInfo{}, s.failHead
	}
	b, ok := s.objects[key]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	return storage.ObjectInfo{Size: int64(len(b))}, nil
}

func (s *memStore) Put(_ context.Context, key, _ string, size int64, body io.Reader) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return io.ErrUnexpectedEOF
	}
	s.puts.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = b
	return nil
}

func (s *memStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

var _ importer.ObjectStore = (*memStore)(nil)

// fakeLinear answers the GraphQL queries of the exporter from an Export.
type fakeLinear struct {
	srv *httptest.Server
	ex  *importer.Export
	key string
	// status, when not 0, is answered to every request instead (401, 500).
	status atomic.Int32
	// block, when set, makes a request wait on it (a slow export).
	block atomic.Pointer[chan struct{}]

	mu       sync.Mutex
	requests int
	authSeen []string
}

var rootField = regexp.MustCompile(`(?s)^\s*query\s*\w*[^{]*\{\s*(\w+)`)

func newFakeLinear(t *testing.T, ex *importer.Export) *fakeLinear {
	t.Helper()
	f := &fakeLinear{ex: ex, key: testKey}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLinear) seen() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, append([]string(nil), f.authSeen...)
}

func (f *fakeLinear) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))
	f.mu.Unlock()
	if ch := f.block.Load(); ch != nil {
		select {
		case <-*ch:
		case <-r.Context().Done():
			return
		}
	}
	if st := int(f.status.Load()); st != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Authentication required, not authenticated"}]}`))
		return
	}
	if r.Header.Get("Authorization") != f.key {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Authentication required, not authenticated"}]}`))
		return
	}
	var req struct {
		Query string `json:"query"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	m := rootField.FindStringSubmatch(req.Query)
	if m == nil {
		http.Error(w, "bad query", http.StatusBadRequest)
		return
	}
	var data any
	switch m[1] {
	case "viewer":
		data = map[string]any{"viewer": map[string]string{"id": "lin-viewer"}}
	case "teams":
		data = conn("teams", f.ex.Teams)
	case "workflowStates":
		data = conn("workflowStates", f.ex.States)
	case "issueLabels":
		data = conn("issueLabels", f.ex.Labels)
	case "users":
		data = conn("users", f.ex.Users)
	case "projects":
		data = conn("projects", f.ex.Projects)
	case "projectMilestones":
		data = conn("projectMilestones", f.ex.Milestones)
	case "cycles":
		data = conn("cycles", f.ex.Cycles)
	case "issues":
		nodes := []map[string]any{}
		for _, is := range f.ex.Issues {
			b, _ := json.Marshal(is)
			var mm map[string]any
			_ = json.Unmarshal(b, &mm)
			hist := is.History
			if hist == nil {
				hist = []importer.HistoryEntry{}
			}
			mm["history"] = map[string]any{"nodes": hist, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}
			nodes = append(nodes, mm)
		}
		data = conn("issues", nodes)
	case "comments":
		data = conn("comments", f.ex.Comments)
	case "documents":
		data = conn("documents", f.ex.Documents)
	case "issueRelations":
		data = conn("issueRelations", f.ex.Relations)
	case "attachments":
		data = conn("attachments", f.ex.Attachments)
	default:
		http.Error(w, "unknown root "+m[1], http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// conn is a one-page GraphQL connection.
func conn[T any](field string, nodes []T) map[string]any {
	if nodes == nil {
		nodes = []T{}
	}
	return map[string]any{field: map[string]any{"nodes": nodes,
		"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}}
}
