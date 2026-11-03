// Command server is the dev-ops API. Boot order: read env, migrate the
// database to the newest migration, print a first-run setup link while the
// install has no user, read the platform mail settings,
// start the event listener, the idempotency sweeper, the cycle
// job, the login mail queue, the sign-up mail queue and the mail settings
// refresh, then serve until SIGTERM.
//
// Subcommands (os.Args[1]), for an operator inside the container:
//
//	server                                   serve (also: server serve)
//	server setup-link                        while no user exists: a fresh setup
//	                                         link on stdout (older links stop
//	                                         working); nothing is logged
//	server grant-platform-admin --user NAME  make an existing human user a
//	                                         platform admin (an install from
//	                                         before first-run setup)
//	server mint-token --org SLUG --user NAME --name N --scopes read,write[,admin]
//	       [--expires-days N]                an org-pinned PAT for a member (an
//	                                         agent such as claude, or the rescue
//	                                         when every admin token is lost); the
//	                                         secret goes to stdout once, nothing
//	                                         is logged
//	server backup-recipient-remove --fingerprint F
//	                                         remove an age backup recipient at once
//	                                         (the host path of the 24 hour hold, LAUN-69)
//	server backup-recipient-cancel --fingerprint F
//	                                         cancel a pending add or removal at once
//	server offsite [keygen|rotate-signing|resume|healthcheck|fetch|unpack|verify|decrypt]
//	                                         the backup uploader and its host commands
//	                                         (LAUN-71, decisions-T16 #10); no command runs the worker
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxuuid "github.com/vgarvardt/pgx-google-uuid/v5"

	"devops/internal/api"
	"devops/internal/auth"
	_ "devops/internal/backup/driver/dropbox" // registers the dropbox destination type (T16 ticket 10)
	_ "devops/internal/backup/driver/s3"      // registers the s3 destination type (T16 ticket 6)
	_ "devops/internal/backup/driver/sftp"    // registers the sftp destination type (T16 ticket 9)
	_ "devops/internal/backup/driver/webdav"  // registers the webdav destination type (T16 ticket 7)
	"devops/internal/backupapi"
	"devops/internal/backupnotice"
	"devops/internal/config"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/gitapi"
	"devops/internal/gitapi/clients"
	"devops/internal/httpx"
	"devops/internal/idempotency"
	"devops/internal/importjob"
	"devops/internal/login"
	"devops/internal/netguard"
	"devops/internal/notify"
	"devops/internal/offsite"
	"devops/internal/platform"
	"devops/internal/setup"
	"devops/internal/signup"
	"devops/internal/storage"
	"devops/internal/tracker"
)

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "", "serve":
		if err := run(); err != nil {
			slog.Error("server stopped", "err", err)
			os.Exit(1)
		}
	case "setup-link":
		os.Exit(setupLinkCmd(os.Args[2:], os.Stdout, os.Stderr))
	case "grant-platform-admin":
		os.Exit(grantPlatformAdminCmd(os.Args[2:], os.Stdout, os.Stderr))
	case "mint-token":
		os.Exit(mintTokenCmd(os.Args[2:], os.Stdout, os.Stderr))
	case "backup-recipient-remove":
		os.Exit(backupRecipientCmd(os.Args[2:], os.Stdout, os.Stderr, false))
	case "backup-recipient-cancel":
		os.Exit(backupRecipientCmd(os.Args[2:], os.Stdout, os.Stderr, true))
	case "offsite":
		// The backup uploader (LAUN-71, decisions-T16 #10): its own small config.
		os.Exit(offsite.Main(os.Args[2:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (want serve, setup-link, grant-platform-admin --user <username>, mint-token --org <slug> --user <username> ..., backup-recipient-remove --fingerprint <64 hex>, backup-recipient-cancel --fingerprint <64 hex> or offsite [command])\n", cmd)
		os.Exit(2)
	}
}

// cliPool opens the database for a subcommand. It does not migrate: the
// subcommands run beside a server that has booted (and migrated) already.
func cliPool(ctx context.Context) (*pgxpool.Pool, config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, cfg, fmt.Errorf("config: %w", err)
	}
	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, cfg, err
	}
	return pool, cfg, nil
}

