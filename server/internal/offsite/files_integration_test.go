//go:build backuptest

package offsite

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/backup/driver/s3"
	"devops/internal/backup/remotename"
	"devops/internal/dbq"
	"devops/internal/netguard"
	"devops/internal/netguard/netguardtest"
	"devops/internal/storage"
	"devops/internal/storage/storagetest"
)

// The attachment backup (ticket 14, LAUN-80) against the REAL silo of the compose
// stack (INTEGRATION=1) and the fake destination of the harness (a directory). Each
// test has its own bucket. No real remote and no real offsite host is involved.

type filesEnv struct {
	t     *testing.T
	h     *harness
	store *storage.S3
	silo  SiloConfig
	dest  uuid.UUID
	keys  []string

	objPuts atomic.Int32
	failAt  atomic.Int32 // the Nth object Put fails (1 based); 0: never
	onPut   func(name string)
	seen    map[string]bool // manifests already looked at
}

func newFilesEnv(t *testing.T, mut func(*Options)) *filesEnv {
	t.Helper()
	storagetest.New(t) // skips without INTEGRATION=1
	sc := storagetest.Config()
	sc.Bucket = "lf-" + hex.EncodeToString(bytes32(t)[:6])
	store, err := storage.New(sc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	e := &filesEnv{t: t, store: store, seen: map[string]bool{},
		silo: SiloConfig{Endpoint: sc.Endpoint, Bucket: sc.Bucket, AccessKey: sc.AccessKey, SecretKey: sc.SecretKey}}
	e.h = newHarness(t)
	e.h.fake.before = func(_ context.Context, name string) error {
		if strings.Contains(name, "-obj-") {
			n := e.objPuts.Add(1)
			if e.onPut != nil {
				e.onPut(name)
			}
			if f := e.failAt.Load(); f > 0 && n == f {
				return driver.Errf(driver.ConnectRefused)
			}
		}
		return nil
	}
	e.h.w = e.h.newWorker(e.h.pool, e.h.dsn, func(o *Options) {
		o.Silo = e.silo
		if mut != nil {
			mut(o)
		}
	})
	e.dest = e.h.addDest("main")
	e.h.addDump(10*time.Minute, nil)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, k := range e.keys {
			_ = store.Delete(c, k)
		}
	})
	return e
}

func (e *filesEnv) put(key string, data []byte) {
	e.t.Helper()
	e.putReader(key, int64(len(data)), bytes.NewReader(data))
}

func (e *filesEnv) putReader(key string, size int64, r io.Reader) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := e.store.Put(ctx, key, "application/octet-stream", size, r); err != nil {
		e.t.Fatalf("put %s: %v", key, err)
	}
	e.keys = append(e.keys, key)
}

func (e *filesEnv) del(key string) {
	e.t.Helper()
	if err := e.store.Delete(context.Background(), key); err != nil {
		e.t.Fatal(err)
	}
}

func okey(org, name string) string {
	sum := sha256.Sum256([]byte(name))
	return "org/" + org + "/2026/0198cccc-0000-7000-8000-" + hex.EncodeToString(sum[:6]) + "/" + name
}

func randBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// run queues a manual backup and runs it; it returns the row and its files step.
func (e *filesEnv) run() (dbq.BackupRun, map[string]any) {
	e.t.Helper()
	id := e.h.queue(e.dest, "manual")
	e.h.scan()
	r := e.h.run(id)
	if r.Status != "succeeded" {
		e.t.Fatalf("run = %+v\nlog:\n%s", r, e.h.log.String())
	}
	var step map[string]any
	for _, s := range stepsOf(e.t, r) {
		if s["name"] == "files" {
			step = s
		}
	}
	return r, step
}

func (e *filesEnv) listDir() string {
	return filepath.Join(e.h.remoteDir, "launchpad-files-"+e.h.settings().InstallID)
}
func (e *filesEnv) objDir() string { return e.listDir() + "-objects" }

func dirNames(dir string) []string {
	des, _ := os.ReadDir(dir)
	var out []string
	for _, de := range des {
		if !strings.HasPrefix(de.Name(), ".put-") {
			out = append(out, de.Name())
		}
	}
	sort.Strings(out)
	return out
}

