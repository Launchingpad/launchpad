package api

import (
	"reflect"
	"slices"
	"sort"
	"testing"

	"devops/internal/moveapi"
)

// TestOpenAPIMoveSchemasMatchModels is TestOpenAPISchemasMatchModels for the
// LAUN-249 models.
func TestOpenAPIMoveSchemasMatchModels(t *testing.T) {
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
	check("MoveExportStarted", moveapi.StartResult{}, true)
	check("SetupRestoreResponse", moveapi.RestoreResponse{}, true)
	check("MoveSummary", moveapi.Summary{}, true)
}
