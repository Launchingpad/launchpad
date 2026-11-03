package api

import (
	"reflect"
	"slices"
	"sort"
	"testing"

	"devops/internal/backupapi"
)

// TestOpenAPIBackupsSchemasMatchModels is TestOpenAPISchemasMatchModels for the
// LAUN-69 backup models, in its own file. Responses: the property names and
// `required` match the Go fields. Inputs: the property names match. Secret
// fields are writeOnly in the input and absent from every response.
func TestOpenAPIBackupsSchemasMatchModels(t *testing.T) {
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
	for name, v := range map[string]any{
		"BackupRecipient": backupapi.Recipient{}, "BackupRecipientList": backupapi.RecipientList{},
		"BackupSettings": backupapi.Settings{}, "BackupRun": backupapi.Run{},
		"BackupDestination": backupapi.Destination{}, "BackupDestinationList": backupapi.DestinationList{},
		"BackupChange": backupapi.ChangeEntry{}, "BackupDump": backupapi.Dump{}, "BackupStatus": backupapi.Status{},
		"BackupStaleDestination": backupapi.StaleDestination{}, "BackupFailedDestination": backupapi.FailedDestination{},
	} {
		check(name, v, true)
	}
	for name, v := range map[string]any{
		"AddBackupRecipientInput": backupapi.AddRecipientInput{}, "BackupSettingsInput": backupapi.SettingsInput{},
		"CreateBackupDestinationInput": backupapi.CreateDestinationInput{},
		"UpdateBackupDestinationInput": backupapi.UpdateDestinationInput{},
		"BackupProbeInput":             backupapi.ProbeInput{}, "BackupTestInput": backupapi.TestInput{},
		"BackupUnsavedTestInput": backupapi.UnsavedTestInput{}, "BackupExchangeInput": backupapi.ExchangeInput{},
	} {
		check(name, v, false)
	}

	// Write-only secrets: in the inputs, never in a response.
	writeOnly := func(schema, prop string) {
		t.Helper()
		p, _ := s.Components.Schemas[schema].Properties[prop].(map[string]any)
		if p["writeOnly"] != true {
			t.Errorf("%s.%s is not writeOnly", schema, prop)
		}
	}
	writeOnly("CreateBackupDestinationInput", "secrets")
	writeOnly("UpdateBackupDestinationInput", "secrets")
	writeOnly("BackupUnsavedTestInput", "secrets")
	writeOnly("BackupExchangeInput", "code")
	for _, resp := range []string{"BackupDestination", "BackupRun", "BackupChange", "BackupStatus", "BackupSettings"} {
		for _, secret := range []string{"secrets", "code", "secretsEnc", "testSecretsEnc", "testConfig", "privateKey", "signingPrivate"} {
			if _, ok := s.Components.Schemas[resp].Properties[secret]; ok {
				t.Errorf("%s (a response) has %s", resp, secret)
			}
		}
	}
}