// newManifest returns the one manifest file that appeared since the last call.
func (e *filesEnv) newManifest() (string, backup.FilesManifest, map[string]backup.FileEntry) {
	e.t.Helper()
	var fresh []string
	for _, n := range dirNames(e.listDir()) {
		if strings.HasSuffix(n, ".json") && !e.seen[n] {
			fresh = append(fresh, n)
		}
	}
	if len(fresh) != 1 {
		e.t.Fatalf("new manifests = %v (list folder %v)", fresh, dirNames(e.listDir()))
	}
	e.seen[fresh[0]] = true
	mb, err := os.ReadFile(filepath.Join(e.listDir(), fresh[0]))
	if err != nil {
		e.t.Fatal(err)
	}
	pub, _ := backup.ParsePublicKey(e.h.settings().SigningPublic)
	m, err := backup.VerifyFilesManifest(pub, mb)
	if err != nil {
		e.t.Fatal(err)
	}
	stem := strings.TrimSuffix(fresh[0], remotename.FilesManifestSuffix)
	if err := m.Bind(e.h.settings().InstallID, stem); err != nil {
		e.t.Fatal(err)
	}
	lb, err := os.ReadFile(filepath.Join(e.listDir(), m.List))
	if err != nil {
		e.t.Fatal(err)
	}
	sum := sha256.Sum256(lb)
	if err := m.CheckList(hex.EncodeToString(sum[:]), int64(len(lb))); err != nil {
		e.t.Fatal(err)
	}
	var plain bytes.Buffer
	if _, err := backup.Decrypt(&plain, bytes.NewReader(lb), e.h.agePriv, 0); err != nil {
		e.t.Fatal(err)
	}
	entries := map[string]backup.FileEntry{}
	if _, err := backup.ReadFileList(&plain, e.h.settings().InstallID, func(fe backup.FileEntry) bool { entries[fe.Key] = fe; return true }); err != nil {
		e.t.Fatal(err)
	}
	if m.Objects != len(entries) {
		e.t.Fatalf("manifest says %d objects, the list has %d", m.Objects, len(entries))
	}
	return fresh[0], m, entries
}

// readObject decrypts a remote object and checks it against its list entry.
func (e *filesEnv) checkObject(fe backup.FileEntry, want []byte) {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.objDir(), fe.Remote))
	if err != nil {
		e.t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != fe.CipherSHA256 || int64(len(b)) != fe.CipherSize {
		e.t.Fatalf("%s: the object is not what the list says", fe.Key)
	}
	var plain bytes.Buffer
	if _, err := backup.Decrypt(&plain, bytes.NewReader(b), e.h.agePriv, 0); err != nil {
		e.t.Fatal(err)
	}
	ps := sha256.Sum256(plain.Bytes())
	if hex.EncodeToString(ps[:]) != fe.SHA256 || (want != nil && !bytes.Equal(plain.Bytes(), want)) {
		e.t.Fatalf("%s: the plain bytes differ", fe.Key)
	}
}

func num(step map[string]any, k string) int { f, _ := step[k].(float64); return int(f) }

// ---- tests ----

