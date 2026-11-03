package importjob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/importer"
	"devops/internal/importer/linear"
)

// endKind is how a job ended, for the session transition of #43.
type endKind int

const (
	endOK endKind = iota
	endMismatch
	endFailed // failed, canceled or interrupted
)

// nextState is the session transition table of decisions-T17 #43 for the end
// of a job: the new state, and false when the session stays as it is (a
// verify that failed for another reason than a mismatch, or a session that
// is not in a state the job could have left).
func nextState(kind string, end endKind, cur string) (string, bool) {
	switch kind {
	case KindExport:
		if cur != StateExporting {
			return "", false
		}
		if end == endOK {
			return StateExported, true
		}
		return StateFailed, true
	case KindDryRun:
		if !stateIn(cur, StateUserMap, StateDryRunOK, StateFailed) {
			return "", false
		}
		if end == endOK {
			return StateDryRunOK, true
		}
		return StateFailed, true
	case KindImport:
		if cur != StateImporting {
			return "", false
		}
		if end == endOK {
			return StateImported, true
		}
		return StateFailed, true
	case KindVerify:
		if !stateIn(cur, StateImported, StateVerified, StateFailed) {
			return "", false
		}
		switch end {
		case endOK:
			return StateVerified, true
		case endMismatch:
			return StateFailed, true
		}
	}
	return "", false
}

// result is what a job's work function reports.
type result struct {
	err       error
	code      string // the error_code when err != nil, set by the work function
	report    *JobCounts
	rows      []importer.VerifyRow
	committed bool
	export    *exportOutcome
}

// exportOutcome is what a finished export adds to its session.
type exportOutcome struct {
	hash   string
	counts map[string]int
	files  fileProblems
}

type workFn func(ctx context.Context, jr *jobRun) result

// jobRun is a job in this process: its row, a snapshot of its session, the
// cancel function of its context and the debounced progress.
type jobRun struct {
	m       *Manager
	job     dbq.ImportJob
	sess    dbq.ImportSession
	orgSlug string
	cancel  context.CancelCauseFunc

	pmu         sync.Mutex
	phase       string
	done, total int64
	dirty       bool
}

// report is the progress callback of the importer and the export. It only
// records; the heartbeat loop writes the row at most once a second.
func (jr *jobRun) report(phase string, done, total int64) {
	if h := jr.m.progressHook; h != nil {
		h(jr.job.Kind, phase)
	}
	jr.pmu.Lock()
	jr.phase, jr.done, jr.total, jr.dirty = phase, done, total, true
	jr.pmu.Unlock()
}

