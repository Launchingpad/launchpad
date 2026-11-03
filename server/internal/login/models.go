package login

import (
	"time"

	"github.com/google/uuid"
)

// Wire models of the /auth routes. api/openapi.yaml documents each of them
// (TestOpenAPISchemasMatchModels keeps the two in step).

// CodeRequest is the POST /auth/otp body.
type CodeRequest struct {
	Email string `json:"email"`
}

// VerifyRequest is the POST /auth/otp/verify body.
type VerifyRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
	// Client is "web" (default, also when absent): cookies. "native" (LAUN-122):
	// the 200 body holds the native token (kind native), no cookie is set, no CSRF value.
	Client string `json:"client,omitempty"`
}

// Client values of VerifyRequest.
const (
	ClientWeb    = "web"
	ClientNative = "native"
)

// User is the person who logged in.
type User struct {
	ID       uuid.UUID `json:"id"`
	Username string    `json:"username"`
	Name     string    `json:"name"`
	Email    string    `json:"email"`
	// IsPlatformAdmin: this person runs the install. It does not
	// depend on the token: an org-pinned token of a platform admin says true
	// here and still cannot use the platform routes.
	IsPlatformAdmin bool `json:"isPlatformAdmin"`
}

// Org is one org the person belongs to, with their role.
type Org struct {
	ID   uuid.UUID `json:"id"`
	Slug string    `json:"slug"`
	Name string    `json:"name"`
	Role string    `json:"role"`
}

// Response is the POST /auth/otp/verify 200 body. CSRFToken equals the
// CSRF cookie (__Host-odt_csrf, or odt_csrf on a local server); send it back
// as X-CSRF-Token on every write.
type Response struct {
	User      User   `json:"user"`
	Orgs      []Org  `json:"orgs"`
	CSRFToken string `json:"csrfToken"`
}

// NativeResponse is the POST /auth/otp/verify 200 body for client "native"
// (LAUN-122). Token is the native token (kind native, LAUN-129): send it as "Authorization: Bearer".
// It is shown once. There is no CSRF value.
type NativeResponse struct {
	User      User      `json:"user"`
	Orgs      []Org     `json:"orgs"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}
