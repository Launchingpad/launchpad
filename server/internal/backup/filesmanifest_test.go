package backup

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"devops/internal/backup/remotename"
)

const (
	fStem = "launchpad-files-a1b2c3d4-list-20261026T031500Z-09af"
	fKey  = "org/0198aaaa-0000-7000-8000-000000000001/2026/0198bbbb-0000-7000-8000-000000000002/photo.png"
)

func goodFilesManifest() FilesManifest {
	return FilesManifest{
		Version: FilesManifestVersion, InstallID: mInst, List: fStem + ".list.age",
		SHA256: strings.Repeat("cd", 32), Size: 999, Time: "20261026T031500Z",
		Objects: 3, Bytes: 4096, LaunchpadVersion: "1.4.2",
	}
}

func goodEntry(t *testing.T, key string) FileEntry {
	t.Helper()
	name, err := remotename.FilesObjectName(mInst, key)
	if err != nil {
		t.Fatal(err)
	}
	return FileEntry{Key: key, Size: 10, ETag: "abc", Modified: "2026-10-26T03:00:00Z", SHA256: strings.Repeat("11", 32),
		Remote: name, CipherSize: 200, CipherSHA256: strings.Repeat("22", 32), LastSeen: "2026-10-26T03:15:00Z"}
}

func TestFilesManifestRoundTripAndBinding(t *testing.T) {
	pub, priv := newKey(t)
	file, err := SignFiles(priv, goodFilesManifest())
	if err != nil {
		t.Fatal(err)
	}
	m, err := VerifyFilesManifest(pub, file)
	if err != nil || m != goodFilesManifest() {
		t.Fatalf("round trip: %v %+v", err, m)
	}
	if err := m.Bind(mInst, fStem); err != nil {
		t.Fatal(err)
	}
	// Another install, another stem, a stem with another time: all refused.
	for _, c := range []struct{ inst, stem string }{
		{"ffffffff", fStem},
		{mInst, "launchpad-files-a1b2c3d4-list-20261026T031500Z-aaaa"},
		{mInst, "launchpad-files-a1b2c3d4-list-20260101T000000Z-09af"},
	} {
		if err := m.Bind(c.inst, c.stem); !errors.Is(err, ErrManifestBinding) {
			t.Fatalf("Bind(%v) = %v", c, err)
		}
	}
	if err := m.CheckList(strings.Repeat("cd", 32), 999); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckList(strings.Repeat("cd", 32), 1000); !errors.Is(err, ErrObjectMismatch) {
		t.Fatal("a list of another size passed")
	}
	if err := m.CheckList(strings.Repeat("ce", 32), 999); !errors.Is(err, ErrObjectMismatch) {
		t.Fatal("a list with another hash passed")
	}
}

