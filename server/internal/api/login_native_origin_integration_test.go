package api

// LAUN-137: the native login mode is refused for a browser. A request with
// Origin, Sec-Fetch-Site or Sec-Fetch-Mode gets the plain 401 invalid_code a
// wrong code gets, before the code is looked at.

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"devops/internal/login"
)

// sameAnswer fails when two answers differ in status, body or any header
// except Date.
func sameAnswer(t *testing.T, what string, a, b resp) {
	t.Helper()
	if a.status != b.status || string(a.body) != string(b.body) {
		t.Fatalf("%s: status/body differ:\n%d %s\n%d %s", what, a.status, a.body, b.status, b.body)
	}
	ah, bh := a.header.Clone(), b.header.Clone()
	ah.Del("Date")
	bh.Del("Date")
	if len(ah) != len(bh) {
		t.Fatalf("%s: headers differ:\n%v\n%v", what, ah, bh)
	}
	for k, v := range ah {
		w := bh.Values(k)
		if !slices.Equal(v, w) { // every value, in order
			t.Fatalf("%s: header %s differs: %v vs %v", what, k, v, w)
		}
	}
}

func TestLoginNative_RefusedForBrowser(t *testing.T) {
	e := newEnv(t)
	code := e.requestCode(chenEmail)
	good := func(h ...string) resp {
		return e.do("POST", verifyPath, "", map[string]any{"email": chenEmail, "code": code, "client": "native"}, h...)
	}
	const origin = "https://launchpad.example.test"
	// The reference: a wrong code (web mode, one real try) with the same Origin.
	wrong := e.must(401, e.do("POST", verifyPath, "", map[string]any{"email": chenEmail, "code": wrongCode(code)}, "Origin", origin))

	// Native + a browser header (any value), with the RIGHT code: the same
	// answer as the wrong code, status, body and headers.
	for _, h := range [][]string{
		{"Origin", origin},
		{"Origin", "null"},
		{"Origin", "https://evil.example"},
		{"Sec-Fetch-Site", "same-origin"},
		{"Sec-Fetch-Mode", "cors"},
	} {
		r := e.must(401, good(h...))
		if r.errCode(t) != "invalid_code" || len(r.cookies()) != 0 {
			t.Fatalf("%v: %s", h, r.body)
		}
		sameAnswer(t, h[0]+"="+h[1], r, wrong)
	}
	// A malformed code from a browser in native mode is the same 401, not a hint.
	e.must(401, e.do("POST", verifyPath, "", map[string]any{"email": "nobody@example.com", "code": "x", "client": "native"}, "Origin", origin))

	// The refused requests spent nothing: only the one real wrong try
	// counts, and the code is not consumed.
	if n := e.countRows(`SELECT count(*) FROM login_codes WHERE attempts = 1 AND consumed_at IS NULL`); n != 1 {
		t.Fatalf("code row not as expected (%d)", n)
	}
	// And the app (no browser headers) can still use the same code.
	var nb login.NativeResponse
	e.must(200, good()).json(t, &nb)
	if nb.Token == "" {
		t.Fatal("no token")
	}
}

// The refused request touches no try at all.
func TestLoginNative_RefusedSpendsNoTry(t *testing.T) {
	e := newEnv(t)
	code := e.requestCode(chenEmail)
	for i := 0; i < 20; i++ { // far over the five-try lock
		e.must(401, e.do("POST", verifyPath, "", map[string]any{"email": chenEmail, "code": code, "client": "native"}, "Origin", "https://x.example"))
	}
	if n := e.countRows(`SELECT count(*) FROM login_codes WHERE attempts > 0 OR consumed_at IS NOT NULL`); n != 0 {
		t.Fatalf("refused requests changed the code row (%d)", n)
	}
	e.must(200, e.do("POST", verifyPath, "", map[string]any{"email": chenEmail, "code": code, "client": "native"}))
}

// Web mode, with Origin or without, is unchanged.
func TestLoginWeb_WithOriginStillWorks(t *testing.T) {
	e := newEnv(t)
	code := e.requestCode(chenEmail)
	r := e.must(200, e.do("POST", verifyPath, "", map[string]any{"email": chenEmail, "code": code},
		"Origin", "https://launchpad.example.test", "Sec-Fetch-Site", "same-origin", "Sec-Fetch-Mode", "cors"))
	if r.cookies()[e.names.Session] == nil {
		t.Fatal("web login with Origin set no cookie")
	}
	code = e.requestCode(chenEmail)
	r = e.must(200, e.do("POST", verifyPath, "", map[string]any{"email": chenEmail, "code": code, "client": "web"}, "Origin", "https://x.example"))
	if r.cookies()[e.names.Session] == nil {
		t.Fatal("client web with Origin set no cookie")
	}
}

func TestFromBrowser(t *testing.T) {
	for h, want := range map[string]bool{"": false, "Origin": true, "Sec-Fetch-Site": true, "Sec-Fetch-Mode": true, "User-Agent": false} {
		r, _ := http.NewRequest("POST", "/", nil)
		if h != "" {
			r.Header[h] = []string{""} // present with an empty value counts
		}
		if got := fromBrowser(r); got != want {
			t.Errorf("%q: %v, want %v", h, got, want)
		}
	}
}

// LAUN-145: a refused request takes the same 300 ms floor as a wrong code, so
// timing does not tell them apart; and it logs one fixed line (the header
// NAME only) so an operator can see why a native client cannot log in.
func TestLoginNative_RefusedIsPaddedAndLogged(t *testing.T) {
	e := newEnvWith(t, envOpts{login: login.Options{Floor: 300 * time.Millisecond}})
	code := e.requestCode(chenEmail)
	const secretOrigin = "https://secret-origin-value.example"
	start := time.Now()
	e.must(401, e.do("POST", verifyPath, "", map[string]any{"email": chenEmail, "code": code, "client": "native"}, "Origin", secretOrigin))
	if el := time.Since(start); el < 290*time.Millisecond {
		t.Fatalf("refused request took %v, floor is 300 ms", el)
	}
	e.must(401, e.do("POST", verifyPath, "", map[string]any{"email": chenEmail, "code": code, "client": "native"}, "Sec-Fetch-Mode", "cors"))
	logs := e.logs.String()
	if n := strings.Count(logs, "native login refused"); n != 2 {
		t.Fatalf("want 2 refusal lines, got %d:\n%s", n, logs)
	}
	if !strings.Contains(logs, "header=Origin") || !strings.Contains(logs, "header=Sec-Fetch-Mode") {
		t.Fatalf("header names missing:\n%s", logs)
	}
	for name, v := range map[string]string{"code": code, "origin value": secretOrigin, "email": chenEmail} {
		if strings.Contains(logs, v) {
			t.Errorf("the %s is in the logs", name)
		}
	}
}
