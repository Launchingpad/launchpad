package importjob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/importer"
	"devops/internal/storage"
)

// adoptName is the one request value that becomes a path (decisions-T17 #44).
var adoptName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// preflight is what a session start needs before it makes anything: the
// store (503 attachments_unavailable) and a PUBLIC_BASE_URL (409
// public_url_required, #42g).
func (m *Manager) preflight(ctx context.Context) error {
	if m.cfg.Store == nil {
		return httpx.Errorf(http.StatusServiceUnavailable, httpx.CodeAttachmentsUnavailable,
			"the web import needs the attachment store (S3_*), which is not configured")
	}
	if m.cfg.PublicBaseURL == "" {
		return httpx.Errorf(http.StatusConflict, httpx.CodePublicURLRequired,
			"the web import needs PUBLIC_BASE_URL: it is written into the imported texts as the download link of every file")
	}
	// A readable store: a Head of a key that is not there answers not found.
	if _, err := m.cfg.Store.Head(ctx, "import-probe/none"); err != nil && !errors.Is(err, storage.ErrNotFound) {
		m.log.WarnContext(ctx, "import: the attachment store did not answer the probe")
		return httpx.Errorf(http.StatusServiceUnavailable, httpx.CodeAttachmentsUnavailable,
			"the attachment store cannot be reached")
	}
	return nil
}

// newSession is what a session row starts with.
type newSession struct {
	dir     string
	adopted bool
	state   string
	hash    *string
	counts  map[string]int
	problem fileProblems
}

// StartSession returns the org's open session, or makes one. With adopt it
// makes the session from a CLI export placed in the work directory (#44).
// created is false when the open session came back.
func (m *Manager) StartSession(ctx context.Context, orgID, userID uuid.UUID, adopt string) (v SessionView, created bool, err error) {
	if err := m.preflight(ctx); err != nil {
		return SessionView{}, false, err
	}
	if s, err := m.q.ImportSessionGetOpen(ctx, orgID); err == nil {
		if adopt != "" {
			return SessionView{}, false, errConflict("an import session is open; close it before adopting an export")
		}
		v, err := m.sessionView(ctx, s)
		return v, false, err
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return SessionView{}, false, fmt.Errorf("open session lookup: %w", err)
	}
	if err := m.ensureWorkDir(); err != nil {
		return SessionView{}, false, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return SessionView{}, false, fmt.Errorf("session id: %w", err)
	}
	ns := newSession{state: StateNew, counts: map[string]int{}, problem: fileProblems{names: []FailedFile{}}}
	if adopt != "" {
		if ns, err = m.adoptExport(ctx, adopt); err != nil {
			return SessionView{}, false, err
		}
	} else {
		ns.dir = sessionDirName(id)
		if err := m.makeSessionDir(ns.dir); err != nil {
			return SessionView{}, false, err
		}
	}
	var row dbq.ImportSession
	err = database.InTx(ctx, m.cfg.Pool, func(tx pgx.Tx) error {
		var err error
		row, err = m.q.WithTx(tx).ImportSessionInsert(ctx, dbq.ImportSessionInsertParams{
			ID: id, OrgID: orgID, CreatedBy: &userID, State: ns.state, ExportDir: ns.dir, ExportHash: ns.hash,
			ExportCounts: mustJSON(ns.counts), FailedFiles: int32(ns.problem.count), //nolint:gosec // small
			FailedFileNames: mustJSON(ns.problem.names),
		})
		return err
	})
	if err != nil {
		if !ns.adopted {
			m.removeDir(ns.dir)
		}
		if isUnique(err) { // another request made the session first
			if s, gerr := m.q.ImportSessionGetOpen(ctx, orgID); gerr == nil && adopt == "" {
				v, verr := m.sessionView(ctx, s)
				return v, false, verr
			}
			return SessionView{}, false, errConflict("an import session is open, or the export directory is in use")
		}
		return SessionView{}, false, fmt.Errorf("create import session: %w", err)
	}
	v, err = m.sessionView(ctx, row)
	return v, true, err
}

