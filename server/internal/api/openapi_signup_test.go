package api

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"devops/internal/setup"
	"devops/internal/signup"
)

// TestOpenAPISignupSchemasMatchModels is TestOpenAPISchemasMatchModels for
// the sign-up models. Responses: the property names and `required` match
// the Go fields. Inputs: the property names match. No response schema has a
// token or code property.
func TestOpenAPISignupSchemasMatchModels(t *testing.T) {
	s := loadSpec(t)
	check := func(name string, v any, isResponse bool) {
		t.Helper()
		sc, ok := s.Components.Schemas[name]
		if !ok {
			t.Fatalf("schema %s missing", name)
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
		for _, p := range props {
			if lp := strings.ToLower(p); strings.Contains(lp, "hash") || strings.Contains(lp, "code") || lp == "token" {
				t.Errorf("%s (a response) has the property %s", name, p)
			}
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
	check("SignupRequest", signup.Request{}, true)
	check("Invite", signup.Invite{}, true)
	check("CreatedInvite", signup.CreatedInvite{}, true)
	check("InviteAcceptResponse", signup.AcceptResponse{}, true)
	check("PlatformAdmin", setup.PlatformAdmin{}, true)
	check("SignupPurgeResult", signup.PurgeResult{}, true)
	check("SignupCreateRequest", signup.CreateRequest{}, false)
	check("SignupConfirmRequest", signup.ConfirmRequest{}, false)
	check("SignupRejectInput", signup.RejectInput{}, false)
	check("InviteInput", signup.InviteInput{}, false)
	check("SignupApproveInput", signup.ApproveInput{}, false)
	check("InviteAcceptInput", signup.AcceptInput{}, false)
	check("AddPlatformAdminInput", setup.AddAdminInput{}, false)
}
