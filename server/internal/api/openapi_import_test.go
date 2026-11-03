package api

import (
	"reflect"
	"slices"
	"sort"
	"testing"

	"devops/internal/importer"
	"devops/internal/importjob"
)

// TestOpenAPIImportSchemasMatchModels is TestOpenAPISchemasMatchModels for
// the LAUN-8 import models: the property names match the Go fields, and for
// a response `required` lists exactly the fields that are always present.
func TestOpenAPIImportSchemasMatchModels(t *testing.T) {
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
	check("ImportSession", importjob.SessionView{}, true)
	check("ImportSessionJobs", importjob.SessionJobs{}, true)
	check("ImportJob", importjob.JobView{}, true)
	check("ImportReport", importjob.JobCounts{}, true)
	check("ImportCounts", importer.Counts{}, true)
	check("ImportExclusion", importer.Exclusion{}, true)
	check("ImportUploads", importer.UploadCounts{}, true)
	check("ImportVerifyRow", importer.VerifyRow{}, true)
	check("ImportFailedFile", importjob.FailedFile{}, true)
	check("ImportExportUser", importjob.ExportUser{}, true)
	check("ImportMember", importjob.MemberRef{}, true)
	check("ImportUsers", importjob.UsersView{}, true)
	check("ImportUserMap", importjob.UserMapView{}, true)
	check("ImportStartSessionInput", startSessionBody{}, false)
	check("ImportDryRunInput", dryRunBody{}, false)
	check("ImportRunInput", runBody{}, false)
}
