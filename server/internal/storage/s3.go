// Package storage is the attachment object store: an S3-compatible bucket
// (silo in every environment). The server never proxies file bytes. Clients
// PUT to a presigned URL and download through a 302 to a presigned GET; the
// server itself only HEADs, DELETEs and (avatars, 2 MiB) GETs objects.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned by Head when the object does not exist.
var ErrNotFound = errors.New("object not found")

// ObjectInfo is what a HEAD tells the server about an uploaded object.
type ObjectInfo struct {
	Size        int64
	ContentType string
}

// Upload is a presigned PUT handed to a client.
type Upload struct {
	URL     string
	Method  string
	Headers map[string]string // the client must send exactly these
	Expires time.Time
}

// Store is the object store the tracker uses. *S3 is the only real one.
type Store interface {
	// PresignPut signs a PUT of exactly size bytes of contentType to key.
	PresignPut(key, contentType string, size int64, ttl time.Duration) Upload
	// PresignGet signs a GET of key. The store answers with the given
	// Content-Type and Content-Disposition.
	PresignGet(key, contentType, disposition string, ttl time.Duration) string
	// Head reads an object's size and type. ErrNotFound when absent.
	Head(ctx context.Context, key string) (ObjectInfo, error)
	// Get opens an object for reading (the avatar route streams it). The
	// caller closes the body. ErrNotFound when absent.
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
	// Delete removes an object. Deleting an absent object is not an error.
	Delete(ctx context.Context, key string) error
}

// Config is the S3 connection. Endpoint is what THIS server dials;
// PublicEndpoint is what goes into the URLs handed to clients (the same
// value unless the server reaches the store on an internal address, as in
// local compose). Both are scheme://host[:port], no path. Requests are
// path-style (/bucket/key), which silo needs.
type Config struct {
	Endpoint       string
	PublicEndpoint string
	Region         string
	Bucket         string
	AccessKey      string
	SecretKey      string
}

// S3 is a Store on an S3-compatible endpoint. It is safe for concurrent use.
type S3 struct {
	internal, public *url.URL
	bucket           string
	creds            credentials
	http             *http.Client // HEAD, DELETE, bucket calls: 30 s in all
	// putHTTP sends object bodies (Put). It has NO total timeout: a
	// client Timeout covers reading the whole body, and a file near 100 MiB
	// over a normal uplink to a remote store takes longer than any fixed
	// value. The caller's ctx bounds it; the transport still times out a
	// dial, a TLS handshake and a server that never answers.
	putHTTP *http.Client
	// putMinRate and putSlack make each Put's deadline: slack plus the time
	// the body takes at the minimum rate. A store that stops reading
	// mid-body therefore fails the upload instead of blocking it for ever.
	putMinRate int64 // bytes per second
	putSlack   time.Duration
	now        func() time.Time
}

// New validates cfg and returns a store. It does not dial.
func New(cfg Config) (*S3, error) {
	in, err := parseEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("S3_ENDPOINT: %w", err)
	}
	pubRaw := cfg.PublicEndpoint
	if pubRaw == "" {
		pubRaw = cfg.Endpoint
	}
	pub, err := parseEndpoint(pubRaw)
	if err != nil {
		return nil, fmt.Errorf("S3_PUBLIC_ENDPOINT: %w", err)
	}
	if cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("S3_BUCKET, S3_ACCESS_KEY and S3_SECRET_KEY are required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	return &S3{
		internal: in, public: pub, bucket: cfg.Bucket,
		creds:      credentials{AccessKey: cfg.AccessKey, SecretKey: cfg.SecretKey, Region: region},
		http:       &http.Client{Timeout: 30 * time.Second},
		putHTTP:    &http.Client{Transport: putTransport()},
		putMinRate: PutMinRate, putSlack: PutSlack,
		now: time.Now,
	}, nil
}

// ParseEndpoint checks an endpoint value (scheme://host[:port]).
func parseEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%q: want scheme://host[:port], e.g. https://silo.example.com", raw)
	}
	// A default port is dropped: net/http sends "Host: host" for
	// https://host:443, and the signature must cover the Host header the
	// store receives, or every request fails with a signature mismatch.
	if p := u.Port(); (u.Scheme == "https" && p == "443") || (u.Scheme == "http" && p == "80") {
		h := u.Hostname()
		if strings.Contains(h, ":") {
			h = "[" + h + "]" // IPv6
		}
		u.Host = h
	}
	return u, nil
}

// PublicIsHTTPS reports whether client URLs use https.
func (s *S3) PublicIsHTTPS() bool { return s.public.Scheme == "https" }

func (s *S3) objectPath(key string) string { return "/" + s.bucket + "/" + key }

func (s *S3) sign(base *url.URL, method, path string, q url.Values, headers map[string]string, ttl time.Duration) string {
	return presign(s.creds, presignRequest{
		Method: method, Scheme: base.Scheme, Host: base.Host, Path: path,
		Query: q, Headers: headers, Now: s.now(), Expires: ttl,
	})
}

// PresignPut implements Store. Content-Type and Content-Length are signed,
// so the store refuses an upload of another type or another size.
func (s *S3) PresignPut(key, contentType string, size int64, ttl time.Duration) Upload {
	headers := map[string]string{
		"content-type":   contentType,
		"content-length": strconv.FormatInt(size, 10),
	}
	return Upload{
		URL:     s.sign(s.public, http.MethodPut, s.objectPath(key), nil, headers, ttl),
		Method:  http.MethodPut,
		Headers: map[string]string{"Content-Type": contentType},
		Expires: s.now().Add(ttl).UTC().Truncate(time.Second),
	}
}