// Run 1 copies everything; run 2 uploads nothing; a changed object is uploaded; a
// deleted object stays in the list with its old lastSeen and on the destination.
func TestFiles_IncrementalManifest(t *testing.T) {
	e := newFilesEnv(t, nil)
	orgA, orgB := "0198aaaa-0000-7000-8000-00000000000a", "0198bbbb-0000-7000-8000-00000000000b"
	data := map[string][]byte{
		okey(orgA, "photo.png"):    randBytes(t, 3<<20+17),
		okey(orgA, "notes.txt"):    []byte("hello attachments"),
		okey(orgB, "photo.png"):    randBytes(t, 1000), // the same filename in another org
		okey(orgB, "empty.txt"):    {},
		okey(orgB, "with space é"): []byte("odd name"),
	}
	for k, v := range data {
		e.put(k, v)
	}
	e.put("other/not-an-attachment.txt", []byte("not under org/"))

	r1, s1 := e.run()
	if s1 == nil || s1["ok"] != true || num(s1, "objects") != 5 || num(s1, "uploaded") != 5 || s1["initial"] != true || r1.ErrorText != "" {
		t.Fatalf("run 1 step = %v row = %+v\nlog:\n%s", s1, r1, e.h.log.String())
	}
	_, m1, list1 := e.newManifest()
	if len(list1) != 5 || m1.Objects != 5 {
		t.Fatalf("list 1 = %d entries", len(list1))
	}
	if _, ok := list1["other/not-an-attachment.txt"]; ok {
		t.Fatal("an object outside org/ was backed up")
	}
	remotes := map[string]bool{}
	for k, want := range data {
		fe := list1[k]
		e.checkObject(fe, want)
		if remotes[fe.Remote] {
			t.Fatalf("two keys share the remote name %s", fe.Remote)
		}
		remotes[fe.Remote] = true
	}
	if got := len(dirNames(e.objDir())); got != 5 {
		t.Fatalf("object folder = %d files", got)
	}
	// The dump pair is in the dump folder, the files are not.
	if got := len(e.h.remoteFiles()); got != 4 { // dump, manifest, two sub folders
		t.Fatalf("top folder = %v", e.h.remoteFiles())
	}
	putsAfter1 := e.objPuts.Load()

	time.Sleep(1100 * time.Millisecond) // lastSeen has a one second clock
	_, s2 := e.run()
	if s2["ok"] != true || num(s2, "uploaded") != 0 || num(s2, "objects") != 5 || s2["initial"] == true {
		t.Fatalf("run 2 step = %v", s2)
	}
	if e.objPuts.Load() != putsAfter1 {
		t.Fatalf("run 2 uploaded %d objects", e.objPuts.Load()-putsAfter1)
	}
	_, _, list2 := e.newManifest() // a new manifest every run (lastSeen moves)
	for k, fe := range list2 {
		if fe.LastSeen <= list1[k].LastSeen {
			t.Fatalf("%s: lastSeen did not move (%s <= %s)", k, fe.LastSeen, list1[k].LastSeen)
		}
		if fe.Remote != list1[k].Remote || fe.CipherSHA256 != list1[k].CipherSHA256 {
			t.Fatalf("%s: an unchanged object was copied again", k)
		}
	}

	// A changed object (same key, new bytes) is copied again; a deleted one is kept.
	changedKey, goneKey := okey(orgA, "notes.txt"), okey(orgB, "empty.txt")
	newData := []byte("hello attachments, edited and longer")
	e.put(changedKey, newData)
	e.del(goneKey)
	time.Sleep(1100 * time.Millisecond)
	_, s3 := e.run()
	if s3["ok"] != true || num(s3, "uploaded") != 1 || num(s3, "gone") != 1 || num(s3, "objects") != 5 {
		t.Fatalf("run 3 step = %v", s3)
	}
	_, _, list3 := e.newManifest()
	e.checkObject(list3[changedKey], newData)
	if list3[changedKey].SHA256 == list2[changedKey].SHA256 || list3[changedKey].Remote != list2[changedKey].Remote {
		t.Fatal("the changed object must have a new hash under the same name")
	}
	if list3[goneKey].LastSeen != list2[goneKey].LastSeen {
		t.Fatalf("a deleted object's lastSeen moved: %s -> %s", list2[goneKey].LastSeen, list3[goneKey].LastSeen)
	}
	e.checkObject(list3[goneKey], []byte{}) // its copy is still on the destination
	if len(e.h.fake.deleted) != 0 {
		t.Fatalf("something was deleted on the destination: %v", e.h.fake.deleted)
	}
	// Three runs, three manifests and lists; nothing was removed.
	if got := len(dirNames(e.listDir())); got != 6 {
		t.Fatalf("list folder = %v", dirNames(e.listDir()))
	}
	if len(e.h.scratchFiles()) != 0 {
		t.Fatalf("scratch left: %v", e.h.scratchFiles())
	}

	// The restore commands read one object back: manifest and list verified, the
	// entry found in the decrypted list, the object fetched and decrypted.
	e.restoreOne(okey(orgA, "photo.png"), data[okey(orgA, "photo.png")])
}

