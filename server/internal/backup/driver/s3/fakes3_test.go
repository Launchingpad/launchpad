package s3

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"devops/internal/storage"
)

// fakeS3 is a small in-memory S3 server for the driver's unit tests. It
// checks the query signature with the same signer, so a request with the wrong
// canonical form is a 403 like it would be on a real service.
type fakeS3 struct {
	mu       sync.Mutex
	bucket   string
	access   string
	secret   string
	region   string
	virtual  bool
	pageSize int
	objects  map[string][]byte
	reqs     []string
	// fault runs first; true means it answered.
	fault func(w http.ResponseWriter, r *http.Request) bool
	srv   *httptest.Server
}

func newFake() *fakeS3 {
	return &fakeS3{bucket: "bkt-test", access: "AKTEST", secret: "SECRETTESTKEY", region: "us-east-1", pageSize: 1000, objects: map[string][]byte{}}
}

func (f *fakeS3) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *fakeS3) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

func (f *fakeS3) put(key string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = b
}

// tokenPrefix makes the continuation tokens hold characters that need query
// encoding (+ / =), as real tokens do.
const tokenPrefix = "a+b/c=="

func s3Err(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, msg)
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if f.fault != nil && f.fault(w, r) {
		return
	}
	if !f.verify(r) {
		s3Err(w, 403, "SignatureDoesNotMatch", "REMOTE-WORDS-MUST-NOT-LEAK")
		return
	}
	path := r.URL.Path
	if f.virtual {
		if !strings.HasPrefix(r.Host, f.bucket+".") {
			s3Err(w, 404, "NoSuchBucket", "x")
			return
		}
		path = "/" + f.bucket + path
	}
	if path == "/"+f.bucket || path == "/"+f.bucket+"/" {
		f.list(w, r)
		return
	}
	pre := "/" + f.bucket + "/"
	if !strings.HasPrefix(path, pre) {
		s3Err(w, 404, "NoSuchBucket", "x")
		return
	}
	key := strings.TrimPrefix(path, pre)
	switch r.Method {
	case http.MethodPut:
		b, err := io.ReadAll(r.Body)
		if err != nil || int64(len(b)) != r.ContentLength {
			s3Err(w, 400, "IncompleteBody", "x")
			return
		}
		f.put(key, b)
		w.WriteHeader(200)
	case http.MethodHead, http.MethodGet:
		f.mu.Lock()
		b, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			s3Err(w, 404, "NoSuchKey", "x")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.WriteHeader(200)
		if r.Method == http.MethodGet {
			_, _ = w.Write(b)
		}
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(204)
	default:
		s3Err(w, 405, "MethodNotAllowed", "x")
	}
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("list-type") != "2" {
		s3Err(w, 400, "InvalidArgument", "x")
		return
	}
	prefix := q.Get("prefix")
	f.mu.Lock()
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	f.mu.Unlock()
	sort.Strings(keys)
	start := 0
	if t := q.Get("continuation-token"); t != "" {
		start, _ = strconv.Atoi(strings.TrimPrefix(t, tokenPrefix))
	}
	end := start + f.pageSize
	trunc := end < len(keys)
	if end > len(keys) {
		end = len(keys)
	}
	type content struct {
		Key  string
		Size int
	}
	res := struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		IsTruncated           bool
		NextContinuationToken string `xml:",omitempty"`
		Contents              []content
	}{IsTruncated: trunc}
	if trunc {
		res.NextContinuationToken = tokenPrefix + strconv.Itoa(end)
	}
	f.mu.Lock()
	for _, k := range keys[start:end] {
		res.Contents = append(res.Contents, content{Key: k, Size: len(f.objects[k])})
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

// verify recomputes the signature of the request with the shared signer.
func (f *fakeS3) verify(r *http.Request) bool {
	q := r.URL.Query()
	date, err := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
	if err != nil || !strings.HasPrefix(q.Get("X-Amz-Credential"), f.access+"/") {
		return false
	}
	exp, _ := strconv.Atoi(q.Get("X-Amz-Expires"))
	rest := url.Values{}
	for k, v := range q {
		if !strings.HasPrefix(k, "X-Amz-") {
			rest[k] = v
		}
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	want := storage.PresignURL(storage.PresignParams{
		AccessKey: f.access, SecretKey: f.secret, Region: f.region, Method: r.Method, Scheme: scheme,
		Host: r.Host, Path: r.URL.Path, Query: rest, Now: date, Expires: time.Duration(exp) * time.Second,
	})
	u, err := url.Parse(want)
	return err == nil && u.Query().Get("X-Amz-Signature") == q.Get("X-Amz-Signature") && q.Get("X-Amz-Signature") != ""
}
