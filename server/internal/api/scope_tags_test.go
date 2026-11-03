package api

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Test C of decisions-T20 #16 and #18: every query carries a scope tag.
//
// A sqlc query (db/queries/*.sql) has a "-- scope: <tag>" comment; a SQL string
// literal in a non-test Go file that names a tenant table (a table with an
// org_id column in db/schema.sql) has a "/* scope: <tag> */" comment. The tags:
//
//	team      filters by the visible set (#15)
//	project   uses the project rule (#36)
//	feed      the change feed predicate of #55 (events.scope, events.team_ids)
//	resolved  by primary key; the caller got the id from a scoped resolver (#14)
//	own       the caller's own rows (#62)
//	org       an org-level table (#7)
//	admin     the route is org admin only
//	system    no caller: the cycle job, the consumer, webhooks, the importer, setup
//	pending   a later build ticket scopes it (the note's inventory names it)
//	none      names no tenant table (checked: a "none" query on a tenant table fails)
//
// The SQL is scanned, not grepped: comments are split from the code, string
// literal contents are blanked (a '--' in a literal starts no comment), and a
// tag counts only in a comment. A table counts wherever its name stands as a
// word in the code (comma joins, sub-selects, LATERAL, "quoted", public.x).
//
// A sqlc team query has the #15 predicate in its code, in parentheses:
// (sqlc.arg(all_teams)::bool OR x.team_id = ANY(sqlc.arg(team_ids)::uuid[]));
// a project query has the #36 shape (all_teams OR NOT EXISTS project_teams OR
// team_id = ANY(team_ids)); a product follows the same rule over product_teams
// (#69, products.sql only); a feed query has the four cases of #55 and the
// is_admin argument. A Go team or project literal sits in a declaration
// (or, for a package-level template, a file) that calls teamScope,
// projectScope or documentScope. Test C cannot check SQL that is composed at
// run time; Test B carries that. Every other tag but none is listed in the
// reviewed golden file testdata/scope_optouts.golden, one line per query, or
// per Go declaration and tag (two literals with the same tag in one function
// share a line).

const scopeGolden = "testdata/scope_optouts.golden"

var (
	scopeCommentRe = regexp.MustCompile(`^(?:--|/\*)[ \t]*scope:[ \t]*([A-Za-z_]+)`)
	sqlNameRe      = regexp.MustCompile(`(?m)^[ \t]*--[ \t]*name:[ \t]*(\w+)`)
	dbqNameRe      = regexp.MustCompile("(?m)^const \\w+ = `-- name: (\\w+) :")
	upperKeywordRe = regexp.MustCompile(`\b(?:SELECT|INSERT|UPDATE|DELETE|FROM|JOIN|WHERE)\b`)
	sqlShapeRe     = regexp.MustCompile(`(?is)\bselect\b.+\bfrom\b|\binsert\s+into\b|\bupdate\s+\w+\s+set\b|\bdelete\s+from\b`)
	teamShapeRe    = regexp.MustCompile(`\(\s*sqlc\.arg\(all_teams\)::bool\s+OR\s+[\w.]+\s*=\s*ANY\(\s*sqlc\.arg\(team_ids\)::uuid\[\]\s*\)`)
	projectShapeRe = regexp.MustCompile(`(?i)\(\s*sqlc\.arg\(all_teams\)::bool\s+OR\s+NOT\s+EXISTS\s*\(\s*SELECT\s+1\s+FROM\s+project_teams\b`)
	// productShapeRe is the #36 shape over product_teams (#69): accepted in
	// db/queries/products.sql only.
	productShapeRe = regexp.MustCompile(`(?i)\(\s*sqlc\.arg\(all_teams\)::bool\s+OR\s+NOT\s+EXISTS\s*\(\s*SELECT\s+1\s+FROM\s+product_teams\b`)
	projectAnyRe   = regexp.MustCompile(`team_id\s*=\s*ANY\(\s*sqlc\.arg\(team_ids\)::uuid\[\]\s*\)`)
	// The #55 feed predicate: admin, unrestricted (not an admin row), org, any
	// (overlap), all (subset, never of an empty set).
	feedShapeRes = []*regexp.Regexp{
		regexp.MustCompile(`\(\s*sqlc\.arg\(is_admin\)::bool\s+OR\s+\(\s*sqlc\.arg\(all_teams\)::bool\s+AND\s+[\w.]*scope\s*<>\s*'admin'\s*\)`),
		regexp.MustCompile(`OR\s+[\w.]*scope\s*=\s*'org'`),
		regexp.MustCompile(`OR\s+\(\s*[\w.]*scope\s*=\s*'any'\s+AND\s+[\w.]*team_ids\s*&&\s*sqlc\.arg\(team_ids\)::uuid\[\]\s*\)`),
		regexp.MustCompile(`OR\s+\(\s*[\w.]*scope\s*=\s*'all'\s+AND\s+cardinality\([\w.]*team_ids\)\s*>\s*0\s+AND\s+[\w.]*team_ids\s*<@\s*sqlc\.arg\(team_ids\)::uuid\[\]\s*\)`),
	}
	scopeTags = map[string]bool{"team": true, "project": true, "feed": true, "resolved": true, "own": true, "org": true,
		"admin": true, "system": true, "pending": true, "none": true}
	scopeHelpers = map[string]bool{"teamScope": true, "projectScope": true, "documentScope": true}
)

