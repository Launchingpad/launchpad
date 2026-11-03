//go:build backuptest

package driver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Local is the local-directory driver of T16 #16. It exists for tests only:
// this file builds under the `backuptest` tag, so the production binary has no
// reference to it (scripts/check-backuptest.sh proves that). It is not a valid
// destination `type` in the schema. Tests register it under a real type name
// with RegisterForTest.
//
// Config: {"dir": "/abs/path"}.
type Local struct{}

type localConfig struct {
	Dir string `json:"dir"`
}

func parseLocal(raw json.RawMessage) (localConfig, error) {
	var c localConfig
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, Errf(ServerError)
	}
	if c.Dir == "" || !filepath.IsAbs(c.Dir) || filepath.Clean(c.Dir) != c.Dir {
		return c, Errf(ServerError)
	}
	return c, nil
}

// Validate implements Driver.
func (Local) Validate(config json.RawMessage) error { _, err := parseLocal(config); return err }

// Identity implements Driver.
func (Local) Identity(config json.RawMessage) (Identity, error) {
	c, err := parseLocal(config)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Type: "local", Folder: strings.Trim(c.Dir, "/")}, nil
}

// Probe implements Driver. A directory has no handshake.
func (Local) Probe(context.Context, Config) (ProbeResult, error) {
	return ProbeResult{Kind: "tls", ChainVerified: true}, nil
}

// Open implements Driver.
func (Local) Open(_ context.Context, c Config) (Remote, error) {
	lc, err := parseLocal(c.Config)
	if err != nil {
		return nil, err
	}
	return &localRemote{dir: lc.Dir}, nil
}

type localRemote struct{ dir string }

func (l *localRemote) path(name string) (string, error) {
	if err := CheckName(name); err != nil {
		return "", err
	}
	return filepath.Join(l.dir, name), nil
}

func (l *localRemote) Test(ctx context.Context, testName string) []Step {
	steps := []Step{{Name: "connect", OK: true}, {Name: "auth", OK: true}}
	fail := func(n string, err error) []Step { return append(steps, Step{Name: n, Code: CodeOf(err)}) }
	body := []byte("launchpad backup test")
	if err := l.Put(ctx, testName, int64(len(body)), strings.NewReader(string(body))); err != nil {
		return fail("write", err)
	}
	steps = append(steps, Step{Name: "write", OK: true})
	rc, err := l.Get(ctx, testName)
	if err != nil {
		return fail("read", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != string(body) {
		return fail("read", Errf(ServerError))
	}
	steps = append(steps, Step{Name: "read", OK: true})
	if err := l.Delete(ctx, testName); err != nil {
		return fail("delete", err)
	}
	return append(steps, Step{Name: "delete", OK: true})
}

func (l *localRemote) Put(_ context.Context, name string, size int64, r io.Reader) error {
	p, err := l.path(name)
	if err != nil {
		return err
	}
	if size < 0 {
		return Errf(TooLarge)
	}
	tmp, err := os.CreateTemp(l.dir, ".put-*")
	if err != nil {
		return MapNetError(err)
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(r, size+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return MapNetError(err)
	}
	if n != size { // too short or too long: nothing is stored
		return Errf(TooLarge)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return MapNetError(err)
	}
	return nil
}

func (l *localRemote) List(_ context.Context, prefix string) ([]Entry, error) {
	des, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, MapNetError(err)
	}
	var out []Entry
	var total int
	for _, de := range des {
		if !strings.HasPrefix(de.Name(), prefix) || de.IsDir() {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		total += len(de.Name()) + 32
		if total > MaxListBody {
			return nil, Errf(TooLarge)
		}
		out = append(out, Entry{Name: de.Name(), Size: fi.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (l *localRemote) Get(_ context.Context, name string) (io.ReadCloser, error) {
	p, err := l.path(name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, Errf(NotFound)
	}
	if err != nil {
		return nil, MapNetError(err)
	}
	return f, nil
}

func (l *localRemote) Delete(_ context.Context, name string) error {
	p, err := l.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return MapNetError(err)
	}
	return nil
}

func (l *localRemote) Close() error { return nil }