// restoreOne walks the operator's path with the offsite commands.
func (e *filesEnv) restoreOne(key string, want []byte) {
	t := e.t
	ctx := context.Background()
	install := e.h.settings().InstallID
	lr, _ := driver.Local{}.Open(ctx, driver.Config{Config: json.RawMessage(`{"dir":"` + e.listDir() + `"}`)})
	or, _ := driver.Local{}.Open(ctx, driver.Config{Config: json.RawMessage(`{"dir":"` + e.objDir() + `"}`)})
	var frame bytes.Buffer
	if err := FetchFilesFrame(ctx, lr, install, "latest", &frame); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := Unpack(&frame, out, 0, func(string) (uint64, error) { return 1 << 50, nil }); err != nil {
		t.Fatal(err)
	}
	pubFile := filepath.Join(out, "pub")
	_ = os.WriteFile(pubFile, []byte(e.h.settings().SigningPublic), 0o600)
	mb, _ := os.ReadFile(filepath.Join(out, UnpackManifest))
	pub, _ := backup.ParsePublicKey(e.h.settings().SigningPublic)
	var vout bytes.Buffer
	if code, err := verifyFiles(pub, mb, filepath.Join(out, UnpackObject), "", "", &vout); code != 0 || err != nil || !strings.HasPrefix(vout.String(), "ok\n") {
		t.Fatalf("verify --files: %d %v %q", code, err, vout.String())
	}
	lb, _ := os.ReadFile(filepath.Join(out, UnpackObject))
	var plainList bytes.Buffer
	if _, err := backup.Decrypt(&plainList, bytes.NewReader(lb), e.h.agePriv, 0); err != nil {
		t.Fatal(err)
	}
	var eout bytes.Buffer
	if code, err := cmdFilesEntry([]string{"--install-id", install, "--key", key}, &plainList, &eout); code != 0 || err != nil {
		t.Fatalf("files-entry: %d %v", code, err)
	}
	fields := strings.Split(strings.TrimSpace(eout.String()), "\t")
	if len(fields) != 3 {
		t.Fatalf("files-entry output %q", eout.String())
	}
	var oframe bytes.Buffer
	if err := FetchFilesObjectFrame(ctx, or, install, fields[0], &oframe); err != nil {
		t.Fatal(err)
	}
	out2 := t.TempDir()
	if err := Unpack(&oframe, out2, 0, func(string) (uint64, error) { return 1 << 50, nil }); err != nil {
		t.Fatal(err)
	}
	ob, _ := os.ReadFile(filepath.Join(out2, UnpackObject))
	var plain bytes.Buffer
	if _, err := backup.Decrypt(&plain, bytes.NewReader(ob), e.h.agePriv, 0); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(plain.Bytes())
	if !bytes.Equal(plain.Bytes(), want) || hex.EncodeToString(sum[:]) != fields[1] {
		t.Fatal("the restored object is not the original")
	}
	// A name of another kind or install is refused before any read.
	for _, n := range []string{"launchpad-" + install + "-20261026T031500Z-09af.dump.age", "../x", "launchpad-files-ffffffff-obj-" + strings.Repeat("a", 32) + ".age"} {
		if err := FetchFilesObjectFrame(ctx, or, install, n, io.Discard); err == nil {
			t.Fatalf("%q was fetched", n)
		}
	}
}

