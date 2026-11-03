package api

import (
	"reflect"
	"slices"
	"sort"
	"testing"

	"devops/internal/notify"
)

// TestOpenAPIPersonalWebhookSchemasMatchModels: the personal webhook models
// match their schemas (LAUN-229, decisions-T40). The URL is writeOnly in the
// inputs and absent from the response.
func TestOpenAPIPersonalWebhookSchemasMatchModels(t *testing.T) {
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
	check("PersonalWebhook", notify.PersonalView{}, true)
	check("PersonalWebhookList", notify.PersonalList{}, true)
	check("PersonalWebhookCreate", notify.PersonalCreateInput{}, false)
	check("PersonalWebhookPatch", notify.PersonalPatchInput{}, false)

	if _, ok := s.Components.Schemas["PersonalWebhook"].Properties["webhookUrl"]; ok {
		t.Error("PersonalWebhook (a response) has webhookUrl")
	}
	for _, n := range []string{"PersonalWebhookCreate", "PersonalWebhookPatch"} {
		p, _ := s.Components.Schemas[n].Properties["webhookUrl"].(map[string]any)
		if p["writeOnly"] != true {
			t.Errorf("%s.webhookUrl is not writeOnly", n)
		}
	}
	for _, n := range []string{"PersonalWebhook", "PersonalWebhookCreate", "PersonalWebhookPatch"} {
		items, _ := s.Components.Schemas[n].Properties["events"].(map[string]any)["items"].(map[string]any)
		var got []string
		for _, v := range items["enum"].([]any) {
			got = append(got, v.(string))
		}
		if !slices.Equal(got, notify.PersonalEvents) {
			t.Errorf("%s.events enum = %v, want %v", n, got, notify.PersonalEvents)
		}
	}
	kind, _ := s.Components.Schemas["PersonalWebhook"].Properties["kind"].(map[string]any)
	var kinds []string
	for _, v := range kind["enum"].([]any) {
		kinds = append(kinds, v.(string))
	}
	if !slices.Equal(kinds, notify.ChatKinds) {
		t.Errorf("PersonalWebhook.kind enum = %v, want %v", kinds, notify.ChatKinds)
	}
}
