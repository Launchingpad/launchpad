package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"runtime"

	"devops/internal/httpx"
	"devops/internal/importjob"
)

// The Linear import in the web app (LAUN-8, decisions-T17 #36 to #45). Every
// route is org admin only (the admin scope, GETs included: the user list
// holds Linear emails). The Linear API key arrives once, in the export
// request (#38): that route is never idempotent, and the key is read from the
// raw body, copied to a byte slice and overwritten.

// importer returns the manager, or writes 404 when the install has none.
func (s *Server) importMgr(w http.ResponseWriter) *importjob.Manager {
	if s.Import == nil {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "the web import is not available")
		return nil
	}
	return s.Import
}

// optionalJSON decodes a body that may be empty (the defaults then stay).
func optionalJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.ContentLength == 0 && r.Header.Get("Content-Type") == "" {
		return nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		return httpx.Errorf(http.StatusRequestEntityTooLarge, httpx.CodePayloadTooLarge, "request body is too large")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := requireJSON(r); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest, "invalid JSON body")
	}
	return nil
}

type startSessionBody struct {
	Adopt string `json:"adopt"`
}

func (s *Server) startImportSession(w http.ResponseWriter, r *http.Request) {
	m := s.importMgr(w)
	if m == nil {
		return
	}
	o, p := ctxOrgPrincipal(r)
	var in startSessionBody
	if err := optionalJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	v, created, err := m.StartSession(r.Context(), o.ID, p.UserID, in.Adopt)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, v)
}

func (s *Server) getCurrentImportSession(w http.ResponseWriter, r *http.Request) {
	m := s.importMgr(w)
	if m == nil {
		return
	}
	o, _ := ctxOrgPrincipal(r)
	v, err := m.Current(r.Context(), o.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

// linearKey reads the key of the export body. The body is read as bytes, the
// one field is checked by shape (printable ASCII, no escape), copied, and
// every buffer that held it is overwritten. The error text is fixed: a JSON
// syntax error would quote a character of the input.
func linearKey(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	bad := httpx.Invalid("linearApiKey is required: a JSON object {\"linearApiKey\": \"...\"}")
	if err := requireJSON(r); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	defer func() { clear(body); runtime.KeepAlive(body) }()
	if err != nil {
		return nil, httpx.Errorf(http.StatusRequestEntityTooLarge, httpx.CodePayloadTooLarge, "request body is too large")
	}
	var fields map[string]json.RawMessage
	defer func() {
		for _, v := range fields {
			clear(v)
		}
	}()
	if json.Unmarshal(body, &fields) != nil || len(fields) != 1 {
		return nil, bad
	}
	raw, ok := fields["linearApiKey"]
	if !ok || len(raw) < 10 || len(raw) > 514 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, bad
	}
	key := raw[1 : len(raw)-1]
	for _, c := range key {
		if c < 0x21 || c > 0x7e || c == '\\' || c == '"' {
			return nil, bad
		}
	}
	return append([]byte(nil), key...), nil
}

func (s *Server) startImportExport(w http.ResponseWriter, r *http.Request) {
	m := s.importMgr(w)
	if m == nil {
		return
	}
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "session")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	key, err := linearKey(w, r)
	defer func() { clear(key); runtime.KeepAlive(key) }()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := m.StartExport(r.Context(), o, id, key)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, v)
}

func (s *Server) listImportUsers(w http.ResponseWriter, r *http.Request) {
	m := s.importMgr(w)
	if m == nil {
		return
	}
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "session")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := m.Users(r.Context(), o.ID, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (s *Server) putImportUserMap(w http.ResponseWriter, r *http.Request) {
	m := s.importMgr(w)
	if m == nil {
		return
	}
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "session")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The importer's own {default, users} shape (a user map template from
	// the CLI, with its "_users" listing, is accepted as it is).
	if err := requireJSON(r); err != nil {
		s.fail(w, r, err)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, httpx.MaxBodyBytes))
	if err != nil {
		s.fail(w, r, httpx.Errorf(http.StatusRequestEntityTooLarge, httpx.CodePayloadTooLarge, "request body is too large"))
		return
	}
	v, err := m.PutUserMap(r.Context(), o.ID, id, body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

type dryRunBody struct {
	AcceptFailedFiles bool `json:"acceptFailedFiles"`
}

func (s *Server) startImportDryRun(w http.ResponseWriter, r *http.Request) {
	m := s.importMgr(w)
	if m == nil {
		return
	}
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "session")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in dryRunBody
	if err := optionalJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := m.StartDryRun(r.Context(), o, id, in.AcceptFailedFiles)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, v)
}

type runBody struct {
	ConfirmOrg string `json:"confirmOrg"`
}

func (s *Server) startImportRun(w http.ResponseWriter, r *http.Request) {
	m := s.importMgr(w)
	if m == nil {
		return
	}
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "session")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in runBody
	if err := requireJSON(r); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := m.StartRun(r.Context(), o, id, in.ConfirmOrg)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, v)
}

func (s *Server) startImportVerify(w http.ResponseWriter, r *http.Request) {
	m := s.importMgr(w)
	if m == nil {
		return
	}
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "session")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := m.StartVerify(r.Context(), o, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, v)
}

func (s *Server) cancelImportJob(w http.ResponseWriter, r *http.Request) {
	m := s.importMgr(w)
	if m == nil {
		return
	}
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "job")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := m.Cancel(r.Context(), o.ID, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, v)
}

func (s *Server) getImportJob(w http.ResponseWriter, r *http.Request) {
	m := s.importMgr(w)
	if m == nil {
		return
	}
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "job")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := m.GetJob(r.Context(), o.ID, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (s *Server) closeImportSession(w http.ResponseWriter, r *http.Request) {
	m := s.importMgr(w)
	if m == nil {
		return
	}
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "session")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := m.CloseSession(r.Context(), o.ID, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
