package offsite

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/storage"
)

// SiloConfig is offsite's own, READ-ONLY access to the product's silo (decisions-T16
// #38, ticket 14): S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY and S3_SECRET_KEY of the
// offsite service. The key is the one deploy.sh makes for offsite alone (policy:
// GetObject, ListBucket, GetBucketLocation); it is never the app's key.
type SiloConfig struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
}

// Enabled reports whether files backup is configured.
func (c SiloConfig) Enabled() bool { return c.Endpoint != "" }

// siloPrefix is the part of the bucket that is copied: the app writes attachments
// only under org/{org_id}/ (decisions-T3b). Anything else in the bucket is not an
// attachment of this install and is never read.
const siloPrefix = "org/"

// MaxFileObjectBytes is the largest attachment the files backup copies (the app's
// own limit is 100 MiB; a bigger object is counted as skipped, never read).
const MaxFileObjectBytes int64 = 4 << 30

const (
	siloListMaxPages = 400     // 400 x 1000 keys, twice the entry cap
	siloListMaxPage  = 8 << 20 // ONE page (1000 keys of 1 KiB is about 1.5 MiB); never a total
	siloSignFor      = 10 * time.Minute
)

var (
	bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionNameRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
)

// siloSource reads the silo bucket. All traffic goes through the silo dialer, which
// connects to the one host and port of the endpoint and nothing else.
type siloSource struct {
	scheme, host string
	cfg          SiloConfig
	hc           *http.Client
	now          func() time.Time
}

func newSiloSource(cfg SiloConfig, dialTimeout time.Duration) (*siloSource, error) {
	d, err := backup.NewSiloDialer(cfg.Endpoint, dialTimeout)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(cfg.Endpoint) // NewSiloDialer parsed it
	if !bucketNameRe.MatchString(cfg.Bucket) || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("offsite: S3_BUCKET, S3_ACCESS_KEY and S3_SECRET_KEY are required with S3_ENDPOINT")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if !regionNameRe.MatchString(cfg.Region) {
		return nil, errors.New("offsite: S3_REGION is not valid")
	}
	return &siloSource{scheme: u.Scheme, host: u.Host, cfg: cfg, hc: d.NewHTTPClient(0), now: time.Now}, nil
}

// siloObject is one entry of the silo listing.
type siloObject struct {
	Key      string
	Size     int64
	ETag     string
	Modified time.Time
}

type siloList struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		ETag         string `xml:"ETag"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
}

func (s *siloSource) url(method, key string, q url.Values) string {
	path := "/" + s.cfg.Bucket
	if key != "" {
		path += "/" + key
	}
	return storage.PresignURL(storage.PresignParams{
		AccessKey: s.cfg.AccessKey, SecretKey: s.cfg.SecretKey, Region: s.cfg.Region,
		Method: method, Scheme: s.scheme, Host: s.host, Path: path, Query: q,
		Now: s.now(), Expires: siloSignFor,
	})
}

// silo errors carry a driver code only; the reply's words are never kept.
func siloStatus(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return driver.Errf(driver.AuthFailed)
	case http.StatusNotFound:
		return driver.Errf(driver.NotFound)
	}
	return driver.ServerErr(status)
}

// validSiloKey is the filter of a listed key: it must be a key under siloPrefix that
// fits the list format. A key that does not is skipped (and counted by the caller).
func validSiloKey(k string) bool {
	if !strings.HasPrefix(k, siloPrefix) || len(k) > backup.MaxFileKeyBytes || !utf8.ValidString(k) {
		return false
	}
	for _, r := range k {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// list pages through the bucket under siloPrefix and calls fn for each object, in
// key order, without holding the listing. bad counts the keys fn never saw.
func (s *siloSource) list(ctx context.Context, fn func(siloObject) error) (bad int, err error) {
	token := ""
	for page := 0; ; page++ {
		if page >= siloListMaxPages {
			return bad, driver.Errf(driver.TooLarge)
		}
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("max-keys", "1000")
		q.Set("prefix", siloPrefix)
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, s.url(http.MethodGet, "", q), nil)
		if rerr != nil {
			return bad, driver.Errf(driver.ServerError)
		}
		resp, rerr := s.hc.Do(req)
		if rerr != nil {
			return bad, driver.MapNetError(rerr)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return bad, siloStatus(resp.StatusCode)
		}
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, siloListMaxPage+1))
		_ = resp.Body.Close()
		if rerr != nil {
			return bad, driver.MapNetError(rerr)
		}
		if len(b) > siloListMaxPage {
			return bad, driver.Errf(driver.TooLarge)
		}
		var lr siloList
		if xerr := xml.NewDecoder(bytes.NewReader(b)).Decode(&lr); xerr != nil {
			return bad, driver.Errf(driver.ServerError)
		}
		for _, c := range lr.Contents {
			if strings.HasSuffix(c.Key, "/") {
				continue // a folder marker is not an object
			}
			mt, terr := time.Parse(time.RFC3339, c.LastModified)
			if !validSiloKey(c.Key) || c.Size < 0 || terr != nil || len(c.ETag) > 128 {
				bad++
				continue
			}
			if err := fn(siloObject{Key: c.Key, Size: c.Size, ETag: strings.Trim(c.ETag, `"`), Modified: mt.UTC()}); err != nil {
				return bad, err
			}
		}
		if !lr.IsTruncated {
			return bad, nil
		}
		token = lr.NextContinuationToken
		if token == "" || len(token) > 2048 {
			return bad, driver.Errf(driver.ServerError)
		}
	}
}

// get opens one object for reading. The caller closes it. size is the length silo
// reports; the caller compares it with the listing (an object that changed).
func (s *siloSource) get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	if !validSiloKey(key) {
		return nil, 0, driver.Errf(driver.NotFound)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url(http.MethodGet, key, nil), nil)
	if err != nil {
		return nil, 0, driver.Errf(driver.ServerError)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, 0, driver.MapNetError(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, 0, siloStatus(resp.StatusCode)
	}
	return resp.Body, resp.ContentLength, nil
}