// sqlUnit is one sqlc query or one Go string literal.
type sqlUnit struct {
	Where string // db/queries/issues.sql or internal/tracker/issue_read.go
	Name  string // the query name, or the enclosing Go declaration
	Line  int
	Text  string
	Go    bool
	// CallsHelper (Go): the enclosing declaration calls a scope helper or is
	// one; for a package-level declaration, the file calls one.
	CallsHelper bool
}

// sqlScan splits SQL into its code and its comments. The contents of string
// literals ('...', E'...', $tag$...$tag$) are blanked in the code; quoted
// identifiers ("issues") are kept. Comments are cut from the code.
func sqlScan(src string) (code string, comments []string) {
	var b strings.Builder
	dollar := regexp.MustCompile(`^\$[A-Za-z_]*\$`)
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '-' && strings.HasPrefix(src[i:], "--"):
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				end = len(src) - i
			}
			comments = append(comments, src[i:i+end])
			b.WriteByte(' ')
			i += end
		case c == '/' && strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				end = len(src) - i - 2
			} else {
				end += 2
			}
			comments = append(comments, src[i:i+2+end])
			b.WriteByte(' ')
			i += 2 + end
		case c == '\'':
			// Backslash escapes exist only in E'...' strings; in a plain
			// string a backslash is an ordinary character ('\' is complete).
			escapes := i > 0 && (src[i-1] == 'E' || src[i-1] == 'e') && (i < 2 || !isWordByte(src[i-2]))
			b.WriteString("''")
			i++
			for i < len(src) {
				if escapes && src[i] == '\\' && i+1 < len(src) {
					i += 2
					continue
				}
				if src[i] == '\'' {
					if i+1 < len(src) && src[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case c == '$' && dollar.MatchString(src[i:]):
			tag := dollar.FindString(src[i:])
			end := strings.Index(src[i+len(tag):], tag)
			if end < 0 {
				end = len(src) - i - len(tag)
			} else {
				end += len(tag)
			}
			b.WriteString("''")
			i += len(tag) + end
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), comments
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// scopeTagsOf returns the scope tags in the comments of a query.
func scopeTagsOf(comments []string) []string {
	var out []string
	for _, c := range comments {
		if m := scopeCommentRe.FindStringSubmatch(c); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// tenantTables reads the tables with an org_id column from db/schema.sql.
func tenantTables(schema string) []string {
	var out []string
	re := regexp.MustCompile(`(?s)CREATE TABLE (?:public\.)?(\w+) \((.*?)\n\);`)
	for _, m := range re.FindAllStringSubmatch(schema, -1) {
		if regexp.MustCompile(`(?m)^\s+org_id `).MatchString(m[2]) {
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// tenantMatch finds tenant tables in SQL code.
type tenantMatch struct {
	word, kwTable *regexp.Regexp
}

func tenantMatcher(tables []string) tenantMatch {
	names := strings.Join(tables, "|")
	return tenantMatch{
		word:    regexp.MustCompile(`(?i)(?:^|[^\w./-])(?:` + names + `)\b`),
		kwTable: regexp.MustCompile(`(?i)\b(?:from|join|into|update|using|table)\s+(?:only\s+)?(?:` + names + `)\b`),
	}
}

func (m tenantMatch) norm(code string) string {
	return regexp.MustCompile(`(?i)\bpublic\.`).ReplaceAllString(strings.ReplaceAll(code, `"`, ""), " ")
}

// names reports whether SQL code names a tenant table as a word, unqualified
// or after public., quoted or not; a name after another qualifier
// (x.issues, a column) or in a URL path (/issues, /project-labels) does not
// count.
func (m tenantMatch) names(code string) bool { return m.word.MatchString(m.norm(code)) }

// goSQL reports whether a Go literal is SQL on a tenant table: a tenant
// table word and SQL in it (an upper case keyword, or a statement shape in any
// case: select ... from, insert into, update x set, delete from), or a keyword
// right before a tenant table in any case. Prose ("update one (id). teams
// replaces the set") is neither.
func (m tenantMatch) goSQL(code string) bool {
	n := m.norm(code)
	return (m.word.MatchString(n) && (upperKeywordRe.MatchString(n) || sqlShapeRe.MatchString(n))) || m.kwTable.MatchString(n)
}

// parseQueryFile splits a sqlc file into its queries.
func parseQueryFile(where, src string) []sqlUnit {
	var out []sqlUnit
	idx := sqlNameRe.FindAllStringSubmatchIndex(src, -1)
	for i, m := range idx {
		end := len(src)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		out = append(out, sqlUnit{Where: where, Name: src[m[2]:m[3]], Line: 1 + strings.Count(src[:m[0]], "\n"), Text: src[m[0]:end]})
	}
	return out
}

// parseGoSource returns every string literal of a Go file with its enclosing
// declaration, and the file's imports.
func parseGoSource(where string, src []byte) ([]sqlUnit, []string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, where, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, err
	}
	var imports []string
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		imports = append(imports, p)
	}
	callsHelper := func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok && scopeHelpers[id.Name] {
					found = true
				}
			}
			return !found
		})
		return found
	}
	fileCalls := callsHelper(f)
	var out []sqlUnit
	for _, d := range f.Decls {
		name, helper := "", false
		switch d := d.(type) {
		case *ast.FuncDecl:
			name = d.Name.Name
			helper = scopeHelpers[name] || callsHelper(d)
		case *ast.GenDecl:
			var names []string
			for _, s := range d.Specs {
				if v, ok := s.(*ast.ValueSpec); ok {
					for _, n := range v.Names {
						names = append(names, n.Name)
					}
				}
			}
			name = strings.Join(names, ",")
			helper = fileCalls
		}
		emit := func(pos token.Pos, text string) {
			out = append(out, sqlUnit{Where: where, Name: name, Line: fset.Position(pos).Line, Text: text, Go: true, CallsHelper: helper})
		}
		var visit func(n ast.Node) bool
		visit = func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BinaryExpr:
				if n.Op != token.ADD {
					return true
				}
				// A + chain: each run of constant string operands is folded
				// into one unit ("SELECT ... FROM " + "issues"); the other
				// operands are walked as usual.
				var run []string
				var runPos token.Pos
				flush := func() {
					if len(run) > 0 {
						emit(runPos, strings.Join(run, ""))
						run = nil
					}
				}
				for _, op := range addOperands(n) {
					if v, ok := constString(op); ok {
						if len(run) == 0 {
							runPos = op.Pos()
						}
						run = append(run, v)
						continue
					}
					flush()
					ast.Inspect(op, visit)
				}
				flush()
				return false
			case *ast.BasicLit:
				if v, ok := constString(n); ok {
					emit(n.Pos(), v)
				}
			}
			return true
		}
		ast.Inspect(d, visit)
	}
	return out, imports, nil
}

