package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/netguard"
	"devops/internal/netguard/netguardtest"
	"devops/internal/storage"
	"devops/internal/storage/storagetest"
)

// The driver against the real silo of the compose stack (INTEGRATION=1; a missing
// silo fails the test, it never skips it). Silo speaks plain http inside the
// compose network, so the dialer lists silo's own address as an operator range.
// No real AWS, Backblaze or MinIO service is reached by any test here.

type siloEnv struct {
	cfg      storage.Config
	endpoint string
	host     string
	policy   netguard.Policy
}

func newSilo(t *testing.T) siloEnv {
	t.Helper()
	storagetest.New(t) // skips without INTEGRATION=1, makes the bucket
	cfg := storagetest.Config()
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	addrs, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip", u.Hostname())
	if err != nil || len(addrs) == 0 {
		t.Fatalf("resolve %s: %v", u.Hostname(), err)
	}
	pol := netguard.Policy{}
	for _, a := range addrs {
		a = a.Unmap()
		pol.Private = append(pol.Private, netip.PrefixFrom(a, a.BitLen()))
		if a.IsLoopback() { // a host-run test: only the test seam opens loopback
			pol = netguardtest.Permit(pol, a.String())
		}
	}
	return siloEnv{cfg: cfg, endpoint: cfg.Endpoint, host: u.Host, policy: pol}
}

func (s siloEnv) driverCfg(t *testing.T, folder string, extra map[string]any) driver.Config {
	t.Helper()
	m := map[string]any{"endpoint": s.endpoint, "bucket": s.cfg.Bucket, "folder": folder}
	for k, v := range extra {
		m[k] = v
	}
	sec, _ := jsonSecrets(s.cfg.AccessKey, s.cfg.SecretKey)
	return driver.Config{Config: cfgJSON(t, m), Secrets: sec, Dialer: dialer(s.policy), AllowPlainHTTP: true}
}

func jsonSecrets(a, s string) ([]byte, error) {
	return []byte(`{"accessKey":"` + a + `","secretKey":"` + s + `"}`), nil
}

func folder(t *testing.T) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "drv-" + hex.EncodeToString(b[:])
}

func TestSilo_Contract(t *testing.T) {
	s := newSilo(t)
	fld := folder(t)
	ctx := context.Background()
	r, err := Driver{}.Open(ctx, s.driverCfg(t, fld, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Test: every step.
	for _, st := range r.Test(ctx, testName) {
		if !st.OK {
			t.Fatalf("Test step %+v", st)
		}
	}

	// Put a few MiB, list, get it back identical, delete it.
	obj := make([]byte, 3<<20+17)
	_, _ = rand.Read(obj)
	put(t, r, objName, obj)
	put(t, r, manName, []byte(`{"payload":"x"}`))

	// A file that is not ours, put with the signer directly in the same folder.
	raw := func(key string, body []byte) {
		u := storage.PresignURL(storage.PresignParams{AccessKey: s.cfg.AccessKey, SecretKey: s.cfg.SecretKey, Region: s.cfg.Region,
			Method: http.MethodPut, Scheme: "http", Host: s.host, Path: "/" + s.cfg.Bucket + "/" + key, Now: time.Now(), Expires: time.Minute})
		req, _ := http.NewRequest(http.MethodPut, u, bytes.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("raw put %s = %d", key, resp.StatusCode)
		}
	}
	raw(fld+"/notes.txt", []byte("not ours"))
	raw(fld+"/sub/"+objName, []byte("nested"))
	raw(folder(t)+"/"+objName, []byte("other folder"))

	es, err := r.List(ctx, "launchpad-")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, e := range es {
		got[e.Name] = e.Size
	}
	if len(es) != 2 || got[objName] != int64(len(obj)) || got[manName] != 15 {
		t.Fatalf("List = %v", es)
	}

	rc, err := r.Get(ctx, objName)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(driver.CapReader(rc, driver.DownloadCap(int64(len(obj)))))
	rc.Close()
	if err != nil || !bytes.Equal(b, obj) {
		t.Fatalf("Get: %v (%d bytes)", err, len(b))
	}
	if _, err := r.Get(ctx, "launchpad-0123abcd-20300101T000000Z-ffff.dump.age"); code(err) != driver.NotFound {
		t.Fatalf("Get missing = %v", err)
	}
	if err := r.Delete(ctx, objName); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, objName); err != nil {
		t.Fatalf("second Delete = %v", err)
	}
	if es, _ = r.List(ctx, "launchpad-"); len(es) != 1 {
		t.Fatalf("after delete: %v", es)
	}
	// The stranger's file is still there: the driver never deleted it.
	if err := r.Delete(ctx, manName); err != nil {
		t.Fatal(err)
	}

	// Prune through the library, as the worker does: keep 2 of 4 pairs.
	stems := []string{
		"launchpad-0123abcd-20261001T031500Z-0001", "launchpad-0123abcd-20261002T031500Z-0002",
		"launchpad-0123abcd-20261003T031500Z-0003", "launchpad-0123abcd-20261004T031500Z-0004",
	}
	for _, st := range stems {
		put(t, r, st+".dump.age", []byte("o"))
		put(t, r, st+".json", []byte("m"))
	}
	res, err := backup.Prune(ctx, r, "0123abcd", 2, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), nil, stems[3])
	if err != nil {
		t.Fatal(err)
	}
	es, _ = r.List(ctx, "launchpad-")
	if len(es) != 4 {
		t.Fatalf("after prune: %v (result %+v)", es, res)
	}
}