func (jr *jobRun) flushProgress(ctx context.Context) {
	jr.pmu.Lock()
	phase, done, total, dirty := jr.phase, jr.done, jr.total, jr.dirty
	jr.dirty = false
	jr.pmu.Unlock()
	if !dirty {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := jr.m.q.ImportJobProgress(cctx, dbq.ImportJobProgressParams{OrgID: jr.job.OrgID, ID: jr.job.ID,
		Phase: phase, Done: done, Total: total}); err != nil {
		jr.m.log.Warn("import: progress write failed", "job", jr.job.ID, "err", err)
	}
}

// startSpec is the rule of one start route.
type startSpec struct {
	kind    string
	allowed []string
	// to is the session state while the job runs ("" keeps it).
	to    string
	check func(ctx context.Context, q *dbq.Queries, s dbq.ImportSession) error
}

// begin makes the job row, and moves the session, in one transaction under
// the session's row lock. The partial unique index is the one-active-job
// guard across instances.
func (m *Manager) begin(ctx context.Context, orgID, sessionID uuid.UUID, spec startSpec) (dbq.ImportJob, dbq.ImportSession, error) {
	var job dbq.ImportJob
	var sess dbq.ImportSession
	err := database.InTx(ctx, m.cfg.Pool, func(tx pgx.Tx) error {
		q := m.q.WithTx(tx)
		s, err := q.ImportSessionLock(ctx, dbq.ImportSessionLockParams{OrgID: orgID, ID: sessionID})
		if err != nil {
			return notFound(err, "import session")
		}
		if s.State == StateClosed {
			return errConflict("the import session is closed")
		}
		if err := activeJobErr(ctx, q, orgID); err != nil {
			return err
		}
		if !stateIn(s.State, spec.allowed...) {
			return errConflict("a %s job cannot start while the session is %s", spec.kind, s.State)
		}
		if spec.check != nil {
			if err := spec.check(ctx, q, s); err != nil {
				return err
			}
		}
		job, err = q.ImportJobInsert(ctx, dbq.ImportJobInsertParams{OrgID: orgID, SessionID: sessionID, Kind: spec.kind,
			ExportHash: s.ExportHash, UserMapHash: s.UserMapHash, InstanceID: m.instance})
		if err != nil {
			if isUnique(err) {
				return errJobActive()
			}
			return fmt.Errorf("create import job: %w", err)
		}
		if spec.kind == KindExport {
			if err := q.ImportSessionClearExport(ctx, dbq.ImportSessionClearExportParams{OrgID: orgID, ID: sessionID}); err != nil {
				return fmt.Errorf("clear export hash: %w", err)
			}
			s.ExportHash = nil
		}
		if spec.to != "" {
			if err := q.ImportSessionSetState(ctx, dbq.ImportSessionSetStateParams{OrgID: orgID, ID: sessionID, State: spec.to}); err != nil {
				return fmt.Errorf("set session state: %w", err)
			}
			s.State = spec.to
		}
		sess = s
		return nil
	})
	return job, sess, err
}

// launch runs a job in its own goroutine, on the manager's context (not the
// request's). Close cancels it.
func (m *Manager) launch(job dbq.ImportJob, sess dbq.ImportSession, orgSlug string, work workFn) {
	ctx, cancel := context.WithCancelCause(m.base)
	jr := &jobRun{m: m, job: job, sess: sess, orgSlug: orgSlug, cancel: cancel}
	m.mu.Lock()
	m.running[job.ID] = jr
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			delete(m.running, job.ID)
			m.mu.Unlock()
			cancel(nil)
		}()
		jr.main(ctx, work)
	}()
}

func (jr *jobRun) main(ctx context.Context, work workFn) {
	m := jr.m
	bg := context.WithoutCancel(ctx)
	n, err := m.q.ImportJobMarkRunning(bg, dbq.ImportJobMarkRunningParams{OrgID: jr.job.OrgID, ID: jr.job.ID})
	if err != nil || n == 0 {
		m.log.Warn("import: job could not start (the row is gone or not queued)", "job", jr.job.ID, "kind", jr.job.Kind, "err", err)
		return
	}
	stop := make(chan struct{})
	var hb sync.WaitGroup
	hb.Go(func() { jr.heartbeat(bg, stop) })
	res := jr.safely(ctx, work)
	close(stop)
	hb.Wait()
	jr.flushProgress(bg)
	jr.finish(ctx, bg, res)
}

// safely runs work and turns a panic into a failed job (the type of the
// panic is logged, never its value).
func (jr *jobRun) safely(ctx context.Context, work workFn) (res result) {
	defer func() {
		if r := recover(); r != nil {
			jr.m.log.Error("import: job panicked", "job", jr.job.ID, "kind", jr.job.Kind, "panic_type", fmt.Sprintf("%T", r))
			res = result{err: errors.New("panic"), code: failCode(jr.job.Kind)}
		}
	}()
	return work(ctx, jr)
}

