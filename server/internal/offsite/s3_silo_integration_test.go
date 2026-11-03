//go:build backuptest

package offsite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/backup/driver/s3"
	"devops/internal/backup/remotename"
	"devops/internal/netguard"
	"devops/internal/netguard/netguardtest"
	"devops/internal/storage/storagetest"
)

// The offsite worker end to end with the REAL s3 driver against the compose silo
// (INTEGRATION=1): three backup runs upload, list and prune (keep_last 2), then a
// fetch reads the newest pair back. A probe and a test run on the saved destination
// go through the same driver. No real AWS, Backblaze or MinIO service is reached.
func TestS3Silo_WorkerEndToEnd(t *testing.T) {
	storagetest.New(t) // skips without INTEGRATION=1; makes the bucket
	sc := storagetest.Config()
	u, err := url.Parse(sc.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	addrs, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip", u.Hostname())
	if err != nil || len(addrs) == 0 {
		t.Fatalf("resolve silo: %v", err)
	}
	pol := netguard.Policy{}
	for _, a := range addrs {
		a = a.Unmap()
		pol.Private = append(pol.Private, netip.PrefixFrom(a, a.BitLen()))
		if a.IsLoopback() {
			pol = netguardtest.Permit(pol, a.String())
		}
	}

	h := newHarness(t)
	driver.RegisterForTest(t, "s3", s3.Driver{}) // replaces the harness fake
	h.w = h.newWorker(h.pool, h.dsn, func(o *Options) { o.Policy = pol })

	folder := "worker-" + hex.EncodeToString(bytes32(t)[:6])
	destID := h.addDest("silo")
	cfg, _ := json.Marshal(map[string]string{"endpoint": sc.Endpoint, "bucket": sc.Bucket, "folder": folder})
	blob, err := h.w.keys.Seal("backup_destinations.secrets_enc", destID.String(),
		[]byte(`{"accessKey":"`+sc.AccessKey+`","secretKey":"`+sc.SecretKey+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE backup_destinations SET config = $2, secrets_enc = $3, secrets_set = '{accessKey,secretKey}', keep_last = 2 WHERE id = $1`,
		destID, cfg, blob)

	// A probe and a test on the saved destination.
	pid := h.queue(destID, "probe")
	h.scan()
	if r := h.run(pid); r.Status != "succeeded" {
		t.Fatalf("probe = %+v\nlog:\n%s", r, h.log.String())
	} else if st := stepsOf(t, r); len(st) != 1 || st[0]["kind"] != "plain" {
		t.Fatalf("probe steps = %v", st)
	}
	tid := h.queue(destID, "test")
	h.scan()
	if r := h.run(tid); r.Status != "succeeded" {
		t.Fatalf("test = %+v\nlog:\n%s", r, h.log.String())
	} else if st := stepsOf(t, r); len(st) != 5 {
		t.Fatalf("test steps = %v", st)
	}

	var last string
	for i := 0; i < 3; i++ {
		// Each run gets a NEWER dump, so the stems differ in their time (equal times
		// tie-break on the random part and the new pair may be kept in addition, T16 ticket 3 note 12).
		h.addDump(time.Duration(30-10*i)*time.Minute, nil)
		id := h.queue(destID, "manual")
		h.scan()
		r := h.run(id)
		if r.Status != "succeeded" || r.RemoteName == nil || r.Sha256 == nil {
			t.Fatalf("run %d = %+v\nlog:\n%s", i, r, h.log.String())
		}
		last = *r.RemoteName
		// A manual run makes the destination busy until it is final; it is.
	}

	rem, err := s3.Driver{}.Open(context.Background(), driver.Config{Config: cfg,
		Secrets: json.RawMessage(`{"accessKey":"` + sc.AccessKey + `","secretKey":"` + sc.SecretKey + `"}`),
		Dialer:  backup.NewDialer(pol, 5*time.Second), AllowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer rem.Close()
	ctx := context.Background()
	es, err := rem.List(ctx, "launchpad-"+h.settings().InstallID+"-")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 4 {
		t.Fatalf("after 3 runs and keep_last 2 the folder holds %v", es)
	}
	defer func() {
		for _, e := range es {
			_ = rem.Delete(ctx, e.Name)
		}
	}()
	stem := strings.TrimSuffix(last, remotename.ObjectSuffix)
	var frame bytes.Buffer
	if err := FetchFrame(ctx, rem, h.settings().InstallID, "latest", &frame); err != nil {
		t.Fatal(err)
	}
	rc, err := rem.Get(ctx, last)
	if err != nil {
		t.Fatal(err)
	}
	obj, _ := io.ReadAll(rc)
	rc.Close()
	sum := sha256.Sum256(obj)
	if !bytes.Contains(frame.Bytes(), obj) {
		t.Fatal("the fetched frame does not hold the newest object")
	}
	if got := hex.EncodeToString(sum[:]); got == "" || !strings.Contains(stem, h.settings().InstallID) {
		t.Fatal("bad stem")
	}
	// The sha256 the run recorded is the object that is on silo.
	if r := h.run(h.lastRunID(t)); r.Sha256 == nil || *r.Sha256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("the recorded sha256 is not the remote object's")
	}
}

// lastRunID is the newest manual backup run (by queue time).
func (h *harness) lastRunID(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := h.pool.QueryRow(context.Background(), `SELECT id FROM backup_runs WHERE trigger = 'manual' ORDER BY queued_at DESC, id DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
