package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/term"

	"devops/internal/config"
	"devops/internal/move"
	"devops/internal/platform"
	"devops/internal/storage"
)

// version is the app version written into a bundle manifest. Set it at build
// time with -ldflags "-X main.version=...".
var version = "dev"

// appVersion is version for code where a local variable hides it.
func appVersion() string { return version }

const moveExportUsage = `usage: server move-export [--no-attachments] [--user NAME] FILE
  Writes the whole install (database and attachment files) to FILE, encrypted
  with a passphrase the server makes. The passphrase is printed ONCE, alone, on
  stdout; keep it apart from the file. FILE must not exist.`

const moveImportUsage = `usage: server move-import [--passphrase-stdin] FILE
  Loads a bundle from move-export into this EMPTY install (no user yet, same
  migration version). The passphrase is read from a hidden prompt, or with
  --passphrase-stdin from the first line of stdin. It is never an argument or
  an environment variable.`

// moveEnv is what the move commands need from a running configuration.
type moveEnv struct {
	pool  *pgxpool.Pool
	keys  move.Keys
	store move.ObjectStore // nil when no S3_* is set
}

// moveKeys builds the keyrings the configuration allows. A missing key leaves
// its keyrings nil; the move then refuses only when a secret needs it.
func moveKeys(cfg config.Config) (move.Keys, error) {
	var k move.Keys
	var err error
	if len(cfg.SettingsKey) > 0 {
		if k.Settings, err = platform.NewKeyring(cfg.SettingsKey); err != nil {
			return k, err
		}
		if k.Git, err = platform.NewGitKeyring(cfg.SettingsKey); err != nil {
			return k, err
		}
		if k.Push, err = platform.NewPushKeyring(cfg.SettingsKey); err != nil {
			return k, err
		}
		if k.Slack, err = platform.NewSlackKeyring(cfg.SettingsKey); err != nil {
			return k, err
		}
	}
	if len(cfg.BackupKey) > 0 {
		if k.Backup, err = platform.NewBackupKeyring(cfg.BackupKey); err != nil {
			return k, err
		}
	}
	return k, nil
}

func openMoveEnv(ctx context.Context) (*moveEnv, func(), error) {
	pool, cfg, err := cliPool(ctx)
	if err != nil {
		return nil, nil, err
	}
	keys, err := moveKeys(cfg)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	env := &moveEnv{pool: pool, keys: keys}
	if cfg.AttachmentsEnabled() {
		st, err := storage.New(cfg.S3)
		if err != nil {
			pool.Close()
			return nil, nil, fmt.Errorf("attachments: %w", err)
		}
		env.store = st
	}
	return env, pool.Close, nil
}

type moveExportArgs struct {
	path          string
	user          string
	noAttachments bool
}

