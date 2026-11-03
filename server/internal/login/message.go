package login

import (
	"net/textproto"
	"sort"
	"strings"
)

// reservedHeaders are written by the mailer itself; a custom header cannot
// replace them (Message-ID is the one exception: see Message).
var reservedHeaders = map[string]bool{
	"From": true, "To": true, "Subject": true, "Date": true, "Mime-Version": true,
	"Content-Type": true, "Content-Transfer-Encoding": true, "Cc": true, "Bcc": true,
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validHeaderName: an RFC 5322 field name, letters, digits and hyphen only.
func validHeaderName(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// validHeaderValue: printable ASCII and tab only. CR, LF and every other
// control character are refused (never stripped), so no value can start a
// new header or end the header block.
func validHeaderValue(v string) bool {
	for _, r := range v {
		if r == '\t' {
			continue
		}
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// checkHeaders refuses a custom header that could inject another one, or
// that replaces a header the mailer sets. The error never holds the value
// (it may hold a token).
func checkHeaders(h map[string]string) error {
	seen := map[string]bool{}
	for k, v := range h {
		canon := textproto.CanonicalMIMEHeaderKey(k)
		switch {
		case !validHeaderName(k):
			return &MailError{Msg: "a message header name is not valid"}
		case reservedHeaders[canon]:
			return &MailError{Msg: "the message header " + canon + " is set by the mailer and cannot be given"}
		case seen[canon]:
			return &MailError{Msg: "the message header " + canon + " is given twice"}
		case !validHeaderValue(v):
			return &MailError{Msg: "a message header value is not valid (control or non-ASCII characters)"}
		}
		seen[canon] = true
	}
	if id, ok := lookupHeader(h, "Message-ID"); ok && !(strings.HasPrefix(id, "<") && strings.HasSuffix(id, ">") && len(id) > 2) {
		return &MailError{Msg: "the Message-ID header must be in angle brackets"}
	}
	return nil
}

// lookupHeader finds a header by name, ignoring case.
func lookupHeader(h map[string]string, name string) (string, bool) {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}