// heartbeat beats every Heartbeat (and reads cancel_requested, which may have
// been set on another instance) and writes the progress every ProgressEvery.
// It runs until stop, also while the job winds down after a cancel.
func (jr *jobRun) heartbeat(bg context.Context, stop <-chan struct{}) {
	m := jr.m
	hb := time.NewTicker(m.cfg.Heartbeat)
	pg := time.NewTicker(m.cfg.ProgressEvery)
	defer hb.Stop()
	defer pg.Stop()
	for {
		select {
		case <-stop:
			return
		case <-pg.C:
			jr.flushProgress(bg)
		case <-hb.C:
			cctx, cancel := context.WithTimeout(bg, 10*time.Second)
			req, err := m.q.ImportJobHeartbeat(cctx, dbq.ImportJobHeartbeatParams{OrgID: jr.job.OrgID, ID: jr.job.ID})
			cancel()
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				// The sweep marked this job interrupted (a stalled process):
				// stop work. An import that has not committed rolls back.
				m.log.Warn("import: the job was taken over by the sweep; stopping it", "job", jr.job.ID)
				jr.cancel(errLost)
				return
			case err != nil:
				m.log.Warn("import: heartbeat failed", "job", jr.job.ID, "err", err)
			case req:
				jr.cancel(errUserCanceled)
			}
		}
	}
}

// resolve turns a result and the context's cancel cause into the final job
// state, the error code and the kind of end.
func (jr *jobRun) resolve(ctx context.Context, res result) (state, code string, end endKind) {
	if res.err == nil {
		return JobSucceeded, "", endOK
	}
	if cause := context.Cause(ctx); cause != nil && !res.committed {
		if errors.Is(cause, errUserCanceled) {
			return JobCanceled, CodeCanceled, endFailed
		}
		return JobFailed, CodeInterrupted, endFailed
	}
	if errors.Is(res.err, importer.ErrMismatch) {
		return JobFailed, CodeVerifyMismatch, endMismatch
	}
	code = res.code
	if code == "" {
		code = failCode(jr.job.Kind)
	}
	return JobFailed, code, endFailed
}

func failCode(kind string) string {
	switch kind {
	case KindExport:
		return CodeExportFailed
	case KindDryRun:
		return CodeDryRunFailed
	case KindImport:
		return CodeImportFailed
	}
	return CodeVerifyFailed
}

var errNotActive = errors.New("the job row is no longer active")

// finish writes the end of the job and the session transition in one
// transaction. It uses its own context: the job's may be canceled.
func (jr *jobRun) finish(ctx, bg context.Context, res result) {
	m := jr.m
	state, code, end := jr.resolve(ctx, res)
	var text *string
	var codep *string
	if code != "" {
		t := errorTexts[code]
		text, codep = &t, &code
	}
	counts := []byte("{}")
	if res.report != nil {
		counts = mustJSON(res.report)
	}
	rows, truncated := res.rows, false
	if len(rows) > maxVerifyRows {
		rows, truncated = rows[:maxVerifyRows], true
	}
	if rows == nil {
		rows = []importer.VerifyRow{}
	}
	if res.err != nil {
		// Our code and the step only; the importer's text can name a person.
		m.log.Error("import: job ended with an error", "job", jr.job.ID, "kind", jr.job.Kind, "code", code,
			"committed", res.committed, "err", scrub(res.err))
	}
	fctx, cancel := context.WithTimeout(bg, 15*time.Second)
	defer cancel()
	err := database.InTx(fctx, m.cfg.Pool, func(tx pgx.Tx) error {
		q := m.q.WithTx(tx)
		s, err := q.ImportSessionLock(fctx, dbq.ImportSessionLockParams{OrgID: jr.job.OrgID, ID: jr.job.SessionID})
		if err != nil {
			return fmt.Errorf("lock session: %w", err)
		}
		n, err := q.ImportJobFinish(fctx, dbq.ImportJobFinishParams{OrgID: jr.job.OrgID, ID: jr.job.ID, State: state,
			Counts: counts, Rows: mustJSON(rows), RowsTruncated: truncated, Committed: res.committed,
			ErrorCode: codep, ErrorText: text})
		if err != nil {
			return fmt.Errorf("finish job: %w", err)
		}
		if n == 0 {
			return errNotActive
		}
		next, ok := nextState(jr.job.Kind, end, s.State)
		if !ok {
			return nil
		}
		if jr.job.Kind == KindExport && end == endOK && res.export != nil {
			return q.ImportSessionSetExport(fctx, dbq.ImportSessionSetExportParams{OrgID: s.OrgID, ID: s.ID, State: next,
				ExportHash: &res.export.hash, ExportCounts: mustJSON(res.export.counts),
				FailedFiles: int32(res.export.files.count), FailedFileNames: mustJSON(res.export.files.names)}) //nolint:gosec // small
		}
		return q.ImportSessionSetState(fctx, dbq.ImportSessionSetStateParams{OrgID: s.OrgID, ID: s.ID, State: next})
	})
	switch {
	case errors.Is(err, errNotActive):
		m.log.Error("import: the job ended after the sweep marked it interrupted; its result is not recorded", "job", jr.job.ID,
			"kind", jr.job.Kind, "committed", res.committed)
	case err != nil:
		m.log.Error("import: could not record the end of a job; the sweep marks it interrupted", "job", jr.job.ID, "err", err)
	}
}

