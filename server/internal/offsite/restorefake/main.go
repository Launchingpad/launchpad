// Command restorefake is a TEST TOOL for deploy/test-restore-remote.sh (T16
// ticket 12, LAUN-79). It is never part of the production image: the Dockerfile
// builds ./cmd/server only, and nothing imports this package.
//
//	restorefake gen   --dump FILE --remote DIR --keys DIR --install-id ID --time 20261028T031500Z
//	    Makes (or reuses) the keys in --keys, then writes one encrypted object and
//	    one signed manifest into --remote, with the real library code
//	    (backup.Encrypt, backup.Sign). Prints the stem.
//	restorefake genfiles --src DIR --remote DIR --keys DIR --install-id ID --time T
//	    Makes the attachment backup of LAUN-80 from the files under --src (the path
//	    under --src is the silo key): one age object per file in
//	    <remote>/launchpad-files-<id>-objects/, the encrypted list and the signed
//	    files manifest in <remote>/launchpad-files-<id>/. Prints the files stem.
//	restorefake serve --dir DIR --addr HOST:PORT --bucket NAME
//	    A path-style S3 server over the files of DIR: ListObjectsV2 and GET. It
//	    does not check a signature (the S3 driver's own tests do).
//
// The keys in --keys: age.key (the private key), age.pub, age-wrong.key,
// signing.pub (base64 public key, the form `verify --public-key` reads),
// signing-wrong.pub.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"devops/internal/backup"
	"devops/internal/backup/remotename"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: restorefake gen|serve ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = gen(os.Args[2:])
	case "genfiles":
		err = genfiles(os.Args[2:])
	case "serve":
		err = serve(os.Args[2:])
	default:
		err = errors.New("unknown command")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "restorefake:", err)
		os.Exit(1)
	}
}

func writeKey(path, content string, mode os.FileMode) error {
	return os.WriteFile(path, []byte(content), mode)
}

// ensureKeys makes the key files that do not exist yet.
func ensureKeys(dir string) (agePub string, signPriv ed25519.PrivateKey, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	p := func(n string) string { return filepath.Join(dir, n) }
	if _, e := os.Stat(p("age.key")); e != nil {
		sec, pub, err := backup.GenerateKeyPair()
		if err != nil {
			return "", nil, err
		}
		if err := writeKey(p("age.key"), "# test key\n"+sec+"\n", 0o600); err != nil {
			return "", nil, err
		}
		if err := writeKey(p("age.pub"), pub+"\n", 0o644); err != nil {
			return "", nil, err
		}
		wsec, _, err := backup.GenerateKeyPair()
		if err != nil {
			return "", nil, err
		}
		if err := writeKey(p("age-wrong.key"), wsec+"\n", 0o600); err != nil {
			return "", nil, err
		}
		spub, spriv, err := backup.GenerateSigningKey()
		if err != nil {
			return "", nil, err
		}
		if err := writeKey(p("signing.pub"), backup.EncodePublicKey(spub)+"\n", 0o644); err != nil {
			return "", nil, err
		}
		if err := writeKey(p("signing.key"), base64.StdEncoding.EncodeToString(spriv)+"\n", 0o600); err != nil {
			return "", nil, err
		}
		wpub, _, err := backup.GenerateSigningKey()
		if err != nil {
			return "", nil, err
		}
		if err := writeKey(p("signing-wrong.pub"), backup.EncodePublicKey(wpub)+"\n", 0o644); err != nil {
			return "", nil, err
		}
	}
	pb, err := os.ReadFile(p("age.pub"))
	if err != nil {
		return "", nil, err
	}
	sb, err := os.ReadFile(p("signing.key"))
	if err != nil {
		return "", nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sb)))
	if err != nil {
		return "", nil, err
	}
	return strings.TrimSpace(string(pb)), ed25519.PrivateKey(raw), nil
}

