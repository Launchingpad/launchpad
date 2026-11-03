package importjob

import (
	"os"
	"testing"
	"time"
)

// TestMeasure_ImportTransaction records (it asserts nothing about speed) how
// much of the pool a running import holds and whether another write on the
// same database waits while the import transaction is open. Run it with
// IMPORT_MEASURE=1 go test -run Measure -v.
func TestMeasure_ImportTransaction(t *testing.T) {
	if os.Getenv("IMPORT_MEASURE") != "1" {
		t.Skip("record-only measurement; set IMPORT_MEASURE=1")
	}
	h := newHarness(t)
	s := h.mapped()
	h.dryDone(s)
	reached, release := h.blockAt(KindImport, "issues")
	start := time.Now()
	j, err := h.runStart(s)
	ok(t, err)
	recv(t, reached, "the issues phase")
	st := h.pool.Stat()
	t.Logf("pool while the import transaction is open: acquired=%d idle=%d total=%d max=%d",
		st.AcquiredConns(), st.IdleConns(), st.TotalConns(), st.MaxConns())
	w := time.Now()
	_, err = h.pool.Exec(ctx(), `UPDATE users SET name = name WHERE username = 'chen'`)
	ok(t, err)
	t.Logf("an unrelated write during the import transaction took %s", time.Since(w))
	var got bool
	ok(t, h.pool.QueryRow(ctx(), `SELECT pg_try_advisory_xact_lock(hashtextextended('devops:team-cycles:' || t.id::text, 0))
		FROM teams t WHERE t.org_id = $1 AND t.key = 'ORB'`, h.org.ID).Scan(&got))
	t.Logf("the hourly cycle job can take the ORB team lock while the import runs: %v (false: it waits for the import)", got)
	release()
	v := h.wait(j.ID)
	t.Logf("import ended %s after %s (fixture of %d issues)", v.State, time.Since(start), 3)
}
