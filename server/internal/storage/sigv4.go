package storage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AWS Signature Version 4, query-string form ("presigned URL"), for S3.
//
// Every call this package makes to the store is a presigned URL, including
// the server's own HEAD, DELETE and bucket calls, so there is exactly one
// signing path to get right. It is checked against the AWS published example
// (TestPresign_AWSExample) and against a real silo in the integration lane.
// Only the standard library's HMAC-SHA256 is used; nothing here is new
// cryptography (golang-security: "never roll your own" is about primitives,
// and SigV4 is a published canonicalisation over HMAC-SHA256).

const (
	sigAlgorithm   = "AWS4-HMAC-SHA256"
	unsignedBody   = "UNSIGNED-PAYLOAD"
	amzDateFormat  = "20060102T150405Z"
	scopeDayFormat = "20060102"
)

// presignRequest is one request to sign. Host is host[:port] exactly as the
// client will send it; Path is the raw, unescaped path ("/bucket/key").
type presignRequest struct {
	Method  string
	Scheme  string
	Host    string
	Path    string
	Query   url.Values        // extra query parameters (response-*); may be nil
	Headers map[string]string // extra signed headers (lowercase names); host is always signed
	Now     time.Time
	Expires time.Duration
}

type credentials struct {
	AccessKey, SecretKey, Region string
}

// presign returns the presigned URL for req.
func presign(c credentials, req presignRequest) string {
	now := req.Now.UTC()
	amzDate := now.Format(amzDateFormat)
	scope := now.Format(scopeDayFormat) + "/" + c.Region + "/s3/aws4_request"

	headers := map[string]string{"host": req.Host}
	for k, v := range req.Headers {
		headers[strings.ToLower(k)] = strings.TrimSpace(v)
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	signedHeaders := strings.Join(names, ";")

	q := url.Values{}
	for k, vs := range req.Query {
		q[k] = append([]string(nil), vs...)
	}
	q.Set("X-Amz-Algorithm", sigAlgorithm)
	q.Set("X-Amz-Credential", c.AccessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(int(req.Expires/time.Second)))
	q.Set("X-Amz-SignedHeaders", signedHeaders)
	canonicalQuery := canonicalQueryString(q)

	var ch strings.Builder
	for _, k := range names {
		ch.WriteString(k)
		ch.WriteByte(':')
		ch.WriteString(headers[k])
		ch.WriteByte('\n')
	}
	canonicalURI := uriEncode(req.Path, false)
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalRequest := strings.Join([]string{
		req.Method, canonicalURI, canonicalQuery, ch.String(), signedHeaders, unsignedBody,
	}, "\n")
	sum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{sigAlgorithm, amzDate, scope, hex.EncodeToString(sum[:])}, "\n")

	key := hmacSHA256([]byte("AWS4"+c.SecretKey), now.Format(scopeDayFormat))
	key = hmacSHA256(key, c.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	return req.Scheme + "://" + req.Host + canonicalURI + "?" + canonicalQuery + "&X-Amz-Signature=" + signature
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// canonicalQueryString sorts by key (then value) and encodes both with the
// SigV4 rules (RFC 3986 unreserved characters kept, everything else %XX).
func canonicalQueryString(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode is the SigV4 UriEncode. With encodeSlash false, '/' is kept (an
// object key path); S3 does not double-encode the path.
func uriEncode(s string, encodeSlash bool) string {
	const hexUpper = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexUpper[c>>4])
			b.WriteByte(hexUpper[c&15])
		}
	}
	return b.String()
}
