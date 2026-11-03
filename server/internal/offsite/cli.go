package offsite

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/backup/remotename"
	"devops/internal/config"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/netguard"
	"devops/internal/platform"
)

// Usage is printed for a bad command line.
const Usage = `usage: server offsite [command]
  (no command)                 run the worker
  keygen                       make an age key pair; prints both keys (the private key too: keep stdout off logs)
  rotate-signing               replace the manifest signing key (prints the new PUBLIC key)
  resume --instance-id ID      write the instance guard file (deploy.sh --backup-resume)
  healthcheck                  exit 0 when the worker process is alive
  fetch (--destination NAME | --config FILE --install-id ID [--allow-cidr CIDR]) --stem STEM|latest
                               write one frame (manifest, object) to stdout
  unpack --out DIR [--max-object-bytes N]
                               read a frame from stdin; write object and manifest.json
  verify --public-key FILE --manifest FILE --object FILE [--install-id ID --stem STEM]
                               check the manifest signature and the object hash
  decrypt --identity FILE      decrypt an age file from stdin to stdout
  fetch ... --files [--stem STEM|latest]   the attachment list frame (signed manifest, encrypted list)
  fetch ... --file-object NAME             one attachment object (empty manifest)
  verify --files ...           check a files manifest and its list
  files-entry --install-id ID --key KEY    read a decrypted list on stdin; print remote, sha256, size`

// Main runs `server offsite <args>` and returns the exit code: 0 done, 1 refused
// or failed, 2 usage. Output of the data commands goes to stdout and NOTHING else
// does; messages go to stderr and never hold a secret or a remote's words.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}
	var err error
	code := 1
	switch cmd {
	case "":
		return runWorker(stderr, getenv)
	case "keygen":
		code, err = cmdKeygen(args, stdout)
	case "healthcheck":
		code, err = cmdHealthcheck(args, getenv)
	case "resume":
		code, err = cmdResume(args, getenv)
	case "rotate-signing":
		code, err = cmdRotateSigning(args, stdout, getenv)
	case "fetch":
		code, err = cmdFetch(args, stdout, getenv)
	case "unpack":
		code, err = cmdUnpack(args, stdin)
	case "verify":
		code, err = cmdVerify(args, stdout)
	case "decrypt":
		code, err = cmdDecrypt(args, stdin, stdout)
	case "files-entry":
		code, err = cmdFilesEntry(args, stdin, stdout)
	default:
		fmt.Fprintln(stderr, Usage)
		return 2
	}
	if err != nil {
		if code == 2 {
			fmt.Fprintln(stderr, "offsite "+cmd+": "+err.Error())
			fmt.Fprintln(stderr, Usage)
		} else {
			fmt.Fprintln(stderr, "offsite "+cmd+": "+safeMessage(err))
		}
	}
	return code
}

func usageErr(msg string) (int, error) { return 2, errors.New(msg) }

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// runWorker is `server offsite` with no command.
func runWorker(stderr io.Writer, getenv func(string) string) int {
	cfg, err := config.LoadOffsite(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "offsite: config:", err)
		return 1
	}
	level := slog.LevelInfo
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	policy, err := backup.PolicyFromEnv(getenv, backup.Gateways{})
	if err != nil {
		fmt.Fprintln(stderr, "offsite:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(stderr, "offsite: cannot open the database:", err)
		return 1
	}
	defer pool.Close()
	w, err := New(Options{Pool: pool, DSN: cfg.DatabaseURL, Log: log, Key: cfg.BackupKey, Policy: policy,
		DumpDir: cfg.DumpDir, WorkDir: cfg.WorkDir, HealthFile: cfg.HealthFile, Version: cfg.Version,
		Silo: SiloConfig{Endpoint: cfg.S3Endpoint, Bucket: cfg.S3Bucket, AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey, Region: cfg.S3Region}})
	if err != nil {
		fmt.Fprintln(stderr, "offsite:", err)
		return 1
	}
	log.Info("offsite: started", "log_env", cfg.LogEnv, "files_backup", cfg.S3Endpoint != "")
	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("offsite: stopped", "err", err)
		return 1
	}
	return 0
}

// cmdKeygen prints a fresh age pair. The private key never touches the server's
// database or log; this command is for deploy.sh --backup-keygen in a throwaway
// container with no network (decisions-T16 #19).
func cmdKeygen(args []string, stdout io.Writer) (int, error) {
	if len(args) != 0 {
		return usageErr("keygen takes no arguments")
	}
	secret, public, err := backup.GenerateKeyPair()
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(stdout, "public key:  %s\nprivate key: %s\n", public, secret)
	return 0, nil
}

// healthMaxAge: the health file must have been touched this recently.
const healthMaxAge = 3 * time.Minute

