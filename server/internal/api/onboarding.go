package api

import (
	"net/http"

	"devops/internal/httpx"
	"devops/internal/tracker"
)

// The first-run checklist and the org settings (LAUN-56;
// docs/internal/decisions-T17-onboarding.md). All routes are org admin
// (admin scope). The body holds booleans and ids only, so an org admin who is
// not a platform admin can read the mail and sign-up state without a platform
// setting, a secret or a provider name.

// platformFacts are the install-level facts the checklist needs.
func (s *Server) platformFacts() tracker.PlatformFacts {
	pf := tracker.PlatformFacts{Capabilities: []string{}}
	if s.Platform != nil {
		pf.MailWorking = s.Platform.MailWorking()
	}
	// The web import needs the manager, the attachment store and PUBLIC_BASE_URL
	// (503 attachments_unavailable / 409 public_url_required otherwise).
	if s.Import != nil && s.svc.AttachmentsEnabled() && s.svc.PublicBaseURLSet() {
		pf.Capabilities = append(pf.Capabilities, tracker.CapLinearImport)
	}
	// The git connection routes are always mounted (webhook mode needs no token key).
	pf.Capabilities = append(pf.Capabilities, tracker.CapGitHosts)
	if s.svc.AttachmentsEnabled() {
		pf.Capabilities = append(pf.Capabilities, tracker.CapAttachments)
	}
	return pf
}

func (s *Server) getOnboarding(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	out, err := s.svc.GetOnboarding(r.Context(), o, s.platformFacts())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// SkipInput is the PUT /orgs/{org}/onboarding/skip body.
type SkipInput struct {
	Step    string `json:"step"`
	Skipped *bool  `json:"skipped"`
}

func (s *Server) setOnboardingSkip(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	if err := requireJSON(r); err != nil {
		s.fail(w, r, err)
		return
	}
	var in SkipInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.Skipped == nil {
		s.fail(w, r, httpx.Invalid("skipped is required (true or false)"))
		return
	}
	out, err := s.svc.SetOnboardingSkip(r.Context(), o, p, in.Step, *in.Skipped, s.platformFacts())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) updateOrg(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	if err := requireJSON(r); err != nil {
		s.fail(w, r, err)
		return
	}
	var in tracker.UpdateOrgInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.svc.UpdateOrg(r.Context(), o, p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