// emailRe finds an address; scrub removes it from a log text.
var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+'-]+@[A-Za-z0-9.-]+`)

func scrub(err error) string {
	s := emailRe.ReplaceAllString(err.Error(), "<email>")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// ---------------------------------------------------------------------------
// Start routes.

// StartExport checks the Linear key, then starts the export job. The key is
// copied for the job and overwritten when the export ends; the caller keeps
// and clears its own slice (decisions-T17 #38).
func (m *Manager) StartExport(ctx context.Context, org *auth.Org, sessionID uuid.UUID, key []byte) (JobView, error) {
	s, err := m.session(ctx, org.ID, sessionID)
	if err != nil {
		return JobView{}, err
	}
	if s.State == StateClosed {
		return JobView{}, errConflict("the import session is closed")
	}
	if err := activeJobErr(ctx, m.q, org.ID); err != nil {
		return JobView{}, err
	}
	if !stateIn(s.State, exportStates...) {
		return JobView{}, errConflict("an export cannot start while the session is %s", s.State)
	}
	if err := m.ensureWorkDir(); err != nil {
		return JobView{}, err
	}
	free, err := m.cfg.FreeBytes(m.cfg.WorkDir)
	if err != nil {
		return JobView{}, fmt.Errorf("free space: %w", err)
	}
	if free < m.cfg.FloorBytes {
		return JobView{}, errNoSpace()
	}
	if err := m.checkKey(ctx, key); err != nil {
		return JobView{}, err
	}
	job, sess, err := m.begin(ctx, org.ID, sessionID, startSpec{kind: KindExport, allowed: exportStates, to: StateExporting})
	if err != nil {
		return JobView{}, err
	}
	m.launch(job, sess, org.Slug, m.exportWork(bytes.Clone(key)))
	return jobView(job, sess), nil
}

var exportStates = []string{StateNew, StateExported, StateUserMap, StateDryRunOK, StateImported, StateVerified, StateFailed}

// checkKey asks Linear who the key belongs to. One try, no back-off: a wrong
// key answers linear_unauthorized and a network error linear_unreachable,
// never with Linear's text (#38).
func (m *Manager) checkKey(ctx context.Context, key []byte) error {
	c, err := linear.NewClient(m.cfg.LinearEndpoint, string(key), m.log)
	if err != nil {
		return httpx.Invalid("linearApiKey is required")
	}
	c.MaxRetries = 0
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var out struct {
		Viewer struct {
			ID string `json:"id"`
		} `json:"viewer"`
	}
	if err := c.Query(cctx, `query { viewer { id } }`, nil, &out); err != nil {
		return linearHTTPError(err)
	}
	return nil
}

func linearHTTPError(err error) *httpx.Error {
	if linearUnauthorized(err) {
		return httpx.Errorf(http.StatusBadGateway, httpx.CodeLinearUnauthorized, "Linear refused the API key")
	}
	return httpx.Errorf(http.StatusBadGateway, httpx.CodeLinearUnreachable, "Linear could not be reached")
}

func linearUnauthorized(err error) bool {
	var api *linear.APIError
	if !errors.As(err, &api) {
		return false
	}
	msg := strings.ToLower(api.Message)
	return api.Status == http.StatusUnauthorized || api.Status == http.StatusForbidden ||
		strings.Contains(msg, "authentication") || strings.Contains(msg, "not authenticated")
}

// StartDryRun starts a dry run. It answers 409 conflict while the export has
// failed files and acceptFailedFiles is not set (#39).
func (m *Manager) StartDryRun(ctx context.Context, org *auth.Org, sessionID uuid.UUID, acceptFailedFiles bool) (JobView, error) {
	job, sess, err := m.begin(ctx, org.ID, sessionID, startSpec{kind: KindDryRun,
		allowed: []string{StateUserMap, StateDryRunOK, StateFailed},
		check: func(_ context.Context, _ *dbq.Queries, s dbq.ImportSession) error {
			if s.ExportHash == nil || len(s.UserMap) == 0 {
				return errConflict("a dry run needs an export and a saved user map")
			}
			if s.FailedFiles > 0 && !acceptFailedFiles {
				return errConflict("%d file(s) of the export were not downloaded; send acceptFailedFiles: true after reading the list", s.FailedFiles)
			}
			return nil
		}})
	if err != nil {
		return JobView{}, err
	}
	m.launch(job, sess, org.Slug, m.importWork(true))
	return jobView(job, sess), nil
}

// StartRun starts the real import. It needs a succeeded dry run on the
// current export and user map, and the org slug typed by the admin.
func (m *Manager) StartRun(ctx context.Context, org *auth.Org, sessionID uuid.UUID, confirmOrg string) (JobView, error) {
	if strings.TrimSpace(confirmOrg) != org.Slug {
		return JobView{}, httpx.Invalid("confirmOrg must be the org slug %q", org.Slug)
	}
	job, sess, err := m.begin(ctx, org.ID, sessionID, startSpec{kind: KindImport,
		allowed: []string{StateExported, StateUserMap, StateDryRunOK, StateFailed, StateImported, StateVerified}, to: StateImporting,
		check: func(ctx context.Context, q *dbq.Queries, s dbq.ImportSession) error {
			if s.ExportHash == nil || s.UserMapHash == nil {
				return httpx.Errorf(http.StatusConflict, httpx.CodeDryRunRequired, "run a dry run first")
			}
			ok, err := q.ImportDryRunCurrent(ctx, dbq.ImportDryRunCurrentParams{OrgID: s.OrgID, SessionID: s.ID,
				ExportHash: *s.ExportHash, UserMapHash: *s.UserMapHash})
			if err != nil {
				return fmt.Errorf("dry run lookup: %w", err)
			}
			if !ok {
				return httpx.Errorf(http.StatusConflict, httpx.CodeDryRunRequired,
					"there is no succeeded dry run on the current export and user map; run a dry run first")
			}
			return nil
		}})
	if err != nil {
		return JobView{}, err
	}
	m.launch(job, sess, org.Slug, m.importWork(false))
	return jobView(job, sess), nil
}

// StartVerify starts a verify job. It needs rows from a committed import.
func (m *Manager) StartVerify(ctx context.Context, org *auth.Org, sessionID uuid.UUID) (JobView, error) {
	job, sess, err := m.begin(ctx, org.ID, sessionID, startSpec{kind: KindVerify,
		allowed: []string{StateImported, StateVerified, StateFailed},
		check: func(ctx context.Context, q *dbq.Queries, s dbq.ImportSession) error {
			if s.UserMapHash == nil {
				return errConflict("a verify needs an export and a saved user map")
			}
			ok, err := q.ImportCommittedExists(ctx, dbq.ImportCommittedExistsParams{OrgID: s.OrgID, SessionID: s.ID})
			if err != nil {
				return fmt.Errorf("committed import lookup: %w", err)
			}
			if !ok {
				return errConflict("nothing was imported in this session, so there is nothing to verify")
			}
			return nil
		}})
	if err != nil {
		return JobView{}, err
	}
	m.launch(job, sess, org.Slug, m.verifyWork())
	return jobView(job, sess), nil
}

// Cancel asks a job to stop. The owning process reads the flag at its next
// heartbeat (it may be another instance); this process stops it at once.
func (m *Manager) Cancel(ctx context.Context, orgID, jobID uuid.UUID) (JobView, error) {
	j, err := m.q.ImportJobGet(ctx, dbq.ImportJobGetParams{OrgID: orgID, ID: jobID})
	if err != nil {
		return JobView{}, notFound(err, "import job")
	}
	if j.State != JobQueued && j.State != JobRunning {
		return JobView{}, errConflict("the job has finished already")
	}
	if _, err := m.q.ImportJobRequestCancel(ctx, dbq.ImportJobRequestCancelParams{OrgID: orgID, ID: jobID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return JobView{}, errConflict("the job has finished already")
		}
		return JobView{}, fmt.Errorf("request cancel: %w", err)
	}
	m.mu.Lock()
	jr := m.running[jobID]
	m.mu.Unlock()
	if jr != nil {
		jr.cancel(errUserCanceled)
	}
	return m.GetJob(ctx, orgID, jobID)
}

// ---------------------------------------------------------------------------
// The work of each kind.

// exportWork runs the Linear export into the session's directory. key is the
// job's own copy; it is overwritten as soon as the clients hold it.
func (m *Manager) exportWork(key []byte) workFn {
	return func(ctx context.Context, jr *jobRun) result {
		keyStr := string(key)
		clear(key)
		c, err := linear.NewClient(m.cfg.LinearEndpoint, keyStr, m.log)
		if err != nil {
			return result{err: err, code: CodeExportFailed}
		}
		dl, err := linear.NewDownloader(keyStr, m.log)
		keyStr = ""
		if err != nil {
			return result{err: err, code: CodeExportFailed}
		}
		free, err := m.cfg.FreeBytes(m.cfg.WorkDir)
		if err != nil {
			return result{err: err, code: CodeExportFailed}
		}
		maxTotal := int64(min(free/2, uint64(maxTotalCap))) //nolint:gosec // capped
		if m.cfg.MaxTotalBytes > 0 {
			maxTotal = m.cfg.MaxTotalBytes
		}
		ex, err := m.exportFn(ctx, c, dl, linear.Options{OutDir: m.exportPath(jr.sess.ExportDir), Files: true,
			MaxFileBytes: 100 << 20, MaxTotalBytes: maxTotal, Log: m.log, Progress: jr.report})
		if err != nil {
			return result{err: err, code: linearCode(err)}
		}
		pr := checkFiles(ex.Files)
		if pr.noSpace {
			return result{err: errors.New("the download limit was reached"), code: CodeNoSpace}
		}
		root, err := m.openExport(jr.sess.ExportDir)
		if err != nil {
			return result{err: err, code: CodeExportFailed}
		}
		defer func() { _ = root.Close() }()
		hash, err := manifestHash(root)
		if err != nil {
			return result{err: err, code: CodeExportFailed}
		}
		return result{export: &exportOutcome{hash: hash, counts: linear.Counts(ex), files: pr}}
	}
}

func linearCode(err error) string {
	var ne net.Error
	var ue *url.Error
	switch {
	case linearUnauthorized(err):
		return CodeLinearUnauthorized
	case errors.As(err, &ne), errors.As(err, &ue):
		return CodeLinearUnreachable
	}
	return CodeExportFailed
}

// planCode is the job error code of a failed plan step.
func planCode(err error, fail string) string {
	if errors.Is(err, errExportChanged) {
		return CodeExportChanged
	}
	return fail
}

var errExportChanged = errors.New("the export on disk changed since the job was approved")

// planOf reads the session's export and builds the plan from its user map.
func (m *Manager) planOf(jr *jobRun) (*importer.Export, *importer.Plan, error) {
	root, err := m.openExport(jr.sess.ExportDir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = root.Close() }()
	// The files must still be the ones the job was approved for.
	if jr.job.ExportHash == nil {
		return nil, nil, errExportChanged
	}
	if h, err := manifestHash(root); err != nil || h != *jr.job.ExportHash {
		return nil, nil, errExportChanged
	}
	ex, err := importer.ReadExportRoot(root)
	if err != nil {
		return nil, nil, err
	}
	um, err := importer.ParseUserMap(jr.sess.UserMap)
	if err != nil {
		return nil, nil, err
	}
	// No counter floors: the CLI's --counter-floor is for a team cutover.
	p, err := importer.BuildPlan(ex, um, nil)
	if err != nil {
		return nil, nil, err
	}
	return ex, p, nil
}

// importWork is the dry run and the real import: importer.Run, which writes
// every row in ONE transaction (a dry run rolls it back).
func (m *Manager) importWork(dry bool) workFn {
	fail := CodeImportFailed
	if dry {
		fail = CodeDryRunFailed
	}
	return func(ctx context.Context, jr *jobRun) result {
		jr.report("plan", 0, 1)
		ex, plan, err := m.planOf(jr)
		if err != nil {
			return result{err: err, code: planCode(err, fail)}
		}
		rep, err := importer.Run(ctx, m.cfg.Pool, ex, plan, importer.Options{Org: jr.orgSlug, DryRun: dry, Store: m.cfg.Store,
			ExportDir: m.exportPath(jr.sess.ExportDir), PublicBaseURL: m.cfg.PublicBaseURL, Log: m.log, Progress: jr.report})
		res := result{}
		if rep != nil {
			res.report, res.rows, res.committed = toJobCounts(rep), rep.Verify, rep.Committed
		}
		if err != nil {
			res.err, res.code = err, fail
		}
		return res
	}
}

// verifyWork compares the export with the database (importer.Verify).
func (m *Manager) verifyWork() workFn {
	return func(ctx context.Context, jr *jobRun) result {
		jr.report("plan", 0, 1)
		_, plan, err := m.planOf(jr)
		if err != nil {
			return result{err: err, code: planCode(err, CodeVerifyFailed)}
		}
		jr.report("verify", 0, 1)
		rows, err := importer.Verify(ctx, m.q, jr.job.OrgID, plan, m.cfg.Store)
		if err != nil {
			return result{err: err, code: CodeVerifyFailed}
		}
		res := result{rows: rows}
		if len(importer.Mismatches(rows)) > 0 {
			res.err, res.code = importer.ErrMismatch, CodeVerifyMismatch
		}
		return res
	}
}

// toJobCounts is the report as the job row keeps it.
func toJobCounts(rep *importer.Report) *JobCounts {
	jc := &JobCounts{DryRun: rep.DryRun, Counts: map[string]importer.Counts{}, ExclusionsTotal: len(rep.Exclusions),
		MergedLabels: rep.MergedLabels, MergedRelations: rep.MergedRelations, MissingCycleNumbers: rep.MissingCycleNumbers,
		NextIssueNumbers: rep.NextNumbers, Cadence: rep.Cadence, PendingUploads: rep.PendingUploads, Uploads: rep.Uploads}
	for k, v := range rep.Counts {
		if v != nil {
			jc.Counts[k] = *v
		}
	}
	jc.Exclusions = rep.Exclusions
	if len(jc.Exclusions) > maxExclusionRep {
		jc.Exclusions = jc.Exclusions[:maxExclusionRep]
	}
	if jc.Exclusions == nil {
		jc.Exclusions = []importer.Exclusion{}
	}
	if jc.MissingCycleNumbers == nil {
		jc.MissingCycleNumbers = map[string][]int32{}
	}
	if jc.NextIssueNumbers == nil {
		jc.NextIssueNumbers = map[string]int32{}
	}
	if jc.Cadence == nil {
		jc.Cadence = map[string]string{}
	}
	return jc
}