func cmdHealthcheck(args []string, getenv func(string) string) (int, error) {
	if len(args) != 0 {
		return usageErr("healthcheck takes no arguments")
	}
	path := getenv("OFFSITE_HEALTH_FILE")
	if path == "" {
		path = "/tmp/offsite.alive"
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 1, errors.New("offsite: the worker has not started")
	}
	if time.Since(fi.ModTime()) > healthMaxAge {
		return 1, errors.New("offsite: the worker is not running")
	}
	return 0, nil
}

// cmdResume writes the instance guard file (decisions-T16 #37). It needs only the
// scratch volume: no database, no network.
func cmdResume(args []string, getenv func(string) string) (int, error) {
	fs := newFlags("resume")
	id := fs.String("instance-id", "", "the new instance id (a uuid)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *id == "" {
		return usageErr("resume needs --instance-id ID")
	}
	u, err := uuid.Parse(*id)
	if err != nil {
		return usageErr("--instance-id must be a uuid")
	}
	dir := getenv("OFFSITE_WORK_DIR")
	if dir == "" {
		dir = "/work"
	}
	if err := WriteInstanceFile(dir, u); err != nil {
		return 1, err
	}
	return 0, nil
}

// SigningKeyReplaced is the change-log kind the signing trigger requires (#22).
const SigningKeyReplaced = "signing_key_replaced"

// cmdRotateSigning replaces the manifest signing key (decisions-T16 #22). It makes
// the pair, seals the private half, and in ONE transaction (no savepoint, as the
// trigger needs) writes a signing_key_replaced change-log row (no actor, no RETURNING:
// the role cannot read the table) and the new key. It prints the new PUBLIC key only.
func cmdRotateSigning(args []string, stdout io.Writer, getenv func(string) string) (int, error) {
	if len(args) != 0 {
		return usageErr("rotate-signing takes no arguments")
	}
	cfg, err := config.LoadOffsite(getenv)
	if err != nil {
		return 1, fmt.Errorf("offsite: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return 1, errors.New("offsite: cannot open the database")
	}
	defer pool.Close()
	keys, err := platform.NewBackupKeyring(cfg.BackupKey)
	if err != nil {
		return 1, err
	}
	pub, err := rotateSigning(ctx, pool, keys)
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(stdout, "new signing public key: %s\n", pub)
	return 0, nil
}

type txBeginner interface {
	BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error)
}

func rotateSigning(ctx context.Context, db txBeginner, keys *platform.BackupKeyring) (string, error) {
	pk, sk, err := backup.GenerateSigningKey()
	if err != nil {
		return "", err
	}
	blob, err := keys.Seal(platform.BackupColSigningKey, platform.BackupSettingsOwner, sk)
	if err != nil {
		return "", err
	}
	pub := backup.EncodePublicKey(pk)
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(pub))
	detail := []byte(`{"newKeyFingerprint":"` + hex.EncodeToString(sum[:])[:12] + `"}`)
	err = pgx.BeginTxFunc(ctx, db, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if err := q.InsertBackupChangeLogNoReturn(ctx, dbq.InsertBackupChangeLogNoReturnParams{
			ID: id, Kind: SigningKeyReplaced, Detail: detail,
		}); err != nil {
			return err
		}
		return q.ReplaceBackupSigningKey(ctx, dbq.ReplaceBackupSigningKeyParams{SigningPublic: pub, SigningPrivateEnc: blob})
	})
	if err != nil {
		return "", errors.New("offsite: cannot replace the signing key")
	}
	return pub, nil
}

