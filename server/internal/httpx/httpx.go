// Package httpx holds the single wire-response writers and the error-code
// registry. Every error the API returns is {"error": msg, "code": code}, and
// code is always one of the Code constants below (the OpenAPI ErrorCode enum
// is checked against Codes by internal/api's drift test).
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Code is a registered wire error code.
type Code string

// The registered error codes. Add a code here AND to the ErrorCode enum in
// api/openapi.yaml; the drift test fails if they differ.
const (
	CodeBadRequest            Code = "bad_request"
	CodeValidationFailed      Code = "validation_failed"
	CodeUnauthenticated       Code = "unauthenticated"
	CodeInvalidToken          Code = "invalid_token"
	CodeTokenExpired          Code = "token_expired"
	CodeTokenRevoked          Code = "token_revoked"
	CodeInsufficientScope     Code = "insufficient_scope"
	CodeForbidden             Code = "forbidden"
	CodeNotFound              Code = "not_found"
	CodeMethodNotAllowed      Code = "method_not_allowed"
	CodeConflict              Code = "conflict"
	CodeVersionConflict       Code = "version_conflict"
	CodeIdempotencyKeyReused  Code = "idempotency_key_reused"
	CodeIdempotencyInProgress Code = "idempotency_in_progress"
	// The first request's write committed but its response was not stored.
	// The key will never re-run it; the caller must read the resource.
	CodeIdempotencyOutcomeUnknown Code = "idempotency_outcome_unknown"
	CodePayloadTooLarge           Code = "payload_too_large"
	CodeUnsupportedMediaType      Code = "unsupported_media_type"
	CodeInternal                  Code = "internal_error"
	// T6a: web login.
	//
	// A login code that is wrong, expired, used, replaced or locked (401).
	// The cases are not told apart, so a caller learns nothing about which.
	CodeInvalidCode Code = "invalid_code"
	// A cookie-authenticated write without a matching X-CSRF-Token (403).
	CodeCSRFFailed Code = "csrf_failed"
	// Too many requests (429).
	CodeRateLimited Code = "rate_limited"
	// The login feature is off: no mail transport is configured (503).
	CodeLoginUnavailable Code = "login_unavailable"
	// no object store is configured, so file attachments are off
	// (503).
	CodeAttachmentsUnavailable Code = "attachments_unavailable"
	// first-run setup.
	//
	// The setup link is wrong, expired, used or replaced by a newer one
	// (401). The cases are not told apart.
	CodeInvalidSetupToken Code = "invalid_setup_token"
	// Setup is done: the install has a user (409).
	CodeSetupComplete Code = "setup_complete"
	// the platform mail test could not send (502). The message
	// is the mailer's safe text: the step and a reply code, never the
	// server's reply text or a secret.
	CodeMailSendFailed Code = "mail_send_failed"
	// LAUN-198 and LAUN-204 (T33, T36): the test message of a chat service could
	// not be posted (502). The message holds a fixed word, never the service's
	// reply or the webhook URL.
	CodeChatSendFailed Code = "chat_send_failed"
	// LAUN-197 (T35): the test push could not be delivered to the relay (502).
	// The message holds a fixed word, never the relay's reply, its URL or a token.
	CodePushSendFailed Code = "push_send_failed"
	// sign-up requests and invites.
	//
	// The sign-up mode of the install does not allow this (403): a sign-up
	// request when the mode is not approval, an invite when it is closed.
	// One fixed message per route, so it tells nothing else.
	CodeSignupUnavailable Code = "signup_unavailable"
	// The invite token is wrong, expired, used or revoked (401). The cases
	// are not told apart.
	CodeInvalidInvite Code = "invalid_invite"
	// A placeAfter / placeBefore neighbour is unknown or no longer in the
	// destination column (409, LAUN-51). The client refetches the column.
	CodeStalePlacement Code = "stale_placement"

	// CodeLengthRequired (411): a GitHub or Bitbucket webhook with no
	// Content-Length (T17 #14).
	CodeLengthRequired Code = "length_required"
	// CodeOverloaded (503 with Retry-After): the signed-body webhooks'
	// in-flight byte budget is spent (T17 #14); also a deadlock or
	// serialization failure on any write (database.Retryable, LAUN-191),
	// which the idempotency middleware never stores.
	CodeOverloaded Code = "overloaded"

	// T17 #12 (LAUN-7): git integration config.
	//
	// More than 8 enabled integrations of one provider in an org (409).
	CodeIntegrationLimit Code = "integration_limit"
	// A token or an HMAC secret must be stored and the server has no
	// SETTINGS_ENCRYPTION_KEY (409).
	CodeEncryptionUnavailable Code = "encryption_unavailable"
	// Token mode needs an absolute https PUBLIC_BASE_URL (409).
	CodePublicURLRequired Code = "public_url_required"
	// LAUN-104: the reasons a git integration save or remove is refused (409),
	// so a client can say which one without reading the message.
	//
	// A second integration of this provider for the same base URL.
	CodeIntegrationExists Code = "integration_exists"
	// Another enabled integration of the provider uses this webhook secret.
	CodeSecretInUse Code = "secret_in_use"
	// Token mode needs a sealed webhook secret; this one is a hash only. Rotate first.
	CodeSecretHashOnly Code = "secret_hash_only"
	// A repository is mapped: the URL cannot change and the integration cannot be removed.
	CodeReposMapped Code = "repos_mapped"
	// The integration changed while the request ran; a retry works.
	CodeIntegrationChanged Code = "integration_changed"
	// A call to the git host failed. Our own codes: no host text is echoed.
	CodeProviderUnreachable  Code = "provider_unreachable"  // 502
	CodeProviderUnauthorized Code = "provider_unauthorized" // 502, the host refused OUR token (not our 401)
	CodeProviderForbidden    Code = "provider_forbidden"    // 502, the token lacks a scope
	CodeProviderRateLimited  Code = "provider_rate_limited" // 503 with Retry-After
	CodeProviderError        Code = "provider_error"        // 502, any other host failure

	// LAUN-8 (T17 #12, #43): the Linear import in the web app.
	//
	// An import job is active (409): any start, a user-map save and a
	// session close while one of the session's jobs runs.
	CodeJobActive Code = "job_active"
	// Cycles cannot be turned on while an import is under way (409).
	CodeImportInProgress Code = "import_in_progress"
	// A real run needs a succeeded dry run on the current export and user
	// map (409).
	CodeDryRunRequired Code = "dry_run_required"
	// Linear refused the API key (502) or could not be reached (502); the
	// message never carries Linear's text.
	CodeLinearUnauthorized Code = "linear_unauthorized"
	CodeLinearUnreachable  Code = "linear_unreachable"
	// The import work directory has too little free space (507).
	CodeNoSpace Code = "no_space"
	// Backups (LAUN-69, decisions-T16 #43). backup_unavailable (503): an action that
	// cannot start now (no BACKUP_ENCRYPTION_KEY, or a destination type this build has no
	// driver for). destination_busy (409): a backup run of the destination is queued,
	// waiting or running.
	CodeBackupUnavailable Code = "backup_unavailable"
	CodeDestinationBusy   Code = "destination_busy"

	// LAUN-191: a team delete is refused (409) while the team still has
	// issues, or is the org's last team or its default team.
	CodeTeamInUse Code = "team_in_use"

	// LAUN-172 (decisions-T27): a cycle write that a rule forbids now (409),
	// and a cycle delete refused because the cycle still has issues (409).
	CodeCycleConflict Code = "cycle_conflict"
	CodeCycleInUse    Code = "cycle_in_use"

	// LAUN-195 (decisions-T29): a project status delete refused (409) because projects still use it, or
	// because it is the last status of its kind.
	CodeStatusInUse      Code = "status_in_use"
	CodeStatusLastOfKind Code = "status_last_of_kind"
)

