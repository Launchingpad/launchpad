package remotename

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const inst = "a1b2c3d4"

func TestParse(t *testing.T) {
	m, err := NewMatcher(inst)
	if err != nil {
		t.Fatal(err)
	}
	good := "launchpad-a1b2c3d4-20261026T031500Z-09af"
	for _, tc := range []struct {
		name string
		ok   bool
		kind Kind
	}{
		{good + ".dump.age", true, KindObject},
		{good + ".json", true, KindManifest},
		{good + ".dump", false, 0},
		{good + ".JSON", false, 0},
		{good + ".json\n", false, 0},
		{" " + good + ".json", false, 0},
		{"launchpad-a1b2c3d5-20261026T031500Z-09af.json", false, 0},  // other install
		{"launchpad-a1b2c3d4-20261326T031500Z-09af.json", false, 0},  // month 13
		{"launchpad-a1b2c3d4-20261026T251500Z-09af.json", false, 0},  // hour 25
		{"launchpad-a1b2c3d4-20261026T031500Z-09AF.json", false, 0},  // upper-case hex
		{"launchpad-a1b2c3d4-20261026T031500Z-09a.json", false, 0},   // 3 hex
		{"launchpad-a1b2c3d4-20261026T031500Z-09af1.json", false, 0}, // 5 hex
		{"../" + good + ".json", false, 0},                           // traversal
		{"sub/" + good + ".json", false, 0},                          // path
		{good + "/../x.json", false, 0},                              // path
		{"launchpad-a1b2c3d4-test-0123abcd.tmp", false, 0},           // test file
		{"launchpad-a1b2c3d4-20261026T031500Z-09af.dump.age.tmp", false, 0},
		{"", false, 0},
	} {
		n, ok := m.Parse(tc.name)
		if ok != tc.ok {
			t.Errorf("Parse(%q) ok=%v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if ok && (n.Kind != tc.kind || n.Stem != good || !n.Time.Equal(time.Date(2026, 10, 26, 3, 15, 0, 0, time.UTC)) || n.Full() != tc.name) {
			t.Errorf("Parse(%q) = %+v", tc.name, n)
		}
	}
}

func TestInstallIDNeverAddsSyntax(t *testing.T) {
	for _, id := range []string{"", ".*", "a1b2c3d", "A1B2C3D4", "a1b2c3d4 ", "a1b2c3d4\n", "(a|b)(c|d)", "a1b2c3d45"} {
		if _, err := NewMatcher(id); err == nil {
			t.Errorf("NewMatcher(%q) accepted", id)
		}
		if _, err := NewStem(id, time.Now(), nil); err == nil {
			t.Errorf("NewStem(%q) accepted", id)
		}
	}
}

func TestNewStem(t *testing.T) {
	m, _ := NewMatcher(inst)
	at := time.Date(2026, 10, 26, 5, 15, 0, 0, time.FixedZone("x", 7200)) // 03:15 UTC
	s, err := NewStem(inst, at, bytes.NewReader([]byte{0x09, 0xaf}))
	if err != nil || s != "launchpad-a1b2c3d4-20261026T031500Z-09af" {
		t.Fatalf("stem = %q, %v", s, err)
	}
	if _, ok := m.ParseStem(s); !ok {
		t.Error("own stem does not parse")
	}
	if _, err := NewStem(inst, at, strings.NewReader("x")); err == nil {
		t.Error("short random source accepted")
	}
	// A second stem for the same dump differs (random, from crypto/rand).
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		s, _ := NewStem(inst, at, nil)
		seen[s] = true
	}
	if len(seen) < 2 {
		t.Error("stems do not vary")
	}
}