// addOperands flattens a + chain into its operands, left to right.
func addOperands(e ast.Expr) []ast.Expr {
	switch x := e.(type) {
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return append(addOperands(x.X), addOperands(x.Y)...)
		}
	case *ast.ParenExpr:
		return addOperands(x.X)
	}
	return []ast.Expr{e}
}

// constString is the value of a string literal (or a parenthesized one).
func constString(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(x.Value)
		return v, err == nil
	case *ast.ParenExpr:
		return constString(x.X)
	}
	return "", false
}

// readGolden reads "tag where name" lines.
func readGolden(src string) (map[string]bool, []string) {
	out := map[string]bool{}
	var bad []string
	sc := bufio.NewScanner(strings.NewReader(src))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if f := strings.Fields(l); len(f) != 3 || !scopeTags[f[0]] || f[0] == "none" || scopedTag(f[0]) {
			bad = append(bad, fmt.Sprintf("%s: bad line %q: want \"<opt-out or resolved tag> <file> <name>\"", scopeGolden, l))
			continue
		}
		out[strings.Join(strings.Fields(l), " ")] = true
	}
	return out, bad
}

// scopeProblems is Test C. It returns the problems and the golden lines the
// units need.
func scopeProblems(units []sqlUnit, tm tenantMatch, golden map[string]bool) ([]string, map[string]bool) {
	var out []string
	need := map[string]bool{}
	for _, u := range units {
		code, comments := sqlScan(u.Text)
		onTenant := tm.names(code)
		if u.Go && !tm.goSQL(code) {
			continue // not SQL on a tenant table
		}
		at := fmt.Sprintf("%s:%d (%s)", u.Where, u.Line, u.Name)
		tags := scopeTagsOf(comments)
		if len(tags) == 0 {
			hint := `a "-- scope: <tag>" line after the name line`
			if u.Go {
				hint = `a "/* scope: <tag> */" comment in the literal`
			}
			out = append(out, fmt.Sprintf("%s: query has no scope tag: add %s (decisions-T20 #16)", at, hint))
			continue
		}
		if len(tags) > 1 {
			out = append(out, fmt.Sprintf("%s: query has %d scope tags %v: keep one", at, len(tags), tags))
			continue
		}
		tag := tags[0]
		hasArgs := strings.Contains(code, "sqlc.arg(all_teams)") && strings.Contains(code, "sqlc.arg(team_ids)")
		switch {
		case !scopeTags[tag]:
			out = append(out, fmt.Sprintf("%s: unknown scope tag %q", at, tag))
		case tag == "none":
			if onTenant {
				out = append(out, fmt.Sprintf("%s: tagged none but names a tenant table: choose a real tag", at))
			}
		case tag == "feed":
			switch {
			case u.Go:
				out = append(out, fmt.Sprintf("%s: tagged feed in Go: the feed is the sqlc query ListEventsAfter only", at))
			case !hasArgs || !feedShaped(withoutComments(u.Text, comments)):
				out = append(out, fmt.Sprintf("%s: tagged feed but its code lacks the #55 predicate "+
					"(is_admin OR (all_teams AND scope <> 'admin') OR scope = 'org' OR any-overlap OR all-subset)", at))
			}
		case tag == "team" || tag == "project":
			switch {
			case u.Go && !u.CallsHelper:
				out = append(out, fmt.Sprintf("%s: tagged %s, but %s calls no teamScope, projectScope or documentScope", at, tag, u.Name))
			case !u.Go && tag == "team" && !teamShapeRe.MatchString(code):
				out = append(out, fmt.Sprintf("%s: tagged team but its code lacks the #15 predicate "+
					"(sqlc.arg(all_teams)::bool OR x.team_id = ANY(sqlc.arg(team_ids)::uuid[])), in parentheses", at))
			case !u.Go && tag == "project" && !((projectShapeRe.MatchString(code) ||
				(u.Where == "db/queries/products.sql" && productShapeRe.MatchString(code))) && projectAnyRe.MatchString(code)):
				out = append(out, fmt.Sprintf("%s: tagged project but its code lacks the #36 predicate "+
					"(sqlc.arg(all_teams)::bool OR NOT EXISTS (SELECT 1 FROM project_teams ...) OR ... team_id = ANY(sqlc.arg(team_ids)::uuid[]))", at))
			}
		default:
			line := tag + " " + u.Where + " " + u.Name
			need[line] = true
			if !golden[line] {
				out = append(out, fmt.Sprintf("%s: %s is not in %s: add the line %q after review", at, tag, scopeGolden, line))
			}
		}
		if !u.Go && hasArgs && !scopedTag(tag) {
			out = append(out, fmt.Sprintf("%s: has the scope arguments but is tagged %s: tag it team, project or feed", at, tag))
		}
	}
	var stale []string
	for l := range golden {
		if !need[l] {
			stale = append(stale, fmt.Sprintf("%s has %q, which no query needs: remove it", scopeGolden, l))
		}
	}
	sort.Strings(stale)
	return append(out, stale...), need
}

