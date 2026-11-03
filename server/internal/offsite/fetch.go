package offsite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/backup/remotename"
)

// pickStem finds the stem to fetch among the entries of a remote listing. "latest"
// is the newest COMPLETE pair by the (unauthenticated) time in the name; a stem is
// checked against the install's name format and must be complete.
func pickStem(installID, want string, entries []driver.Entry) (stem string, objSize int64, err error) {
	m, err := remotename.NewMatcher(installID)
	if err != nil {
		return "", 0, err
	}
	type pair struct {
		t        time.Time
		obj, man bool
		size     int64
	}
	pairs := map[string]*pair{}
	for _, e := range entries {
		n, ok := m.Parse(e.Name)
		if !ok {
			continue
		}
		p := pairs[n.Stem]
		if p == nil {
			p = &pair{t: n.Time, size: -1}
			pairs[n.Stem] = p
		}
		if n.Kind == remotename.KindObject {
			p.obj, p.size = true, e.Size
		} else {
			p.man = true
		}
	}
	if want != "latest" {
		if _, ok := m.ParseStem(want); !ok {
			return "", 0, errors.New("offsite: the stem is not a backup name of this install")
		}
		p := pairs[want]
		if p == nil || !p.obj || !p.man {
			return "", 0, driver.Errf(driver.NotFound)
		}
		return want, p.size, nil
	}
	var stems []string
	for s, p := range pairs {
		if p.obj && p.man {
			stems = append(stems, s)
		}
	}
	if len(stems) == 0 {
		return "", 0, driver.Errf(driver.NotFound)
	}
	sort.Slice(stems, func(i, j int) bool {
		a, b := pairs[stems[i]], pairs[stems[j]]
		if !a.t.Equal(b.t) {
			return a.t.After(b.t)
		}
		return stems[i] > stems[j]
	})
	return stems[0], pairs[stems[0]].size, nil
}

// FetchFrame lists the install's files on a remote, picks the stem ("latest" or a
// stem) and writes one frame (decisions-T16 #39): the manifest, then the object. It
// reads the object through a cap of its declared size plus a small slack, and writes
// NOTHING else to out. The manifest is not verified here: `verify` does that, with
// the public key the admin saved.
func FetchFrame(ctx context.Context, r driver.Remote, installID, want string, out io.Writer) error {
	if !remotename.ValidInstallID(installID) {
		return errors.New("offsite: the install id must be 8 lower-case hex characters")
	}
	entries, err := r.List(ctx, "launchpad-"+installID+"-")
	if err != nil {
		return err
	}
	stem, size, err := pickStem(installID, want, entries)
	if err != nil {
		return err
	}
	if size < 0 {
		return errors.New("offsite: the remote does not report the object size")
	}
	if size > MaxObjectBytes {
		return driver.Errf(driver.TooLarge)
	}
	mrc, err := r.Get(ctx, remotename.ManifestName(stem))
	if err != nil {
		return err
	}
	manifest, err := driver.ReadCapped(mrc, backup.MaxManifestBytes)
	_ = mrc.Close()
	if err != nil {
		return err
	}
	orc, err := r.Get(ctx, remotename.ObjectName(stem))
	if err != nil {
		return err
	}
	defer orc.Close()
	return WriteFrame(out, manifest, size, driver.CapReader(orc, driver.DownloadCap(size)))
}

// pickFilesStem is pickStem for the files folder: the newest COMPLETE pair (list and
// manifest) of the install, or the one named by want.
func pickFilesStem(installID, want string, entries []driver.Entry) (stem string, listSize int64, err error) {
	m, err := remotename.NewFilesMatcher(installID)
	if err != nil {
		return "", 0, err
	}
	type pair struct {
		t         time.Time
		list, man bool
		size      int64
	}
	pairs := map[string]*pair{}
	for _, e := range entries {
		n, ok := m.Parse(e.Name)
		if !ok {
			continue
		}
		p := pairs[n.Stem]
		if p == nil {
			p = &pair{t: n.Time, size: -1}
			pairs[n.Stem] = p
		}
		if n.Manifest {
			p.man = true
		} else {
			p.list, p.size = true, e.Size
		}
	}
	if want != "latest" {
		if _, ok := m.ParseStem(want); !ok {
			return "", 0, errors.New("offsite: the stem is not a files list name of this install")
		}
		p := pairs[want]
		if p == nil || !p.list || !p.man {
			return "", 0, driver.Errf(driver.NotFound)
		}
		return want, p.size, nil
	}
	var stems []string
	for st, p := range pairs {
		if p.list && p.man {
			stems = append(stems, st)
		}
	}
	if len(stems) == 0 {
		return "", 0, driver.Errf(driver.NotFound)
	}
	sort.Slice(stems, func(i, j int) bool {
		a, b := pairs[stems[i]], pairs[stems[j]]
		if !a.t.Equal(b.t) {
			return a.t.After(b.t)
		}
		return stems[i] > stems[j]
	})
	return stems[0], pairs[stems[0]].size, nil
}

