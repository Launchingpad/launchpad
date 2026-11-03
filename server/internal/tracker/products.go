package tracker

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/text/unicode/norm"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
)

// Products (LAUN-62; docs/internal/decisions-T18-products.md): a named group
// of teams. A product owns nothing else. Archive and restore are PATCH
// {archived}; delete works only on an archived product (#9).

const (
	// maxProductsPerOrg counts archived products too (#9).
	maxProductsPerOrg = 200
	maxProductTeams   = 200
)

var (
	// productSlugRe: starts with a letter, no double and no trailing hyphen.
	productSlugRe = regexp.MustCompile(`^[a-z]([a-z0-9]|-[a-z0-9])*$`)
	// canonicalUUIDRe is the ONLY id form of a {product} path (#12), either
	// case. It is not uuid.Parse, which also takes 32 hex digits, braces and
	// urn:uuid: (those are slugs here).
	canonicalUUIDRe = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// Product is a product as listed.
type Product struct {
	ID          uuid.UUID  `json:"id"`
	Slug        string     `json:"slug"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Icon        *string    `json:"icon"`
	Color       *string    `json:"color"`
	Version     int32      `json:"version"`
	TeamKeys    []string   `json:"teamKeys"` // non-archived teams, by key
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	ArchivedAt  *time.Time `json:"archivedAt"`
}

// ProductDetail is GET /products/{product}: the product with its
// non-archived teams.
type ProductDetail struct {
	Product
	Teams []Team `json:"teams"`
}

// ParseProductRef tells an id from a slug (#12): only the canonical
// 36-character form is an id.
func parseProductRef(ref string) (id uuid.UUID, isID bool) {
	if !canonicalUUIDRe.MatchString(ref) {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(ref)
	return id, err == nil
}

// resolveProduct finds a product of the org by slug or canonical id. No
// Linear id resolves (#9). pgx.ErrNoRows when there is none, or when the
// caller sees none of its teams (the project rule, T20 #36, #69).
func resolveProduct(ctx context.Context, q *dbq.Queries, org *auth.Org, ref string) (dbq.Product, error) {
	all, ids := org.Access.Args()
	ref = strings.TrimSpace(ref)
	if id, ok := parseProductRef(ref); ok {
		return q.GetProductByID(ctx, dbq.GetProductByIDParams{OrgID: org.ID, ID: id, AllTeams: all, TeamIds: ids})
	}
	return q.GetProductBySlug(ctx, dbq.GetProductBySlugParams{OrgID: org.ID, Slug: strings.ToLower(ref), AllTeams: all, TeamIds: ids})
}

// slugFromName derives a slug (#12): NFKD, marks dropped, lower case, every
// run of characters outside [a-z0-9] one hyphen, trimmed, cut to 40.
func slugFromName(name string) string {
	var b strings.Builder
	hyphen := false
	for _, r := range norm.NFKD.String(name) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		r = unicode.ToLower(r)
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if hyphen && b.Len() > 0 {
				b.WriteByte('-')
			}
			hyphen = false
			b.WriteRune(r)
		} else {
			hyphen = true
		}
	}
	s := b.String()
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

func validateProductSlug(s string) error {
	if !productSlugRe.MatchString(s) || len(s) > 40 {
		return httpx.Invalid("slug must be 1-40 characters: lower case letters, digits and single hyphens, starting with a letter")
	}
	if canonicalUUIDRe.MatchString(s) {
		return httpx.Invalid("a slug cannot be in the form of a UUID")
	}
	return nil
}

func validateProductName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if l := utf8.RuneCountInString(n); l < 1 || l > 200 {
		return "", httpx.Invalid("name must be 1-200 characters")
	}
	return n, nil
}

// normIcon and normColor: "" and null both mean none.
func normIcon(v string) (*string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(v) > 8 {
		return nil, httpx.Invalid("icon must be at most 8 characters")
	}
	return &v, nil
}

func normColor(v string) (*string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if !colorRe.MatchString(v) {
		return nil, httpx.Invalid("color must be #RRGGBB")
	}
	return &v, nil
}

func eqStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func toProduct(p dbq.Product, keys []string) Product {
	if keys == nil {
		keys = []string{}
	}
	return Product{ID: p.ID, Slug: p.Slug, Name: p.Name, Description: p.Description, Icon: p.Icon, Color: p.Color,
		Version: p.Version, TeamKeys: keys, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, ArchivedAt: p.ArchivedAt}
}

// productTeamKeys fills the team keys of a batch with ONE query: the teams
// org sees only (T20 #69).
func productTeamKeys(ctx context.Context, q *dbq.Queries, org *auth.Org, ids []uuid.UUID) (map[uuid.UUID][]string, error) {
	out := map[uuid.UUID][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	all, tids := org.Access.Args()
	rows, err := q.ListTeamsForProducts(ctx, dbq.ListTeamsForProductsParams{OrgID: org.ID, ProductIds: ids, AllTeams: all, TeamIds: tids})
	if err != nil {
		return nil, fmt.Errorf("product teams: %w", err)
	}
	for _, r := range rows {
		out[r.ProductID] = append(out[r.ProductID], r.Key)
	}
	return out, nil
}

// attachProductIDs fills Team.ProductIDs of a batch with ONE query over
// product_teams (T18 #16). Only non-archived products are listed.
func attachProductIDs(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, teams []Team) error {
	if len(teams) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(teams))
	for i, t := range teams {
		ids[i] = t.ID
	}
	rows, err := q.ListProductIDsForTeams(ctx, dbq.ListProductIDsForTeamsParams{OrgID: orgID, TeamIds: ids})
	if err != nil {
		return fmt.Errorf("team products: %w", err)
	}
	by := map[uuid.UUID][]uuid.UUID{}
	for _, r := range rows {
		by[r.TeamID] = append(by[r.TeamID], r.ProductID)
	}
	for i := range teams {
		if v := by[teams[i].ID]; v != nil {
			teams[i].ProductIDs = v
		}
	}
	return nil
}

func (s *Service) productView(ctx context.Context, q *dbq.Queries, org *auth.Org, p dbq.Product) (Product, error) {
	keys, err := productTeamKeys(ctx, q, org, []uuid.UUID{p.ID})
	if err != nil {
		return Product{}, err
	}
	return toProduct(p, keys[p.ID]), nil
}

// ParseProductListQuery reads GET /products: includeArchived only; any other
// parameter is a 400 (the list has no paging).
func ParseProductListQuery(v url.Values) (includeArchived bool, err error) {
	for k := range v {
		if k != "includeArchived" {
			return false, httpx.Invalid("unknown query parameter %q", k)
		}
	}
	if includeArchived, err = parseBool(v.Get("includeArchived")); err != nil {
		return false, httpx.Invalid("includeArchived must be true or false")
	}
	return includeArchived, nil
}

// ListProducts lists the org's products by name. No paging: capped at 200.
func (s *Service) ListProducts(ctx context.Context, org *auth.Org, includeArchived bool) (httpx.Page[Product], error) {
	all, ids := org.Access.Args()
	rows, err := s.q.ListProducts(ctx, dbq.ListProductsParams{OrgID: org.ID, IncludeArchived: includeArchived, AllTeams: all, TeamIds: ids})
	if err != nil {
		return httpx.Page[Product]{}, fmt.Errorf("list products: %w", err)
	}
	return s.productPage(ctx, org, rows)
}

func (s *Service) productPage(ctx context.Context, org *auth.Org, rows []dbq.Product) (httpx.Page[Product], error) {
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	keys, err := productTeamKeys(ctx, s.q, org, ids)
	if err != nil {
		return httpx.Page[Product]{}, err
	}
	items := make([]Product, 0, len(rows))
	for _, r := range rows {
		items = append(items, toProduct(r, keys[r.ID]))
	}
	return httpx.Page[Product]{Items: items}, nil
}

// GetProduct reads a product by slug or id, archived or not.
func (s *Service) GetProduct(ctx context.Context, org *auth.Org, ref string) (ProductDetail, error) {
	p, err := resolveProduct(ctx, s.q, org, ref)
	if err != nil {
		return ProductDetail{}, notFound(err, "product")
	}
	all, ids := org.Access.Args()
	rows, err := s.q.ListProductTeams(ctx, dbq.ListProductTeamsParams{OrgID: org.ID, ProductID: p.ID, AllTeams: all, TeamIds: ids})
	if err != nil {
		return ProductDetail{}, fmt.Errorf("product teams: %w", err)
	}
	teams := make([]Team, 0, len(rows))
	keys := make([]string, 0, len(rows))
	for _, t := range rows {
		teams = append(teams, toTeam(t))
		keys = append(keys, t.Key)
	}
	if err := attachProductIDs(ctx, s.q, org.ID, teams); err != nil {
		return ProductDetail{}, err
	}
	return ProductDetail{Product: toProduct(p, keys), Teams: teams}, nil
}

// ListTeamProducts lists the non-archived products of a team.
func (s *Service) ListTeamProducts(ctx context.Context, org *auth.Org, teamRef string) (httpx.Page[Product], error) {
	t, err := resolveTeam(ctx, s.q, org, teamRef)
	if err != nil {
		return httpx.Page[Product]{}, notFound(err, "team")
	}
	rows, err := s.q.ListProductsForTeam(ctx, dbq.ListProductsForTeamParams{OrgID: org.ID, TeamID: t.ID})
	if err != nil {
		return httpx.Page[Product]{}, fmt.Errorf("list team products: %w", err)
	}
	return s.productPage(ctx, org, rows)
}

// resolveProductTeams turns keys or ids into team ids. Unknown (or another
// org's) is 400; an archived team is 400 "team is archived" (#9), checked
// here because the resolver does not filter archived teams.
func resolveProductTeams(ctx context.Context, q *dbq.Queries, org *auth.Org, refs []string) ([]uuid.UUID, error) {
	if len(refs) > maxProductTeams {
		return nil, httpx.Invalid("at most %d teams per product", maxProductTeams)
	}
	out := make([]uuid.UUID, 0, len(refs))
	for _, r := range refs {
		t, err := resolveTeam(ctx, q, org, strings.TrimSpace(r))
		if err != nil {
			return nil, invalidRef(err, "unknown team %q", r)
		}
		if t.ArchivedAt != nil {
			return nil, httpx.Invalid("team %q is archived", r)
		}
		if !slices.Contains(out, t.ID) {
			out = append(out, t.ID)
		}
	}
	return out, nil
}

// CreateProductInput is the POST /products body.
type CreateProductInput struct {
	Name        string   `json:"name"`
	Slug        string   `json:"slug"`
	Description string   `json:"description"`
	Icon        string   `json:"icon"`
	Color       string   `json:"color"`
	Teams       []string `json:"teams"` // keys or ids
}

// CreateProduct makes a product in one transaction and emits ONE event,
// product.created, with the team ids.
func (s *Service) CreateProduct(ctx context.Context, org *auth.Org, p *auth.Principal, in CreateProductInput) (ProductDetail, error) {
	name, err := validateProductName(in.Name)
	if err != nil {
		return ProductDetail{}, err
	}
	if err := validateLongText("description", in.Description); err != nil {
		return ProductDetail{}, err
	}
	icon, err := normIcon(in.Icon)
	if err != nil {
		return ProductDetail{}, err
	}
	color, err := normColor(in.Color)
	if err != nil {
		return ProductDetail{}, err
	}
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		slug = slugFromName(name)
		if slug == "" || slug[0] < 'a' || slug[0] > 'z' {
			return ProductDetail{}, httpx.Invalid("cannot make a slug from this name; send a slug")
		}
	}
	if err := validateProductSlug(slug); err != nil {
		return ProductDetail{}, err
	}
	var id uuid.UUID
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		// One create at a time per org: two creates cannot both pass at 199.
		if err := q.LockProductsOrg(ctx, org.ID.String()); err != nil {
			return fmt.Errorf("lock products: %w", err)
		}
		n, err := q.CountProducts(ctx, org.ID)
		if err != nil {
			return fmt.Errorf("count products: %w", err)
		}
		if n >= maxProductsPerOrg {
			return conflict("an org can have at most %d products, archived ones included", maxProductsPerOrg)
		}
		teams, err := resolveProductTeams(ctx, q, org, in.Teams)
		if err != nil {
			return err
		}
		row, err := q.InsertProduct(ctx, dbq.InsertProductParams{OrgID: org.ID, Slug: slug, Name: name,
			Description: in.Description, Icon: icon, Color: color})
		if err != nil {
			if database.IsPgCode(err, database.CodeUniqueViolation) {
				return conflict("product slug %s is already used in this org", slug)
			}
			return fmt.Errorf("insert product: %w", err)
		}
		id = row.ID
		for _, t := range teams {
			if _, err := q.InsertProductTeam(ctx, dbq.InsertProductTeamParams{OrgID: org.ID, ProductID: id, TeamID: t}); err != nil {
				return fmt.Errorf("insert product team: %w", err)
			}
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.ProductCreated, EntityType: "product", EntityID: id,
			ActorID: &p.UserID, Payload: map[string]any{"id": id, "slug": slug, "teamIds": nonNilIDs(teams)}},
			productEventScope(teams, true))
		return err
	})
	if err != nil {
		return ProductDetail{}, err
	}
	return s.GetProduct(ctx, org, id.String())
}

func nonNilIDs(ids []uuid.UUID) []uuid.UUID {
	if ids == nil {
		return []uuid.UUID{}
	}
	return ids
}

// UpdateProductInput is the PATCH /products/{product} body. Absent leaves a
// field alone; null clears icon and color. Archive and restore are
// archived: true / false.
type UpdateProductInput struct {
	Version     *int32            `json:"version"`
	Name        httpx.Opt[string] `json:"name"`
	Slug        httpx.Opt[string] `json:"slug"`
	Description httpx.Opt[string] `json:"description"`
	Icon        httpx.Opt[string] `json:"icon"`
	Color       httpx.Opt[string] `json:"color"`
	Archived    httpx.Opt[bool]   `json:"archived"`
}

// UpdateProduct applies a partial update under the row lock. A PATCH that
// changes nothing writes nothing and emits nothing.
func (s *Service) UpdateProduct(ctx context.Context, org *auth.Org, p *auth.Principal, ref string, in UpdateProductInput) (ProductDetail, error) {
	var id uuid.UUID
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		found, err := resolveProduct(ctx, q, org, ref)
		if err != nil {
			return notFound(err, "product")
		}
		id = found.ID
		cur, err := q.LockProduct(ctx, dbq.LockProductParams{OrgID: org.ID, ID: id})
		if err != nil {
			return notFound(err, "product")
		}
		if in.Version != nil && *in.Version != cur.Version {
			return httpx.Errorf(http.StatusConflict, httpx.CodeVersionConflict,
				"product is at version %d, not %d; re-read it and retry", cur.Version, *in.Version)
		}
		if in.Archived.Set && in.Archived.Null {
			return httpx.Invalid("archived cannot be null")
		}
		if cur.ArchivedAt != nil && (in.Name.Set || in.Slug.Set || in.Description.Set || in.Icon.Set || in.Color.Set) {
			return conflict("product is archived; restore it first (archived: false)")
		}
		next := dbq.UpdateProductParams{OrgID: org.ID, ID: id, Slug: cur.Slug, Name: cur.Name,
			Description: cur.Description, Icon: cur.Icon, Color: cur.Color, ArchivedAt: cur.ArchivedAt}
		var changes []string
		if in.Name.Set {
			if in.Name.Null {
				return httpx.Invalid("name cannot be null")
			}
			n, err := validateProductName(in.Name.V)
			if err != nil {
				return err
			}
			if n != cur.Name {
				next.Name, changes = n, append(changes, "name")
			}
		}
		if in.Slug.Set {
			if in.Slug.Null {
				return httpx.Invalid("slug cannot be null")
			}
			sl := strings.TrimSpace(in.Slug.V)
			if err := validateProductSlug(sl); err != nil {
				return err
			}
			if sl != cur.Slug {
				next.Slug, changes = sl, append(changes, "slug")
			}
		}
		if in.Description.Set {
			v := ""
			if !in.Description.Null {
				v = in.Description.V
			}
			if err := validateLongText("description", v); err != nil {
				return err
			}
			if v != cur.Description {
				next.Description, changes = v, append(changes, "description")
			}
		}
		for _, f := range []struct {
			name string
			opt  httpx.Opt[string]
			norm func(string) (*string, error)
			cur  *string
			dst  **string
		}{{"icon", in.Icon, normIcon, cur.Icon, &next.Icon}, {"color", in.Color, normColor, cur.Color, &next.Color}} {
			if !f.opt.Set {
				continue
			}
			var v *string
			if !f.opt.Null {
				if v, err = f.norm(f.opt.V); err != nil {
					return err
				}
			}
			if !eqStrPtr(v, f.cur) {
				*f.dst, changes = v, append(changes, f.name)
			}
		}
		if in.Archived.Set {
			switch {
			case in.Archived.V && cur.ArchivedAt == nil:
				now := s.now()
				next.ArchivedAt, changes = &now, append(changes, "archived")
			case !in.Archived.V && cur.ArchivedAt != nil:
				next.ArchivedAt, changes = nil, append(changes, "archived")
			}
		}
		if len(changes) == 0 {
			return nil
		}
		updated, err := q.UpdateProduct(ctx, next)
		if err != nil {
			if database.IsPgCode(err, database.CodeUniqueViolation) {
				return conflict("product slug %s is already used in this org", next.Slug)
			}
			return fmt.Errorf("update product: %w", err)
		}
		links, err := q.ListProductTeamLinks(ctx, dbq.ListProductTeamLinksParams{OrgID: org.ID, ProductID: id})
		if err != nil {
			return fmt.Errorf("product teams: %w", err)
		}
		teamIDs := make([]uuid.UUID, 0, len(links))
		for _, l := range links {
			teamIDs = append(teamIDs, l.TeamID)
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.ProductUpdated, EntityType: "product", EntityID: id,
			ActorID: &p.UserID, Payload: map[string]any{"id": id, "slug": updated.Slug, "changes": changes, "version": updated.Version}},
			productEventScope(teamIDs, false))
		return err
	})
	if err != nil {
		return ProductDetail{}, err
	}
	return s.GetProduct(ctx, org, id.String())
}

// DeleteProduct removes an ARCHIVED product and its product_teams rows. It
// never touches a team, an issue or a project.
func (s *Service) DeleteProduct(ctx context.Context, org *auth.Org, p *auth.Principal, ref string) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		found, err := resolveProduct(ctx, q, org, ref)
		if err != nil {
			return notFound(err, "product")
		}
		cur, err := q.LockProduct(ctx, dbq.LockProductParams{OrgID: org.ID, ID: found.ID})
		if err != nil {
			return notFound(err, "product")
		}
		if cur.ArchivedAt == nil {
			return conflict("product is not archived; archive it first (PATCH archived: true)")
		}
		links, err := q.ListProductTeamLinks(ctx, dbq.ListProductTeamLinksParams{OrgID: org.ID, ProductID: cur.ID})
		if err != nil {
			return fmt.Errorf("product teams: %w", err)
		}
		teamIDs := make([]uuid.UUID, 0, len(links))
		for _, l := range links {
			teamIDs = append(teamIDs, l.TeamID)
		}
		if err := q.DeleteProduct(ctx, dbq.DeleteProductParams{OrgID: org.ID, ID: cur.ID}); err != nil {
			return fmt.Errorf("delete product: %w", err)
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.ProductDeleted, EntityType: "product", EntityID: cur.ID,
			ActorID: &p.UserID, Payload: map[string]any{"id": cur.ID, "slug": cur.Slug, "teamIds": teamIDs}},
			productEventScope(teamIDs, true))
		return err
	})
}

// lockActiveProduct resolves and locks a product for a membership change;
// an archived one is 409 (restore it first).
func lockActiveProduct(ctx context.Context, q *dbq.Queries, org *auth.Org, ref string) (dbq.Product, error) {
	found, err := resolveProduct(ctx, q, org, ref)
	if err != nil {
		return found, notFound(err, "product")
	}
	cur, err := q.LockProduct(ctx, dbq.LockProductParams{OrgID: org.ID, ID: found.ID})
	if err != nil {
		return cur, notFound(err, "product")
	}
	if cur.ArchivedAt != nil {
		return cur, conflict("product is archived; restore it first")
	}
	return cur, nil
}

func emitTeamsChanged(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, p *auth.Principal, prod dbq.Product, added, removed []uuid.UUID) error {
	links, err := q.ListProductTeamLinks(ctx, dbq.ListProductTeamLinksParams{OrgID: orgID, ProductID: prod.ID})
	if err != nil {
		return fmt.Errorf("product teams: %w", err)
	}
	ids := make([]uuid.UUID, 0, len(links))
	for _, l := range links {
		ids = append(ids, l.TeamID)
	}
	_, err = events.Emit(ctx, q, orgID, events.Event{Type: events.ProductTeamsChanged, EntityType: "product", EntityID: prod.ID,
		ActorID: &p.UserID, Payload: map[string]any{"id": prod.ID, "slug": prod.Slug, "teamIds": ids,
			"added": nonNilIDs(added), "removed": nonNilIDs(removed)}},
		productEventScope(append(slices.Clone(ids), removed...), true)) // every team the payload names
	return err
}

// SetProductTeams replaces the set of NON-archived teams to match teams. The
// rows of archived teams are kept, never dropped silently (#9). A replace
// that changes nothing writes nothing and emits nothing.
func (s *Service) SetProductTeams(ctx context.Context, org *auth.Org, p *auth.Principal, ref string, refs []string) error {
	if refs == nil {
		return httpx.Invalid("teams is required")
	}
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		prod, err := lockActiveProduct(ctx, q, org, ref)
		if err != nil {
			return err
		}
		want, err := resolveProductTeams(ctx, q, org, refs)
		if err != nil {
			return err
		}
		links, err := q.ListProductTeamLinks(ctx, dbq.ListProductTeamLinksParams{OrgID: org.ID, ProductID: prod.ID})
		if err != nil {
			return fmt.Errorf("product teams: %w", err)
		}
		var added, removed []uuid.UUID
		have := map[uuid.UUID]bool{}
		for _, l := range links {
			have[l.TeamID] = true
			if !l.TeamArchived && !slices.Contains(want, l.TeamID) {
				removed = append(removed, l.TeamID)
			}
		}
		for _, w := range want {
			if !have[w] {
				added = append(added, w)
			}
		}
		if len(added) == 0 && len(removed) == 0 {
			return nil
		}
		for _, t := range added {
			if _, err := q.InsertProductTeam(ctx, dbq.InsertProductTeamParams{OrgID: org.ID, ProductID: prod.ID, TeamID: t}); err != nil {
				return fmt.Errorf("add product team: %w", err)
			}
		}
		for _, t := range removed {
			if _, err := q.DeleteProductTeam(ctx, dbq.DeleteProductTeamParams{OrgID: org.ID, ProductID: prod.ID, TeamID: t}); err != nil {
				return fmt.Errorf("remove product team: %w", err)
			}
		}
		return emitTeamsChanged(ctx, q, org.ID, p, prod, added, removed)
	})
}

// AddProductTeam adds one team; a team already in the product writes nothing.
func (s *Service) AddProductTeam(ctx context.Context, org *auth.Org, p *auth.Principal, ref, teamRef string) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		prod, err := lockActiveProduct(ctx, q, org, ref)
		if err != nil {
			return err
		}
		t, err := resolveTeam(ctx, q, org, strings.TrimSpace(teamRef))
		if err != nil {
			return notFound(err, "team")
		}
		if t.ArchivedAt != nil {
			return httpx.Invalid("team %q is archived", teamRef)
		}
		n, err := q.InsertProductTeam(ctx, dbq.InsertProductTeamParams{OrgID: org.ID, ProductID: prod.ID, TeamID: t.ID})
		if err != nil {
			return fmt.Errorf("add product team: %w", err)
		}
		if n == 0 {
			return nil
		}
		return emitTeamsChanged(ctx, q, org.ID, p, prod, []uuid.UUID{t.ID}, nil)
	})
}

// RemoveProductTeam removes one team (an archived member too); a team that is
// not in the product writes nothing.
func (s *Service) RemoveProductTeam(ctx context.Context, org *auth.Org, p *auth.Principal, ref, teamRef string) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		prod, err := lockActiveProduct(ctx, q, org, ref)
		if err != nil {
			return err
		}
		t, err := resolveTeam(ctx, q, org, strings.TrimSpace(teamRef))
		if err != nil {
			return notFound(err, "team")
		}
		n, err := q.DeleteProductTeam(ctx, dbq.DeleteProductTeamParams{OrgID: org.ID, ProductID: prod.ID, TeamID: t.ID})
		if err != nil {
			return fmt.Errorf("remove product team: %w", err)
		}
		if n == 0 {
			return nil
		}
		return emitTeamsChanged(ctx, q, org.ID, p, prod, nil, []uuid.UUID{t.ID})
	})
}