// scopedTag: the tags whose query carries the scope arguments and is checked
// by its content, not listed in the golden file.
func scopedTag(tag string) bool { return tag == "team" || tag == "project" || tag == "feed" }

// withoutComments is the SQL with its comments cut out and its literals kept:
// the feed predicate compares scope with literals ('org', 'any'), which the
// blanked code no longer shows. A comment cannot fake the predicate.
func withoutComments(src string, comments []string) string {
	for _, c := range comments {
		src = strings.Replace(src, c, " ", 1)
	}
	return src
}

// feedShaped: the code has every case of the #55 predicate.
func feedShaped(code string) bool {
	for _, re := range feedShapeRes {
		if !re.MatchString(code) {
			return false
		}
	}
	return true
}

// nameProblems compares the query names Test C parsed with the names sqlc
// generated (internal/dbq), so a query the splitter misses cannot hide.
func nameProblems(parsed []string, generated []string) []string {
	have := map[string]bool{}
	for _, n := range parsed {
		if have[n] {
			return []string{fmt.Sprintf("query %s is parsed twice", n)}
		}
		have[n] = true
	}
	var out []string
	gen := map[string]bool{}
	for _, n := range generated {
		gen[n] = true
		if !have[n] {
			out = append(out, fmt.Sprintf("internal/dbq has query %s, which Test C did not parse from db/queries: fix the splitter", n))
		}
	}
	for n := range have {
		if !gen[n] {
			out = append(out, fmt.Sprintf("Test C parsed query %s, which internal/dbq does not have: run make sqlc", n))
		}
	}
	sort.Strings(out)
	return out
}