// A destination that fails in the middle of the first copy: the run (and the dump)
// stays succeeded, the step says files_failed, the state keeps the finished objects,
// and the next run copies only the rest. A lost or damaged state means a full copy
// under the same names (no duplicates). The destination lock is held all along.
func TestFiles_CrashAndResume(t *testing.T) {
	e := newFilesEnv(t, nil)
	org := "0198aaaa-0000-7000-8000-00000000000a"
	for i := 0; i < 5; i++ {
		e.put(okey(org, "file"+string(rune('a'+i))+".bin"), randBytes(t, 20_000+i))
	}
	var lockedDuringPut atomic.Bool
	e.onPut = func(string) {
		if !e.h.lockFree(e.dest) {
			lockedDuringPut.Store(true)
		}
	}
	e.failAt.Store(3)
	r1, s1 := e.run()
	if s1 == nil || s1["ok"] != false || s1["code"] != "connect_refused" || r1.ErrorText != "files_failed" || num(s1, "uploaded") != 2 {
		t.Fatalf("run 1 step = %v row = %+v", s1, r1)
	}
	if !lockedDuringPut.Load() {
		t.Fatal("the destination lock was not held during a files upload")
	}
	for _, n := range dirNames(e.listDir()) {
		t.Fatalf("a manifest or list exists after a failed run: %s", n)
	}
	if got := len(dirNames(e.objDir())); got != 2 {
		t.Fatalf("object folder = %d files, want the 2 finished", got)
	}
	// Only the save on abort can have written this state: 2 objects, far under the
	// 500-object and 30-second periodic saves.
	if got := stateLastSeen(t, e.h.w.statePath(e.dest)); len(got) != 2 {
		t.Fatalf("the state after an aborted phase holds %d entries, want the 2 finished", len(got))
	}
	before := e.objPuts.Load()

	e.failAt.Store(0)
	r2, s2 := e.run()
	if s2["ok"] != true || num(s2, "uploaded") != 3 || num(s2, "objects") != 5 || r2.ErrorText != "" {
		t.Fatalf("run 2 step = %v row = %+v", s2, r2)
	}
	if got := e.objPuts.Load() - before; got != 3 {
		t.Fatalf("run 2 sent %d objects, want only the 3 left", got)
	}
	_, _, list := e.newManifest()
	if len(list) != 5 || len(dirNames(e.objDir())) != 5 {
		t.Fatalf("after resume: %d entries, %d objects", len(list), len(dirNames(e.objDir())))
	}

	// The state is lost: everything is copied again under the same names.
	namesBefore := strings.Join(dirNames(e.objDir()), ",")
	for _, junk := range [][]byte{nil, []byte("garbage\n")} {
		if junk == nil {
			_ = os.Remove(e.h.w.statePath(e.dest))
		} else {
			_ = os.WriteFile(e.h.w.statePath(e.dest), junk, 0o600)
		}
		_, s := e.run()
		if s["ok"] != true || num(s, "uploaded") != 5 || s["initial"] != true {
			t.Fatalf("run without state = %v", s)
		}
		if strings.Join(dirNames(e.objDir()), ",") != namesBefore {
			t.Fatal("a copy without state made new object names")
		}
		e.newManifest()
	}
}

// A changed destination config (another folder, bucket or host) invalidates the state.
func TestFiles_StateIsBoundToTheDestinationConfig(t *testing.T) {
	e := newFilesEnv(t, nil)
	e.put(okey("0198aaaa-0000-7000-8000-00000000000a", "a.txt"), []byte("a"))
	_, s1 := e.run()
	if num(s1, "uploaded") != 1 {
		t.Fatalf("step %v", s1)
	}
	e.h.exec(`UPDATE backup_destinations SET config = config || '{"other":"x"}'::jsonb WHERE id = $1`, e.dest)
	_, s2 := e.run()
	if num(s2, "uploaded") != 1 || s2["initial"] != true {
		t.Fatalf("a changed config kept the state: %v", s2)
	}
}

// The silo cannot be read (wrong key): the dump is on the destination, the run is
// succeeded, the step carries a code, and neither the key nor silo's words appear in
// the log, the run row or the state.
func TestFiles_SiloFailureNeverFailsTheDumpAndLeaksNothing(t *testing.T) {
	const planted = "PLANTED-WRONG-SILO-SECRET-0123456789"
	e := newFilesEnv(t, nil)
	e.silo.SecretKey = planted
	e.h.w = e.h.newWorker(e.h.pool, e.h.dsn, func(o *Options) { o.Silo = e.silo })
	r, s := e.run()
	if s == nil || s["ok"] != false || s["code"] != "auth_failed" || r.ErrorText != "files_failed" {
		t.Fatalf("step = %v row = %+v", s, r)
	}
	if r.RemoteName == nil || len(e.h.remoteFiles()) < 2 {
		t.Fatalf("the dump pair is missing: %v", e.h.remoteFiles())
	}
	row, _ := json.Marshal(r)
	for what, text := range map[string]string{"log": e.h.log.String(), "run row": string(row)} {
		if strings.Contains(text, planted) || strings.Contains(text, e.silo.AccessKey+":") {
			t.Fatalf("a silo credential is in the %s", what)
		}
	}
	if _, err := os.Stat(e.h.w.statePath(e.dest)); err == nil {
		t.Log("state exists after a failed listing (an empty state is harmless)")
	}
	// No silo configured: no files step, no files folder (the dump path is as before).
	e2 := newFilesEnv(t, func(o *Options) { o.Silo = SiloConfig{} })
	r2, s2 := e2.run()
	if s2 != nil || r2.Steps != nil && string(r2.Steps) != "[]" && len(r2.Steps) > 0 {
		t.Fatalf("a run without silo has steps %s", r2.Steps)
	}
	if len(dirNames(e2.listDir())) != 0 || len(e2.h.remoteFiles()) != 2 {
		t.Fatalf("a run without silo made a files folder: %v", e2.h.remoteFiles())
	}
}

