package offsite

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/backup/remotename"
	"devops/internal/dbq"
)

// The attachment (files) backup of decisions-T16 #38, ticket 14 (LAUN-80). It is the
// last phase of a backup run, after the dump is uploaded and verified, under the
// run's destination lock. A failure of this phase never fails the run (the dump is
// safe on the remote): the run stays `succeeded`, its steps hold a `files` entry
// with ok=false and a code, and its error_text says files_failed or files_incomplete.
//
// What it does, in order:
//  1. list the silo bucket under org/ (read-only key, silo dialer);
//  2. compare each object with the local STATE (key, size, etag): offsite holds only
//     public keys, so it cannot read the encrypted list it wrote earlier; the state
//     file on the scratch volume is its memory (judgment call 3);
//  3. copy every new or changed object: stream silo -> age (scratch) -> destination,
//     one at a time, named by a hash of the key (overwrite on a change);
//  4. write the encrypted list of ALL known objects, then the signed manifest
//     (objects first, list second, manifest last);
//  5. nothing is ever deleted on the destination.

// FilesRunStats is what one files phase did.
type FilesRunStats struct {
	Objects       int    `json:"objects"`  // objects now in the list
	Uploaded      int    `json:"uploaded"` // copied in this run
	UploadedBytes int64  `json:"uploadedBytes"`
	Bytes         int64  `json:"bytes"`    // plain bytes of the list's objects
	Skipped       int    `json:"skipped"`  // not copied: too large, odd key, changed while read
	Failed        int    `json:"failed"`   // silo could not give them
	Gone          int    `json:"gone"`     // in the list, no longer in silo (kept)
	Repaired      int    `json:"repaired"` // missing on the destination, copied again
	Initial       bool   `json:"initial"`  // no state: a full first copy
	Pruned        int    `json:"pruned"`   // objects deleted on the destination (retention, LAUN-81)
	PrunedBytes   int64  `json:"prunedBytes"`
	PruneFailed   int    `json:"pruneFailed"`            // retention deletes that failed (the entry is kept)
	ListsPruned   int    `json:"listsPruned"`            // old list and manifest files deleted
	PruneSkipped  bool   `json:"pruneSkipped,omitempty"` // nothing was pruned this run
	PruneSkip     string `json:"pruneSkip,omitempty"`    // why: no_window, empty_listing, bad_keys, long_gap, mass_gone
	Millis        int64  `json:"ms"`
}

const (
	filesStateVersion  = 1
	filesStateSaveEach = 30 * time.Second
	filesStateSaveN    = 500
	filesMaxSiloFails  = 10 // in a row: the silo is unreachable, stop
	filesShardMod      = 256
	filesScratchSlack  = 1 << 20
)

// siloFailure marks an error that came from reading silo (not from the destination).
type siloFailure struct{ err error }

func (e *siloFailure) Error() string { return e.err.Error() }
func (e *siloFailure) Unwrap() error { return e.err }

// errChanged: the object changed or vanished while it was being copied.
var errChanged = errors.New("offsite: the object changed while it was copied")

type filesRun struct {
	w          *Worker
	run        dbq.BackupRun
	dest       dbq.BackupDestination
	installID  string
	recipients []string
	priv       ed25519.PrivateKey
	listRemote driver.Remote
	objRemote  driver.Remote
	now        time.Time
	scratch    string
	window     time.Duration // retention window; 0: no pruning this run
}

