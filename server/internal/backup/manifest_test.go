package backup

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	mInst = "a1b2c3d4"
	mStem = "launchpad-a1b2c3d4-20261026T031500Z-09af"
)

func goodManifest() Manifest {
	return Manifest{
		Version: ManifestVersion, InstallID: mInst, Object: mStem + ".dump.age",
		SHA256: strings.Repeat("ab", 32), Size: 12345, DumpTime: "20261026T031500Z",
		SchemaVersion: 19, LaunchpadVersion: "1.4.2",
	}
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func reEnvelope(t *testing.T, file []byte, edit func(payload map[string]any), keepSig bool, priv ed25519.PrivateKey) []byte {
	t.Helper()
	var env envelope
	json.Unmarshal(file, &env)
	raw, _ := base64.StdEncoding.DecodeString(env.Payload)
	var m map[string]any
	json.Unmarshal(raw, &m)
	edit(m)
	np, _ := json.Marshal(m)
	env.Payload = base64.StdEncoding.EncodeToString(np)
	if !keepSig {
		env.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, signedBytes(np)))
	}
	out, _ := json.Marshal(env)
	return out
}

func TestManifestRoundTrip(t *testing.T) {
	pub, priv := newKey(t)
	file, err := Sign(priv, goodManifest())
	if err != nil {
		t.Fatal(err)
	}
	m, err := VerifyManifest(pub, file)
	if err != nil || m != goodManifest() {
		t.Fatalf("verify: %+v, %v", m, err)
	}
	if err := m.Bind(mInst, mStem); err != nil {
		t.Errorf("bind: %v", err)
	}
	if err := m.CheckObject(strings.Repeat("ab", 32), 12345); err != nil {
		t.Errorf("object: %v", err)
	}
	// The file holds no host, URL or secret field.
	var raw map[string]any
	json.Unmarshal(file, &raw)
	if len(raw) != 2 {
		t.Errorf("envelope fields: %v", raw)
	}
	pk, err := ParsePublicKey(" " + EncodePublicKey(pub) + "\n")
	if err != nil || !pk.Equal(pub) {
		t.Errorf("public key text: %v", err)
	}
}

func TestManifestRejected(t *testing.T) {
	pub, priv := newKey(t)
	otherPub, otherPriv := newKey(t)
	file, _ := Sign(priv, goodManifest())
	var env envelope
	json.Unmarshal(file, &env)

	tamperedSig := func() []byte {
		s, _ := base64.StdEncoding.DecodeString(env.Signature)
		s[0] ^= 1
		e := env
		e.Signature = base64.StdEncoding.EncodeToString(s)
		b, _ := json.Marshal(e)
		return b
	}()
	otherFile, _ := Sign(otherPriv, goodManifest())
	_ = otherPub

	big := append([]byte(nil), file...)
	big = append(big, bytes.Repeat([]byte(" "), MaxManifestBytes)...)

	cases := []struct {
		name string
		file []byte
		want error
	}{
		{"signed by another key", otherFile, ErrManifestSignature},
		{"signature changed", tamperedSig, ErrManifestSignature},
		{"sha changed, old signature", reEnvelope(t, file, func(m map[string]any) { m["sha256"] = strings.Repeat("cd", 32) }, true, nil), ErrManifestSignature},
		{"size changed, old signature", reEnvelope(t, file, func(m map[string]any) { m["size"] = 1 }, true, nil), ErrManifestSignature},
		{"object renamed, old signature", reEnvelope(t, file, func(m map[string]any) { m["object"] = "launchpad-a1b2c3d4-20261026T031500Z-ffff.dump.age" }, true, nil), ErrManifestSignature},
		{"signature empty", func() []byte { e := env; e.Signature = ""; b, _ := json.Marshal(e); return b }(), ErrManifestFormat},
		{"signature short", func() []byte {
			e := env
			e.Signature = base64.StdEncoding.EncodeToString([]byte("short"))
			b, _ := json.Marshal(e)
			return b
		}(), ErrManifestFormat},
		{"payload not base64", []byte(`{"payload":"@@@","signature":"AA=="}`), ErrManifestFormat},
		{"unknown envelope field", []byte(strings.Replace(string(file), `{"payload"`, `{"extra":1,"payload"`, 1)), ErrManifestFormat},
		{"trailing bytes", append(append([]byte(nil), file...), []byte(` {}`)...), ErrManifestFormat},
		{"not json", []byte("nope"), ErrManifestFormat},
		{"empty", nil, ErrManifestFormat},
		{"oversize", big, ErrManifestTooLarge},
		// Correctly signed but invalid payloads (a bug or a hostile signer must still be refused).
		{"signed: unknown field", reEnvelope(t, file, func(m map[string]any) { m["host"] = "evil" }, false, priv), ErrManifestFormat},
		{"signed: version 2", reEnvelope(t, file, func(m map[string]any) { m["version"] = 2 }, false, priv), ErrManifestFormat},
		{"signed: object with a path", reEnvelope(t, file, func(m map[string]any) { m["object"] = "../" + mStem + ".dump.age" }, false, priv), ErrManifestFormat},
		{"signed: object is a manifest name", reEnvelope(t, file, func(m map[string]any) { m["object"] = mStem + ".json" }, false, priv), ErrManifestFormat},
		{"signed: other install in the name", reEnvelope(t, file, func(m map[string]any) { m["installId"] = "ffffffff" }, false, priv), ErrManifestFormat},
		{"signed: sha upper case", reEnvelope(t, file, func(m map[string]any) { m["sha256"] = strings.Repeat("AB", 32) }, false, priv), ErrManifestFormat},
		{"signed: sha short", reEnvelope(t, file, func(m map[string]any) { m["sha256"] = "ab" }, false, priv), ErrManifestFormat},
		{"signed: negative size", reEnvelope(t, file, func(m map[string]any) { m["size"] = -1 }, false, priv), ErrManifestFormat},
		{"signed: dump time differs from the name", reEnvelope(t, file, func(m map[string]any) { m["dumpTime"] = "20261026T031501Z" }, false, priv), ErrManifestFormat},
		{"signed: dump time not UTC form", reEnvelope(t, file, func(m map[string]any) { m["dumpTime"] = "2026-10-26T03:15:00Z" }, false, priv), ErrManifestFormat},
		{"signed: version string with a newline", reEnvelope(t, file, func(m map[string]any) { m["launchpadVersion"] = "1.0\nx" }, false, priv), ErrManifestFormat},
		{"signed: string where a number belongs", reEnvelope(t, file, func(m map[string]any) { m["size"] = "12" }, false, priv), ErrManifestFormat},
	}
	for _, tc := range cases {
		if _, err := VerifyManifest(pub, tc.file); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, err := VerifyManifest(pub[:10], file); !errors.Is(err, ErrBadKey) {
		t.Error("short key accepted")
	}
	for _, bad := range []string{"", "!!!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := ParsePublicKey(bad); !errors.Is(err, ErrBadKey) {
			t.Errorf("ParsePublicKey(%q) = %v", bad, err)
		}
	}
}