// FetchFilesFrame writes one frame of the files backup: the signed files manifest, then
// the encrypted list. r is a Remote opened on the LIST folder (remotename.FilesListFolder).
// The manifest is not verified here: `verify --files` does that.
func FetchFilesFrame(ctx context.Context, r driver.Remote, installID, want string, out io.Writer) error {
	if !remotename.ValidInstallID(installID) {
		return errors.New("offsite: the install id must be 8 lower-case hex characters")
	}
	entries, err := r.List(ctx, remotename.FilesListPrefix(installID))
	if err != nil {
		return err
	}
	stem, size, err := pickFilesStem(installID, want, entries)
	if err != nil {
		return err
	}
	if size < 0 {
		return errors.New("offsite: the remote does not report the list size")
	}
	if size > MaxObjectBytes {
		return driver.Errf(driver.TooLarge)
	}
	mrc, err := r.Get(ctx, remotename.FilesManifestName(stem))
	if err != nil {
		return err
	}
	manifest, err := driver.ReadCapped(mrc, backup.MaxManifestBytes)
	_ = mrc.Close()
	if err != nil {
		return err
	}
	lrc, err := r.Get(ctx, remotename.FilesListName(stem))
	if err != nil {
		return err
	}
	defer lrc.Close()
	return WriteFrame(out, manifest, size, driver.CapReader(lrc, driver.DownloadCap(size)))
}

// FetchFilesObjectFrame writes a frame with an EMPTY manifest and one attachment object
// (its remote name from the list). r is a Remote opened on the OBJECT folder. The
// object's integrity is the sha256 in the (verified, decrypted) list.
func FetchFilesObjectFrame(ctx context.Context, r driver.Remote, installID, name string, out io.Writer) error {
	if !remotename.ValidInstallID(installID) {
		return errors.New("offsite: the install id must be 8 lower-case hex characters")
	}
	if !strings.HasPrefix(name, "launchpad-files-"+installID+"-obj-") || !remotename.Valid(name) {
		return errors.New("offsite: the name is not an attachment object of this install")
	}
	entries, err := r.List(ctx, name)
	if err != nil {
		return err
	}
	size := int64(-1)
	for _, e := range entries {
		if e.Name == name {
			size = e.Size
		}
	}
	if size < 0 {
		return driver.Errf(driver.NotFound)
	}
	if size > MaxObjectBytes {
		return driver.Errf(driver.TooLarge)
	}
	orc, err := r.Get(ctx, name)
	if err != nil {
		return err
	}
	defer orc.Close()
	return WriteFrame(out, nil, size, driver.CapReader(orc, driver.DownloadCap(size)))
}

// ConfigFile is the JSON the admin writes for `fetch --config` (decisions-T16 #39):
// {"type":"s3","endpoint":...,"bucket":...,"accessKey":...,"secretKey":...} or
// {"type":"webdav","url":...,"username":...,"password":...,"folder":...}. The secret
// names of each type are split from the rest; the rest is the driver's typed config.
type ConfigFile struct {
	Type    string
	Config  json.RawMessage
	Secrets json.RawMessage
}

var secretNames = map[string][]string{
	"s3":      {"accessKey", "secretKey"},
	"webdav":  {"password"},
	"sftp":    {"password", "privateKey"},
	"dropbox": {"appKey", "appSecret", "refreshToken"},
}

// maxConfigFile is a sanity cap on the file.
const maxConfigFile = 256 << 10

// ReadConfigFile reads and splits a --config file.
func ReadConfigFile(path string) (ConfigFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return ConfigFile{}, errors.New("offsite: cannot open the config file")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigFile+1))
	if err != nil || len(b) > maxConfigFile {
		return ConfigFile{}, errors.New("offsite: cannot read the config file")
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return ConfigFile{}, errors.New("offsite: the config file is not a JSON object")
	}
	var typ string
	if t, ok := all["type"]; !ok || json.Unmarshal(t, &typ) != nil || typ == "" {
		return ConfigFile{}, errors.New("offsite: the config file needs a type")
	}
	names, ok := secretNames[typ]
	if !ok {
		return ConfigFile{}, errors.New("offsite: the config file has an unknown type")
	}
	secrets, cfg := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	for k, v := range all {
		if k == "type" {
			continue
		}
		isSecret := false
		for _, n := range names {
			isSecret = isSecret || n == k
		}
		if isSecret {
			secrets[k] = v
		} else {
			cfg[k] = v
		}
	}
	cb, _ := json.Marshal(cfg)
	sb, _ := json.Marshal(secrets)
	return ConfigFile{Type: typ, Config: cb, Secrets: sb}, nil
}

// safeMessage is what a command prints for an error: the code text of a driver
// error, or a fixed sentence of ours. It never prints a wrapped cause.
func safeMessage(err error) string {
	var de *driver.Error
	if errors.As(err, &de) {
		return de.Error()
	}
	switch {
	case errors.Is(err, ErrFrameVersion), errors.Is(err, ErrFrameManifest), errors.Is(err, ErrFrameObject),
		errors.Is(err, ErrFrameShort), errors.Is(err, ErrFrameTrailing), errors.Is(err, ErrFrameNoSpace),
		errors.Is(err, backup.ErrDecrypt), errors.Is(err, backup.ErrBadIdentity), errors.Is(err, backup.ErrTooLarge),
		errors.Is(err, backup.ErrManifestFormat), errors.Is(err, backup.ErrManifestSignature),
		errors.Is(err, backup.ErrManifestTooLarge), errors.Is(err, backup.ErrManifestBinding),
		errors.Is(err, backup.ErrObjectMismatch), errors.Is(err, backup.ErrFileList), errors.Is(err, backup.ErrBadKey):
		return err.Error()
	}
	if strings.HasPrefix(err.Error(), "offsite: ") && !strings.ContainsAny(err.Error(), "\n") && len(err.Error()) < 160 {
		// Our own fixed sentences start with "offsite: " and wrap nothing.
		if errors.Unwrap(err) == nil {
			return err.Error()
		}
	}
	return "offsite: the command failed"
}

var _ = fmt.Sprintf
