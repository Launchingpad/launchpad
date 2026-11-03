package api

import (
	"reflect"
	"slices"
	"sort"
	"testing"

	"devops/internal/notify"
)

// TestOpenAPIChatSchemasMatchModels: the chat service models match their schemas
// (LAUN-204). The URL is writeOnly in the input and absent from the response.
func TestOpenAPIChatSchemasMatchModels(t *testing.T) {
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
	check("ChatService", notify.ChatView{}, true)
	check("ChatServiceList", notify.ChatList{}, true)
	check("ChatTestResult", notify.ChatTestResult{}, true)
	check("ChatServiceCreate", notify.ChatCreateInput{}, false)
	check("ChatServicePatch", notify.ChatPatchInput{}, false)

	if _, ok := s.Components.Schemas["ChatService"].Properties["webhookUrl"]; ok {
		t.Error("ChatService (a response) has webhookUrl")
	}
	for _, n := range []string{"ChatServiceCreate", "ChatServicePatch"} {
		p, _ := s.Components.Schemas[n].Properties["webhookUrl"].(map[string]any)
		if p["writeOnly"] != true {
			t.Errorf("%s.webhookUrl is not writeOnly", n)
		}
	}
	// The event list of the schemas is the notify event list.
	var want []string
	for _, e := range notify.Events {
		want = append(want, string(e))
	}
	for _, n := range []string{"ChatService", "ChatServiceCreate", "ChatServicePatch"} {
		items, _ := s.Components.Schemas[n].Properties["events"].(map[string]any)["items"].(map[string]any)
		var got []string
		for _, v := range items["enum"].([]any) {
			got = append(got, v.(string))
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s.events enum = %v, want %v", n, got, want)
		}
	}
	// The kind enum is the notify kind list, and the error words are the CHECK words.
	kind, _ := s.Components.Schemas["ChatService"].Properties["kind"].(map[string]any)
	var kinds []string
	for _, v := range kind["enum"].([]any) {
		kinds = append(kinds, v.(string))
	}
	if !slices.Equal(kinds, notify.ChatKinds) {
		t.Errorf("ChatService.kind enum = %v, want %v", kinds, notify.ChatKinds)
	}
}
