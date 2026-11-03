package api

import (
	"reflect"
	"slices"
	"sort"
	"testing"

	"devops/internal/platform"
)

// TestOpenAPIPlatformSchemasMatchModels is TestOpenAPISchemasMatchModels
// for the platform models, kept in its own file. Responses: the
// property names and `required` match the Go fields. Inputs: the property
// names match. Secrets are writeOnly in the input and absent from the
// response.
func TestOpenAPIPlatformSchemasMatchModels(t *testing.T) {
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
	check("PlatformSettings", platform.Settings{}, true)
	check("ActiveMail", platform.ActiveMail{}, true)
	check("MailTestResult", platform.MailTestResult{}, true)
	check("PlatformSettingsInput", platform.SettingsInput{}, false)

	out := s.Components.Schemas["PlatformSettings"].Properties
	for _, secret := range []string{"postmarkToken", "smtpPassword"} {
		if _, ok := out[secret]; ok {
			t.Errorf("PlatformSettings (a response) has the secret %s", secret)
		}
		p, _ := s.Components.Schemas["PlatformSettingsInput"].Properties[secret].(map[string]any)
		if p["writeOnly"] != true {
			t.Errorf("PlatformSettingsInput.%s is not writeOnly", secret)
		}
	}
}
