package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"devops/api"
	"devops/internal/auth"
	"devops/internal/httpx"
	"devops/internal/login"
	"devops/internal/setup"
	"devops/internal/tracker"
)

type spec struct {
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Schemas map[string]struct {
			Required   []string       `yaml:"required"`
			Properties map[string]any `yaml:"properties"`
			Enum       []string       `yaml:"enum"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

func loadSpec(t *testing.T) spec {
	t.Helper()
	var s spec
	if err := yaml.Unmarshal(openapi.OpenAPI, &s); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	return s
}

// routesOf walks the real route tree. No database is needed to build it.
func routesOf(t *testing.T) []string {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	s := NewServer(nil, auth.NewAuthenticator(nil, log), nil, func(h http.Handler) http.Handler { return h }, nil, log)
	var out []string
	err := chi.Walk(s.Routes(), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if len(route) > 1 {
			route = strings.TrimSuffix(route, "/")
		}
		out = append(out, method+" "+route)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

var httpMethods = map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true, "head": true, "options": true}

// TestOpenAPIMatchesRoutes fails when a route is added, removed or renamed
// without the same change in api/openapi.yaml (and the other way round).
func TestOpenAPIMatchesRoutes(t *testing.T) {
	s := loadSpec(t)
	var documented []string
	for path, ops := range s.Paths {
		for m := range ops {
			if httpMethods[m] {
				documented = append(documented, strings.ToUpper(m)+" "+path)
			}
		}
	}
	sort.Strings(documented)
	routes := routesOf(t)
	for _, r := range routes {
		if !slices.Contains(documented, r) {
			t.Errorf("route %s is not in api/openapi.yaml", r)
		}
	}
	for _, d := range documented {
		if !slices.Contains(routes, d) {
			t.Errorf("api/openapi.yaml documents %s, which the router does not serve", d)
		}
	}
}

// TestOpenAPIErrorCodes keeps the ErrorCode enum equal to httpx.Codes.
func TestOpenAPIErrorCodes(t *testing.T) {
	s := loadSpec(t)
	want := make([]string, len(httpx.Codes))
	for i, c := range httpx.Codes {
		want[i] = string(c)
	}
	if got := s.Components.Schemas["ErrorCode"].Enum; !slices.Equal(got, want) {
		t.Fatalf("ErrorCode enum = %v\nhttpx.Codes  = %v", got, want)
	}
}

// jsonFields returns the JSON names of t's fields and which are optional
// (omitempty / omitzero).
func jsonFields(typ reflect.Type) (all []string, optional map[string]bool) {
	optional = map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		if f.Anonymous && tag == "" && f.Type.Kind() == reflect.Struct { // an embedded struct is flattened, as encoding/json does
			sub, subOpt := jsonFields(f.Type)
			all = append(all, sub...)
			for k, v := range subOpt {
				optional[k] = v
			}
			continue
		}
		if tag == "-" || !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		all = append(all, name)
		if strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero") {
			optional[name] = true
		}
	}
	sort.Strings(all)
	return all, optional
}

// TestOpenAPISchemasMatchModels checks every wire struct against its schema:
// the property names must match, and for response objects `required` must
// list exactly the fields that are always present.
func TestOpenAPISchemasMatchModels(t *testing.T) {
	s := loadSpec(t)
	responses := map[string]any{
		"Error": httpx.ErrorBody{}, "UserRef": tracker.UserRef{}, "Member": tracker.Member{}, "Me": tracker.Me{},
		"Org": tracker.Org{}, "TeamRef": tracker.TeamRef{}, "Team": tracker.Team{}, "StateRef": tracker.StateRef{},
		"WorkflowState": tracker.WorkflowState{}, "LabelRef": tracker.LabelRef{}, "Label": tracker.Label{},
		"IssueRef": tracker.IssueRef{}, "Issue": tracker.Issue{}, "Relation": tracker.Relation{},
		"Relations": tracker.Relations{}, "StateChange": tracker.StateChange{}, "Comment": tracker.Comment{},
		"Attachment": tracker.Attachment{}, "Event": tracker.Event{}, "Token": tracker.Token{},
		"CreatedToken": tracker.CreatedToken{},
		"LoginUser":    login.User{}, "LoginOrg": login.Org{}, "LoginResponse": login.Response{}, "LoginNativeResponse": login.NativeResponse{},
		//
		"SetupStatus": setup.Status{}, "SetupResponse": setup.Response{},
		// T3
		"Project": tracker.Project{}, "Milestone": tracker.Milestone{}, "Cycle": tracker.Cycle{},
		"CycleSnapshot": tracker.CycleSnapshot{}, "Document": tracker.Document{},
		// T3b
		"PreparedUpload": tracker.PreparedUpload{},
		// T5
		"GitIntegration": tracker.GitIntegration{},
		// T17 (LAUN-7)
		"GitIntegrationCreated": tracker.GitIntegrationCreated{}, "GitIntegrationCheck": tracker.GitIntegrationCheck{},
		"WebhookResult": tracker.WebhookResult{},
		// T17 (LAUN-86)
		"GitRotateResult": tracker.GitRotateResult{}, "GitRepo": tracker.GitRepo{},
		"GitRepoRemoved": tracker.GitRepoRemoved{}, "AvailableRepo": tracker.AvailableRepo{},
		// T18 (LAUN-62)
		"Product": tracker.Product{}, "ProductDetail": tracker.ProductDetail{},
		// T17 (LAUN-83)
		"ChangeRequest": tracker.ChangeRequest{}, "HookRefreshResult": tracker.HookRefreshResult{},
		"BackfillResult": tracker.BackfillResult{},
		// LAUN-56
		"OnboardingStep": tracker.OnboardingStep{}, "Onboarding": tracker.Onboarding{},
		"AgentGuidance": tracker.Guidance{}, "AgentGuidanceEntry": tracker.GuidanceEntry{},
		// LAUN-199
		"GitAccount": tracker.GitAccount{}, "GitAccounts": tracker.GitAccounts{},
		"GitIdentity": tracker.GitIdentity{}, "GitIdentities": tracker.GitIdentities{},
		// LAUN-130
		"Session": tracker.Session{}, "RevokedSessions": tracker.RevokedSessions{},
		// LAUN-194
		"ProjectLabelRef": tracker.ProjectLabelRef{}, "ProjectLabel": tracker.ProjectLabel{},
		// LAUN-225 (decisions-T20 #48)
		"TeamAccessResult": tracker.TeamAccessResult{}, "TeamIssueCount": tracker.TeamIssueCount{},
		"RemovedNotifyRows": tracker.RemovedNotifyRows{},
	}
	inputs := map[string]any{
		"CreateIssueInput": tracker.CreateIssueInput{}, "UpdateIssueInput": tracker.UpdateIssueInput{},
		"TeamInput": tracker.TeamInput{}, "LabelInput": tracker.LabelInput{}, "CommentInput": tracker.CommentInput{},
		"RelationInput": tracker.RelationInput{}, "TokenInput": tracker.TokenInput{},
		"LoginCodeRequest": login.CodeRequest{}, "LoginVerifyRequest": login.VerifyRequest{},
		"SetupRequest": setup.Request{}, "SetupUserInput": setup.UserInput{}, "SetupOrgInput": setup.OrgInput{},
		"SetupTeamInput": setup.TeamInput{},
		// T3
		"ProjectLabelInput": tracker.ProjectLabelInput{}, "UpdateProjectLabelInput": tracker.UpdateProjectLabelInput{},
		"UpdateTeamInput": tracker.UpdateTeamInput{}, "CreateProjectInput": tracker.CreateProjectInput{},
		"UpdateProjectInput": tracker.UpdateProjectInput{}, "CreateMilestoneInput": tracker.CreateMilestoneInput{},
		"UpdateMilestoneInput": tracker.UpdateMilestoneInput{}, "UpdateCycleInput": tracker.UpdateCycleInput{},
		"CreateDocumentInput": tracker.CreateDocumentInput{}, "UpdateDocumentInput": tracker.UpdateDocumentInput{},
		// T3b
		"PrepareUploadInput": tracker.PrepareUploadInput{}, "CreateAttachmentInput": tracker.CreateAttachmentInput{},
		// T5
		"CreateGitIntegrationInput": tracker.CreateGitIntegrationInput{},
		"UpdateGitIntegrationInput": tracker.UpdateGitIntegrationInput{},
		"CreateGitRepoInput":        tracker.CreateGitRepoInput{},
		"UpdateGitRepoInput":        tracker.UpdateGitRepoInput{},
		"RotateGitSecretInput":      rotateGitSecretBody{},
		"CommitGitSecretInput":      commitGitSecretBody{},
		//
		"AgentInput": tracker.AgentInput{},
		// T18 (LAUN-62)
		"CreateProductInput": tracker.CreateProductInput{}, "UpdateProductInput": tracker.UpdateProductInput{},
		"SetProductTeamsInput": SetProductTeamsInput{},
		// LAUN-56
		"UpdateOrgInput": tracker.UpdateOrgInput{}, "OnboardingSkipInput": SkipInput{},
		// LAUN-200
		"AgentGuidanceInput": tracker.GuidanceInput{}, "GitAccountInput": tracker.GitAccountInput{},
		// LAUN-225
		"TeamAccessInput": tracker.TeamAccessInput{},
	}
	check := func(name string, v any, isResponse bool) {
		sc, ok := s.Components.Schemas[name]
		if !ok {
			t.Errorf("schema %s missing", name)
			return
		}
		fields, optional := jsonFields(reflect.TypeOf(v))
		var props []string
		for p := range sc.Properties {
			props = append(props, p)
		}
		sort.Strings(props)
		if !slices.Equal(fields, props) {
			t.Errorf("%s: Go fields %v\n  schema properties %v", name, fields, props)
		}
		if !isResponse {
			return
		}
		var always []string
		for _, f := range fields {
			if !optional[f] {
				always = append(always, f)
			}
		}
		req := slices.Clone(sc.Required)
		sort.Strings(req)
		if !slices.Equal(always, req) {
			t.Errorf("%s: always-present fields %v\n  schema required %v", name, always, req)
		}
	}
	for name, v := range responses {
		check(name, v, true)
	}
	for name, v := range inputs {
		check(name, v, false)
	}
}

// flowMapNullKeys walks the YAML tree and reports every key of a flow map
// ({ a: b, c: d }) that looks like a cut fragment. An unquoted comma inside a
// plain scalar ends the scalar there: `{ description: a, b }` parses as
// description "a" plus a key "b" with no value, and `{ description: a, see the b: c }`
// parses as a bogus key "see the b". The text after the comma is lost (LAUN-139,
// LAUN-140).
//
// This guard catches the SYMPTOMS only: a flow-map key with an empty value, or
// a key that contains a space. It does not catch every possible comma cut. In
// particular a duplicate key (`description: x, type: integer`) is NOT caught by
// this test. Nor is a one-word fragment that has a value (`description: a, see: b`).
func flowMapNullKeys(n *yaml.Node, out *[]string) {
	if n.Kind == yaml.MappingNode && n.Style&yaml.FlowStyle != 0 {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if v.Kind == yaml.ScalarNode && v.Tag == "!!null" && v.Value == "" {
				*out = append(*out, fmt.Sprintf("line %d: key %q has no value: an unquoted comma cut the text before it; quote the description text", k.Line, k.Value))
			} else if strings.Contains(k.Value, " ") {
				*out = append(*out, fmt.Sprintf("line %d: key %q contains a space: an unquoted comma cut the text before it; quote the description text", k.Line, k.Value))
			}
		}
	}
	for _, c := range n.Content {
		flowMapNullKeys(c, out)
	}
}

// TestOpenAPIFlowMapsHaveNoCutText fails when an unquoted comma in a
// flow-map description cut the text short.
func TestOpenAPIFlowMapsHaveNoCutText(t *testing.T) {
	var root yaml.Node
	if err := yaml.Unmarshal(openapi.OpenAPI, &root); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	var bad []string
	flowMapNullKeys(&root, &bad)
	for _, b := range bad {
		t.Error(b)
	}
}
