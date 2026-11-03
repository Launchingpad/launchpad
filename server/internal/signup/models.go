package signup

import (
	"time"

	"github.com/google/uuid"

	"devops/internal/login"
)

// Wire models of the sign-up and invite routes. api/openapi.yaml documents
// each of them (TestOpenAPISignupSchemasMatchModels keeps the two in step).

// CreateRequest is the POST /signups body.
type CreateRequest struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Username string `json:"username"`
	Note     string `json:"note"` // optional, at most 1000 characters
}

// ConfirmRequest is the POST /signups/confirm body.
type ConfirmRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

// Request is a sign-up request as an org admin sees it. It never holds the
// confirm code or its hash.
type Request struct {
	ID              uuid.UUID  `json:"id"`
	Email           string     `json:"email"`
	Name            string     `json:"name"`
	Username        string     `json:"username"`
	Note            string     `json:"note"`
	Status          string     `json:"status"` // pending | approved | rejected | expired
	EmailVerified   bool       `json:"emailVerified"`
	EmailVerifiedAt *time.Time `json:"emailVerifiedAt"`
	CreatedAt       time.Time  `json:"createdAt"`
	ExpiresAt       time.Time  `json:"expiresAt"`
	DecidedAt       *time.Time `json:"decidedAt"`
	DecidedBy       *uuid.UUID `json:"decidedBy"`
	UserID          *uuid.UUID `json:"userId"`
	RejectReason    string     `json:"rejectReason"`
	Blocked         bool       `json:"blocked"`
}

// RejectInput is the POST /signups/{id}/reject body. Both fields are
// optional.
type RejectInput struct {
	Reason string `json:"reason"`
	Block  bool   `json:"block"`
}

// InviteInput is the POST /invites body. Role is member (default) or admin.
// TeamAccess is "all" (the default when absent) or "selected"; Teams are keys
// or ids of teams of this org, required (1 to 50) for "selected" and refused
// for "all" and for an admin invite (decisions-T20 #26, #30).
type InviteInput struct {
	Email      string   `json:"email"`
	Role       string   `json:"role"`
	TeamAccess string   `json:"teamAccess"`
	Teams      []string `json:"teams"`
}

// ApproveInput is the optional POST /signups/{id}/approve body
// (decisions-T20 #29): the same team access fields as an invite. No body is
// "all".
type ApproveInput struct {
	TeamAccess string   `json:"teamAccess"`
	Teams      []string `json:"teams"`
}

// Invite is an org invite as an org admin sees it. It never holds the token
// or its hash.
type Invite struct {
	ID         uuid.UUID  `json:"id"`
	Email      string     `json:"email"`
	Role       string     `json:"role"`
	Status     string     `json:"status"`  // pending | accepted | revoked | expired
	Emailed    bool       `json:"emailed"` // the link went by email, not to the admin
	CreatedBy  *uuid.UUID `json:"createdBy"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	AcceptedAt *time.Time `json:"acceptedAt"`
	AcceptedBy *uuid.UUID `json:"acceptedBy"`
	RevokedAt  *time.Time `json:"revokedAt"`
	// TeamAccess is what the accept gives: "all" or "selected".
	TeamAccess string `json:"teamAccess"`
	// Teams are the keys of the invite's teams that still exist, sorted;
	// empty for "all".
	Teams []string `json:"teams"`
}

// CreatedInvite is the POST /invites 201 body. When the link was emailed
// (Emailed), Secret and URL are absent: only the address gets the token.
// Otherwise they carry it, and this body is its only copy (only
// sha256(token) is stored).
type CreatedInvite struct {
	Invite  Invite `json:"invite"`
	Secret  string `json:"secret,omitempty"`
	URL     string `json:"url,omitempty"`
	Emailed bool   `json:"emailed"` // the link was queued to the address
}

// AcceptInput is the POST /invites/accept body. Name and username are
// needed only when the invited email is not a user yet.
type AcceptInput struct {
	Token    string `json:"token"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

// AcceptResponse is the POST /invites/accept body. Status created: a new
// user, a session (cookies) and Session, the login body. Status joined: the
// email was a user already; the membership is added and NO session starts
// (the person logs in as usual).
type AcceptResponse struct {
	Status  string          `json:"status"` // created | joined
	Org     login.Org       `json:"org"`
	Session *login.Response `json:"session,omitempty"`
}

// PurgeResult is the DELETE /signups/unconfirmed body.
type PurgeResult struct {
	Deleted int64 `json:"deleted"`
}

// Sign-up request states.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	StatusExpired  = "expired"
)

// Invite states (derived, not stored).
const (
	InviteAccepted = "accepted"
	InviteRevoked  = "revoked"
)

// AcceptResponse statuses.
const (
	AcceptCreated = "created"
	AcceptJoined  = "joined"
)
