package setup

import (
	"devops/internal/login"
	"devops/internal/tracker"
)

// Wire models of the /setup routes. api/openapi.yaml documents each of them
// (TestOpenAPISchemasMatchModels keeps the two in step).

// Status is the GET /setup body.
type Status struct {
	SetupRequired bool `json:"setupRequired"`
}

// Request is the POST /setup body.
type Request struct {
	Token string    `json:"token"` // the fragment of the setup link
	User  UserInput `json:"user"`
	Org   OrgInput  `json:"org"`
	Team  TeamInput `json:"team"`
}

// UserInput is the first user: a platform admin and the org's admin.
type UserInput struct {
	Username string `json:"username"`
	Name     string `json:"name"`
	Email    string `json:"email"`
}

// OrgInput is the install's one org.
type OrgInput struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// TeamInput is the org's first team.
type TeamInput struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// Response is the POST /setup 201 body: the login body (as POST
// /auth/otp/verify) plus the first admin token. token.secret is shown here
// once and stored nowhere.
type Response struct {
	User      login.User           `json:"user"`
	Orgs      []login.Org          `json:"orgs"`
	CSRFToken string               `json:"csrfToken"`
	Token     tracker.CreatedToken `json:"token"`
}
