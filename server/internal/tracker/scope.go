package tracker

import (
	"fmt"

	"devops/internal/auth"
)

// Team scope helpers (T20 #15). The scope predicate lives in the SQL
// statement that finds the row (#11) and is ALWAYS emitted, for every caller:
// an org admin and a member with access to all teams bind all_teams = true, so
// the OR is satisfied and the text is the same. Only the arguments differ.
//
// These two functions are the only place the SQL text of a hand-written query
// lives. A sqlc query carries the same text with sqlc.arg(all_teams) and
// sqlc.arg(team_ids).

// argFunc binds a value to the next positional parameter of a hand-written
// query and returns its placeholder ("$3").
type argFunc func(v any) string

// teamScope returns the condition "the team in col is visible to org". It adds
// the two arguments (all_teams, team_ids) and never returns an empty clause.
func teamScope(org *auth.Org, col string, arg argFunc) string {
	all, ids := org.Access.Args()
	return fmt.Sprintf("(%s::bool OR %s = ANY(%s::uuid[]))", arg(all), col, arg(ids))
}

// projectScope returns the condition "the project aliased as p is visible to
// org" (T20 #36): the caller sees one of its teams, or it has none (an org-level
// project). It adds the two arguments.
func projectScope(org *auth.Org, p string, arg argFunc) string {
	all, ids := org.Access.Args()
	a, t := arg(all), arg(ids)
	return fmt.Sprintf(`/* scope: project */ (%[1]s::bool
		OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = %[3]s.org_id AND pst.project_id = %[3]s.id)
		OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = %[3]s.org_id AND pst.project_id = %[3]s.id
			AND pst.team_id = ANY(%[2]s::uuid[])))`, a, t, p)
}

// documentScope returns the condition "the document aliased as d is visible to
// org": a document with no project is org-level (T20 #7, #38); otherwise it
// follows its project (projectScope).
func documentScope(org *auth.Org, d string, arg argFunc) string {
	return fmt.Sprintf(`/* scope: project */ (%[1]s.project_id IS NULL OR EXISTS (SELECT 1 FROM projects dp WHERE dp.org_id = %[1]s.org_id AND dp.id = %[1]s.project_id AND %[2]s))`,
		d, projectScope(org, "dp", arg))
}
