package move

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestGeneratePassphrase(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		p, err := GeneratePassphrase()
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 39 || strings.Count(p, "-") != 7 || len(p) < minPassphraseLen {
			t.Fatalf("shape: %q", p)
		}
		if seen[p] {
			t.Fatal("repeated passphrase")
		}
		seen[p] = true
		if NormalizePassphrase(strings.ToLower(" "+p+"\n")) != p {
			t.Fatalf("normalize: %q", p)
		}
	}
}

func TestCopyTextEscapeRoundTrip(t *testing.T) {
	for _, in := range [][]byte{
		[]byte("plain"), []byte("a\tb\nc\\d\re"), {0, 1, 2, 255, '\\', '\n'}, []byte(`\x41`), nil,
	} {
		out := unescapeCopy(escapeCopy(in))
		if !bytes.Equal(out, in) && !(len(in) == 0 && len(out) == 0) {
			t.Errorf("round trip of %q gave %q", in, out)
		}
	}
	if got := unescapeCopy([]byte(`\\x6162`)); string(got) != `\x6162` {
		t.Errorf("escaped bytea prefix: %q", got)
	}
	if got := unescapeCopy([]byte(`a\101\x42`)); string(got) != "aAB" {
		t.Errorf("octal and hex: %q", got)
	}
}

func TestByteaField(t *testing.T) {
	raw := byteaToField([]byte{0xde, 0xad, 0xbe, 0xef})
	if string(raw) != `\\xdeadbeef` {
		t.Fatalf("field: %s", raw)
	}
	b, err := byteaFromField(raw)
	if err != nil || !bytes.Equal(b, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("back: %x %v", b, err)
	}
}

func TestRowFieldCountMismatch(t *testing.T) {
	cols := colIndex([]string{"a", "b"})
	if _, err := newRow(cols, []byte("x")); !errors.Is(err, ErrBundleCorrupt) {
		t.Fatalf("got %v", err)
	}
	r, err := newRow(cols, []byte("x\t\\N"))
	if err != nil || !r.isNull("b") || r.isNull("a") {
		t.Fatalf("row: %v", err)
	}
	r.setText("b", "t\tv")
	if string(r.bytes()) != "x\tt\\tv" {
		t.Fatalf("bytes: %q", r.bytes())
	}
}

func TestFramesRoundTripAndLimits(t *testing.T) {
	var buf bytes.Buffer
	fw := &frameWriter{w: &buf}
	if err := fw.writeJSON(frameTable, tableHeader{Name: "x", Columns: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if err := fw.write(frameEnd, nil); err != nil {
		t.Fatal(err)
	}
	fr := &frameReader{r: bytes.NewReader(buf.Bytes())}
	var h tableHeader
	if err := fr.expectJSON(frameTable, &h); err != nil || h.Name != "x" {
		t.Fatalf("table: %v", err)
	}
	if _, err := fr.expect(frameData); !errors.Is(err, ErrBundleCorrupt) {
		t.Fatalf("wrong type: %v", err)
	}
	// A cut stream and an absurd length are corrupt, not a panic or a huge allocation.
	cut := &frameReader{r: bytes.NewReader(buf.Bytes()[:3])}
	if _, _, err := cut.read(); !errors.Is(err, ErrBundleCorrupt) {
		t.Fatalf("cut: %v", err)
	}
	huge := &frameReader{r: bytes.NewReader([]byte{'D', 0xff, 0xff, 0xff, 0xff})}
	if _, _, err := huge.read(); !errors.Is(err, ErrBundleCorrupt) {
		t.Fatalf("huge: %v", err)
	}
}
