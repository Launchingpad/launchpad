package api

import (
	"reflect"
	"slices"
	"sort"
	"testing"

	"devops/internal/notify"
)

// TestOpenAPIPushSchemasMatchModels: the push models match their schemas
// (LAUN-197). The relay URL, the relay secret and the device token are
// writeOnly in the inputs and absent from every response.
func TestOpenAPIPushSchemasMatchModels(t *testing.T) {
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
	check("PushRelay", notify.PushView{}, true)
	check("PushTestResult", notify.PushTestResult{}, true)
	check("PushRelayInput", notify.PushInput{}, false)
	check("PushDeviceInput", notify.DeviceInput{}, false)
	check("PushDevice", notify.DeviceView{}, true)
	check("PushDevices", notify.DeviceList{}, true)
	check("NotificationPush", notify.PushInfo{}, true)

	for _, c := range []struct{ schema, field string }{
		{"PushRelayInput", "relayUrl"}, {"PushRelayInput", "relaySecret"}, {"PushDeviceInput", "token"},
	} {
		p, _ := s.Components.Schemas[c.schema].Properties[c.field].(map[string]any)
		if p["writeOnly"] != true {
			t.Errorf("%s.%s is not writeOnly", c.schema, c.field)
		}
	}
	for _, c := range []struct{ schema, field string }{
		{"PushRelay", "relayUrl"}, {"PushRelay", "relaySecret"}, {"PushRelay", "url"},
		{"PushDevice", "token"}, {"PushDevices", "token"},
	} {
		if _, ok := s.Components.Schemas[c.schema].Properties[c.field]; ok {
			t.Errorf("%s (a response) has %s", c.schema, c.field)
		}
	}
	// The lastError enum is the set of words the sender can store.
	want := []string{notify.PushRejected, notify.PushUnauthorized, notify.PushRateLimited, notify.PushUnavailable,
		notify.PushUnreachable, notify.PushBlocked, notify.PushRedirect, notify.PushUnreadable}
	le, _ := s.Components.Schemas["PushRelay"].Properties["lastError"].(map[string]any)
	var got []string
	for _, v := range le["enum"].([]any) {
		if str, ok := v.(string); ok {
			got = append(got, str)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("PushRelay.lastError enum = %v, want %v", got, want)
	}
}
