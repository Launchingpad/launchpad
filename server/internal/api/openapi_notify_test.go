package api

import (
	"reflect"
	"slices"
	"sort"
	"testing"

	"devops/internal/notify"
)

// TestOpenAPINotifySchemasMatchModels is TestOpenAPISchemasMatchModels for the
// LAUN-32 notification models, in its own file: the property names match the
// Go fields, and for a response `required` matches the always-present fields.
func TestOpenAPINotifySchemasMatchModels(t *testing.T) {
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
	check("NotificationSettings", notify.View{}, true)
	check("NotificationEventSettings", notify.EventView{}, true)
	check("NotificationTeamEvent", notify.TeamEventView{}, true)
	check("NotificationDefault", notify.DefaultPair{}, true)
	check("NotificationOverride", notify.Override{}, true)
	check("NotificationResolved", notify.ResolvedPair{}, true)
	check("NotificationResolvedChannel", notify.ResolvedChannel{}, true)
	check("NotificationMail", notify.MailInfo{}, true)
	check("IssueSubscription", notify.Subscription{}, true)
	check("NotificationSettingsInput", notify.SettingsInput{}, false)
	check("NotificationTeamSettingsInput", notify.TeamSettingsInput{}, false)
	check("NotificationEventInput", notify.EventInput{}, false)
	check("IssueSubscriptionInput", notify.SubscriptionInput{}, false)
	check("InboxNotification", notify.InboxItem{}, true)
	check("InboxActor", notify.InboxActor{}, true)
	check("InboxIssue", notify.InboxIssue{}, true)
	check("InboxData", notify.InboxData{}, true)
	check("NotificationUnreadCount", notify.UnreadCount{}, true)
	check("NotificationReadAllResult", notify.ReadAllResult{}, true)
	check("NotificationPatch", notify.InboxPatch{}, false)
	check("NotificationReadAllInput", notify.ReadAllInput{}, false)
	check("ViewPreference", notify.ViewPreference{}, true)
	check("ViewPreferenceInput", notify.ViewPreferenceInput{}, false)
	check("ViewPreferenceList", notify.ViewPreferenceList{}, true)
	check("UserPreferences", notify.Preferences{}, true)
	check("UserPreferencesInput", notify.PreferencesInput{}, false)

	// The event enum is the Go list.
	var want []string
	for _, e := range notify.Events {
		want = append(want, string(e))
	}
	got := s.Components.Schemas["NotificationEvent"].Enum
	if !slices.Equal(got, want) {
		t.Errorf("NotificationEvent enum %v, want %v", got, want)
	}
}