// setupLinkCmd: `server setup-link`. Exit 0 with the link alone on stdout;
// 3 when setup is complete (no link); 1 on an error; 2 on bad usage. The
// link goes to stdout only, never to the log.
func setupLinkCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: server setup-link (no arguments)")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, cfg, err := cliPool(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "setup-link:", err)
		return 1
	}
	defer pool.Close()
	secret, minted, err := setup.MintIfRequired(ctx, pool)
	if err != nil {
		fmt.Fprintln(stderr, "setup-link:", err, "(has the server booted on this database, so it is migrated?)")
		return 1
	}
	if !minted {
		fmt.Fprintln(stderr, "setup-link: setup is complete (this install has a user), so there is no setup link")
		return 3
	}
	fmt.Fprintln(stdout, setup.Link(cfg.PublicBaseURL, secret))
	fmt.Fprintln(stderr, "setup-link: valid 24h, single use; every older setup link no longer works")
	if cfg.PublicBaseURL == "" {
		fmt.Fprintln(stderr, "setup-link: PUBLIC_BASE_URL is unset; put the web app's address in front of the path")
	}
	return 0
}

// grantPlatformAdminCmd: `server grant-platform-admin --user <username>`.
func grantPlatformAdminCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("grant-platform-admin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	user := fs.String("user", "", "the username to make a platform admin")
	if err := fs.Parse(args); err != nil || *user == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: server grant-platform-admin --user <username>")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, _, err := cliPool(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "grant-platform-admin:", err)
		return 1
	}
	defer pool.Close()
	already, err := setup.GrantPlatformAdmin(ctx, pool, *user)
	if err != nil {
		fmt.Fprintln(stderr, "grant-platform-admin:", err)
		return 1
	}
	if already {
		fmt.Fprintf(stdout, "%s is a platform admin already; nothing changed\n", *user)
		return 0
	}
	fmt.Fprintf(stdout, "%s is now a platform admin. A token or session made before this keeps its scopes: log in again (or use a token with the admin scope and no org pin) for the platform routes\n", *user)
	return 0
}

// backupRecipientCmd: `server backup-recipient-remove --fingerprint F` and
// `server backup-recipient-cancel --fingerprint F` (LAUN-69, decisions-T16 #20).
// The fingerprint is the FULL sha256 (64 hex) of the age public key. Remove
// takes the key out at once; cancel drops a pending add or a pending removal.
// Both write a change-log row with no actor and need no BACKUP_ENCRYPTION_KEY.
// Remove refuses to leave no active recipient while a destination is enabled.
// Exit 0 done; 1 refused or failed; 2 usage.
func backupRecipientCmd(args []string, stdout, stderr io.Writer, cancel bool) int {
	name := "backup-recipient-remove"
	if cancel {
		name = "backup-recipient-cancel"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fp := fs.String("fingerprint", "", "the full sha256 (64 hex characters) of the age public key")
	if err := fs.Parse(args); err != nil || *fp == "" || fs.NArg() != 0 {
		fmt.Fprintf(stderr, "usage: server %s --fingerprint <64 hex characters>\n", name)
		return 2
	}
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	pool, _, err := cliPool(ctx)
	if err != nil {
		fmt.Fprintln(stderr, name+":", err)
		return 1
	}
	defer pool.Close()
	svc, err := backupapi.New(pool, slog.New(slog.NewTextHandler(stderr, nil)), backupapi.Options{})
	if err != nil {
		fmt.Fprintln(stderr, name+":", err)
		return 1
	}
	return backupRecipient(ctx, svc, *fp, cancel, stdout, stderr)
}

// backupRecipient does the work of backupRecipientCmd on a service.
func backupRecipient(ctx context.Context, svc *backupapi.Service, fp string, cancel bool, stdout, stderr io.Writer) int {
	if cancel {
		what, err := svc.HostCancelRecipient(ctx, fp)
		if err != nil {
			fmt.Fprintln(stderr, "backup-recipient-cancel:", err)
			return 1
		}
		fmt.Fprintf(stdout, "cancelled the pending %s of recipient %s\n", what, fp[:12])
		return 0
	}
	if err := svc.HostRemoveRecipient(ctx, fp); err != nil {
		fmt.Fprintln(stderr, "backup-recipient-remove:", err)
		return 1
	}
	fmt.Fprintf(stdout, "removed recipient %s; backups made from now on are not encrypted to it\n", fp[:12])
	return 0
}

// mintTokenUsage is the mint-token usage line.
const mintTokenUsage = "usage: server mint-token --org <slug> --user <username> --name <token name> --scopes read,write[,admin] [--expires-days N]"

// mintTokenArgs are the parsed mint-token flags.
type mintTokenArgs struct {
	org string
	in  tracker.TokenInput
}

// parseMintTokenArgs reads the mint-token flags. --expires-days is a pointer
// only when given, so "not given" (no expiry, a human only) and a bad 0 stay
// apart, as expiresInDays does in POST /tokens.
func parseMintTokenArgs(args []string, stderr io.Writer) (mintTokenArgs, bool) {
	fs := flag.NewFlagSet("mint-token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	org := fs.String("org", "", "the org slug the token is pinned to")
	user := fs.String("user", "", "the username the token belongs to (an active member of the org)")
	name := fs.String("name", "", "the token name (1-200 characters)")
	scopes := fs.String("scopes", "", "comma list of read, write, admin (admin only for an org admin)")
	days := fs.Int("expires-days", 0, "days until it expires: required (1-365) for an agent, optional (1-3650) for a human")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *org == "" || *user == "" || *name == "" || *scopes == "" {
		fmt.Fprintln(stderr, mintTokenUsage)
		return mintTokenArgs{}, false
	}
	sc := strings.Split(*scopes, ",")
	for i := range sc {
		sc[i] = strings.TrimSpace(sc[i])
	}
	a := mintTokenArgs{org: *org, in: tracker.TokenInput{Name: *name, User: *user, Scopes: sc}}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "expires-days" {
			d := *days
			a.in.ExpiresInDays = &d
		}
	})
	return a, true
}