func gen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	dump := fs.String("dump", "", "the plain dump file")
	remote := fs.String("remote", "", "the directory the S3 fake serves")
	keys := fs.String("keys", "", "the key directory")
	install := fs.String("install-id", "", "install id")
	tm := fs.String("time", "", "dump time, "+remotename.TimeLayout)
	if err := fs.Parse(args); err != nil {
		return err
	}
	t, err := time.Parse(remotename.TimeLayout, *tm)
	if err != nil {
		return errors.New("bad --time")
	}
	pub, priv, err := ensureKeys(*keys)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*remote, 0o755); err != nil {
		return err
	}
	stem, err := remotename.NewStem(*install, t, nil)
	if err != nil {
		return err
	}
	src, err := os.Open(*dump)
	if err != nil {
		return err
	}
	defer src.Close()
	objPath := filepath.Join(*remote, remotename.ObjectName(stem))
	dst, err := os.Create(objPath)
	if err != nil {
		return err
	}
	res, err := backup.Encrypt(dst, src, []string{pub})
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	m := backup.Manifest{Version: backup.ManifestVersion, InstallID: *install, Object: remotename.ObjectName(stem),
		SHA256: res.CipherSHA256, Size: res.CipherBytes, DumpTime: t.UTC().Format(remotename.TimeLayout),
		SchemaVersion: 21, LaunchpadVersion: "test"}
	mb, err := backup.Sign(priv, m)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*remote, remotename.ManifestName(stem)), mb, 0o644); err != nil {
		return err
	}
	// The sha256 of the object, for the test's database record.
	h := sha256.New()
	f, err := os.Open(objPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	fmt.Printf("%s %s\n", stem, hex.EncodeToString(h.Sum(nil)))
	return nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory")
	addr := fs.String("addr", ":9000", "listen address")
	bucket := fs.String("bucket", "backups", "bucket name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/"+*bucket)
		if rest == r.URL.Path {
			http.Error(w, "no bucket", 404)
			return
		}
		key := strings.TrimPrefix(rest, "/")
		if key == "" {
			list(w, r, *dir)
			return
		}
		parts := strings.Split(key, "/")
		if len(parts) > 2 || strings.Contains(key, "\\") {
			http.Error(w, "bad key", 400)
			return
		}
		for _, p := range parts {
			if p == "" || p == "." || p == ".." {
				http.Error(w, "bad key", 400)
				return
			}
		}
		f, err := os.Open(filepath.Join(*dir, filepath.Join(parts...)))
		if err != nil {
			http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", 404)
			return
		}
		defer f.Close()
		st, _ := f.Stat()
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
		w.WriteHeader(200)
		if r.Method == http.MethodGet {
			_, _ = io.Copy(w, f)
		}
	})
	return http.ListenAndServe(*addr, h)
}

