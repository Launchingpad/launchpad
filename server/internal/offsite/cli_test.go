package offsite

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/backup/remotename"
)

type driverEntry = driver.Entry

func two(n int) string { return string([]byte{byte('0' + n/10), byte('0' + n%10)}) }

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func run(t *testing.T, stdin string, getenv func(string) string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := Main(args, strings.NewReader(stdin), &out, &errb, getenv)
	return rc, out.String(), errb.String()
}

func TestMain_UsageAndKeygen(t *testing.T) {
	if rc, _, e := run(t, "", env(nil), "nonsense"); rc != 2 || !strings.Contains(e, "usage") {
		t.Fatalf("unknown command: %d %q", rc, e)
	}
	rc, out, _ := run(t, "", env(nil), "keygen")
	if rc != 0 || !strings.Contains(out, "age1") || !strings.Contains(out, "AGE-SECRET-KEY-1") {
		t.Fatalf("keygen: %d %q", rc, out)
	}
	if rc, _, _ := run(t, "", env(nil), "keygen", "extra"); rc != 2 {
		t.Fatalf("keygen with an argument: %d", rc)
	}
}

func TestMain_Healthcheck(t *testing.T) {
	f := filepath.Join(t.TempDir(), "alive")
	e := env(map[string]string{"OFFSITE_HEALTH_FILE": f})
	if rc, _, _ := run(t, "", e, "healthcheck"); rc != 1 {
		t.Fatalf("no file: %d", rc)
	}
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if rc, _, _ := run(t, "", e, "healthcheck"); rc != 0 {
		t.Fatalf("fresh file: %d", rc)
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(f, old, old); err != nil {
		t.Fatal(err)
	}
	if rc, _, _ := run(t, "", e, "healthcheck"); rc != 1 {
		t.Fatalf("stale file: %d", rc)
	}
}

func TestMain_Resume(t *testing.T) {
	dir := t.TempDir()
	e := env(map[string]string{"OFFSITE_WORK_DIR": dir})
	id := uuid.New()
	if rc, _, msg := run(t, "", e, "resume", "--instance-id", id.String()); rc != 0 {
		t.Fatalf("resume: %d %q", rc, msg)
	}
	b, _ := os.ReadFile(filepath.Join(dir, InstanceFile))
	if strings.TrimSpace(string(b)) != id.String() {
		t.Fatalf("instance file = %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(dir, InstanceFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if rc, _, _ := run(t, "", e, "resume", "--instance-id", "not-a-uuid"); rc != 2 {
		t.Fatalf("bad id: %d", rc)
	}
	if rc, _, _ := run(t, "", e, "resume"); rc != 2 {
		t.Fatalf("no id: %d", rc)
	}
}

// signedPair makes a manifest and an object the way the worker does.
func signedPair(t *testing.T) (pubFile, manFile, objFile, stem string, m backup.Manifest) {
	t.Helper()
	dir := t.TempDir()
	pub, priv, err := backup.GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	obj := []byte("age-encryption.org/v1\nciphertext")
	sum := sha256.Sum256(obj)
	dt := time.Date(2026, 10, 26, 3, 15, 0, 0, time.UTC)
	stem, err = remotename.NewStem("0123abcd", dt, nil)
	if err != nil {
		t.Fatal(err)
	}
	m = backup.Manifest{Version: backup.ManifestVersion, InstallID: "0123abcd", Object: remotename.ObjectName(stem),
		SHA256: hex.EncodeToString(sum[:]), Size: int64(len(obj)), DumpTime: dt.Format(remotename.TimeLayout),
		SchemaVersion: 20, LaunchpadVersion: "v1"}
	signed, err := backup.Sign(priv, m)
	if err != nil {
		t.Fatal(err)
	}
	pubFile, manFile, objFile = filepath.Join(dir, "pub"), filepath.Join(dir, "m.json"), filepath.Join(dir, "obj")
	for f, b := range map[string][]byte{pubFile: []byte(backup.EncodePublicKey(pub) + "\n"), manFile: signed, objFile: obj} {
		if err := os.WriteFile(f, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func TestVerify_Command(t *testing.T) {
	pubFile, manFile, objFile, stem, _ := signedPair(t)
	rc, out, e := run(t, "", env(nil), "verify", "--public-key", pubFile, "--manifest", manFile, "--object", objFile,
		"--install-id", "0123abcd", "--stem", stem)
	if rc != 0 || !strings.Contains(out, "dumpTime: 20261026T031500Z") || !strings.Contains(out, "installId: 0123abcd") {
		t.Fatalf("good pair: %d %q %q", rc, out, e)
	}

	// A changed object.
	bad := filepath.Join(t.TempDir(), "obj")
	b, _ := os.ReadFile(objFile)
	b[len(b)-1] ^= 1
	_ = os.WriteFile(bad, b, 0o600)
	if rc, _, _ := run(t, "", env(nil), "verify", "--public-key", pubFile, "--manifest", manFile, "--object", bad); rc != 1 {
		t.Fatalf("changed object: %d", rc)
	}
	// A changed manifest (one byte of the envelope).
	mb, _ := os.ReadFile(manFile)
	mb2 := bytes.Replace(mb, []byte(`"signature":"`), []byte(`"signature":"AAAA`), 1)
	badMan := filepath.Join(t.TempDir(), "m.json")
	_ = os.WriteFile(badMan, mb2, 0o600)
	if rc, _, _ := run(t, "", env(nil), "verify", "--public-key", pubFile, "--manifest", badMan, "--object", objFile); rc != 1 {
		t.Fatalf("changed manifest: %d", rc)
	}
	// A bad signature: another key signed it.
	otherPub, _, _ := ed25519.GenerateKey(nil)
	wrong := filepath.Join(t.TempDir(), "pub")
	_ = os.WriteFile(wrong, []byte(backup.EncodePublicKey(otherPub)), 0o600)
	if rc, _, e := run(t, "", env(nil), "verify", "--public-key", wrong, "--manifest", manFile, "--object", objFile); rc != 1 ||
		!strings.Contains(e, "signature") {
		t.Fatalf("wrong key: %d %q", rc, e)
	}
	// A renamed pair: the stem is not the manifest's.
	other, _ := remotename.NewStem("0123abcd", time.Date(2026, 10, 25, 3, 15, 0, 0, time.UTC), nil)
	if rc, _, _ := run(t, "", env(nil), "verify", "--public-key", pubFile, "--manifest", manFile, "--object", objFile,
		"--install-id", "0123abcd", "--stem", other); rc != 1 {
		t.Fatalf("renamed pair: %d", rc)
	}
	if rc, _, _ := run(t, "", env(nil), "verify", "--public-key", pubFile, "--manifest", manFile, "--object", objFile,
		"--install-id", "ffffffff", "--stem", stem); rc != 1 {
		t.Fatalf("wrong install id: %d", rc)
	}
	// Usage.
	if rc, _, _ := run(t, "", env(nil), "verify", "--public-key", pubFile); rc != 2 {
		t.Fatalf("usage: %d", rc)
	}
	if rc, _, _ := run(t, "", env(nil), "verify", "--public-key", pubFile, "--manifest", manFile, "--object", objFile, "--stem", stem); rc != 2 {
		t.Fatalf("stem without install id: %d", rc)
	}
}

func TestDecrypt_Command(t *testing.T) {
	secret, public, err := backup.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	var ct bytes.Buffer
	if _, err := backup.Encrypt(&ct, strings.NewReader("the plain dump"), []string{public}); err != nil {
		t.Fatal(err)
	}
	idFile := filepath.Join(t.TempDir(), "id.txt")
	_ = os.WriteFile(idFile, []byte("# created: now\n# public key: "+public+"\n"+secret+"\n"), 0o600)
	rc, out, e := run(t, ct.String(), env(nil), "decrypt", "--identity", idFile)
	if rc != 0 || out != "the plain dump" {
		t.Fatalf("decrypt: %d %q %q", rc, out, e)
	}
	// A wrong key prints a fixed message and nothing on stdout.
	other, _, _ := backup.GenerateKeyPair()
	_ = os.WriteFile(idFile, []byte(other), 0o600)
	rc, out, e = run(t, ct.String(), env(nil), "decrypt", "--identity", idFile)
	if rc != 1 || out != "" || strings.Contains(e, other) {
		t.Fatalf("wrong key: %d %q %q", rc, out, e)
	}
}

func TestUnpack_Command(t *testing.T) {
	out := t.TempDir()
	rc, _, e := run(t, string(frame(1, 1, []byte("m"), 3, []byte("abc"), nil)), env(nil), "unpack", "--out", out, "--max-object-bytes", "100")
	// Real free space is large enough in a test directory, but the 1 GiB slack may not fit
	// a tiny tmpfs; accept the no-space refusal as the other correct answer.
	if rc != 0 && !strings.Contains(e, "free space") {
		t.Fatalf("unpack: %d %q", rc, e)
	}
	if rc, _, _ := run(t, "x", env(nil), "unpack"); rc != 2 {
		t.Fatalf("no --out: %d", rc)
	}
}

func TestReadConfigFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(p, []byte(`{"type":"s3","endpoint":"https://s3.example","bucket":"b","accessKey":"AK","secretKey":"SK"}`), 0o600)
	c, err := ReadConfigFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Type != "s3" || strings.Contains(string(c.Config), "AK") || strings.Contains(string(c.Config), "SK") ||
		!strings.Contains(string(c.Config), "endpoint") || !strings.Contains(string(c.Secrets), `"accessKey":"AK"`) {
		t.Fatalf("split wrong: %s | %s", c.Config, c.Secrets)
	}
	for _, bad := range []string{`not json`, `{}`, `{"type":"ftp"}`, `{"type":""}`, `[]`} {
		_ = os.WriteFile(p, []byte(bad), 0o600)
		if _, err := ReadConfigFile(p); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if _, err := ReadConfigFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing file was accepted")
	}
}

// pickStem: latest is the newest COMPLETE pair; a stem must be complete and belong to the install.
func TestPickStem(t *testing.T) {
	mk := func(day int, rnd string) string { return "launchpad-0123abcd-202610" + two(day) + "T031500Z-" + rnd }
	ents := func(names ...string) []driverEntry {
		var out []driverEntry
		for _, n := range names {
			out = append(out, driverEntry{Name: n, Size: 7})
		}
		return out
	}
	a, b, c := mk(24, "aaaa"), mk(25, "bbbb"), mk(26, "cccc")
	got, size, err := pickStem("0123abcd", "latest", ents(a+".dump.age", a+".json", b+".dump.age", b+".json", c+".dump.age"))
	if err != nil || got != b || size != 7 {
		t.Fatalf("latest skips the incomplete newest: %q %d %v", got, size, err)
	}
	if _, _, err := pickStem("0123abcd", c, ents(c+".dump.age")); err == nil {
		t.Fatal("an incomplete stem was picked")
	}
	if _, _, err := pickStem("0123abcd", "../../etc/passwd", ents(a+".dump.age", a+".json")); err == nil {
		t.Fatal("a path was accepted as a stem")
	}
	other := "launchpad-ffffffff-20261026T031500Z-aaaa"
	if _, _, err := pickStem("0123abcd", other, ents(other+".dump.age", other+".json")); err == nil {
		t.Fatal("another install's stem was accepted")
	}
	if _, _, err := pickStem("0123abcd", "latest", nil); err == nil {
		t.Fatal("latest of nothing")
	}
}