// mintTokenCmd: `server mint-token ...`. Exit 0 with the secret alone on
// stdout; 1 on a refusal or an error (the reason on stderr); 2 on bad usage.
// The secret is never logged: this command writes no log line at all.
func mintTokenCmd(args []string, stdout, stderr io.Writer) int {
	a, ok := parseMintTokenArgs(args, stderr)
	if !ok {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, _, err := cliPool(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "mint-token:", err)
		return 1
	}
	defer pool.Close()
	return mintToken(ctx, pool, a, stdout, stderr)
}

// mintToken mints through tracker.MintOperatorToken and prints the result:
// the secret (and nothing else) on stdout, the description on stderr.
func mintToken(ctx context.Context, pool *pgxpool.Pool, a mintTokenArgs, stdout, stderr io.Writer) int {
	t, err := tracker.New(pool).MintOperatorToken(ctx, a.org, a.in)
	if err != nil {
		var he *httpx.Error
		switch {
		case errors.As(err, &he):
			fmt.Fprintln(stderr, "mint-token: refused:", he.Msg)
		case errors.Is(err, tracker.ErrMintNotFound):
			fmt.Fprintln(stderr, "mint-token: refused:", err, "(an agent is made with POST /api/v1/orgs/<org>/agents)")
		default:
			fmt.Fprintln(stderr, "mint-token:", err, "(has the server booted on this database, so it is migrated?)")
		}
		return 1
	}
	fmt.Fprintln(stdout, t.Secret)
	exp := "never"
	if t.Token.ExpiresAt != nil {
		exp = t.Token.ExpiresAt.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(stderr, "mint-token: token %q (id %s, prefix %s) for %s in org %s; scopes %s; expires %s.\n",
		t.Token.Name, t.Token.ID, t.Token.Prefix, t.Token.User.Username, strings.ToLower(strings.TrimSpace(a.org)),
		strings.Join(t.Token.Scopes, ","), exp)
	fmt.Fprintln(stderr, "mint-token: the secret above is the only copy (only its hash is stored); revoke it with DELETE /api/v1/orgs/<org>/tokens/<id>")
	return 0
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	level := slog.LevelInfo
	if cfg.LogEnv == "local" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	version, err := database.Migrate(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	log.Info("migrate: complete", "version", version)

	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// First-run setup: while no user exists, every boot mints a
	// new link (closing the older ones) and logs it once. That line is the
	// only credential this server ever logs.
	if secret, minted, err := setup.MintIfRequired(ctx, pool); err != nil {
		return fmt.Errorf("setup: %w", err)
	} else if minted {
		setup.LogLink(log, cfg.PublicBaseURL, secret)
	}

	// Background workers. Each exits when bgCtx is cancelled; wg waits for
	// them before the pool closes (golang-concurrency: every goroutine has
	// an owner and an exit).
	bgCtx, cancelBg := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancelBg()
		wg.Wait()
	}()

	// Login email: the login service sends through a switch,
	// and the platform mail settings choose its transport (provider env is
	// the environment's, as before). Boot reads them once; a PUT applies at
	// once; every server re-reads them each minute.
	envMail, err := environmentMail(cfg, log)
	if err != nil {
		return err
	}
	mailSwitch := login.NewMailSwitch()
	plat, err := platform.New(pool, log, platform.Options{Key: cfg.SettingsKey, Env: envMail, Switch: mailSwitch})
	if err != nil {
		return err
	}
	if cfg.SettingsKey == nil {
		log.Warn("mail settings: SETTINGS_ENCRYPTION_KEY is unset, so this server cannot store a mail secret (Postmark token, SMTP password); docs/deploy.md 'Mail settings'")
	}
	// Backups (LAUN-68, decisions-T16 #12): BACKUP_ENCRYPTION_KEY is optional at
	// boot. The API tickets build the keyring from it; here it is only checked,
	// so a key that cannot be used stops the boot early. Never logged.
	if cfg.BackupKey == nil {
		log.Info("backups: BACKUP_ENCRYPTION_KEY is unset, so this server cannot store a backup secret until it is set")
	} else if _, err := platform.NewBackupKeyring(cfg.BackupKey); err != nil {
		return err
	}
	if err := plat.Refresh(ctx); err != nil {
		log.Error("mail settings: could not read them at boot; login email uses the environment's settings until the next refresh", "err", err)
		plat.UseEnvironment()
	}
	mailQueue := login.NewQueue(mailSwitch, log, 64)
	wg.Go(func() { mailQueue.Run(bgCtx) })
	wg.Go(func() { plat.Run(bgCtx, platform.RefreshInterval) })

	queries := dbq.New(pool)
	broker := events.NewBroker()
	svc := tracker.New(pool)
	svc.SetPublicBaseURL(cfg.PublicBaseURL)
	// The sealed git secrets (T17 #11): the second keyring scope of the same
	// SETTINGS_ENCRYPTION_KEY. Without the key a sealed secret reads as
	// unreadable and its webhook answers 401; the boot goes on.
	if cfg.SettingsKey != nil {
		gitKeys, err := platform.NewGitKeyring(cfg.SettingsKey)
		if err != nil {
			return err
		}
		svc.SetGitSealer(gitKeys)
	}
	// Outbound calls to git hosts (T17 #31, LAUN-7): ONE dial-time guard. The
	// git policy refuses the always-refused ranges, the metadata addresses and
	// every subnet attached to this container (re-read hourly). Until the first
	// read succeeds the guard fails closed (every private address refused).
	attached := netguard.NewAttached(nil, nil)
	if err := attached.Refresh(); err != nil {
		log.Error("git hosts: cannot read this container's network addresses; private git hosts stay refused until a later read succeeds", "err", err)
	}
	wg.Go(func() {
		attached.Run(bgCtx, attachedRefreshInterval, func(err error) {
			log.Error("git hosts: could not re-read this container's network addresses", "err", err)
		})
	})
	gitNet, err := gitHostAccess(cfg, attached)
	if err != nil {
		return err
	}
	svc.SetGitNet(gitNet)
	store, err := attachmentStore(ctx, cfg, log, svc)
	if err != nil {
		return err
	}
	srv := api.NewServer(
		svc,
		auth.NewAuthenticator(queries, log),
		broker,
		idempotency.Middleware(queries, log, idempotency.Options{}),
		pool, log,
	)
	srv.Login = login.New(pool, log, mailQueue.Deliver, login.Options{
		Floor: loginFloor, FixedCode: cfg.FixedOTP, Pepper: []byte(cfg.OTPPepper), Available: mailSwitch.Ready,
	})
	srv.Notify().SetMailReady(notify.MailReady(mailSwitch.Ready, cfg.PublicBaseURL))
	srv.Setup = setup.New(pool, setup.Options{Floor: loginFloor})
	// Sign-up requests and invites: their email goes through
	// the same switch as login codes, on a queue this function owns.
	signups := signup.New(pool, log, signup.Options{
		Floor: loginFloor, Pepper: []byte(cfg.OTPPepper), PublicBaseURL: cfg.PublicBaseURL, Mailer: mailSwitch,
	})
	srv.Signup = signups
	srv.TrustedProxies = cfg.TrustedProxies
	wg.Go(func() { signups.Run(bgCtx) })
	srv.Platform = plat
	// Scheduled backups (LAUN-69): the /platform/backups routes. They only
	// queue rows for offsite; BACKUP_ENCRYPTION_KEY may be unset (checked above).
	backups, err := backupapi.New(pool, log, backupapi.Options{Key: cfg.BackupKey, MailReady: mailSwitch.Ready, FilesStored: func() bool { return store != nil }})
	if err != nil {
		return err
	}
	srv.Backups = backups
	// Backup notices (LAUN-75, decisions-T16 #41): run mail (at-most-once), change-log
	// mail (at-least-once) and the stale alert, every minute, through the platform
	// mail switch; and the hourly row cleanup (#29). Both are safe on several instances.
	backupNotices := backupnotice.New(pool, log, backupnotice.Options{Mailer: mailSwitch, PublicBaseURL: cfg.PublicBaseURL})
	wg.Go(func() { backupNotices.Run(bgCtx, backupnotice.Interval) })
	wg.Go(func() { backups.RunCleanup(bgCtx, backupapi.CleanupInterval, log) })
	// The web Linear import (LAUN-8): jobs run in this process. The sweep
	// marks a job whose owner died interrupted and cleans the work directory.
	// A work directory that cannot be used never stops the boot: the session
	// start then answers an error and the log says why.
	importCfg := importjob.Config{Pool: pool, WorkDir: cfg.ImportWorkDir, PublicBaseURL: cfg.PublicBaseURL, Log: log}
	if store != nil { // a nil *S3 in the interface would not be nil
		importCfg.Store = store
	}
	imports := importjob.New(importCfg)
	if err := imports.EnsureWorkDir(); err != nil {
		log.Error("import: the work directory is not usable; starting an import session fails until it is fixed (IMPORT_WORK_DIR, docs/deploy.md)", "dir", cfg.ImportWorkDir, "err", err)
	}
	srv.Import = imports
	wg.Go(func() { imports.Run(bgCtx) })
	plat.SetLoginCheck(srv.Login.Enabled) // GET /platform/settings loginAvailable
	srv.SecureCookies = cfg.SecureCookies()
	srv.CORSOrigins = cfg.WebOrigins
	log.Info("web", "cors_origins", len(cfg.WebOrigins), "secure_cookies", srv.SecureCookies, "trusted_proxies", len(cfg.TrustedProxies))

	wg.Go(func() {
		events.Listen(bgCtx, func(c context.Context) (*pgx.Conn, error) {
			conn, err := pgx.Connect(c, cfg.DatabaseURL)
			if err != nil {
				return nil, err
			}
			pgxuuid.Register(conn.TypeMap())
			return conn, nil
		}, broker, log)
	})
	wg.Go(func() { idempotency.Sweep(bgCtx, queries, log, time.Hour) })
	// The notification consumer (LAUN-33): reads the change feed, never the
	// write path. It wakes on the broker and on a ticker. No write calls it.
	notifyConsumer := notify.NewConsumer(pool, log)
	notifyWake, notifyUnsub := broker.SubscribeAll()
	wg.Go(func() {
		defer notifyUnsub()
		notifyConsumer.Run(bgCtx, notifyWake, notify.ConsumerInterval)
	})
	// The notification email sender (LAUN-34): sends the outbox rows the
	// consumer made, through the platform mail switch. With mail provider
	// none, or no PUBLIC_BASE_URL, nothing is sent and nothing fails
	// (decisions-T13 #21). No write calls it.
	notifySender := notify.NewSender(pool, log, mailSwitch, cfg.PublicBaseURL)
	if cfg.PublicBaseURL == "" {
		log.Warn("notifications: PUBLIC_BASE_URL is empty, so notification email is not sent (a mail needs a working unsubscribe and issue link); the inbox still works")
	}
	wg.Go(func() { notifySender.Run(bgCtx, notify.SendInterval) })
	// The chat services (LAUN-198 and LAUN-204, decisions-T33 and T36): the
	// admin API and the post sender. Slack and Discord posts go to their fixed
	// hosts; a custom service posts to the host the admin typed. All go through
	// the netguard dial guard (public addresses only, no redirect, no proxy, the
	// app's own subnets refused). Without SETTINGS_ENCRYPTION_KEY a webhook
	// cannot be stored. The keyring is the Slack one (same key, same info string).
	var chatKeys *platform.SlackKeyring
	if cfg.SettingsKey != nil {
		if chatKeys, err = platform.NewSlackKeyring(cfg.SettingsKey); err != nil {
			return err
		}
	}
	chatPoster := notify.NewChatPoster(notify.NewChatClient(attached))
	srv.Chat = notify.NewChatAdmin(pool, log, chatKeys, chatPoster, cfg.PublicBaseURL)
	chatSender := notify.NewChatSender(pool, log, chatKeys, chatPoster, cfg.PublicBaseURL)
	wg.Go(func() { chatSender.Run(bgCtx, notify.ChatSendInterval) })
	// A member's own webhooks (LAUN-229, decisions-T40): the chat keyring and
	// the chat poster (one guarded client), a sender of their own.
	srv.Personal = notify.NewPersonalWebhooks(pool, log, chatKeys, chatPoster, cfg.PublicBaseURL)
	personalSender := notify.NewPersonalWebhookSender(pool, log, chatKeys, chatPoster, cfg.PublicBaseURL)
	wg.Go(func() { personalSender.Run(bgCtx, notify.ChatSendInterval) })
	// Mobile push (LAUN-197, decisions-T35): the admin and device API and the
	// sender. This server holds no APNs or FCM key: it posts a bare "wake" to the
	// org's relay (a URL the admin enters, so the control is the netguard dial
	// guard: public addresses only, no redirect, no proxy). Without
	// SETTINGS_ENCRYPTION_KEY neither a relay nor a device token can be stored.
	var pushKeys *platform.PushKeyring
	if cfg.SettingsKey != nil {
		if pushKeys, err = platform.NewPushKeyring(cfg.SettingsKey); err != nil {
			return err
		}
	}
	pushPoster := notify.NewPushPoster(notify.NewPushClient(attached))
	srv.Push = notify.NewPushService(pool, log, pushKeys, pushPoster)
	pushSender := notify.NewPushSender(pool, log, pushKeys, pushPoster)
	wg.Go(func() { pushSender.Run(bgCtx, notify.PushSendInterval) })
	// The cycle job (T3): one run at boot, then hourly. Safe with several
	// instances (a per-team advisory lock), and idempotent.
	wg.Go(func() { tracker.RunCycleJob(bgCtx, svc.MaintainCycles, time.Now, tracker.CycleJobInterval, log) })

	// The project update reminder job (LAUN-196): boot, then hourly. It only
	// emits events; the notify consumer makes the notifications.
	wg.Go(func() {
		tracker.RunUpdateReminderJob(bgCtx, svc.RemindProjectUpdates, time.Now, tracker.CycleJobInterval, log)
	})

	// The git webhook dedupe keys older than 30 days (LAUN-82): boot, then hourly.
	wg.Go(func() {
		tracker.RunGitEventPrune(bgCtx, svc.PruneGitWebhookEvents, time.Now, tracker.CycleJobInterval, log)
	})

	// The notifications retention job (LAUN-44): boot, then hourly; a session
	// try advisory lock keeps several instances from pruning together.
	wg.Go(func() { notify.RunRetention(bgCtx, pool, time.Now, notify.RetentionInterval, log) })

	// The events backfill (T20 #57, LAUN-222): once at every boot. It places
	// the change-feed rows made before migration 0038, or by an older image
	// after a rollback (scope legacy, hidden from restricted members until
	// then). A session try advisory lock keeps it to one instance. It emits no
	// event and logs counts only.
	wg.Go(func() { events.RunBackfill(bgCtx, pool, log) })

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second, // the SSE handler clears it per stream
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	// Shutdown does not cancel long-lived request contexts, so tell the
	// SSE streams to finish.
	httpSrv.RegisterOnShutdown(broker.Close)

	errCh := make(chan error, 1)
	go func() {
		log.Info("server starting", "port", cfg.Port, "log_env", cfg.LogEnv)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("listen: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// loginFloor is the least time the public login routes take, so a member
// and a non-member get their answer in the same time.
const loginFloor = 300 * time.Millisecond

// environmentMail is the transport of mail provider "env": from LOG_ENV
// and POSTMARK_SERVER_TOKEN, exactly the choice the server made before
// per-install mail settings existed. It fails closed: with no transport it returns a nil mailer, and
// (while the provider is env) the login routes answer 503. A code is
// logged only on a local/test server (login.NewDevLogMailer refuses
// otherwise).
func environmentMail(cfg config.Config, log *slog.Logger) (platform.EnvMail, error) {
	switch cfg.LoginMode() {
	case config.LoginPostmark:
		log.Info("login (environment): codes are emailed through Postmark")
		return platform.EnvMail{Mailer: login.NewPostmarkMailer(cfg.PostmarkToken, cfg.PostmarkFrom, cfg.PostmarkStream),
			Transport: platform.TransportPostmark}, nil
	case config.LoginDevLog:
		m, err := login.NewDevLogMailer(log, cfg.DevSeams())
		if err != nil {
			return platform.EnvMail{}, err
		}
		log.Warn("login (environment): no POSTMARK_SERVER_TOKEN, so login codes are written to this log (LOG_ENV local/test only)",
			"fixed_code", cfg.FixedOTP != "")
		return platform.EnvMail{Mailer: m, Transport: platform.TransportDevLog}, nil
	default:
		return platform.EnvMail{Transport: platform.TransportNone}, nil
	}
}

// attachmentStore connects the object store. With no S3_* set,
// file attachments are off: the server starts and the file routes answer
// 503 attachments_unavailable. A local/test server creates its bucket; any
// other server only checks it, and a failed check is logged, not fatal, so
// a storage fault does not take the tracker down.
func attachmentStore(ctx context.Context, cfg config.Config, log *slog.Logger, svc *tracker.Service) (*storage.S3, error) {
	if !cfg.AttachmentsEnabled() {
		log.Warn("attachments: OFF. No S3_* is set, so the file attachment routes answer 503 attachments_unavailable")
		return nil, nil
	}
	st, err := storage.New(cfg.S3)
	if err != nil {
		return nil, fmt.Errorf("attachments: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if cfg.DevSeams() {
		if err := st.EnsureBucket(cctx); err != nil {
			return nil, fmt.Errorf("attachments: create bucket: %w", err)
		}
	} else if err := st.CheckBucket(cctx); err != nil {
		log.Error("attachments: bucket check failed; uploads and downloads will fail until it is fixed", "bucket", cfg.S3.Bucket, "err", err)
	}
	svc.SetStore(st)
	log.Info("attachments: on", "bucket", cfg.S3.Bucket)
	return st, nil
}

// attachedRefreshInterval is how often the app's own subnets are re-read
// (T17 #31: at boot and each hour).
const attachedRefreshInterval = time.Hour

// gitHostAccess builds what the git config routes need to reach a host: the
// git SSRF policy (the environment's switches plus the app's own attached
// subnets, which no switch can open) and the guarded API client factory. Every
// client dials through the same netguard.Dialer, so no client can skip the guard.
func gitHostAccess(cfg config.Config, attached netguard.Dynamic) (*tracker.GitNet, error) {
	policy, err := netguard.GitPolicy(cfg.GitAllowPrivateProviders, cfg.GitAllowPrivateCIDRs, attached)
	if err != nil {
		return nil, err
	}
	dialer := &netguard.Dialer{Policy: policy}
	return &tracker.GitNet{
		Policy:    policy,
		NewClient: clients.Factory(gitapi.Options{HTTP: gitapi.NewHTTPClient(dialer)}),
		Local:     cfg.DevSeams(),
	}, nil
}
