package offsite

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func frame(version uint32, mlen uint64, manifest []byte, olen uint64, obj []byte, trailing []byte) []byte {
	var b bytes.Buffer
	var h [12]byte
	binary.BigEndian.PutUint32(h[:4], version)
	binary.BigEndian.PutUint64(h[4:], mlen)
	b.Write(h[:])
	b.Write(manifest)
	var l [8]byte
	binary.BigEndian.PutUint64(l[:], olen)
	b.Write(l[:])
	b.Write(obj)
	b.Write(trailing)
	return b.Bytes()
}

func plenty(string) (uint64, error) { return 1 << 60, nil }

func TestFrame_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, []byte(`{"m":1}`), 5, strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := Unpack(&buf, dir, 0, plenty); err != nil {
		t.Fatal(err)
	}
	m, _ := os.ReadFile(filepath.Join(dir, UnpackManifest))
	o, _ := os.ReadFile(filepath.Join(dir, UnpackObject))
	if string(m) != `{"m":1}` || string(o) != "hello" {
		t.Fatalf("got %q %q", m, o)
	}
	des, _ := os.ReadDir(dir)
	if len(des) != 2 {
		t.Fatalf("unpack wrote %d files, want 2 fixed names", len(des))
	}
	for _, de := range des {
		fi, _ := de.Info()
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v", de.Name(), fi.Mode())
		}
	}
}

func TestWriteFrame_RefusesALyingSource(t *testing.T) {
	if err := WriteFrame(io.Discard, nil, 10, strings.NewReader("short")); !errors.Is(err, ErrFrameShort) {
		t.Fatalf("short source: %v", err)
	}
	if err := WriteFrame(io.Discard, nil, 3, strings.NewReader("longer")); !errors.Is(err, ErrFrameTrailing) {
		t.Fatalf("long source: %v", err)
	}
	if err := WriteFrame(io.Discard, nil, MaxObjectBytes+1, strings.NewReader("")); !errors.Is(err, ErrFrameObject) {
		t.Fatalf("huge declared length: %v", err)
	}
	if err := WriteFrame(io.Discard, make([]byte, 1<<20+1), 0, strings.NewReader("")); err == nil {
		t.Fatal("a manifest above 1 MiB was written")
	}
}

// Every hostile frame of decisions-T16 #39: huge and negative lengths, short and
// long bodies, extra bytes, a wrong version, a manifest above 1 MiB.
func TestUnpack_HostileFrames(t *testing.T) {
	obj := []byte("0123456789")
	cases := []struct {
		name string
		in   []byte
		max  int64
		free func(string) (uint64, error)
		want error
	}{
		{"empty stream", nil, 0, plenty, ErrFrameShort},
		{"short header", []byte{0, 0, 0, 1, 0}, 0, plenty, ErrFrameShort},
		{"wrong version", frame(2, 1, []byte("m"), 10, obj, nil), 0, plenty, ErrFrameVersion},
		{"version zero", frame(0, 1, []byte("m"), 10, obj, nil), 0, plenty, ErrFrameVersion},
		{"manifest length huge", frame(1, 1<<62, []byte("m"), 10, obj, nil), 0, plenty, ErrFrameManifest},
		{"manifest length negative as int64", frame(1, 1<<63, []byte("m"), 10, obj, nil), 0, plenty, ErrFrameManifest},
		{"manifest length max uint64", frame(1, ^uint64(0), []byte("m"), 10, obj, nil), 0, plenty, ErrFrameManifest},
		{"manifest above 1 MiB", frame(1, 1<<20+1, make([]byte, 1<<20+1), 10, obj, nil), 0, plenty, ErrFrameManifest},
		{"manifest body short", frame(1, 100, []byte("m"), 0, nil, nil), 0, plenty, ErrFrameShort},
		{"object length negative as int64", frame(1, 1, []byte("m"), 1<<63, obj, nil), 0, plenty, ErrFrameObject},
		{"object length max uint64", frame(1, 1, []byte("m"), ^uint64(0), obj, nil), 0, plenty, ErrFrameObject},
		{"object above the cap", frame(1, 1, []byte("m"), 11, obj, nil), 10, plenty, ErrFrameObject},
		{"object above 5 GiB", frame(1, 1, []byte("m"), uint64(MaxObjectBytes)+1, nil, nil), 0, plenty, ErrFrameObject},
		{"object body short", frame(1, 1, []byte("m"), 20, obj, nil), 0, plenty, ErrFrameShort},
		{"object body long", frame(1, 1, []byte("m"), 5, obj, nil), 0, plenty, ErrFrameTrailing},
		{"trailing byte", frame(1, 1, []byte("m"), 10, obj, []byte("x")), 0, plenty, ErrFrameTrailing},
		{"no room for the object", frame(1, 1, []byte("m"), 10, obj, nil), 0,
			func(string) (uint64, error) { return 10 + 1<<30 - 1, nil }, ErrFrameNoSpace},
		{"free space unknown", frame(1, 1, []byte("m"), 10, obj, nil), 0,
			func(string) (uint64, error) { return 0, errors.New("x") }, ErrFrameNoSpace},
	}
	for _, c := range cases {
		dir := t.TempDir()
		err := Unpack(bytes.NewReader(c.in), dir, c.max, c.free)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
		if des, _ := os.ReadDir(dir); len(des) != 0 {
			t.Errorf("%s: left %d files behind", c.name, len(des))
		}
	}
	// And the good one, to show the table is not all failures.
	dir := t.TempDir()
	if err := Unpack(bytes.NewReader(frame(1, 1, []byte("m"), 10, obj, nil)), dir, 0, plenty); err != nil {
		t.Fatal(err)
	}
}

// A hostile stream cannot choose a file name: the frame has no field for one, and
// unpack never overwrites (the output files are created new).
func TestUnpack_NeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, UnpackObject), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Unpack(bytes.NewReader(frame(1, 1, []byte("m"), 1, []byte("x"), nil)), dir, 0, plenty)
	if err == nil {
		t.Fatal("unpack overwrote an existing file")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, UnpackObject)); string(b) != "mine" {
		t.Fatalf("the existing file changed: %q", b)
	}
}