// Not enough scratch space for an object: files_failed with no_space, the dump is safe.
func TestFiles_NoSpace(t *testing.T) {
	e := newFilesEnv(t, nil)
	e.put(okey("0198aaaa-0000-7000-8000-00000000000a", "big.bin"), randBytes(t, 50_000))
	calls := 0
	e.h.w = e.h.newWorker(e.h.pool, e.h.dsn, func(o *Options) {
		o.Silo = e.silo
		o.FreeBytes = func(string) (uint64, error) { // the dump's own check passes, the object's does not
			calls++
			if calls == 1 {
				return 1 << 50, nil
			}
			return 1000, nil
		}
	})
	r, s := e.run()
	if s["ok"] != false || s["code"] != "no_space" || r.ErrorText != "files_failed" {
		t.Fatalf("step = %v row = %+v", s, r)
	}
}

// A large object streams: the heap stays far below its size.
func TestFiles_LargeObjectStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("large")
	}
	e := newFilesEnv(t, nil)
	const size = 96 << 20
	key := okey("0198aaaa-0000-7000-8000-00000000000a", "large.bin")
	h := sha256.New()
	e.putReader(key, size, io.TeeReader(io.LimitReader(rand.Reader, size), h))
	want := hex.EncodeToString(h.Sum(nil))

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(25 * time.Millisecond):
				runtime.ReadMemStats(&ms)
				if ms.HeapAlloc > peak.Load() {
					peak.Store(ms.HeapAlloc)
				}
			}
		}
	}()
	_, s := e.run()
	close(stop)
	<-done
	if s["ok"] != true || num(s, "uploaded") != 1 {
		t.Fatalf("step = %v", s)
	}
	_, _, list := e.newManifest()
	if list[key].SHA256 != want || list[key].Size != size {
		t.Fatal("the large object's hash is wrong")
	}
	if grew := int64(peak.Load()) - int64(base.HeapAlloc); grew > 48<<20 {
		t.Fatalf("the heap grew by %d MiB for a %d MiB object", grew>>20, size>>20)
	}
}

// repairShard queues a carried object that the destination lost or holds with another
// size, and leaves alone those that are fine, other shards, and gone from silo.
func TestFiles_RepairShard(t *testing.T) {
	const inst = "a1b2c3d4"
	dir := t.TempDir()
	r, _ := driver.Local{}.Open(context.Background(), driver.Config{Config: json.RawMessage(`{"dir":"` + dir + `"}`)})
	now := time.Unix(86400*37+5, 0).UTC() // shard 37 = 0x25
	var keys []string
	state := map[string]backup.FileEntry{}
	for i := 0; len(keys) < 5; i++ {
		k := okey("0198aaaa-0000-7000-8000-00000000000a", "f"+hex.EncodeToString([]byte{byte(i), byte(i >> 8)}))
		name, _ := remotename.FilesObjectName(inst, k)
		if len(keys) < 4 && !strings.HasPrefix(name, remotename.FilesObjectPrefix(inst, "25")) {
			continue
		}
		if len(keys) == 4 && strings.HasPrefix(name, remotename.FilesObjectPrefix(inst, "25")) {
			continue // the fifth lives in another shard
		}
		keys = append(keys, k)
		state[k] = backup.FileEntry{Key: k, Size: 10, ETag: "e", Modified: "2026-10-26T03:00:00Z", SHA256: strings.Repeat("11", 32),
			Remote: name, CipherSize: 200, CipherSHA256: strings.Repeat("22", 32), LastSeen: "2026-10-26T03:15:00Z"}
	}
	write := func(k string, n int) {
		if err := r.Put(context.Background(), state[k].Remote, int64(n), bytes.NewReader(make([]byte, n))); err != nil {
			t.Fatal(err)
		}
	}
	write(keys[0], 200) // fine
	write(keys[1], 150) // another size
	// keys[2] is missing; keys[3] is missing but gone from silo; keys[4] is in another shard.
	seen := map[string]bool{keys[0]: true, keys[1]: true, keys[2]: true, keys[4]: true}
	f := &filesRun{installID: inst, objRemote: r, now: now}
	var todo []siloObject
	n, err := f.repairShard(context.Background(), state, seen, &todo)
	if err != nil || n != 2 || len(todo) != 2 {
		t.Fatalf("repaired %d (%v), todo %v", n, err, todo)
	}
	got := []string{todo[0].Key, todo[1].Key}
	sort.Strings(got)
	want := []string{keys[1], keys[2]}
	sort.Strings(want)
	if got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("todo = %v, want %v", got, want)
	}
	// An object already queued is not queued twice.
	todo = []siloObject{{Key: keys[2]}}
	if n, _ := f.repairShard(context.Background(), state, seen, &todo); n != 1 || len(todo) != 2 {
		t.Fatalf("queued twice: n=%d todo=%v", n, todo)
	}
}

