package storage

import (
	"testing"
	"time"
)

// TestPresign_AWSExample is the presigned GET from the AWS S3 SigV4 docs
// ("Authenticating Requests: Using Query Parameters", example
// examplebucket/test.txt, 2013-05-24, 86400 s). The expected signature is
// AWS's published value, so it pins the canonicalisation, the scope and the
// key derivation together.
func TestPresign_AWSExample(t *testing.T) {
	got := presign(credentials{
		AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Region:    "us-east-1",
	}, presignRequest{
		Method: "GET", Scheme: "https", Host: "examplebucket.s3.amazonaws.com", Path: "/test.txt",
		Now: time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC), Expires: 86400 * time.Second,
	})
	want := "https://examplebucket.s3.amazonaws.com/test.txt" +
		"?X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host" +
		"&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if got != want {
		t.Fatalf("presign:\n got %s\nwant %s", got, want)
	}
}

func TestUriEncode(t *testing.T) {
	tests := []struct {
		name, in    string
		encodeSlash bool
		want        string
	}{
		{name: "unreserved kept", in: "aZ09-_.~", want: "aZ09-_.~"},
		{name: "path slash kept", in: "org/x/2026/a b.png", want: "org/x/2026/a%20b.png"},
		{name: "query slash encoded", in: "a/b", encodeSlash: true, want: "a%2Fb"},
		{name: "utf8 bytes", in: "é", want: "%C3%A9"},
		{name: "plus and equals", in: "a+b=c", encodeSlash: true, want: "a%2Bb%3Dc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := uriEncode(tc.in, tc.encodeSlash); got != tc.want {
				t.Fatalf("uriEncode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