func TestValid(t *testing.T) {
	for _, tc := range []struct {
		n  string
		ok bool
	}{
		{"launchpad-a1b2c3d4-20261026T031500Z-09af.dump.age", true},
		{"launchpad-ffffffff-20261026T031500Z-09af.json", true}, // any install: the driver does not know it
		{"launchpad-a1b2c3d4-test-0123456789abcdef.tmp", true},
		{"", false}, {".", false}, {"..", false}, {"/etc/passwd", false}, {"../x", false},
		{"launchpad-a1b2c3d4-20261026T031500Z-09af.json/", false},
		{"launchpad-a1b2c3d4-20261026T031500Z-09af.json\x00", false},
		{"launchpad-a1b2c3d4-20261026T031500Z-09af.json?x=1", false},
		{"launchpad-a1b2c3d4-20261026T031500Z-09af.json%2e", false},
		{"launchpad-a1b2c3d4-test-12.tmp", false},
		{"launchpad-a1b2c3d4-test-" + strings.Repeat("a", 65) + ".tmp", false},
		{strings.Repeat("a", 5000), false},
	} {
		if Valid(tc.n) != tc.ok {
			t.Errorf("Valid(%q) = %v", tc.n, !tc.ok)
		}
	}
	if _, err := TestName(inst, "zz"); err == nil {
		t.Error("bad random accepted")
	}
	if n, err := TestName(inst, "0123abcd"); err != nil || !Valid(n) {
		t.Errorf("TestName = %q, %v", n, err)
	}
}

func TestFilesNames(t *testing.T) {
	obj, err := FilesObjectName(inst, "org/x/2026/y/file.png")
	if err != nil || !Valid(obj) {
		t.Fatalf("object name %q: %v", obj, err)
	}
	obj2, _ := FilesObjectName(inst, "org/x/2026/y/file.pnG")
	other, _ := FilesObjectName("ffffffff", "org/x/2026/y/file.png")
	if obj == obj2 || obj == other {
		t.Fatal("a name must depend on the key and the install")
	}
	stem, err := NewFilesStem(inst, time.Date(2026, 10, 26, 3, 15, 0, 0, time.UTC), bytes.NewReader([]byte{0x09, 0xaf}))
	if err != nil || stem != "launchpad-files-a1b2c3d4-list-20261026T031500Z-09af" {
		t.Fatalf("stem %q %v", stem, err)
	}
	fm, _ := NewFilesMatcher(inst)
	for _, n := range []string{FilesListName(stem), FilesManifestName(stem)} {
		if !Valid(n) {
			t.Errorf("%q is not valid", n)
		}
		if _, ok := fm.Parse(n); !ok {
			t.Errorf("%q does not parse", n)
		}
	}
	// A files name is foreign to the dump matcher (retention never deletes it), and a
	// dump name is foreign to the files matcher.
	dm, _ := NewMatcher(inst)
	for _, n := range []string{obj, FilesListName(stem), FilesManifestName(stem)} {
		if _, ok := dm.Parse(n); ok {
			t.Errorf("the dump matcher parsed %q", n)
		}
	}
	if _, ok := fm.Parse("launchpad-a1b2c3d4-20261026T031500Z-09af.json"); ok {
		t.Error("the files matcher parsed a dump manifest")
	}
	// Another install, a month 13, a path, a newline.
	for _, n := range []string{
		"launchpad-files-ffffffff-list-20261026T031500Z-09af.json",
		"launchpad-files-a1b2c3d4-list-20261326T031500Z-09af.json",
		"../launchpad-files-a1b2c3d4-list-20261026T031500Z-09af.json",
		"launchpad-files-a1b2c3d4-list-20261026T031500Z-09af.json\n",
	} {
		if _, ok := fm.Parse(n); ok {
			t.Errorf("parsed %q", n)
		}
	}
	for _, n := range []string{"launchpad-files-a1b2c3d4-obj-zz.age", "launchpad-files-a1b2c3d4-obj-" + strings.Repeat("a", 31) + ".age",
		"launchpad-files-A1B2C3D4-obj-" + strings.Repeat("a", 32) + ".age", "launchpad-files-a1b2c3d4-obj-" + strings.Repeat("a", 32) + ".age/x"} {
		if Valid(n) {
			t.Errorf("%q is valid", n)
		}
	}
	if p := FilesObjectPrefix(inst, "ab"); !strings.HasPrefix(obj, FilesObjectPrefix(inst, obj[len("launchpad-files-a1b2c3d4-obj-"):][:2])) || p == "" {
		t.Fatal("prefix")
	}
	lf, _ := FilesListFolder(inst)
	of, _ := FilesObjectFolder(inst)
	if strings.HasPrefix(lf, "launchpad-"+inst+"-") || strings.HasPrefix(of, "launchpad-"+inst+"-") || strings.HasPrefix(of+"/", lf+"/") {
		t.Fatalf("folders collide with the dump prefix or each other: %q %q", lf, of)
	}
	if _, err := FilesListFolder("nope"); err == nil {
		t.Fatal("a bad install id made a folder")
	}
}