// A signature made for another purpose with the same key does not verify (domain separation).
func TestManifestDomainSeparation(t *testing.T) {
	pub, priv := newKey(t)
	payload, _ := json.Marshal(goodManifest())
	env, _ := json.Marshal(envelope{
		Payload:   base64.StdEncoding.EncodeToString(payload),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload)), // no domain prefix
	})
	if _, err := VerifyManifest(pub, env); !errors.Is(err, ErrManifestSignature) {
		t.Errorf("unprefixed signature accepted: %v", err)
	}
}

func TestSignRefusesBadManifests(t *testing.T) {
	_, priv := newKey(t)
	for name, edit := range map[string]func(*Manifest){
		"version":   func(m *Manifest) { m.Version = 0 },
		"install":   func(m *Manifest) { m.InstallID = "x" },
		"object":    func(m *Manifest) { m.Object = "x.dump.age" },
		"sha":       func(m *Manifest) { m.SHA256 = "" },
		"size":      func(m *Manifest) { m.Size = -1 },
		"time":      func(m *Manifest) { m.DumpTime = "20261027T031500Z" },
		"launchpad": func(m *Manifest) { m.LaunchpadVersion = "" },
		"schema":    func(m *Manifest) { m.SchemaVersion = -1 },
	} {
		m := goodManifest()
		edit(&m)
		if _, err := Sign(priv, m); err == nil {
			t.Errorf("%s: signed", name)
		}
	}
	if _, err := Sign(priv[:5], goodManifest()); !errors.Is(err, ErrBadKey) {
		t.Error("short private key accepted")
	}
}

// Replayed, swapped and reordered manifests (T16 #22, #39).
func TestManifestBinding(t *testing.T) {
	pub, priv := newKey(t)
	m := goodManifest()
	oldStem := "launchpad-a1b2c3d4-20261020T031500Z-1111"
	old := m
	old.Object, old.DumpTime = oldStem+".dump.age", "20261020T031500Z"
	oldFile, _ := Sign(priv, old)
	newFile, _ := Sign(priv, m)

	vOld, err := VerifyManifest(pub, oldFile)
	if err != nil {
		t.Fatal(err)
	}
	vNew, _ := VerifyManifest(pub, newFile)
	for _, tc := range []struct {
		name    string
		m       Manifest
		install string
		stem    string
		ok      bool
	}{
		{"matching", vNew, mInst, mStem, true},
		{"old manifest replayed for the newest stem", vOld, mInst, mStem, false},
		{"new manifest swapped onto the old stem", vNew, mInst, oldStem, false},
		{"other install", vNew, "ffffffff", mStem, false},
		{"same time, other random part", vNew, mInst, "launchpad-a1b2c3d4-20261026T031500Z-0000", false},
		{"stem of another install", vNew, mInst, "launchpad-ffffffff-20261026T031500Z-09af", false},
		{"stem that is a path", vNew, mInst, "../" + mStem, false},
		{"empty stem", vNew, mInst, "", false},
		{"bad install id", vNew, "..", mStem, false},
	} {
		if err := tc.m.Bind(tc.install, tc.stem); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// A pair whose object was changed: sha or size differs.
	for _, tc := range []struct {
		sha  string
		size int64
		ok   bool
	}{
		{vNew.SHA256, vNew.Size, true},
		{strings.Repeat("00", 32), vNew.Size, false},
		{vNew.SHA256, vNew.Size + 1, false},
		{vNew.SHA256, 0, false},
		{"", vNew.Size, false},
		{strings.ToUpper(vNew.SHA256), vNew.Size, false},
	} {
		if err := vNew.CheckObject(tc.sha, tc.size); (err == nil) != tc.ok {
			t.Errorf("CheckObject(%q,%d) = %v", tc.sha, tc.size, err)
		}
	}
	// The signature does not prove freshness: the old pair still verifies by itself.
	if vOld.Bind(mInst, oldStem) != nil {
		t.Error("an old, correctly signed pair must verify on its own (restore.sh prints its age)")
	}
	_ = time.Now
}