// productionGoFiles walks internal/ and cmd/ for non-test Go files, without
// the generated internal/dbq and the test-only internal/testdb.
func productionGoFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if d.IsDir() && (rel == "internal/dbq" || rel == "internal/testdb") {
				return fs.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = b
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// scopeSources is what Test C and the #19 guard read.
type scopeSources struct {
	units   []sqlUnit // sqlc queries first, then Go literals
	gofiles map[string][]byte
	tenant  tenantMatch
}

func loadScopeSources(t *testing.T) scopeSources {
	t.Helper()
	root := filepath.Join("..", "..")
	schema, err := os.ReadFile(filepath.Join(root, "db", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	tables := tenantTables(string(schema))
	if len(tables) < 40 {
		t.Fatalf("found only %d tenant tables in db/schema.sql: the parse is stale", len(tables))
	}
	var src scopeSources
	src.tenant = tenantMatcher(tables)
	files, err := filepath.Glob(filepath.Join(root, "db", "queries", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range parseQueryFile("db/queries/"+filepath.Base(f), string(b)) {
			parsed = append(parsed, u.Name)
			src.units = append(src.units, u)
		}
	}
	gen, err := filepath.Glob(filepath.Join(root, "internal", "dbq", "*.sql.go"))
	if err != nil {
		t.Fatal(err)
	}
	var generated []string
	for _, f := range gen {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range dbqNameRe.FindAllStringSubmatch(string(b), -1) {
			generated = append(generated, m[1])
		}
	}
	if len(generated) < 500 {
		t.Fatalf("found only %d generated queries in internal/dbq: the pattern is stale", len(generated))
	}
	for _, p := range nameProblems(parsed, generated) {
		t.Error(p)
	}
	src.gofiles = productionGoFiles(t, root)
	var goSQL int
	for where, b := range src.gofiles {
		u, imports, err := parseGoSource(where, b)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range u {
			if code, _ := sqlScan(x.Text); src.tenant.goSQL(code) {
				goSQL++
			}
		}
		src.units = append(src.units, u...)
		// testdb is skipped above: its direct SQL is test data. It must stay
		// test-only.
		for _, im := range imports {
			if im == "devops/internal/testdb" {
				t.Errorf("%s imports internal/testdb: only tests may, or Test C must scan it", where)
			}
		}
	}
	if goSQL < 15 {
		t.Fatalf("found only %d Go SQL literals on tenant tables: the scan is stale", goSQL)
	}
	return src
}

// TestQueriesDeclareScope is Test C of decisions-T20 #18.
func TestQueriesDeclareScope(t *testing.T) {
	src := loadScopeSources(t)
	gb, err := os.ReadFile(scopeGolden)
	if err != nil {
		t.Fatal(err)
	}
	golden, bad := readGolden(string(gb))
	for _, b := range bad {
		t.Error(b)
	}
	problems, _ := scopeProblems(src.units, src.tenant, golden)
	for _, p := range problems {
		t.Error(p)
	}
	for _, p := range scopedCallSiteProblems(src.units, src.gofiles) {
		t.Error(p)
	}
	for _, p := range systemCallerProblems(src.units, src.gofiles) {
		t.Error(p)
	}
}

// unitTag is the one scope tag of a unit, or "".
func unitTag(u sqlUnit) string {
	_, comments := sqlScan(u.Text)
	if tags := scopeTagsOf(comments); len(tags) == 1 {
		return tags[0]
	}
	return ""
}

// scopedCallSiteProblems: a sqlc params struct can leave AllTeams out and
// still compile, and an empty AllTeams is false (the query then finds
// nothing). So every production literal of a team or project query's params
// names AllTeams AND TeamIds; AllTeams is never the constant true, and when it
// is a variable, the file sets that variable from .Access.Args(). A request
// passes org.Access.Args(); code with no caller uses the System copy of the
// query. (It replaces the 2a source scan of internal/tracker.)
func scopedCallSiteProblems(units []sqlUnit, gofiles map[string][]byte) []string {
	var names []string
	for _, u := range units {
		if t := unitTag(u); !u.Go && scopedTag(t) {
			names = append(names, u.Name)
		}
	}
	if len(names) == 0 {
		return []string{"no team or project query: the tags are gone"}
	}
	re := regexp.MustCompile(`dbq\.(` + strings.Join(names, "|") + `)Params\{`)
	valRe := regexp.MustCompile(`AllTeams:\s*([^,}\s]+)`)
	identRe := regexp.MustCompile(`^[A-Za-z_]\w*$`)
	var out []string
	var seen int
	for where, b := range gofiles {
		src := string(b)
		for _, m := range re.FindAllStringSubmatchIndex(src, -1) {
			seen++
			depth, end := 1, m[1]
			for ; end < len(src) && depth > 0; end++ {
				switch src[end] {
				case '{':
					depth++
				case '}':
					depth--
				}
			}
			lit := src[m[1]:end]
			at := fmt.Sprintf("%s:%d", where, 1+strings.Count(src[:m[0]], "\n"))
			name := src[m[2]:m[3]]
			v := valRe.FindStringSubmatch(lit)
			switch {
			case v == nil:
				out = append(out, fmt.Sprintf("%s: dbq.%sParams has no AllTeams: pass org.Access.Args(), or use the System copy", at, name))
			case !strings.Contains(lit, "TeamIds"):
				out = append(out, fmt.Sprintf("%s: dbq.%sParams sets AllTeams but not TeamIds", at, name))
			case v[1] == "true":
				out = append(out, fmt.Sprintf("%s: dbq.%sParams hard-codes AllTeams: true: use the System copy of the query", at, name))
			case !identRe.MatchString(v[1]):
				out = append(out, fmt.Sprintf("%s: dbq.%sParams sets AllTeams to %s: use a variable from .Access.Args()", at, name, v[1]))
			case !regexp.MustCompile(`\b` + v[1] + `\s*,\s*\w+\s*:?=\s*[\w.]*\.Access\.Args\(\)`).MatchString(src):
				out = append(out, fmt.Sprintf("%s: dbq.%sParams sets AllTeams: %s, which this file never sets from .Access.Args()", at, name, v[1]))
			}
		}
	}
	if seen == 0 {
		out = append(out, "the scan found no scoped query call: the pattern is stale")
	}
	sort.Strings(out)
	return out
}

// systemCallerProblems: the unscoped System copy of a scoped query is for
// code with no caller (#14). A handler in internal/api must never call one.
// (The service layer is reviewed through the golden file: each System query
// is listed there as system.)
func systemCallerProblems(units []sqlUnit, gofiles map[string][]byte) []string {
	var names []string
	for _, u := range units {
		if !u.Go && strings.HasSuffix(u.Name, "System") {
			names = append(names, u.Name)
		}
	}
	if len(names) == 0 {
		return []string{"no System query: the pattern is stale"}
	}
	re := regexp.MustCompile(`\.(` + strings.Join(names, "|") + `)\(`)
	var out []string
	for where, b := range gofiles {
		if !strings.HasPrefix(where, "internal/api/") {
			continue
		}
		for _, m := range re.FindAllStringSubmatchIndex(string(b), -1) {
			out = append(out, fmt.Sprintf("%s:%d: a handler calls the unscoped %s: use the scoped query with org.Access",
				where, 1+strings.Count(string(b[:m[0]]), "\n"), string(b[m[2]:m[3]])))
		}
	}
	sort.Strings(out)
	return out
}

// TestQueriesDeclareScope_Fails shows Test C fails on each trick: an untagged
// query (sqlc and Go, and Go with a comma join or a quoted public table), a
// tag or the arguments only in a comment, a '--' in a string literal, a
// predicate of the wrong shape, a new opt-out not in the golden file, a stale
// golden line, a splitter miss, and bad call sites.
func TestQueriesDeclareScope_Fails(t *testing.T) {
	tm := tenantMatcher(tenantTables("CREATE TABLE public.issues (\n    org_id uuid NOT NULL,\n    id uuid\n);\n" +
		"CREATE TABLE public.comments (\n    org_id uuid NOT NULL\n);\nCREATE TABLE public.users (\n    id uuid\n);"))
	units := parseQueryFile("db/queries/x.sql", `-- name: Untagged :one
SELECT * FROM issues WHERE org_id = $1;

-- name: NewOptOut :many
-- scope: system
SELECT * FROM issues;

-- name: Listed :many
-- scope: admin
SELECT * FROM issues WHERE org_id = $1;

-- name: TeamNoArgs :many
-- scope: team
SELECT * FROM issues WHERE org_id = $1;

-- name: TeamArgsInComment :many
-- scope: team
-- (sqlc.arg(all_teams)::bool OR team_id = ANY(sqlc.arg(team_ids)::uuid[]))
SELECT * FROM issues WHERE org_id = $1;

-- name: TeamNoParens :many
-- scope: team
SELECT * FROM issues WHERE org_id = $1 AND sqlc.arg(all_teams)::bool OR team_id = ANY(sqlc.arg(team_ids)::uuid[]);

-- name: TeamOK :many
-- scope: team
SELECT * FROM issues WHERE org_id = $1 AND (sqlc.arg(all_teams)::bool OR team_id = ANY(sqlc.arg(team_ids)::uuid[]));

-- name: FeedNoAll :many
-- scope: feed
SELECT * FROM issues WHERE org_id = $1 AND (sqlc.arg(is_admin)::bool OR (sqlc.arg(all_teams)::bool AND scope <> 'admin')
  OR scope = 'org' OR (scope = 'any' AND team_ids && sqlc.arg(team_ids)::uuid[])
  OR (scope = 'all' AND team_ids <@ sqlc.arg(team_ids)::uuid[]));

-- name: FeedInComment :many
-- scope: feed
-- (sqlc.arg(is_admin)::bool OR (sqlc.arg(all_teams)::bool AND scope <> 'admin') OR scope = 'org'
-- OR (scope = 'any' AND team_ids && sqlc.arg(team_ids)::uuid[])
-- OR (scope = 'all' AND cardinality(team_ids) > 0 AND team_ids <@ sqlc.arg(team_ids)::uuid[]))
SELECT * FROM issues WHERE org_id = $1 AND sqlc.arg(all_teams)::bool AND sqlc.arg(team_ids)::uuid[] IS NOT NULL;

-- name: FeedOK :many
-- scope: feed
SELECT * FROM issues e WHERE org_id = $1 AND (sqlc.arg(is_admin)::bool OR (sqlc.arg(all_teams)::bool AND e.scope <> 'admin')
  OR e.scope = 'org' OR (e.scope = 'any' AND e.team_ids && sqlc.arg(team_ids)::uuid[])
  OR (e.scope = 'all' AND cardinality(e.team_ids) > 0 AND e.team_ids <@ sqlc.arg(team_ids)::uuid[]));

-- name: ProjectWrongShape :one
-- scope: project
SELECT 1 FROM issues WHERE (sqlc.arg(all_teams)::bool OR team_id = ANY(sqlc.arg(team_ids)::uuid[]));

-- name: ProductShapeOutside :one
-- scope: project
SELECT 1 FROM projects p WHERE (sqlc.arg(all_teams)::bool
  OR NOT EXISTS (SELECT 1 FROM product_teams x WHERE x.product_id = p.id)
  OR EXISTS (SELECT 1 FROM product_teams x WHERE x.product_id = p.id AND x.team_id = ANY(sqlc.arg(team_ids)::uuid[])));

-- name: NoneOnTenant :one
-- scope: none
SELECT 1 FROM users u, issues i WHERE u.id = i.id;

-- name: NoneLateral :one
-- scope: none
SELECT 1 FROM users u, LATERAL (SELECT 1 FROM "public"."comments" c) x;

-- name: NoneHiddenByLiteral :one
-- scope: none
SELECT '--', 1 FROM issues;

-- name: TagInLiteral :one
SELECT '-- scope: system' FROM issues;

-- name: NoneBackslash :one
-- scope: none
SELECT '\', 1 FROM issues;

-- name: NoneEscapedOK :one
-- scope: none
SELECT E'it\'s', 1 FROM users WHERE name = 'issues';

-- name: NoneOK :one
-- scope: none
SELECT 1 FROM users WHERE name = 'issues' AND x.issues IS NULL;
`)
	gosrc := []byte(`package x

const tmpl = "/* scope: team */ SELECT id FROM issues"

func List() {
	_ = "SELECT id FROM issues WHERE org_id = $1"
	_ = "remove it from the list"
	_ = "unknown issues: try again"
	_ = "update one (id). teams replaces the set"
	_ = "/* scope: resolved */ select id from issues where id = $1"
	_ = "SELECT 1 FROM users u, \"public\".\"issues\" i"
	_ = "DELETE /comments/{comment}"
	_ = "select c.id, c.body " + "from comments c where c.org_id = $1"
}
`)
	gu, _, err := parseGoSource("internal/x/x.go", gosrc)
	if err != nil {
		t.Fatal(err)
	}
	units = append(units, gu...)
	golden := map[string]bool{"admin db/queries/x.sql Listed": true, "org db/queries/x.sql Gone": true}
	problems, _ := scopeProblems(units, tm, golden)
	got := strings.Join(problems, "\n")
	for _, want := range []string{
		"db/queries/x.sql:1 (Untagged): query has no scope tag",
		`(NewOptOut): system is not in testdata/scope_optouts.golden: add the line "system db/queries/x.sql NewOptOut"`,
		"(TeamNoArgs): tagged team but its code lacks the #15 predicate",
		"(TeamArgsInComment): tagged team but its code lacks the #15 predicate",
		"(TeamNoParens): tagged team but its code lacks the #15 predicate",
		"(ProjectWrongShape): tagged project but its code lacks the #36 predicate",
		"(ProductShapeOutside): tagged project but its code lacks the #36 predicate",
		"(FeedNoAll): tagged feed but its code lacks the #55 predicate",
		"(FeedInComment): tagged feed but its code lacks the #55 predicate",
		"(NoneOnTenant): tagged none but names a tenant table",
		"(NoneLateral): tagged none but names a tenant table",
		"(NoneHiddenByLiteral): tagged none but names a tenant table",
		"(NoneBackslash): tagged none but names a tenant table",
		"internal/x/x.go:13 (List): query has no scope tag",
		"(TagInLiteral): query has no scope tag",
		"internal/x/x.go:6 (List): query has no scope tag",
		"internal/x/x.go:11 (List): query has no scope tag",
		"(tmpl): tagged team, but tmpl calls no teamScope",
		`resolved internal/x/x.go List`,
		`has "org db/queries/x.sql Gone", which no query needs`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("problems do not say %q; got:\n%s", want, got)
		}
	}
	for _, clean := range []string{"(Listed)", "(TeamOK)", "(FeedOK)", "(NoneOK)", "x.go:7", "x.go:8", "x.go:9", "x.go:12", "(NoneEscapedOK)"} {
		if strings.Contains(got, clean) {
			t.Errorf("a clean unit was reported (%s):\n%s", clean, got)
		}
	}
	if len(problems) != 20 {
		t.Errorf("got %d problems, want 20:\n%s", len(problems), got)
	}
	if _, bad := readGolden("# c\nteam db/queries/x.sql A\nsystem db/queries/x.sql B\n"); len(bad) != 1 {
		t.Errorf("a team line in the golden file must be refused: %v", bad)
	}

	// A splitter miss: sqlc generated a query Test C did not parse.
	if p := nameProblems([]string{"A"}, []string{"A", "B"}); len(p) != 1 || !strings.Contains(p[0], "query B") {
		t.Errorf("name problems = %v", p)
	}
	if got := parseQueryFile("q.sql", "  --  name:  Spaced :one\nSELECT 1;\n"); len(got) != 1 || got[0].Name != "Spaced" {
		t.Errorf("a spaced name line is not parsed: %v", got)
	}

	// Call sites: a constant true, a variable not from Access.Args(), none.
	scoped := []sqlUnit{{Name: "GetThing", Text: "-- name: GetThing :one\n-- scope: team\nSELECT 1"}}
	files := map[string][]byte{"internal/x/a.go": []byte(`package x
func a() {
	_ = dbq.GetThingParams{OrgID: o, AllTeams: true, TeamIds: nil}
	yes := true
	_ = dbq.GetThingParams{OrgID: o, AllTeams: yes, TeamIds: nil}
	_ = dbq.GetThingParams{OrgID: o}
	all, ids := org.Access.Args()
	_ = dbq.GetThingParams{OrgID: o, AllTeams: all, TeamIds: ids}
}`)}
	cs := strings.Join(scopedCallSiteProblems(scoped, files), "\n")
	for _, want := range []string{"a.go:3: dbq.GetThingParams hard-codes AllTeams: true", "a.go:5: dbq.GetThingParams sets AllTeams: yes",
		"a.go:6: dbq.GetThingParams has no AllTeams"} {
		if !strings.Contains(cs, want) {
			t.Errorf("call-site problems do not say %q; got:\n%s", want, cs)
		}
	}
	if strings.Contains(cs, "a.go:8") {
		t.Errorf("a good call site was reported:\n%s", cs)
	}
	sys := systemCallerProblems([]sqlUnit{{Name: "GetThingSystem"}}, map[string][]byte{
		"internal/api/h.go": []byte("package api\nfunc h() { q.GetThingSystem(ctx) }"), "internal/tracker/s.go": []byte("q.GetThingSystem(ctx)")})
	if len(sys) != 1 || !strings.Contains(sys[0], "internal/api/h.go:2") {
		t.Errorf("system caller problems = %v", sys)
	}
}
