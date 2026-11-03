package importer

import (
	"regexp"
	"sort"
)

// uploadURLRe is THE definition of an uploads.linear.app URL, used by the
// export (which files to download), the plan (who owns a file), the
// rewrite and verify's leftover check, so they cannot disagree. It matches
// a bare link, an image `![x](url)`, a link `[x](url)` or an HTML
// attribute, with http or https and any case in the scheme and host. It
// stops at whitespace and at the characters that end a markdown or HTML
// URL (a backquote too: it closes a code span), and needs at least one character of path, so prose that only
// mentions the host (`uploads.linear.app/...`, `https://uploads.linear.app/<org>/`)
// is not a URL.
var uploadURLRe = regexp.MustCompile(`(?i)https?://uploads\.linear\.app/[^\s()<>"'\x60\]\[]+`)

// uploadPrefixRe is the scheme and host CanonicalUploadURL normalises.
var uploadPrefixRe = regexp.MustCompile(`(?i)^https?://uploads\.linear\.app`)

// CanonicalUploadURL is u with its scheme and host in the form Linear
// serves them (https, lower case); the path is kept as it is (it is case
// sensitive). Files are keyed by this form.
func CanonicalUploadURL(u string) string {
	return uploadPrefixRe.ReplaceAllString(u, "https://uploads.linear.app")
}

// UploadURLs is every distinct uploads.linear.app URL in texts, in its
// canonical form, sorted.
func UploadURLs(texts ...string) []string {
	set := map[string]bool{}
	for _, t := range texts {
		for _, u := range uploadURLRe.FindAllString(t, -1) {
			set[CanonicalUploadURL(u)] = true
		}
	}
	out := make([]string, 0, len(set))
	for u := range set {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// IsUploadURL reports whether s is exactly one uploads.linear.app URL.
func IsUploadURL(s string) bool {
	loc := uploadURLRe.FindStringIndex(s)
	return loc != nil && loc[0] == 0 && loc[1] == len(s)
}

// RewriteUploads replaces every uploads.linear.app URL in text for which
// resolve (given the canonical form) returns ok. A URL resolve does not know stays as it is, so a
// file whose upload is still pending keeps a working (Linear-hosted) link.
// It returns the new text and how many URLs it left unresolved.
func RewriteUploads(text string, resolve func(url string) (string, bool)) (string, int) {
	pending := 0
	out := uploadURLRe.ReplaceAllStringFunc(text, func(u string) string {
		if to, ok := resolve(CanonicalUploadURL(u)); ok {
			return to
		}
		pending++
		return u
	})
	return out, pending
}