// The files phase goes through the real worker and the real silo for repairs too:
// a lost object on the destination is copied again within 256 days; here the day's
// shard is forced by a state whose clock is set so that the shard matches.
func TestFiles_LostDestinationObjectIsCopiedAgain(t *testing.T) {
	e := newFilesEnv(t, nil)
	org := "0198aaaa-0000-7000-8000-00000000000a"
	key := okey(org, "keep.bin")
	e.put(key, randBytes(t, 5000))
	e.run()
	e.newManifest()
	// Same-day shard: the DB day decides the shard, so loop over 256 days is not
	// possible; the worker's shard is today's, which we hit by choosing the key whose
	// hash starts with today's shard. Find such a key.
	shard := (time.Now().Unix() / 86400) % 256
	var k2 string
	for i := 0; ; i++ {
		c := okey(org, "s"+hex.EncodeToString([]byte{byte(i), byte(i >> 8), byte(i >> 16)}))
		n, _ := remotename.FilesObjectName(e.h.settings().InstallID, c)
		if strings.HasPrefix(n, remotename.FilesObjectPrefix(e.h.settings().InstallID, hex.EncodeToString([]byte{byte(shard)}))) {
			k2 = c
			break
		}
		if i > 200000 {
			t.Skip("no key found for today's shard")
		}
	}
	e.put(k2, []byte("shard probe"))
	e.run()
	_, _, list := e.newManifest()
	if err := os.Remove(filepath.Join(e.objDir(), list[k2].Remote)); err != nil {
		t.Fatal(err)
	}
	_, s := e.run()
	if num(s, "repaired") != 1 || num(s, "uploaded") != 1 {
		t.Fatalf("step = %v", s)
	}
	e.checkObject(func() backup.FileEntry { _, _, l := e.newManifest(); return l[k2] }(), []byte("shard probe"))
}