func parseMoveExportArgs(args []string, stderr io.Writer) (moveExportArgs, bool) {
	fs := flag.NewFlagSet("move-export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	noAtt := fs.Bool("no-attachments", false, "leave the attachment files out")
	user := fs.String("user", "", "the admin username to record in the manifest (optional)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, moveExportUsage)
		return moveExportArgs{}, false
	}
	return moveExportArgs{path: fs.Arg(0), user: *user, noAttachments: *noAtt}, true
}

// moveExportCmd: `server move-export FILE`. Exit 0 with the passphrase alone
// on stdout; 1 on an error (the reason on stderr, the file removed); 2 on bad
// usage. Nothing here writes a log line; the passphrase is never stored.
func moveExportCmd(args []string, stdout, stderr io.Writer) int {
	a, ok := parseMoveExportArgs(args, stderr)
	if !ok {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	env, closeEnv, err := openMoveEnv(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "move-export:", err)
		return 1
	}
	defer closeEnv()
	return runMoveExport(ctx, env, a, stdout, stderr)
}

func runMoveExport(ctx context.Context, env *moveEnv, a moveExportArgs, stdout, stderr io.Writer) int {
	var by uuid.UUID
	if a.user != "" {
		err := env.pool.QueryRow(ctx, `SELECT id FROM users WHERE username = $1 AND NOT is_agent`, strings.ToLower(strings.TrimSpace(a.user))).Scan(&by)
		if errors.Is(err, pgx.ErrNoRows) {
			fmt.Fprintf(stderr, "move-export: no human user %q\n", a.user)
			return 1
		} else if err != nil {
			fmt.Fprintln(stderr, "move-export:", err)
			return 1
		}
	}
	if !a.noAttachments && env.store == nil {
		fmt.Fprintln(stderr, "move-export: this server has no object store (S3_*); use --no-attachments to export the database alone")
		return 1
	}
	pass, err := move.GeneratePassphrase()
	if err != nil {
		fmt.Fprintln(stderr, "move-export:", err)
		return 1
	}
	f, err := os.OpenFile(a.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fmt.Fprintln(stderr, "move-export:", err)
		return 1
	}
	fail := func(err error) int {
		_ = f.Close()
		_ = os.Remove(a.path)
		fmt.Fprintln(stderr, "move-export:", err)
		return 1
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	m, err := move.Export(ctx, bw, move.ExportOptions{
		Pool: env.pool, Keys: env.keys, Store: env.store, Passphrase: pass, ExportedBy: by,
		AppVersion: version, IncludeAttachments: !a.noAttachments,
	})
	if err != nil {
		return fail(err)
	}
	if err := bw.Flush(); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(a.path)
		fmt.Fprintln(stderr, "move-export:", err)
		return 1
	}
	fmt.Fprintln(stdout, pass)
	fmt.Fprintf(stderr, "move-export: wrote %s: %d tables, %d rows, %d files (%d bytes), migration version %d.\n",
		a.path, len(m.Tables), m.TotalRows(), m.ObjectCount, m.Bytes, m.MigrationVersion)
	fmt.Fprintln(stderr, "move-export: the passphrase above is the only copy and opens everything in the file, secrets included; keep it apart from the file.")
	return 0
}

type moveImportArgs struct {
	path            string
	passphraseStdin bool
}

func parseMoveImportArgs(args []string, stderr io.Writer) (moveImportArgs, bool) {
	fs := flag.NewFlagSet("move-import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stdin := fs.Bool("passphrase-stdin", false, "read the passphrase from the first line of stdin")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, moveImportUsage)
		return moveImportArgs{}, false
	}
	return moveImportArgs{path: fs.Arg(0), passphraseStdin: *stdin}, true
}

// readPassphrase takes the passphrase from stdin (first line) or from a
// hidden prompt on the terminal. It never reads an argument or the environment.
func readPassphrase(stdin *os.File, fromStdin bool, stderr io.Writer) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			return "", errors.New("no passphrase on stdin")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	if !term.IsTerminal(int(stdin.Fd())) {
		return "", errors.New("stdin is not a terminal; use --passphrase-stdin")
	}
	fmt.Fprint(stderr, "Passphrase: ")
	b, err := term.ReadPassword(int(stdin.Fd()))
	fmt.Fprintln(stderr)
	if err != nil {
		return "", errors.New("could not read the passphrase")
	}
	return string(b), nil
}

// moveImportCmd: `server move-import FILE`. Exit 0 on success; 1 on a refusal
// or an error (the install is left in setup state); 2 on bad usage.
func moveImportCmd(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	a, ok := parseMoveImportArgs(args, stderr)
	if !ok {
		return 2
	}
	pass, err := readPassphrase(stdin, a.passphraseStdin, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "move-import:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	env, closeEnv, err := openMoveEnv(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "move-import:", err)
		return 1
	}
	defer closeEnv()
	return runMoveImport(ctx, env, a.path, pass, stderr)
}

func runMoveImport(ctx context.Context, env *moveEnv, path, pass string, stderr io.Writer) int {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(stderr, "move-import:", err)
		return 1
	}
	defer func() { _ = f.Close() }()
	m, err := move.Import(ctx, bufio.NewReaderSize(f, 1<<20), move.ImportOptions{
		Pool: env.pool, Keys: env.keys, Store: env.store, Passphrase: pass,
	})
	if err != nil {
		fmt.Fprintln(stderr, "move-import: refused, nothing was changed:", err)
		return 1
	}
	fmt.Fprintf(stderr, "move-import: loaded %d tables, %d rows and %d files (bundle from app %s, migration version %d).\n",
		len(m.Tables), m.TotalRows(), m.ObjectCount, m.AppVersion, m.MigrationVersion)
	fmt.Fprintln(stderr, "move-import: backup destinations are OFF and this install has a new backup install id; review them, then enable. Users sign in with their existing email.")
	return 0
}