// subFolderConfig returns the destination config with `sub` added to its folder.
func subFolderConfig(drv driver.Driver, raw json.RawMessage, sub string) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, driver.Errf(driver.ServerError)
	}
	key := "folder"
	if _, ok := m["dir"]; ok { // the test-only local driver
		key = "dir"
	}
	var cur string
	if v, ok := m[key]; ok {
		if err := json.Unmarshal(v, &cur); err != nil {
			return nil, driver.Errf(driver.ServerError)
		}
	}
	if cur == "" {
		cur = sub
	} else {
		cur = cur + "/" + sub
	}
	b, _ := json.Marshal(cur)
	m[key] = b
	out, err := json.Marshal(m)
	if err != nil {
		return nil, driver.Errf(driver.ServerError)
	}
	if err := drv.Validate(out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- the state file ----

type stateHeader struct {
	Version   int    `json:"version"`
	InstallID string `json:"installId"`
	Config    string `json:"config"`
}

func (w *Worker) statePath(dest uuid.UUID) string {
	return filepath.Join(w.o.WorkDir, "files-state-"+dest.String()+".jsonl")
}

// stateConfigHash binds the state to ONE destination configuration: a changed
// config (another bucket, folder or host) makes the state invalid, so the next run
// copies everything again.
func stateConfigHash(dest dbq.BackupDestination, installID string) string {
	h := sha256.New()
	h.Write([]byte(dest.ID.String() + "\n" + dest.Type + "\n" + installID + "\n"))
	h.Write(dest.Config)
	return hex.EncodeToString(h.Sum(nil))
}

// loadState reads the state. ANY problem (missing, damaged, another install or
// config) is nil: a full copy is the safe answer, never a guess.
func loadState(path, installID, cfgHash string) map[string]backup.FileEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	line, err := br.ReadBytes('\n')
	if err != nil || len(line) > 1024 {
		return nil
	}
	var h stateHeader
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if dec.Decode(&h) != nil || h.Version != filesStateVersion || h.InstallID != installID || h.Config != cfgHash {
		return nil
	}
	m := map[string]backup.FileEntry{}
	if _, err := backup.ReadFileList(br, installID, func(e backup.FileEntry) bool { m[e.Key] = e; return true }); err != nil {
		return nil
	}
	return m
}