// A tampered payload, a wrong key, a bad envelope and a dump manifest all fail.
func TestFilesManifestTamperAndDomain(t *testing.T) {
	pub, priv := newKey(t)
	otherPub, otherPriv := newKey(t)
	file, _ := SignFiles(priv, goodFilesManifest())

	if _, err := VerifyFilesManifest(otherPub, file); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("wrong key: %v", err)
	}
	if f2, _ := SignFiles(otherPriv, goodFilesManifest()); true {
		if _, err := VerifyFilesManifest(pub, f2); !errors.Is(err, ErrManifestSignature) {
			t.Fatalf("signed by another key: %v", err)
		}
	}
	// Change one payload field, keep the old signature.
	var env envelope
	_ = json.Unmarshal(file, &env)
	raw, _ := base64.StdEncoding.DecodeString(env.Payload)
	changed := bytes.Replace(raw, []byte(`"objects":3`), []byte(`"objects":2`), 1)
	if bytes.Equal(raw, changed) {
		t.Fatal("test bug: nothing changed")
	}
	env.Payload = base64.StdEncoding.EncodeToString(changed)
	tampered, _ := json.Marshal(env)
	if _, err := VerifyFilesManifest(pub, tampered); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("tampered payload: %v", err)
	}
	// Re-signed with the right key but a bad field: format error.
	bad := goodFilesManifest()
	bad.List = "launchpad-files-a1b2c3d4-list-20261026T031500Z-09af.json" // a manifest name, not a list
	if _, err := SignFiles(priv, bad); !errors.Is(err, ErrManifestFormat) {
		t.Fatalf("bad list name signed: %v", err)
	}
	// Domain separation both ways.
	dump, _ := Sign(priv, goodManifest())
	if _, err := VerifyFilesManifest(pub, dump); err == nil {
		t.Fatal("a dump manifest passed as a files manifest")
	}
	if _, err := VerifyManifest(pub, file); err == nil {
		t.Fatal("a files manifest passed as a dump manifest")
	}
	// The signature itself is bound to the domain: a payload that is valid for the
	// other kind, signed over the other domain, is refused as a bad signature.
	fp, _ := json.Marshal(goodFilesManifest())
	crossFiles, _ := json.Marshal(envelope{Payload: base64.StdEncoding.EncodeToString(fp),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, signedBytes(fp)))}) // the dump domain
	if _, err := VerifyFilesManifest(pub, crossFiles); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("a files payload signed in the dump domain: %v", err)
	}
	dp, _ := json.Marshal(goodManifest())
	crossDump, _ := json.Marshal(envelope{Payload: base64.StdEncoding.EncodeToString(dp),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, filesSignedBytes(dp)))}) // the files domain
	if _, err := VerifyManifest(pub, crossDump); !errors.Is(err, ErrManifestSignature) {
		t.Fatalf("a dump payload signed in the files domain: %v", err)
	}
	// A key of the wrong size, a long file, junk.
	if _, err := VerifyFilesManifest(ed25519.PublicKey("short"), file); !errors.Is(err, ErrBadKey) {
		t.Fatal("short key")
	}
	if _, err := VerifyFilesManifest(pub, bytes.Repeat([]byte("x"), MaxManifestBytes+1)); !errors.Is(err, ErrManifestTooLarge) {
		t.Fatal("long file")
	}
	if _, err := VerifyFilesManifest(pub, []byte("not json")); !errors.Is(err, ErrManifestFormat) {
		t.Fatal("junk")
	}
}

func TestFileListRoundTripAndRefusals(t *testing.T) {
	var buf bytes.Buffer
	e1, e2 := goodEntry(t, fKey), goodEntry(t, strings.Replace(fKey, "photo.png", "other file.pdf", 1))
	for _, e := range []FileEntry{e1, e2} {
		if err := EncodeFileEntry(&buf, e); err != nil {
			t.Fatal(err)
		}
	}
	good := buf.String()
	var got []string
	n, err := ReadFileList(strings.NewReader(good), mInst, func(e FileEntry) bool { got = append(got, e.Key); return true })
	if err != nil || n != 2 || len(got) != 2 {
		t.Fatalf("read: %d %v %v", n, err, got)
	}
	// Another install: the remote name does not match its key.
	if _, err := ReadFileList(strings.NewReader(good), "ffffffff", func(FileEntry) bool { return true }); !errors.Is(err, ErrFileList) {
		t.Fatalf("another install: %v", err)
	}
	lines := strings.SplitAfter(good, "\n")
	for name, in := range map[string]string{
		"truncated line":   good[:len(good)-5],
		"no final newline": strings.TrimSuffix(good, "\n"),
		"duplicate key":    lines[0] + lines[0],
		"empty line":       lines[0] + "\n" + lines[1],
		"unknown field":    strings.Replace(lines[0], `"key"`, `"extra":1,"key"`, 1),
		"junk":             "hello\n",
		"remote swapped":   strings.Replace(lines[0], e1.Remote, e2.Remote, 1),
		"bad hash":         strings.Replace(lines[0], strings.Repeat("11", 32), "zz", 1),
		"bad time":         strings.Replace(lines[0], "2026-10-26T03:15:00Z", "yesterday", 1),
		"long line":        strings.Repeat("a", maxFileLineBytes+10) + "\n",
	} {
		if _, err := ReadFileList(strings.NewReader(in), mInst, func(FileEntry) bool { return true }); !errors.Is(err, ErrFileList) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Keys the list must refuse.
	for name, key := range map[string]string{
		"empty": "", "control": "org/a\nb", "invalid utf8": "org/\xff", "too long": "org/" + strings.Repeat("a", MaxFileKeyBytes),
	} {
		e := goodEntry(t, fKey)
		e.Key = key
		if e.Validate(mInst) == nil {
			t.Errorf("key %q passed", name)
		}
	}
	// fn returning false stops early without an error.
	c := 0
	if _, err := ReadFileList(strings.NewReader(good), mInst, func(FileEntry) bool { c++; return false }); err != nil || c != 1 {
		t.Fatalf("early stop: %d %v", c, err)
	}
}
