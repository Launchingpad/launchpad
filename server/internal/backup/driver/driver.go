// Package driver is the interface every backup destination type implements
// (T16 #16), the registry of the types this binary supports, the safe error
// codes (#32) and the reply-size caps. The runner knows no protocol.
//
// A driver reaches a user-entered host only through a *netguard.Dialer built
// on the backup policy (internal/backup.NewDialer). It never builds its own
// net.Dialer or http.Client.
package driver

import (
	"context"
	"encoding/json"
	"io"
	"sort"
	"sync"

	"devops/internal/netguard"
)

// Caps on what a driver reads from a remote (T16 #16; the 16 MiB list size is
// a guess).
const (
	// MaxListBody caps a list or PROPFIND reply.
	MaxListBody = 16 << 20
	// DownloadSlack is added to a declared object size for a download cap.
	DownloadSlack = 64 << 10
)

// Identity is the canonical (type, host, bucket, folder) the unique rule of
// T16 #25 compares. Host is lower-case without a default port; Folder has no
// trailing slash and, for object stores, no leading slash. A driver whose folder can be
// absolute (sftp) keeps its one leading slash: absolute and relative are different places.
type Identity struct {
	Type   string
	Host   string
	Bucket string
	Folder string
}

// Config is what a driver needs to talk to ONE destination. Secrets are the
// opened secrets JSON; a driver never logs, returns or wraps one.
type Config struct {
	Config  json.RawMessage // the typed JSON config of the type (no secrets)
	Secrets json.RawMessage // opened secrets; nil for a probe
	Dialer  *netguard.Dialer
	// AllowPlainHTTP is set by the caller when an http:// URL's host is in an
	// operator-listed range (T16 #34); a driver refuses http:// otherwise.
	AllowPlainHTTP bool
}

// ProbeResult is the outcome of phase 1 (T16 #36): the handshake without
// credentials. Nothing is sent after it.
type ProbeResult struct {
	Kind          string // "tls", "ssh", or "plain" (http://, no handshake)
	Fingerprint   string // leaf SPKI sha256 (hex) or SSH host key sha256
	ChainVerified bool   // TLS only: the chain verified for the host name
}

// Step is one line of a test result (connect, auth, write, read, delete).
type Step struct {
	Name string
	OK   bool
	Code Code // empty when OK
}

// Entry is one name in a remote listing. Size is -1 when unknown.
type Entry struct {
	Name string
	Size int64
}

// Remote is an opened destination. Every name passed to Put, Get and Delete
// satisfies remotename.Valid; a driver checks it again (see CheckName).
type Remote interface {
	// Test authenticates, writes a small random file, lists, reads it back,
	// compares and deletes it (T16 #36 phase 2). testName is the file name.
	Test(ctx context.Context, testName string) []Step
	// Put uploads exactly size bytes from r under name.
	Put(ctx context.Context, name string, size int64, r io.Reader) error
	// List returns the entries whose name starts with prefix. A reply larger
	// than MaxListBody is refused with too_large.
	List(ctx context.Context, prefix string) ([]Entry, error)
	// Get returns a reader for name. The caller closes it. The driver cannot
	// know the object's size: the CALLER wraps the reader with
	// CapReader(r, DownloadCap(size)) using the size from the manifest or run row.
	Get(ctx context.Context, name string) (io.ReadCloser, error)
	// Delete removes name. A missing name is not an error.
	Delete(ctx context.Context, name string) error
	Close() error
}

// Driver is one destination type.
type Driver interface {
	// Validate checks the typed JSON config of the type.
	Validate(config json.RawMessage) error
	// Identity returns the canonical identity of a valid config.
	Identity(config json.RawMessage) (Identity, error)
	// Probe is phase 1 of T16 #36: no credentials, nothing sent after the
	// handshake.
	Probe(ctx context.Context, c Config) (ProbeResult, error)
	// Open returns a Remote for a destination.
	Open(ctx context.Context, c Config) (Remote, error)
}

var (
	mu       sync.RWMutex
	registry = map[string]Driver{}
)

// Register adds a driver for a type. It PANICS on a duplicate type, an empty
// type or a nil driver: a second registration is a build mistake.
func Register(typ string, d Driver) {
	if typ == "" || d == nil {
		panic("driver: Register needs a type and a driver")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := registry[typ]; ok {
		panic("driver: duplicate driver type " + typ)
	}
	registry[typ] = d
}

// Lookup returns the driver of a type.
func Lookup(typ string) (Driver, bool) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := registry[typ]
	return d, ok
}

// SupportedTypes lists the registered types, sorted: the API's
// `supportedTypes`. A type with no driver is absent.
func SupportedTypes() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Exchanger is implemented by a driver whose destination needs a one-time
// authorization code turned into a long-lived token (Dropbox, T16 #9). The
// offsite worker calls it for an `exchange` run: c.Secrets holds the app key and
// secret, code is the single-use code. The returned refresh token is sealed by
// the worker; the driver never stores, logs or wraps it in an error.
type Exchanger interface {
	Exchange(ctx context.Context, c Config, code string) (refreshToken string, err error)
}