// cmdFetch writes one frame to stdout (decisions-T16 #39).
func cmdFetch(args []string, stdout io.Writer, getenv func(string) string) (int, error) {
	fs := newFlags("fetch")
	destName := fs.String("destination", "", "a saved destination (by name)")
	cfgFile := fs.String("config", "", "a JSON file with the destination's config and credentials")
	installID := fs.String("install-id", "", "with --config: the install id of the remote files")
	allow := fs.String("allow-cidr", "", "with --config: a comma list of private CIDRs the fetch may reach (#35)")
	stem := fs.String("stem", "latest", "the backup stem, or latest")
	files := fs.Bool("files", false, "fetch the attachment (files) list and its manifest instead of a dump")
	fileObject := fs.String("file-object", "", "fetch one attachment object by its remote name (from the list)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usageErr("bad arguments")
	}
	if *files && *fileObject != "" {
		return usageErr("give at most one of --files and --file-object")
	}
	if (*destName == "") == (*cfgFile == "") {
		return usageErr("give exactly one of --destination and --config")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var (
		typ     string
		cfg     []byte
		secrets []byte
		dialer  *netguard.Dialer
		install string
	)
	if *cfgFile != "" {
		if *installID == "" {
			return usageErr("--config needs --install-id")
		}
		cf, err := ReadConfigFile(*cfgFile)
		if err != nil {
			return 1, err
		}
		pol, err := backup.Policy(*allow, backup.Gateways{})
		if err != nil {
			return 1, err
		}
		typ, cfg, secrets, install = cf.Type, cf.Config, cf.Secrets, *installID
		dialer = backup.NewDialer(pol, dialTimeout)
	} else {
		oc, err := config.LoadOffsite(getenv)
		if err != nil {
			return 1, fmt.Errorf("offsite: config: %w", err)
		}
		pol, err := backup.PolicyFromEnv(getenv, backup.Gateways{})
		if err != nil {
			return 1, err
		}
		pool, err := database.Open(ctx, oc.DatabaseURL)
		if err != nil {
			return 1, errors.New("offsite: cannot open the database")
		}
		defer pool.Close()
		keys, err := platform.NewBackupKeyring(oc.BackupKey)
		if err != nil {
			return 1, err
		}
		q := dbq.New(pool)
		d, err := q.GetBackupDestinationByName(ctx, *destName)
		if err != nil {
			return 1, errors.New("offsite: no such destination")
		}
		s, err := q.GetBackupSettings(ctx)
		if err != nil {
			return 1, errors.New("offsite: cannot read the backup settings")
		}
		secrets = []byte(`{}`)
		if d.SecretsEnc != nil {
			if secrets, err = keys.Open(platform.BackupColDestinationSecrets, d.ID.String(), d.SecretsEnc); err != nil {
				return 1, driver.Errf(driver.SecretsUnreadable)
			}
		}
		typ, cfg, install = d.Type, d.Config, s.InstallID
		dialer = backup.NewDialer(pol, dialTimeout)
	}
	drv, ok := driver.Lookup(typ)
	if !ok {
		return 1, driver.Errf(driver.ServerError)
	}
	dcfg := driver.Config{Config: cfg, Secrets: secrets, Dialer: dialer,
		// http:// only when the operator listed a range; the driver checks the
		// connected address at dial time.
		AllowPlainHTTP: len(dialer.Policy.Private) > 0}
	sub := ""
	var err error
	switch {
	case *files:
		sub, err = remotename.FilesListFolder(install)
	case *fileObject != "":
		sub, err = remotename.FilesObjectFolder(install)
	}
	if err != nil {
		return 1, err
	}
	if sub != "" {
		if dcfg.Config, err = subFolderConfig(drv, cfg, sub); err != nil {
			return 1, err
		}
	}
	remote, err := drv.Open(ctx, dcfg)
	if err != nil {
		return 1, err
	}
	defer remote.Close()
	bw := bufio.NewWriterSize(stdout, 256<<10)
	switch {
	case *files:
		err = FetchFilesFrame(ctx, remote, install, *stem, bw)
	case *fileObject != "":
		err = FetchFilesObjectFrame(ctx, remote, install, *fileObject, bw)
	default:
		err = FetchFrame(ctx, remote, install, *stem, bw)
	}
	if err != nil {
		return 1, err
	}
	if err := bw.Flush(); err != nil {
		return 1, errors.New("offsite: cannot write the frame")
	}
	return 0, nil
}

func cmdUnpack(args []string, stdin io.Reader) (int, error) {
	fs := newFlags("unpack")
	out := fs.String("out", "", "the directory to write object and manifest.json into")
	max := fs.Int64("max-object-bytes", MaxObjectBytes, "the largest object accepted")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *out == "" {
		return usageErr("unpack needs --out DIR")
	}
	if err := Unpack(stdin, *out, *max, nil); err != nil {
		return 1, err
	}
	return 0, nil
}

// cmdVerify checks a fetched pair with no network and no database (#39): the
// Ed25519 signature of the manifest first, then (when given) the binding to the
// install id and stem, then the object's sha256 and size against the manifest.
func cmdVerify(args []string, stdout io.Writer) (int, error) {
	fs := newFlags("verify")
	keyFile := fs.String("public-key", "", "the saved signing public key (a file)")
	manFile := fs.String("manifest", "", "the manifest file")
	objFile := fs.String("object", "", "the encrypted object file")
	installID := fs.String("install-id", "", "bind the manifest to this install id")
	stem := fs.String("stem", "", "bind the manifest to this stem")
	files := fs.Bool("files", false, "the manifest and object are a files manifest and its encrypted list")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *keyFile == "" || *manFile == "" || *objFile == "" {
		return usageErr("verify needs --public-key, --manifest and --object")
	}
	if (*installID == "") != (*stem == "") {
		return usageErr("--install-id and --stem go together")
	}
	kb, err := readSmall(*keyFile, 4096)
	if err != nil {
		return 1, err
	}
	pub, err := backup.ParsePublicKey(string(kb))
	if err != nil {
		return 1, err
	}
	mb, err := readSmall(*manFile, backup.MaxManifestBytes)
	if err != nil {
		return 1, err
	}
	if *files {
		return verifyFiles(pub, mb, *objFile, *installID, *stem, stdout)
	}
	m, err := backup.VerifyManifest(pub, mb)
	if err != nil {
		return 1, err
	}
	if *installID != "" {
		if err := m.Bind(*installID, *stem); err != nil {
			return 1, err
		}
	}
	f, err := os.Open(*objFile)
	if err != nil {
		return 1, errors.New("offsite: cannot open the object file")
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 1, errors.New("offsite: cannot read the object file")
	}
	if err := m.CheckObject(hex.EncodeToString(h.Sum(nil)), n); err != nil {
		return 1, err
	}
	fmt.Fprintf(stdout, "ok\ninstallId: %s\nobject: %s\ndumpTime: %s\nschemaVersion: %d\nlaunchpadVersion: %s\n",
		m.InstallID, m.Object, m.DumpTime, m.SchemaVersion, m.LaunchpadVersion)
	return 0, nil
}

// verifyFiles is `verify --files`: the files manifest's signature, its binding, then
// the encrypted list's size and sha256.
func verifyFiles(pub ed25519.PublicKey, mb []byte, objFile, installID, stem string, stdout io.Writer) (int, error) {
	m, err := backup.VerifyFilesManifest(pub, mb)
	if err != nil {
		return 1, err
	}
	if installID != "" {
		if err := m.Bind(installID, stem); err != nil {
			return 1, err
		}
	}
	f, err := os.Open(objFile)
	if err != nil {
		return 1, errors.New("offsite: cannot open the object file")
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 1, errors.New("offsite: cannot read the object file")
	}
	if err := m.CheckList(hex.EncodeToString(h.Sum(nil)), n); err != nil {
		return 1, err
	}
	fmt.Fprintf(stdout, "ok\ninstallId: %s\nlist: %s\ntime: %s\nobjects: %d\nbytes: %d\nlaunchpadVersion: %s\n",
		m.InstallID, m.List, m.Time, m.Objects, m.Bytes, m.LaunchpadVersion)
	return 0, nil
}

// cmdFilesEntry reads a decrypted list from stdin and prints "remote sha256 size" of
// one key (tab separated). No network, no database.
func cmdFilesEntry(args []string, stdin io.Reader, stdout io.Writer) (int, error) {
	fs := newFlags("files-entry")
	installID := fs.String("install-id", "", "the install id of the list")
	key := fs.String("key", "", "the silo key (org/<org id>/<yyyy>/<uuid>/<filename>)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *installID == "" || *key == "" {
		return usageErr("files-entry needs --install-id and --key")
	}
	if !remotename.ValidInstallID(*installID) {
		return usageErr("bad install id")
	}
	var found *backup.FileEntry
	_, err := backup.ReadFileList(stdin, *installID, func(e backup.FileEntry) bool {
		if e.Key == *key {
			c := e
			found = &c
			return false
		}
		return true
	})
	if err != nil {
		return 1, err
	}
	if found == nil {
		return 1, errors.New("offsite: the key is not in the list")
	}
	fmt.Fprintf(stdout, "%s\t%s\t%d\n", found.Remote, found.SHA256, found.Size)
	return 0, nil
}

func readSmall(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("offsite: cannot open a file")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, errors.New("offsite: cannot read a file")
	}
	if int64(len(b)) > max {
		return nil, backup.ErrManifestTooLarge
	}
	return b, nil
}

// cmdDecrypt decrypts an age file from stdin to stdout with a private key file.
// The plain dump goes only to stdout (decisions-T16 #39).
func cmdDecrypt(args []string, stdin io.Reader, stdout io.Writer) (int, error) {
	fs := newFlags("decrypt")
	idFile := fs.String("identity", "", "the age private key (a file)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *idFile == "" {
		return usageErr("decrypt needs --identity FILE")
	}
	b, err := readSmall(*idFile, 64<<10)
	if err != nil {
		return 1, err
	}
	id := ""
	for _, line := range bytes.Split(b, []byte("\n")) {
		l := strings.TrimSpace(string(line))
		if l != "" && !strings.HasPrefix(l, "#") {
			id = l
			break
		}
	}
	bw := bufio.NewWriterSize(stdout, 256<<10)
	if _, err := backup.Decrypt(bw, stdin, id, 0); err != nil {
		return 1, err
	}
	if err := bw.Flush(); err != nil {
		return 1, errors.New("offsite: cannot write the plain dump")
	}
	return 0, nil
}