// Codes lists every registered code, in declaration order.
var Codes = []Code{
	CodeBadRequest, CodeValidationFailed, CodeUnauthenticated, CodeInvalidToken,
	CodeTokenExpired, CodeTokenRevoked, CodeInsufficientScope, CodeForbidden,
	CodeNotFound, CodeMethodNotAllowed, CodeConflict, CodeVersionConflict,
	CodeIdempotencyKeyReused, CodeIdempotencyInProgress, CodeIdempotencyOutcomeUnknown, CodePayloadTooLarge,
	CodeUnsupportedMediaType, CodeInternal,
	CodeInvalidCode, CodeCSRFFailed, CodeRateLimited, CodeLoginUnavailable,
	CodeAttachmentsUnavailable,
	CodeInvalidSetupToken, CodeSetupComplete,
	CodeMailSendFailed, CodeChatSendFailed, CodePushSendFailed,
	CodeSignupUnavailable, CodeInvalidInvite,
	CodeStalePlacement,
	CodeLengthRequired, CodeOverloaded,
	CodeIntegrationLimit, CodeEncryptionUnavailable, CodePublicURLRequired,
	CodeIntegrationExists, CodeSecretInUse, CodeSecretHashOnly, CodeReposMapped, CodeIntegrationChanged,
	CodeProviderUnreachable, CodeProviderUnauthorized, CodeProviderForbidden,
	CodeProviderRateLimited, CodeProviderError,
	CodeJobActive, CodeImportInProgress, CodeDryRunRequired,
	CodeLinearUnauthorized, CodeLinearUnreachable, CodeNoSpace,
	CodeBackupUnavailable, CodeDestinationBusy,
	CodeTeamInUse, CodeCycleConflict, CodeCycleInUse,
	CodeStatusInUse, CodeStatusLastOfKind,
}