// PresignGet implements Store.
func (s *S3) PresignGet(key, contentType, disposition string, ttl time.Duration) string {
	q := url.Values{}
	if contentType != "" {
		q.Set("response-content-type", contentType)
	}
	if disposition != "" {
		q.Set("response-content-disposition", disposition)
	}
	return s.sign(s.public, http.MethodGet, s.objectPath(key), q, nil, ttl)
}

// serverCallTTL is the life of the URLs the server signs for itself.
const serverCallTTL = time.Minute

func (s *S3) do(ctx context.Context, method, path string) (*http.Response, error) {
	return s.send(ctx, s.http, method, path, nil, nil, 0)
}

// putTransport is the default transport with a bound on the wait for the
// response headers, which starts only after the body is sent.
func putTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = time.Minute
	return t
}

// send makes one server-side call on a URL it signs itself. headers are
// signed and sent; body (with its exact size) is sent when not nil.
func (s *S3) send(ctx context.Context, client *http.Client, method, path string, headers map[string]string, body io.Reader, size int64) (*http.Response, error) {
	u := s.sign(s.internal, method, path, nil, headers, serverCallTTL)
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		// The URL holds a signature: a url.Error would print it.
		return nil, fmt.Errorf("s3 %s: %w", method, stripURL(err))
	}
	for k, v := range headers {
		if strings.EqualFold(k, "content-length") {
			continue // set through ContentLength
		}
		req.Header.Set(k, v)
	}
	if body != nil {
		req.ContentLength = size
	}
	resp, err := client.Do(req)
	if err != nil {
		// The URL holds a signature; report only the method and status.
		return nil, fmt.Errorf("s3 %s: %w", method, stripURL(err))
	}
	return resp, nil
}

// stripURL drops the URL from a *url.Error, keeping its cause.
func stripURL(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// PutMinRate and PutSlack set the deadline of one Put: PutSlack plus size
// at PutMinRate. 128 KiB/s is far below any working uplink, so only a store
// that has stopped reading hits it: a 100 MiB file gets 2 + 13.3 minutes.
const (
	PutMinRate int64 = 128 << 10
	PutSlack         = 2 * time.Minute
)

// putDeadline is the time one Put of size bytes may take.
func (s *S3) putDeadline(size int64) time.Duration {
	return s.putSlack + time.Duration(size)*time.Second/time.Duration(s.putMinRate)
}

// Put stores size bytes of contentType at key, from the server itself (the
// importer). It has a deadline from the size (putDeadline) on top of ctx. Like a presigned client PUT it signs the type and the length,
// so the store refuses a body of another size.
func (s *S3) Put(ctx context.Context, key, contentType string, size int64, body io.Reader) error {
	headers := map[string]string{"content-type": contentType, "content-length": strconv.FormatInt(size, 10)}
	ctx, cancel := context.WithTimeout(ctx, s.putDeadline(size))
	defer cancel()
	resp, err := s.send(ctx, s.putHTTP, http.MethodPut, s.objectPath(key), headers, body, size)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("s3 PUT object: status %d", resp.StatusCode)
	}
	return nil
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// Head implements Store.
func (s *S3) Head(ctx context.Context, key string) (ObjectInfo, error) {
	resp, err := s.do(ctx, http.MethodHead, s.objectPath(key))
	if err != nil {
		return ObjectInfo{}, err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		return ObjectInfo{Size: resp.ContentLength, ContentType: resp.Header.Get("Content-Type")}, nil
	case http.StatusNotFound:
		return ObjectInfo{}, ErrNotFound
	default:
		return ObjectInfo{}, fmt.Errorf("s3 HEAD object: status %d", resp.StatusCode)
	}
}

// Get implements Store. The body is the response itself: the caller must
// close it, and bounds its own read.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	resp, err := s.do(ctx, http.MethodGet, s.objectPath(key))
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, ObjectInfo{Size: resp.ContentLength, ContentType: resp.Header.Get("Content-Type")}, nil
	case http.StatusNotFound:
		drain(resp)
		return nil, ObjectInfo{}, ErrNotFound
	default:
		drain(resp)
		return nil, ObjectInfo{}, fmt.Errorf("s3 GET object: status %d", resp.StatusCode)
	}
}

// Delete implements Store.
func (s *S3) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, s.objectPath(key))
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("s3 DELETE object: status %d", resp.StatusCode)
	}
	return nil
}

// EnsureBucket creates the bucket when it does not exist (local and test
// stacks). Production buckets are provisioned by the operator; the server
// calls CheckBucket there instead.
func (s *S3) EnsureBucket(ctx context.Context) error {
	ok, err := s.bucketExists(ctx)
	if err != nil || ok {
		return err
	}
	resp, err := s.do(ctx, http.MethodPut, "/"+s.bucket)
	if err != nil {
		return err
	}
	defer drain(resp)
	// 409: another process created it first.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("s3 create bucket %q: status %d", s.bucket, resp.StatusCode)
	}
	return nil
}

// CheckBucket fails when the bucket is missing or unreachable.
func (s *S3) CheckBucket(ctx context.Context) error {
	ok, err := s.bucketExists(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("s3 bucket %q does not exist", s.bucket)
	}
	return nil
}

func (s *S3) bucketExists(ctx context.Context) (bool, error) {
	resp, err := s.do(ctx, http.MethodHead, "/"+s.bucket)
	if err != nil {
		return false, err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("s3 HEAD bucket %q: status %d", s.bucket, resp.StatusCode)
	}
}
