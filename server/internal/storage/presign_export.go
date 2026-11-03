package storage

import (
	"net/url"
	"time"
)

// PresignParams is one request to sign for another package that talks to a
// user-entered S3 service (the backup driver, T16 ticket 6). It uses the same
// signer as the attachment store, so there is one SigV4 implementation to
// get right (TestPresign_AWSExample pins it).
type PresignParams struct {
	AccessKey, SecretKey, Region string
	Method                       string // GET, PUT, HEAD, DELETE
	Scheme                       string // http or https
	Host                         string // host[:port] exactly as the client sends it
	Path                         string // raw, unescaped path ("/bucket/key")
	Query                        url.Values
	Now                          time.Time
	Expires                      time.Duration
}

// PresignURL returns the signed URL for p. The body is UNSIGNED-PAYLOAD, so
// a caller can stream a body it has not hashed.
func PresignURL(p PresignParams) string {
	return presign(credentials{AccessKey: p.AccessKey, SecretKey: p.SecretKey, Region: p.Region}, presignRequest{
		Method: p.Method, Scheme: p.Scheme, Host: p.Host, Path: p.Path,
		Query: p.Query, Now: p.Now, Expires: p.Expires,
	})
}