// ErrorBody is the wire shape of every error.
type ErrorBody struct {
	Error string `json:"error"`
	Code  Code   `json:"code"`
}

// WriteJSON writes v as a JSON body with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the error envelope.
func WriteError(w http.ResponseWriter, status int, code Code, msg string) {
	WriteJSON(w, status, ErrorBody{Error: msg, Code: code})
}

// Error is a typed API error. Service code returns it; WriteErr maps it.
type Error struct {
	Status int
	Code   Code
	Msg    string
	// RetryAfter, when above 0, is sent as the Retry-After header (seconds).
	RetryAfter int
}

func (e *Error) Error() string { return e.Msg }

// Errorf builds an *Error.
func Errorf(status int, code Code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Msg: fmt.Sprintf(format, args...)}
}

// NotFound is the common 404.
func NotFound(what string) *Error {
	return &Error{Status: http.StatusNotFound, Code: CodeNotFound, Msg: what + " not found"}
}

// Invalid is the common 400 for a field that failed validation.
func Invalid(format string, args ...any) *Error {
	return Errorf(http.StatusBadRequest, CodeValidationFailed, format, args...)
}

// WriteErr writes err. An *Error keeps its status and code; anything else is
// an internal error, whose detail is logged by the caller and never sent.
func WriteErr(w http.ResponseWriter, err error) (internal bool) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		if apiErr.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(apiErr.RetryAfter))
		}
		WriteError(w, apiErr.Status, apiErr.Code, apiErr.Msg)
		return false
	}
	WriteError(w, http.StatusInternalServerError, CodeInternal, "internal error")
	return true
}

// MaxBodyBytes bounds every JSON request body.
const MaxBodyBytes = 1 << 20

// DecodeJSON reads one JSON object into dst. Unknown fields are rejected so a
// misspelled field fails loudly instead of being ignored.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return Errorf(http.StatusUnsupportedMediaType, CodeUnsupportedMediaType, "content type must be application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			return Errorf(http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "request body is larger than %d bytes", MaxBodyBytes)
		case errors.Is(err, io.EOF):
			return Errorf(http.StatusBadRequest, CodeBadRequest, "request body is empty")
		default:
			return Errorf(http.StatusBadRequest, CodeBadRequest, "invalid JSON body: %v", err)
		}
	}
	if dec.More() {
		return Errorf(http.StatusBadRequest, CodeBadRequest, "request body must be a single JSON object")
	}
	return nil
}

// Opt is a JSON field that tells "absent" from "null" from a value. PATCH
// bodies use it: absent leaves a field alone, null clears it.
type Opt[T any] struct {
	Set  bool
	Null bool
	V    T
}

// UnmarshalJSON records that the field was present.
func (o *Opt[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Null = true
		return nil
	}
	return json.Unmarshal(b, &o.V)
}

// Page is the list envelope: {items, nextCursor}.
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}
