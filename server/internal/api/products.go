package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"devops/internal/httpx"
	"devops/internal/tracker"
)

// Products (LAUN-62; docs/internal/decisions-T18-products.md). Thin handlers:
// parse, call the service, write. The path {product} is a slug or a
// canonical id (resolved in the service, never uuid.Parse).

func (s *Server) listProducts(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	include, err := tracker.ParseProductListQuery(r.URL.Query())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	page, err := s.svc.ListProducts(r.Context(), o, include)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (s *Server) createProduct(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.CreateProductInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	pr, err := s.svc.CreateProduct(r.Context(), o, p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, pr)
}

func (s *Server) getProduct(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	pr, err := s.svc.GetProduct(r.Context(), o, chi.URLParam(r, "product"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pr)
}

func (s *Server) updateProduct(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.UpdateProductInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	pr, err := s.svc.UpdateProduct(r.Context(), o, p, chi.URLParam(r, "product"), in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pr)
}

func (s *Server) deleteProduct(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	if err := s.svc.DeleteProduct(r.Context(), o, p, chi.URLParam(r, "product")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetProductTeamsInput is the PUT /products/{product}/teams body.
type SetProductTeamsInput struct {
	Teams []string `json:"teams"` // keys or ids
}

func (s *Server) setProductTeams(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in SetProductTeamsInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.SetProductTeams(r.Context(), o, p, chi.URLParam(r, "product"), in.Teams); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) addProductTeam(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	if err := s.svc.AddProductTeam(r.Context(), o, p, chi.URLParam(r, "product"), chi.URLParam(r, "team")); err != nil {
		s.fail(w, r, pathGone(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeProductTeam(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	if err := s.svc.RemoveProductTeam(r.Context(), o, p, chi.URLParam(r, "product"), chi.URLParam(r, "team")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listTeamProducts(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	page, err := s.svc.ListTeamProducts(r.Context(), o, chi.URLParam(r, "team"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}