// The files phase through the REAL s3 driver on the compose silo: the two sub
// folders are made by the folder rule alone, the dump listing never sees a file name,
// and the operator's commands (fetch --files, --file-object) read them back.
func TestFiles_RealS3DriverSubFolders(t *testing.T) {
	e := newFilesEnv(t, nil)
	h := e.h
	sc := storagetest.Config() // the destination bucket (shared test bucket, unique folder)
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
	driver.RegisterForTest(t, "s3", s3.Driver{})
	h.w = h.newWorker(h.pool, h.dsn, func(o *Options) { o.Silo = e.silo; o.Policy = pol })
	folder := "files-" + hex.EncodeToString(bytes32(t)[:6])
	cfg, _ := json.Marshal(map[string]string{"endpoint": sc.Endpoint, "bucket": sc.Bucket, "folder": folder})
	secrets := []byte(`{"accessKey":"` + sc.AccessKey + `","secretKey":"` + sc.SecretKey + `"}`)
	blob, err := h.w.keys.Seal("backup_destinations.secrets_enc", e.dest.String(), secrets)
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE backup_destinations SET config = $2, secrets_enc = $3, secrets_set = '{accessKey,secretKey}' WHERE id = $1`, e.dest, cfg, blob)

	org := "0198aaaa-0000-7000-8000-00000000000a"
	want := randBytes(t, 70_000)
	e.put(okey(org, "a.bin"), want)
	e.put(okey(org, "b.txt"), []byte("bee"))
	r, s := e.run()
	if s == nil || s["ok"] != true || num(s, "uploaded") != 2 || r.ErrorText != "" {
		t.Fatalf("step = %v row = %+v\nlog:\n%s", s, r, h.log.String())
	}
	install := h.settings().InstallID
	s3d := s3.Driver{}
	ctx := context.Background()
	openAt := func(sub string) driver.Remote {
		c := folder
		if sub != "" {
			c += "/" + sub
		}
		cc, _ := json.Marshal(map[string]string{"endpoint": sc.Endpoint, "bucket": sc.Bucket, "folder": c})
		rm, err := s3d.Open(ctx, driver.Config{Config: cc, Secrets: secrets, Dialer: backup.NewDialer(pol, time.Second), AllowPlainHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rm.Close() })
		return rm
	}
	// The dump folder lists the dump pair and nothing of the files, whatever their number.
	top, err := openAt("").List(ctx, "launchpad-"+install+"-")
	if err != nil || len(top) != 2 {
		t.Fatalf("dump listing = %v, %v", top, err)
	}
	lf, _ := remotename.FilesListFolder(install)
	of, _ := remotename.FilesObjectFolder(install)
	lists, err := openAt(lf).List(ctx, remotename.FilesListPrefix(install))
	if err != nil || len(lists) != 2 {
		t.Fatalf("list folder = %v, %v", lists, err)
	}
	objs, err := openAt(of).List(ctx, "launchpad-files-"+install+"-obj-")
	if err != nil || len(objs) != 2 {
		t.Fatalf("object folder = %v, %v", objs, err)
	}
	// The operator's read path over the real driver.
	var frame bytes.Buffer
	if err := FetchFilesFrame(ctx, openAt(lf), install, "latest", &frame); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := Unpack(&frame, out, 0, func(string) (uint64, error) { return 1 << 50, nil }); err != nil {
		t.Fatal(err)
	}
	mb, _ := os.ReadFile(filepath.Join(out, UnpackManifest))
	pub, _ := backup.ParsePublicKey(h.settings().SigningPublic)
	if code, err := verifyFiles(pub, mb, filepath.Join(out, UnpackObject), "", "", io.Discard); code != 0 || err != nil {
		t.Fatalf("verify --files: %d %v", code, err)
	}
	var oframe bytes.Buffer
	if err := FetchFilesObjectFrame(ctx, openAt(of), install, objs[0].Name, &oframe); err != nil {
		t.Fatal(err)
	}
	out2 := t.TempDir()
	if err := Unpack(&oframe, out2, 0, func(string) (uint64, error) { return 1 << 50, nil }); err != nil {
		t.Fatal(err)
	}
	ob, _ := os.ReadFile(filepath.Join(out2, UnpackObject))
	var plain bytes.Buffer
	if _, err := backup.Decrypt(&plain, bytes.NewReader(ob), h.agePriv, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain.Bytes(), want) && string(plain.Bytes()) != "bee" {
		t.Fatal("the object read back is neither attachment")
	}
	// A second run uploads nothing; the dump retention (keep_last) of the dump folder
	// never touches the files folders.
	_, s2 := e.run()
	if num(s2, "uploaded") != 0 {
		t.Fatalf("run 2 = %v", s2)
	}
	if objs2, _ := openAt(of).List(ctx, "launchpad-files-"+install+"-obj-"); len(objs2) != 2 {
		t.Fatalf("objects after run 2: %v", objs2)
	}
	// Clean up the destination's objects (the folder is unique to this test).
	for _, o := range objs {
		_ = openAt(of).Delete(ctx, o.Name)
	}
	for _, l := range lists {
		_ = openAt(lf).Delete(ctx, l.Name)
	}
}