// adoptExport validates a CLI export directory under the work directory and
// reads its manifest (decisions-T17 #44). The name is matched, the directory
// is Lstat'd (a symlink or a file is refused) and opened with os.Root, so no
// later step can leave it.
func (m *Manager) adoptExport(ctx context.Context, name string) (newSession, error) {
	bad := func(format string, args ...any) error { return httpx.Invalid(format, args...) }
	if !adoptName.MatchString(name) || name == "." || name == ".." {
		return newSession{}, bad("adopt must be the name of a directory in the import work directory (letters, digits, '.', '_', '-')")
	}
	if strings.HasPrefix(name, sessionPrefix) {
		return newSession{}, bad("adopt cannot name a directory the server made for a session")
	}
	root, err := os.OpenRoot(m.cfg.WorkDir)
	if err != nil {
		return newSession{}, fmt.Errorf("open work dir: %w", err)
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Lstat(name)
	if err != nil || !fi.IsDir() {
		return newSession{}, bad("there is no export directory with that name in the import work directory")
	}
	sub, err := root.OpenRoot(name)
	if err != nil {
		return newSession{}, bad("there is no export directory with that name in the import work directory")
	}
	defer func() { _ = sub.Close() }()
	if used, err := m.q.ImportSessionDirInUse(ctx, name); err != nil {
		return newSession{}, fmt.Errorf("export directory lookup: %w", err)
	} else if used {
		return newSession{}, errConflict("that export directory is in use by an open import session")
	}
	ex, err := importer.ReadExportRoot(sub)
	if err != nil {
		// The importer's messages name files and counts, never a record.
		return newSession{}, bad("the directory is not a valid export: %v", err)
	}
	hash, err := manifestHash(sub)
	if err != nil {
		return newSession{}, err
	}
	pr := checkFiles(ex.Files)
	if pr.noSpace {
		return newSession{}, errNoSpace()
	}
	return newSession{dir: name, adopted: true, state: StateExported, hash: &hash, counts: ex.Manifest.Counts, problem: pr}, nil
}

// manifestHash is the sha256 (hex) of manifest.json, the export's identity.
func manifestHash(root *os.Root) (string, error) {
	b, err := root.ReadFile("manifest.json")
	if err != nil {
		return "", fmt.Errorf("read manifest: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Current is the org's open session.
func (m *Manager) Current(ctx context.Context, orgID uuid.UUID) (SessionView, error) {
	s, err := m.q.ImportSessionGetOpen(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SessionView{}, httpx.NotFound("import session")
		}
		return SessionView{}, fmt.Errorf("open session lookup: %w", err)
	}
	return m.sessionView(ctx, s)
}

// session loads a session of the org.
func (m *Manager) session(ctx context.Context, orgID, id uuid.UUID) (dbq.ImportSession, error) {
	s, err := m.q.ImportSessionGet(ctx, dbq.ImportSessionGetParams{OrgID: orgID, ID: id})
	if err != nil {
		return s, notFound(err, "import session")
	}
	return s, nil
}

func (m *Manager) sessionView(ctx context.Context, s dbq.ImportSession) (SessionView, error) {
	jobs, err := m.q.ImportJobsLastByKind(ctx, dbq.ImportJobsLastByKindParams{OrgID: s.OrgID, SessionID: s.ID})
	if err != nil {
		return SessionView{}, fmt.Errorf("session jobs: %w", err)
	}
	v := SessionView{ID: s.ID, State: s.State, CreatedBy: s.CreatedBy, ExportCounts: decodeCounts(s.ExportCounts),
		HasUserMap: len(s.UserMap) > 0, FailedFiles: int(s.FailedFiles), FailedFileNames: decodeFailed(s.FailedFileNames),
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, ClosedAt: s.ClosedAt}
	for _, j := range jobs {
		jv := jobView(j, s)
		switch j.Kind {
		case KindExport:
			v.Jobs.Export = &jv
		case KindDryRun:
			v.Jobs.DryRun = &jv
		case KindImport:
			v.Jobs.Import = &jv
		case KindVerify:
			v.Jobs.Verify = &jv
		}
	}
	return v, nil
}

func decodeCounts(b []byte) map[string]int {
	out := map[string]int{}
	_ = json.Unmarshal(b, &out)
	return out
}

func decodeFailed(b []byte) []FailedFile {
	out := []FailedFile{}
	_ = json.Unmarshal(b, &out)
	return out
}

func jobView(j dbq.ImportJob, s dbq.ImportSession) JobView {
	v := JobView{ID: j.ID, SessionID: j.SessionID, Kind: j.Kind, State: j.State, Phase: j.Phase, Done: j.Done, Total: j.Total,
		RowsTruncated: j.RowsTruncated, Committed: j.Committed, ErrorCode: j.ErrorCode, ErrorText: j.ErrorText,
		CancelRequested: j.CancelRequested, FailedFiles: int(s.FailedFiles), FailedFileNames: decodeFailed(s.FailedFileNames),
		CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt, Rows: []importer.VerifyRow{}}
	var jc JobCounts
	if len(j.Counts) > 2 && json.Unmarshal(j.Counts, &jc) == nil {
		v.Report = &jc
	}
	_ = json.Unmarshal(j.Rows, &v.Rows)
	return v
}

// GetJob is one job of the org with its progress and result.
func (m *Manager) GetJob(ctx context.Context, orgID, id uuid.UUID) (JobView, error) {
	j, err := m.q.ImportJobGet(ctx, dbq.ImportJobGetParams{OrgID: orgID, ID: id})
	if err != nil {
		return JobView{}, notFound(err, "import job")
	}
	s, err := m.q.ImportSessionGet(ctx, dbq.ImportSessionGetParams{OrgID: orgID, ID: j.SessionID})
	if err != nil {
		return JobView{}, notFound(err, "import session")
	}
	return jobView(j, s), nil
}

// CloseSession closes a session with no active job and sweeps its work
// directory (decisions-T17 #39, #43). The user map goes with it.
func (m *Manager) CloseSession(ctx context.Context, orgID, id uuid.UUID) error {
	dir, err := m.closeSession(ctx, orgID, id)
	if err != nil {
		return err
	}
	m.removeDir(dir)
	return nil
}

// closeSession is the row side of a close; it returns the directory to remove.
func (m *Manager) closeSession(ctx context.Context, orgID, id uuid.UUID) (string, error) {
	var dir string
	err := database.InTx(ctx, m.cfg.Pool, func(tx pgx.Tx) error {
		q := m.q.WithTx(tx)
		s, err := q.ImportSessionLock(ctx, dbq.ImportSessionLockParams{OrgID: orgID, ID: id})
		if err != nil {
			return notFound(err, "import session")
		}
		if s.State == StateClosed {
			return errConflict("the import session is closed already")
		}
		if err := activeJobErr(ctx, q, orgID); err != nil {
			return err
		}
		dir = s.ExportDir
		return q.ImportSessionClose(ctx, dbq.ImportSessionCloseParams{OrgID: orgID, ID: id})
	})
	return dir, err
}

// activeJobErr is job_active when the org has an active job.
func activeJobErr(ctx context.Context, q *dbq.Queries, orgID uuid.UUID) error {
	if _, err := q.ImportJobActive(ctx, orgID); err == nil {
		return errJobActive()
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("active job lookup: %w", err)
	}
	return nil
}

// Users lists the Linear users of the export with a suggested match (email,
// then username) and the org's members (decisions-T17 #41, #45).
func (m *Manager) Users(ctx context.Context, orgID, id uuid.UUID) (UsersView, error) {
	s, err := m.session(ctx, orgID, id)
	if err != nil {
		return UsersView{}, err
	}
	if s.State == StateClosed || s.ExportHash == nil {
		return UsersView{}, errConflict("the session has no export yet")
	}
	users, err := m.exportUsers(s)
	if err != nil {
		return UsersView{}, err
	}
	members, err := m.q.ListOrgMembers(ctx, orgID)
	if err != nil {
		return UsersView{}, fmt.Errorf("list members: %w", err)
	}
	var cur *importer.UserMap
	if len(s.UserMap) > 0 {
		cur, _ = importer.ParseUserMap(s.UserMap)
	}
	v := UsersView{Users: make([]ExportUser, 0, len(users)), Members: []MemberRef{}}
	byEmail, byName := map[string]string{}, map[string]string{}
	for _, mem := range members {
		if mem.DeactivatedAt != nil {
			continue
		}
		v.Members = append(v.Members, MemberRef{Username: mem.Username, Name: mem.Name, IsAgent: mem.IsAgent})
		byName[strings.ToLower(mem.Username)] = mem.Username
		if mem.Email != nil {
			byEmail[strings.ToLower(*mem.Email)] = mem.Username
		}
	}
	for _, u := range users {
		eu := ExportUser{ID: u.ID, Name: u.Name, DisplayName: u.DisplayName, Email: u.Email, Active: u.Active, App: u.App}
		if name, ok := byEmail[strings.ToLower(u.Email)]; ok && u.Email != "" {
			eu.Suggested = &name
		} else {
			local, _, _ := strings.Cut(strings.ToLower(u.Email), "@")
			for _, c := range []string{strings.ToLower(u.DisplayName), strings.ToLower(u.Name), local} {
				if name, ok := byName[c]; ok && c != "" {
					eu.Suggested = &name
					break
				}
			}
		}
		if cur != nil {
			if name, ok := cur.Resolve(u); ok {
				eu.Mapped = &name
			}
		}
		v.Users = append(v.Users, eu)
	}
	if cur != nil {
		v.Map = &UserMapView{Default: cur.Default, Users: cur.Users}
	}
	return v, nil
}

// exportUsers reads the users of a session's export.
func (m *Manager) exportUsers(s dbq.ImportSession) ([]importer.User, error) {
	root, err := m.openExport(s.ExportDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	users, err := importer.ReadJSONL[importer.User](root, importer.FileUsers)
	if err != nil {
		return nil, fmt.Errorf("read export users: %w", err)
	}
	return users, nil
}

// userMapAllowed is the state rule of PUT .../usermap (#43).
func userMapAllowed(s dbq.ImportSession) error {
	switch s.State {
	case StateExported, StateUserMap, StateDryRunOK, StateFailed:
		if s.ExportHash == nil {
			return errConflict("the session has no export yet")
		}
		return nil
	}
	return errConflict("a user map cannot be saved while the session is %s", s.State)
}

// PutUserMap saves the importer's {default, users} map (#41, #45). Every
// username must be an active member of the org, and every Linear user of the
// export must resolve. A map change recomputes user_map_hash, so a dry run
// taken before it is no longer current.
func (m *Manager) PutUserMap(ctx context.Context, orgID, id uuid.UUID, body []byte) (UserMapView, error) {
	s, err := m.session(ctx, orgID, id)
	if err != nil {
		return UserMapView{}, err
	}
	if s.State == StateClosed {
		return UserMapView{}, errConflict("the import session is closed")
	}
	if err := activeJobErr(ctx, m.q, orgID); err != nil {
		return UserMapView{}, err
	}
	if err := userMapAllowed(s); err != nil {
		return UserMapView{}, err
	}
	um, err := importer.ParseUserMap(body)
	if err != nil {
		return UserMapView{}, httpx.Invalid("%v", err)
	}
	members, err := m.q.ListOrgMembers(ctx, orgID)
	if err != nil {
		return UserMapView{}, fmt.Errorf("list members: %w", err)
	}
	active := map[string]bool{}
	for _, mem := range members {
		if mem.DeactivatedAt == nil {
			active[mem.Username] = true
		}
	}
	for _, name := range um.Usernames() {
		if !active[name] {
			return UserMapView{}, httpx.Invalid("%q is not an active member of this org (invite them first, or make an agent)", name)
		}
	}
	users, err := m.exportUsers(s)
	if err != nil {
		return UserMapView{}, err
	}
	unmapped := 0
	for _, u := range users {
		if _, ok := um.Resolve(u); !ok {
			unmapped++
		}
	}
	if unmapped > 0 {
		return UserMapView{}, httpx.Invalid("%d Linear user(s) have no username and the map has no default", unmapped)
	}
	canon := UserMapView{Default: um.Default, Users: um.Users}
	if canon.Users == nil {
		canon.Users = map[string]string{}
	}
	raw := mustJSON(canon)
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	err = database.InTx(ctx, m.cfg.Pool, func(tx pgx.Tx) error {
		q := m.q.WithTx(tx)
		cur, err := q.ImportSessionLock(ctx, dbq.ImportSessionLockParams{OrgID: orgID, ID: id})
		if err != nil {
			return notFound(err, "import session")
		}
		if cur.State == StateClosed {
			return errConflict("the import session is closed")
		}
		if err := activeJobErr(ctx, q, orgID); err != nil {
			return err
		}
		if err := userMapAllowed(cur); err != nil {
			return err
		}
		return q.ImportSessionSetUserMap(ctx, dbq.ImportSessionSetUserMapParams{
			OrgID: orgID, ID: id, State: StateUserMap, UserMap: raw, UserMapHash: &hash})
	})
	if err != nil {
		return UserMapView{}, err
	}
	return canon, nil
}

// stateIn reports whether state is one of set.
func stateIn(state string, set ...string) bool { return slices.Contains(set, state) }