func TestSilo_WrongCredentials(t *testing.T) {
	s := newSilo(t)
	c := s.driverCfg(t, folder(t), nil)
	c.Secrets = []byte(`{"accessKey":"` + s.cfg.AccessKey + `","secretKey":"definitely-wrong-secret"}`)
	r, err := Driver{}.Open(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	steps := r.Test(context.Background(), testName)
	if len(steps) != 2 || steps[1].Name != "auth" || steps[1].Code != driver.AuthFailed {
		t.Fatalf("steps = %+v", steps)
	}
	if err := r.Put(context.Background(), objName, 1, bytes.NewReader([]byte("x"))); code(err) != driver.AuthFailed {
		t.Fatalf("Put = %v", err)
	}
	c.Secrets = []byte(`{"accessKey":"nope-nope","secretKey":"definitely-wrong-secret"}`)
	r2, _ := Driver{}.Open(context.Background(), c)
	defer r2.Close()
	if _, err := r2.List(context.Background(), "launchpad-"); code(err) != driver.AuthFailed {
		t.Fatalf("List with an unknown key = %v", err)
	}
}

func TestSilo_NoSuchBucket(t *testing.T) {
	s := newSilo(t)
	c := s.driverCfg(t, "", map[string]any{"bucket": "no-such-bucket-" + folder(t)})
	r, _ := Driver{}.Open(context.Background(), c)
	defer r.Close()
	if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.NotFound {
		t.Fatalf("List = %v", err)
	}
}

func TestSilo_RefusedByThePolicy(t *testing.T) {
	s := newSilo(t)
	// The real backup policy with no operator range: silo's address is private (or
	// loopback) and the name "silo" is a service name. Nothing is sent.
	pol, err := backup.Policy("", backup.Gateways{Read: func() ([]netip.Addr, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	c := s.driverCfg(t, folder(t), nil)
	c.Dialer = backup.NewDialer(pol, 2*time.Second)
	r, err := Driver{}.Open(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Put(context.Background(), objName, 1, bytes.NewReader([]byte("x"))); code(err) != driver.SSRFBlocked {
		t.Fatalf("Put = %v", err)
	}
	if _, err := (Driver{}).Probe(context.Background(), c); code(err) != driver.SSRFBlocked {
		t.Fatalf("Probe = %v", err)
	}
	// Not allowed to use plain http at all.
	c = s.driverCfg(t, folder(t), nil)
	c.AllowPlainHTTP = false
	if _, err := (Driver{}).Open(context.Background(), c); code(err) != driver.SSRFBlocked {
		t.Fatalf("Open without AllowPlainHTTP = %v", err)
	}
}

// silo has no TLS in the stack, so the pin tests put a TLS reverse proxy in front
// of it. The proxy keeps the Host header the client signed.
func TestSilo_TLSPinThroughAProxy(t *testing.T) {
	s := newSilo(t)
	target, _ := url.Parse(s.endpoint)
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	defer proxy.Close()
	sum := sha256.Sum256(proxy.Certificate().RawSubjectPublicKeyInfo)
	pin := hex.EncodeToString(sum[:])
	pol := netguardtest.Permit(netguard.Policy{}, "127.0.0.1")
	mk := func(pin string) driver.Config {
		c := s.driverCfg(t, folder(t), map[string]any{"endpoint": proxy.URL, "tlsPin": pin})
		c.Dialer = dialer(pol)
		c.AllowPlainHTTP = false
		return c
	}
	ctx := context.Background()

	r, err := Driver{}.Open(ctx, mk(pin))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, st := range r.Test(ctx, testName) {
		if !st.OK {
			t.Fatalf("pinned Test step %+v", st)
		}
	}

	bad, _ := Driver{}.Open(ctx, mk("00"+pin[2:]))
	defer bad.Close()
	if err := bad.Put(ctx, objName, 1, bytes.NewReader([]byte("x"))); code(err) != driver.TLSPinMismatch {
		t.Fatalf("wrong pin Put = %v", err)
	}

	none, _ := Driver{}.Open(ctx, mk(""))
	defer none.Close()
	if _, err := none.List(ctx, "launchpad-"); code(err) != driver.TLSUntrusted {
		t.Fatalf("no pin = %v", err)
	}

	pr, err := Driver{}.Probe(ctx, mk(""))
	if err != nil || pr.Kind != "tls" || pr.Fingerprint != pin || pr.ChainVerified {
		t.Fatalf("Probe = %+v, %v", pr, err)
	}
}

func TestSilo_Probe(t *testing.T) {
	s := newSilo(t)
	pr, err := Driver{}.Probe(context.Background(), s.driverCfg(t, "", nil))
	if err != nil || pr.Kind != "plain" {
		t.Fatalf("Probe = %+v, %v", pr, err)
	}
}
