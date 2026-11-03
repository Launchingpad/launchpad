package api

import (
	"net/http"
	"time"

	"devops/internal/auth"
	"devops/internal/httpx"
	"devops/internal/login"
	"devops/internal/setup"
	"devops/internal/signup"
)

// The sign-up, invite and platform-admin routes
// (docs/internal/decisions-T12-signups-invites.md). The three public routes have the
// /auth/otp shape: requireJSON (no cross-site form post), no
// Idempotency-Key, a floor, and limits; they read no cookie.

// requestSignup: POST /signups. 202 {} whether or not anything was stored.
func (s *Server) requestSignup(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if s.Signup == nil {
		s.fail(w, r, signup.ErrSignupUnavailable)
		return
	}
	err := requireJSON(r)
	var in signup.CreateRequest
	if err == nil {
		err = httpx.DecodeJSON(w, r, &in)
	}
	if err == nil {
		err = s.Signup.RequestSignup(r.Context(), s.clientKey(r), in)
	}
	s.Signup.Pad(r.Context(), start)
	if err != nil {
		s.failLogin(w, r, err) // Retry-After on a 429
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, struct{}{})
}

// confirmSignup: POST /signups/confirm. 204 when the code proved the email.
func (s *Server) confirmSignup(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if s.Signup == nil {
		s.fail(w, r, signup.ErrSignupUnavailable)
		return
	}
	err := requireJSON(r)
	var in signup.ConfirmRequest
	if err == nil {
		err = httpx.DecodeJSON(w, r, &in)
	}
	if err == nil {
		err = s.Signup.ConfirmEmail(r.Context(), s.clientKey(r), in)
	}
	s.Signup.Pad(r.Context(), start)
	if err != nil {
		s.failLogin(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// acceptInvite: POST /invites/accept. 201 with the session cookies for a
// new user; 200 without a session when the email was a user already.
func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if s.Signup == nil {
		s.fail(w, r, signup.ErrInvalidInvite)
		return
	}
	err := requireJSON(r)
	var in signup.AcceptInput
	if err == nil {
		err = httpx.DecodeJSON(w, r, &in)
	}
	var (
		sess login.Session
		out  signup.AcceptResponse
	)
	if err == nil {
		sess, out, err = s.Signup.AcceptInvite(r.Context(), s.clientKey(r), in)
	}
	s.Signup.Pad(r.Context(), start)
	if err != nil {
		s.failLogin(w, r, err)
		return
	}
	if out.Status != signup.AcceptCreated {
		httpx.WriteJSON(w, http.StatusOK, out)
		return
	}
	n := s.cookieNames()
	http.SetCookie(w, s.sessionCookie(n.Session, sess.Secret, true, sess.ExpiresAt))
	http.SetCookie(w, s.sessionCookie(n.CSRF, sess.CSRF, false, sess.ExpiresAt))
	httpx.WriteJSON(w, http.StatusCreated, out)
}

var errSignupOff = httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound, "route not found")

// listSignups: GET /orgs/{org}/signups?status=&limit=.
func (s *Server) listSignups(w http.ResponseWriter, r *http.Request) {
	if s.Signup == nil {
		s.fail(w, r, errSignupOff)
		return
	}
	o, _ := ctxOrgPrincipal(r)
	limit, err := signup.ParseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.Signup.ListRequests(r.Context(), o, r.URL.Query().Get("status"), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[signup.Request]{Items: out})
}

// purgeUnconfirmedSignups: DELETE /orgs/{org}/signups/unconfirmed.
func (s *Server) purgeUnconfirmedSignups(w http.ResponseWriter, r *http.Request) {
	if s.Signup == nil {
		s.fail(w, r, errSignupOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	out, err := s.Signup.PurgeUnconfirmed(r.Context(), o, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// approveSignup: POST /orgs/{org}/signups/{signup}/approve.
func (s *Server) approveSignup(w http.ResponseWriter, r *http.Request) {
	if s.Signup == nil {
		s.fail(w, r, errSignupOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "signup")
	// The body is optional (decisions-T20 #29): none is "all", as before.
	var in signup.ApproveInput
	if err == nil {
		err = optionalJSON(w, r, &in)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.Signup.Approve(r.Context(), o, p, id, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// rejectSignup: POST /orgs/{org}/signups/{signup}/reject {reason?, block?}.
func (s *Server) rejectSignup(w http.ResponseWriter, r *http.Request) {
	if s.Signup == nil {
		s.fail(w, r, errSignupOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "signup")
	if err == nil {
		err = requireJSON(r)
	}
	var in signup.RejectInput
	if err == nil {
		err = httpx.DecodeJSON(w, r, &in)
	}
	var out signup.Request
	if err == nil {
		out, err = s.Signup.Reject(r.Context(), o, p, id, in)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// createInvite: POST /orgs/{org}/invites. The 201 body is the token's only
// copy.
func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	if s.Signup == nil {
		s.fail(w, r, errSignupOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	err := requireJSON(r)
	var in signup.InviteInput
	if err == nil {
		err = httpx.DecodeJSON(w, r, &in)
	}
	var out signup.CreatedInvite
	if err == nil {
		out, err = s.Signup.CreateInvite(r.Context(), o, p, in)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

// listInvites: GET /orgs/{org}/invites?limit=.
func (s *Server) listInvites(w http.ResponseWriter, r *http.Request) {
	if s.Signup == nil {
		s.fail(w, r, errSignupOff)
		return
	}
	o, _ := ctxOrgPrincipal(r)
	limit, err := signup.ParseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.Signup.ListInvites(r.Context(), o, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[signup.Invite]{Items: out})
}

// revokeInvite: DELETE /orgs/{org}/invites/{invite}.
func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request) {
	if s.Signup == nil {
		s.fail(w, r, errSignupOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "invite")
	if err == nil {
		err = s.Signup.RevokeInvite(r.Context(), o, p, id)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listPlatformAdmins: GET /platform/admins.
func (s *Server) listPlatformAdmins(w http.ResponseWriter, r *http.Request) {
	out, err := setup.ListPlatformAdmins(r.Context(), s.pool)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[setup.PlatformAdmin]{Items: out})
}

// addPlatformAdmin: POST /platform/admins {user}. 201 when the user became
// one, 200 when they were one already.
func (s *Server) addPlatformAdmin(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	err := requireJSON(r)
	var in setup.AddAdminInput
	if err == nil {
		err = httpx.DecodeJSON(w, r, &in)
	}
	var (
		out     setup.PlatformAdmin
		created bool
	)
	if err == nil {
		out, created, err = setup.AddPlatformAdmin(r.Context(), s.pool, s.log, p, in)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, out)
}

// removePlatformAdmin: DELETE /platform/admins/{user}.
func (s *Server) removePlatformAdmin(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	id, err := pathUUID(r, "user")
	if err == nil {
		err = setup.RemovePlatformAdmin(r.Context(), s.pool, s.log, p, id)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