func list(w http.ResponseWriter, r *http.Request, dir string) {
	prefix := r.URL.Query().Get("prefix")
	sub := ""
	if i := strings.LastIndex(prefix, "/"); i >= 0 { // a folder: list its files, keep the folder in the key
		sub, prefix = prefix[:i], prefix[i+1:]
		if strings.ContainsAny(sub, "/\\") || sub == "." || sub == ".." {
			http.Error(w, "bad prefix", 400)
			return
		}
		dir = filepath.Join(dir, sub)
	}
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	type content struct {
		Key  string
		Size int64
	}
	res := struct {
		XMLName     xml.Name `xml:"ListBucketResult"`
		IsTruncated bool
		Contents    []content
	}{}
	for _, n := range names {
		st, err := os.Stat(filepath.Join(dir, n))
		if err == nil {
			k := n
			if sub != "" {
				k = sub + "/" + n
			}
			res.Contents = append(res.Contents, content{Key: k, Size: st.Size()})
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

func genfiles(args []string) error {
	fs := flag.NewFlagSet("genfiles", flag.ContinueOnError)
	src := fs.String("src", "", "a directory; the path under it is the silo key")
	remote := fs.String("remote", "", "the directory the S3 fake serves")
	keys := fs.String("keys", "", "the key directory")
	install := fs.String("install-id", "", "install id")
	tm := fs.String("time", "", "list time, "+remotename.TimeLayout)
	hostile := fs.Bool("hostile", false, "also write nine signed lists (older than --time) whose entry is unsafe: restore.sh must refuse each")
	badTime := fs.String("bad-sha-time", "", "also write a second, signed list at this time whose first entry has a wrong plain sha256")
	if err := fs.Parse(args); err != nil {
		return err
	}
	t, err := time.Parse(remotename.TimeLayout, *tm)
	if err != nil {
		return errors.New("bad --time")
	}
	pub, priv, err := ensureKeys(*keys)
	if err != nil {
		return err
	}
	lf, err := remotename.FilesListFolder(*install)
	if err != nil {
		return err
	}
	of, _ := remotename.FilesObjectFolder(*install)
	listDir, objDir := filepath.Join(*remote, lf), filepath.Join(*remote, of)
	for _, d := range []string{listDir, objDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	var entries []backup.FileEntry
	var total int64
	err = filepath.WalkDir(*src, func(p string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return werr
		}
		rel, _ := filepath.Rel(*src, p)
		key := filepath.ToSlash(rel)
		name, err := remotename.FilesObjectName(*install, key)
		if err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		plain := sha256.New()
		st, _ := in.Stat()
		out, err := os.Create(filepath.Join(objDir, name))
		if err != nil {
			return err
		}
		res, err := backup.Encrypt(out, io.TeeReader(in, plain), []string{pub})
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		total += st.Size()
		entries = append(entries, backup.FileEntry{Key: key, Size: st.Size(), ETag: "e", Modified: t.UTC().Format(time.RFC3339),
			SHA256: hex.EncodeToString(plain.Sum(nil)), Remote: name, CipherSize: res.CipherBytes, CipherSHA256: res.CipherSHA256,
			LastSeen: t.UTC().Format(time.RFC3339)})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	var writePlain func(t time.Time, plain string, n int) (string, error)
	write := func(t time.Time, entries []backup.FileEntry) (string, error) {
		var plainList strings.Builder
		for _, e := range entries {
			if err := backup.EncodeFileEntry(&plainList, e); err != nil {
				return "", err
			}
		}
		return writePlain(t, plainList.String(), len(entries))
	}
	writePlain = func(t time.Time, plain string, n int) (string, error) {
		stem, err := remotename.NewFilesStem(*install, t, nil)
		if err != nil {
			return "", err
		}
		var plainList strings.Builder
		plainList.WriteString(plain)
		lp := filepath.Join(listDir, remotename.FilesListName(stem))
		lo, err := os.Create(lp)
		if err != nil {
			return "", err
		}
		res, err := backup.Encrypt(lo, strings.NewReader(plainList.String()), []string{pub})
		if cerr := lo.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", err
		}
		mb, err := backup.SignFiles(priv, backup.FilesManifest{Version: backup.FilesManifestVersion, InstallID: *install,
			List: remotename.FilesListName(stem), SHA256: res.CipherSHA256, Size: res.CipherBytes,
			Time: t.UTC().Format(remotename.TimeLayout), Objects: n, Bytes: total, LaunchpadVersion: "test"})
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(listDir, remotename.FilesManifestName(stem)), mb, 0o644); err != nil {
			return "", err
		}

		return stem, nil
	}
	stem, err := write(t, entries)
	if err != nil {
		return err
	}
	fmt.Println(stem)
	if *hostile {
		base := entries[0]
		long := strings.Repeat("a", 256)
		cases := []struct {
			key, remote string
			dup         bool
		}{
			{key: "org/x/../y"}, {key: "/org/x"}, {key: "org//x"}, {key: "org/a\\b"}, {key: "org/a\nb"},
			{key: "dup", dup: true}, {key: "org/" + long + "/f"},
			{key: "org/x/f", remote: "launchpad-files-" + *install + "-obj-" + strings.Repeat("f", 32) + ".age"},
			{key: "org/x/f", remote: "launchpad-files-ffffffff-obj-" + strings.Repeat("f", 32) + ".age"},
		}
		for i, c := range cases {
			e := base
			e.Key = c.key
			if c.key == "dup" {
				e.Key = base.Key
			}
			if c.remote != "" {
				e.Remote = c.remote
			} else if !c.dup {
				n, _ := remotename.FilesObjectName(*install, e.Key)
				e.Remote = n
			}
			b, _ := json.Marshal(e) // raw JSON: no validation, as a hostile signer would write it
			plain := string(b) + "\n"
			n := 1
			if c.dup {
				plain += string(b) + "\n"
				n = 2
			}
			hs, err := writePlain(t.Add(-time.Duration(48+i)*time.Hour), plain, n)
			if err != nil {
				return err
			}
			fmt.Println(hs)
		}
	}
	if *badTime != "" {
		bt, err := time.Parse(remotename.TimeLayout, *badTime)
		if err != nil {
			return errors.New("bad --bad-sha-time")
		}
		bad := append([]backup.FileEntry(nil), entries...)
		bad[0].SHA256 = strings.Repeat("0", 64)
		bstem, err := write(bt, bad)
		if err != nil {
			return err
		}
		fmt.Println(bstem)
	}
	return nil
}