func sortedKeys(m map[string]backup.FileEntry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// saveState writes the state atomically (temp file, fsync, rename), mode 0600.
func saveState(path, installID, cfgHash string, m map[string]backup.FileEntry) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 256<<10)
	hb, _ := json.Marshal(stateHeader{Version: filesStateVersion, InstallID: installID, Config: cfgHash})
	_, err = bw.Write(append(hb, '\n'))
	for _, k := range sortedKeys(m) {
		if err != nil {
			break
		}
		err = backup.EncodeFileEntry(bw, m[k])
	}
	if err == nil {
		err = bw.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// ---- the phase ----

func fmtT(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// execute runs the phase. It returns the stats even on an error (what was done).
func (f *filesRun) execute(ctx context.Context) (st FilesRunStats, err error) {
	began := time.Now()
	defer func() { st.Millis = time.Since(began).Milliseconds() }()
	w := f.w
	cfgHash := stateConfigHash(f.dest, f.installID)
	spath := w.statePath(f.dest.ID)
	state := loadState(spath, f.installID, cfgHash)
	st.Initial = state == nil
	if state == nil {
		state = map[string]backup.FileEntry{}
	}
	stateNewest := backup.NewestLastSeen(state, f.now) // before this run bumps anything
	nowS := fmtT(f.now)

	// 1 and 2: list silo and compare.
	seen := map[string]bool{}
	var todo []siloObject
	bad, err := w.silo.list(ctx, func(o siloObject) error {
		if seen[o.Key] {
			// A real listing never repeats a key. A hostile one could repeat one to fill todo
			// (memory): the repeat is dropped before anything is stored, and counted.
			st.Skipped++
			return nil
		}
		if len(seen) >= backup.MaxFileEntries {
			return driver.Errf(driver.TooLarge)
		}
		seen[o.Key] = true
		// Every key still in silo is "seen now", whatever happens to its copy: ticket 15
		// prunes by lastSeen, so a failed or skipped copy must not make a live key look old.
		e, inState := state[o.Key]
		if inState {
			e.LastSeen = nowS
			state[o.Key] = e
		}
		if o.Size > MaxFileObjectBytes {
			st.Skipped++
			return nil
		}
		if inState && e.Size == o.Size && e.ETag == o.ETag {
			e.Modified = fmtT(o.Modified)
			state[o.Key] = e
			return nil
		}
		todo = append(todo, o)
		return nil
	})
	st.Skipped += bad
	if err != nil {
		return st, &siloFailure{err}
	}
	// Retention first (ticket 15): it shrinks the state BEFORE new keys are added, so a state
	// full of gone keys cannot make save() fail with too_large.
	if f.window > 0 {
		keys, skip := backup.PlanFilesPrune(state, seen, f.now, f.window, stateNewest, bad)
		if skip != "" {
			st.PruneSkipped, st.PruneSkip = true, skip
		} else if err := f.pruneGone(ctx, keys, state, &st, spath, cfgHash); err != nil {
			return st, err
		}
	} else {
		st.PruneSkipped, st.PruneSkip = true, "no_window"
	}
	for k := range state {
		if !seen[k] {
			st.Gone++
		}
	}

	// A one-shard spot check of the destination: a carried object that is missing
	// or has another size is copied again. Over all shards a full pass takes 256 runs.
	if !st.Initial {
		n, rerr := f.repairShard(ctx, state, seen, &todo)
		if rerr != nil {
			return st, rerr
		}
		st.Repaired = n
	}

	// 3: copy.
	sort.Slice(todo, func(i, j int) bool { return todo[i].Key < todo[j].Key })
	lastSave, sinceSave, failsInRow := time.Now(), 0, 0
	save := func() error {
		if len(state) > backup.MaxFileEntries {
			return driver.Errf(driver.TooLarge)
		}
		if err := saveState(spath, f.installID, cfgHash, state); err != nil {
			return internal(err)
		}
		lastSave, sinceSave = time.Now(), 0
		return nil
	}
	for _, o := range todo {
		if err := ctx.Err(); err != nil {
			_ = save()
			return st, context.Cause(ctx)
		}
		e, cerr := f.copyOne(ctx, o, nowS)
		switch {
		case cerr == nil:
			state[o.Key] = e
			st.Uploaded++
			st.UploadedBytes += e.Size
			failsInRow = 0
			sinceSave++
			if sinceSave >= filesStateSaveN || time.Since(lastSave) >= filesStateSaveEach {
				if err := save(); err != nil {
					return st, err
				}
			}
		case errors.Is(cerr, errChanged):
			st.Skipped++ // the next run sees the new size and etag
		default:
			var sf *siloFailure
			if !errors.As(cerr, &sf) {
				_ = save()
				return st, cerr // the destination or our scratch: stop
			}
			st.Failed++
			failsInRow++
			if failsInRow >= filesMaxSiloFails {
				_ = save()
				return st, cerr
			}
		}
	}
	if err := save(); err != nil {
		return st, err
	}

	// 4: the list and the manifest.
	st.Objects = len(state)
	for _, e := range state {
		st.Bytes += e.Size
	}
	stem, err := f.writeManifest(ctx, state, st)
	if err != nil {
		return st, err
	}
	if f.window > 0 {
		f.pruneLists(ctx, stem, &st)
	}
	return st, nil
}

// pruneGone deletes the remote copy of every object that left silo more than the window ago
// (judgment calls of ticket 15) and drops its state entry. The entry is dropped ONLY after the
// destination confirmed the delete (or said the object is already absent); any other error
// keeps it. A crash between the delete and the save repeats the delete next run (absent is
// success). Ten failures in a row stop the pass (the destination is unreachable).
func (f *filesRun) pruneGone(ctx context.Context, keys []string, state map[string]backup.FileEntry, st *FilesRunStats, spath, cfgHash string) error {
	if len(keys) == 0 {
		return nil
	}
	fails := 0
	for _, k := range keys {
		if ctx.Err() != nil {
			break
		}
		e := state[k]
		// The name must be the one this install derives from the key: a state line that names anything
		// else (planted, damaged) is never deleted on the destination.
		if want, werr := remotename.FilesObjectName(f.installID, k); werr != nil || e.Remote != want {
			st.PruneFailed++ // a state line with a foreign name: keep it, delete nothing
			continue
		}
		if err := f.objRemote.Delete(ctx, e.Remote); err != nil && driver.CodeOf(err) != driver.NotFound {
			st.PruneFailed++
			if fails++; fails >= filesMaxSiloFails {
				break
			}
			continue
		}
		fails = 0
		delete(state, k)
		st.Pruned++
		st.PrunedBytes += e.Size
	}
	if st.Pruned > 0 {
		if err := saveState(spath, f.installID, cfgHash, state); err != nil {
			return internal(err)
		}
	}
	return nil
}

// pruneLists deletes the files manifests and lists older than the window, never the one this
// run wrote. A failure is counted and the rest go on; a listing error prunes nothing.
func (f *filesRun) pruneLists(ctx context.Context, keepStem string, st *FilesRunStats) {
	entries, err := f.listRemote.List(ctx, remotename.FilesListPrefix(f.installID))
	if err != nil {
		st.PruneFailed++
		return
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	skipList := false // the manifest of this pair could not be deleted: keep its list
	for _, n := range backup.PlanListPrune(f.installID, names, f.now, f.window, keepStem) {
		if ctx.Err() != nil {
			st.PruneFailed++
			return
		}
		isManifest := strings.HasSuffix(n, remotename.FilesManifestSuffix)
		if !isManifest && skipList {
			skipList = false
			continue
		}
		if err := f.listRemote.Delete(ctx, n); err != nil && driver.CodeOf(err) != driver.NotFound {
			st.PruneFailed++
			skipList = isManifest
			continue
		}
		skipList = false
		if strings.HasSuffix(n, remotename.FilesManifestSuffix) {
			st.ListsPruned++
		}
	}
}

// repairShard adds to todo the carried objects of today's shard that the destination
// lacks. A listing the destination refuses as too large (a WebDAV, SFTP or Dropbox
// folder of many attachments) skips the check; any other error is returned.
func (f *filesRun) repairShard(ctx context.Context, state map[string]backup.FileEntry, seen map[string]bool, todo *[]siloObject) (int, error) {
	shard := fmt.Sprintf("%02x", (f.now.Unix()/86400)%filesShardMod)
	prefix := remotename.FilesObjectPrefix(f.installID, shard)
	entries, err := f.objRemote.List(ctx, prefix)
	if err != nil {
		if driver.CodeOf(err) == driver.TooLarge {
			return 0, nil
		}
		return 0, err
	}
	have := map[string]int64{}
	for _, e := range entries {
		have[e.Name] = e.Size
	}
	queued := map[string]bool{}
	for _, o := range *todo {
		queued[o.Key] = true
	}
	n := 0
	for _, k := range sortedKeys(state) {
		e := state[k]
		if !seen[k] || queued[k] || !strings.HasPrefix(e.Remote, prefix) {
			continue // gone from silo (nothing to copy from), or already queued
		}
		if sz, ok := have[e.Remote]; ok && (sz < 0 || sz == e.CipherSize) {
			continue
		}
		mt, _ := time.Parse(time.RFC3339, e.Modified)
		*todo = append(*todo, siloObject{Key: e.Key, Size: e.Size, ETag: e.ETag, Modified: mt})
		n++
	}
	return n, nil
}

// copyOne streams one object: silo -> age (scratch file) -> destination. The
// scratch file is deleted whatever happens.
func (f *filesRun) copyOne(ctx context.Context, o siloObject, nowS string) (backup.FileEntry, error) {
	w := f.w
	name, err := remotename.FilesObjectName(f.installID, o.Key)
	if err != nil {
		return backup.FileEntry{}, internal(err)
	}
	free, err := w.o.FreeBytes(w.o.WorkDir)
	if err != nil {
		return backup.FileEntry{}, internal(err)
	}
	if free < uint64(o.Size)+filesScratchSlack {
		return backup.FileEntry{}, driver.Errf(driver.NoSpace)
	}
	defer os.Remove(f.scratch)

	octx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	body, clen, err := w.silo.get(octx, o.Key)
	if err != nil {
		if ctx.Err() != nil { // the run's deadline or a shutdown, not the silo's fault
			return backup.FileEntry{}, context.Cause(ctx)
		}
		if driver.CodeOf(err) == driver.NotFound {
			return backup.FileEntry{}, errChanged // deleted since the listing
		}
		return backup.FileEntry{}, &siloFailure{err}
	}
	defer body.Close()
	if clen >= 0 && clen != o.Size {
		return backup.FileEntry{}, errChanged
	}
	prog := newProgress(io.LimitReader(body, o.Size+1))
	stop := watchStall(octx, cancel, prog, w.o.StallAfter)
	enc, rerr, werr := f.encryptTo(prog)
	stop()
	if errors.Is(context.Cause(octx), errStalled) {
		return backup.FileEntry{}, &siloFailure{driver.Errf(driver.Stalled)}
	}
	if rerr != nil {
		if ctx.Err() != nil {
			return backup.FileEntry{}, context.Cause(ctx)
		}
		return backup.FileEntry{}, &siloFailure{driver.MapNetError(rerr)}
	}
	if werr != nil {
		return backup.FileEntry{}, scratchError(werr)
	}
	if enc.PlainBytes != o.Size {
		return backup.FileEntry{}, errChanged
	}
	if err := f.put(ctx, f.objRemote, name, f.scratch, enc.CipherBytes); err != nil {
		return backup.FileEntry{}, err
	}
	return backup.FileEntry{
		Key: o.Key, Size: o.Size, ETag: o.ETag, Modified: fmtT(o.Modified), SHA256: enc.PlainSHA256,
		Remote: name, CipherSize: enc.CipherBytes, CipherSHA256: enc.CipherSHA256, LastSeen: nowS,
	}, nil
}

// encryptTo encrypts src into the scratch file. It returns a read error of src and
// a write error of the scratch file apart, so a silo fault is not blamed on the disk.
func (f *filesRun) encryptTo(src io.Reader) (enc backup.EncryptResult, rerr, werr error) {
	out, err := os.OpenFile(f.scratch, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return enc, nil, err
	}
	var dst io.Writer = out
	if f.w.o.ScratchWrap != nil {
		dst = f.w.o.ScratchWrap(out)
	}
	bw := bufio.NewWriterSize(dst, 256<<10)
	rt := &readTracker{r: src}
	enc, err = backup.Encrypt(bw, rt, f.recipients)
	if err == nil {
		err = bw.Flush()
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if rt.err != nil {
		return enc, rt.err, nil
	}
	return enc, nil, err
}

// put uploads a scratch file under a stall watchdog.
func (f *filesRun) put(ctx context.Context, r driver.Remote, name, path string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return internal(err)
	}
	defer file.Close()
	sctx, scancel := context.WithCancelCause(ctx)
	defer scancel(nil)
	prog := newProgress(file)
	stop := watchStall(sctx, scancel, prog, f.w.o.StallAfter)
	perr := r.Put(sctx, name, size, prog)
	stop()
	if perr != nil {
		if errors.Is(context.Cause(sctx), errStalled) {
			return driver.Errf(driver.Stalled)
		}
		return perr
	}
	return nil
}

// writeManifest writes the encrypted list, then the signed manifest, and checks both
// are on the destination with the right sizes. On a failure after the list went up it
// deletes the names it chose, best effort.
func (f *filesRun) writeManifest(ctx context.Context, state map[string]backup.FileEntry, st FilesRunStats) (stem string, err error) {
	stem, err = remotename.NewFilesStem(f.installID, f.now, nil)
	if err != nil {
		return "", internal(err)
	}
	listName, manName := remotename.FilesListName(stem), remotename.FilesManifestName(stem)
	keys := sortedKeys(state)
	pr, pw := io.Pipe()
	go func() {
		bw := bufio.NewWriterSize(pw, 64<<10)
		var werr error
		for _, k := range keys {
			if werr = backup.EncodeFileEntry(bw, state[k]); werr != nil {
				break
			}
		}
		if werr == nil {
			werr = bw.Flush()
		}
		_ = pw.CloseWithError(werr)
	}()
	out, err := os.OpenFile(f.scratch, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		_ = pr.Close()
		return "", scratchError(err)
	}
	defer os.Remove(f.scratch)
	bw := bufio.NewWriterSize(out, 256<<10)
	enc, eerr := backup.Encrypt(bw, pr, f.recipients)
	_ = pr.CloseWithError(errors.New("done")) // frees the writer if Encrypt stopped early
	if eerr == nil {
		eerr = bw.Flush()
	}
	if eerr == nil {
		eerr = out.Sync()
	}
	if cerr := out.Close(); eerr == nil {
		eerr = cerr
	}
	if eerr != nil {
		return "", scratchError(eerr)
	}
	uploaded := false
	defer func() {
		if err != nil && uploaded {
			f.w.deletePartial(f.listRemote, listName, manName)
		}
	}()
	uploaded = true
	if err := f.put(ctx, f.listRemote, listName, f.scratch, enc.CipherBytes); err != nil {
		return "", err
	}
	manifest, err := backup.SignFiles(f.priv, backup.FilesManifest{
		Version: backup.FilesManifestVersion, InstallID: f.installID, List: listName,
		SHA256: enc.CipherSHA256, Size: enc.CipherBytes, Time: f.now.UTC().Format(remotename.TimeLayout),
		Objects: st.Objects, Bytes: st.Bytes, LaunchpadVersion: f.w.o.Version,
	})
	if err != nil {
		return "", internal(err)
	}
	if err := f.listRemote.Put(ctx, manName, int64(len(manifest)), bytes.NewReader(manifest)); err != nil {
		return "", err
	}
	entries, err := f.listRemote.List(ctx, stem)
	if err != nil {
		return "", err
	}
	var gotList, gotMan bool
	for _, e := range entries {
		switch e.Name {
		case listName:
			if e.Size >= 0 && e.Size != enc.CipherBytes {
				return "", driver.Errf(driver.ServerError)
			}
			gotList = true
		case manName:
			if e.Size >= 0 && e.Size != int64(len(manifest)) {
				return "", driver.Errf(driver.ServerError)
			}
			gotMan = true
		}
	}
	if !gotList || !gotMan {
		return "", driver.Errf(driver.NotFound)
	}
	return stem, nil
}

// filesPhase is called by doBackup after the dump is verified. It never returns an
// error: it returns the step for backup_runs.steps and a warning text for error_text.
func (w *Worker) filesPhase(ctx context.Context, run dbq.BackupRun, dest dbq.BackupDestination, drv driver.Driver,
	cfg driver.Config, installID string, recipients []string, priv ed25519.PrivateKey, now time.Time, window time.Duration) (step []byte, warn string) {
	fail := func(code driver.Code, st *FilesRunStats) ([]byte, string) {
		w.log.Warn("offsite: the files backup failed", "run", run.ID.String(), "code", string(code))
		return filesStepJSON(false, string(code), st), "files_failed"
	}
	lf, err1 := remotename.FilesListFolder(installID)
	of, err2 := remotename.FilesObjectFolder(installID)
	if err1 != nil || err2 != nil {
		return fail(driver.ServerError, nil)
	}
	open := func(sub string) (driver.Remote, error) {
		c, err := subFolderConfig(drv, dest.Config, sub)
		if err != nil {
			return nil, err
		}
		cc := cfg
		cc.Config = c
		return drv.Open(ctx, cc)
	}
	lr, err := open(lf)
	if err != nil {
		return fail(driver.CodeOf(err), nil)
	}
	defer lr.Close()
	or, err := open(of)
	if err != nil {
		return fail(driver.CodeOf(err), nil)
	}
	defer or.Close()
	f := &filesRun{w: w, run: run, dest: dest, installID: installID, recipients: recipients, priv: priv,
		listRemote: lr, objRemote: or, now: now, scratch: w.scratchPath(run.ID), window: window}
	st, err := f.execute(ctx)
	if err != nil {
		code := classify(ctx, err).code
		var sf *siloFailure
		if errors.As(err, &sf) {
			w.log.Warn("offsite: the files backup could not read silo", "run", run.ID.String(), "code", string(code))
		}
		return fail(code, &st)
	}
	ok, code, warn := filesOutcome(&st)
	return filesStepJSON(ok, code, &st), warn
}

// filesOutcome decides the step and the run warning of a finished phase. Copy problems
// (skipped or unreadable objects) make the step not ok and win. A retention failure alone
// keeps ok=true (the copy is complete; files_not_backed_up counts ok steps) and shows only as
// the code and the warning.
func filesOutcome(st *FilesRunStats) (ok bool, code, warn string) {
	if st.Failed > 0 || st.Skipped > 0 {
		return false, "incomplete", "files_incomplete"
	}
	if st.PruneFailed > 0 {
		return true, "prune_failed", "prune_failed"
	}
	return true, "", ""
}

type filesStep struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Code string `json:"code,omitempty"`
	*FilesRunStats
}

func filesStepJSON(ok bool, code string, st *FilesRunStats) []byte {
	b, _ := json.Marshal([]filesStep{{Name: "files", OK: ok, Code: code, FilesRunStats: st}})
	return b
}
