// Command importer moves a Linear workspace into Launchpad.
//
//	importer export --out DIR [--no-files]
//	    Read Linear (LINEAR_API_KEY, read-only) into DIR: one JSONL file per
//	    entity, files/ with the uploads.linear.app files, manifest.json and
//	    usermap.template.json.
//	importer import --in DIR --user-map FILE [--dry-run] --org SLUG [--counter-floor TEAM=N]
//	    Write the export into DATABASE_URL. --dry-run does it all in one
//	    transaction and rolls it back. A real run verifies the counts after
//	    the commit and exits 3 on a mismatch.
//	importer verify --in DIR --user-map FILE --org SLUG
//	    Compare the export with the database; exit 3 on a mismatch.
//
// The order at cutover (decisions-T3 #35, decisions-T4): export, dry run,
// import, verify, THEN turn cycles on with
// PATCH /api/v1/orgs/SLUG/teams/KEY {"cyclesEnabled": true}.
//
// Exit codes: 0 ok, 1 error, 2 usage, 3 verification mismatch.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/importer"
	"devops/internal/importer/linear"
	"devops/internal/storage"
)

const (
	exitError    = 1
	exitUsage    = 2
	exitMismatch = 3
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string) int {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: importer export|import|verify [flags] (see the package comment)")
		return exitUsage
	}
	var err error
	switch args[0] {
	case "export":
		err = cmdExport(ctx, args[1:], log)
	case "import":
		err = cmdImport(ctx, args[1:], log, false)
	case "verify":
		err = cmdImport(ctx, args[1:], log, true)
	default:
		fmt.Fprintf(os.Stderr, "importer: unknown command %q (want export, import or verify)\n", args[0])
		return exitUsage
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "importer:", err)
	}
	return exitCode(err)
}

// exitCode maps a command's error to the process exit code.
func exitCode(err error) int {
	var ue usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ue):
		return exitUsage
	case errors.Is(err, importer.ErrMismatch):
		return exitMismatch
	default:
		return exitError
	}
}

type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func cmdExport(ctx context.Context, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	out := fs.String("out", "", "export directory (required)")
	noFiles := fs.Bool("no-files", false, "do not download uploads.linear.app files")
	maxFileMB := fs.Int64("max-file-mb", 100, "largest file to download, MB")
	maxTotalGB := fs.Int64("max-total-gb", 10, "stop downloading after this many GB")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if *out == "" {
		return usageError{"--out is required"}
	}
	key := os.Getenv("LINEAR_API_KEY")
	c, err := linear.NewClient(linear.Endpoint, key, log)
	if err != nil {
		return err
	}
	dl, err := linear.NewDownloader(key, log)
	if err != nil {
		return err
	}
	key = ""
	start := time.Now()
	ex, err := linear.Export(ctx, c, dl, linear.Options{
		OutDir: *out, Files: !*noFiles, MaxFileBytes: *maxFileMB << 20, MaxTotalBytes: *maxTotalGB << 30, Log: log,
	})
	if err != nil {
		return err
	}
	counts, err := linear.MarshalCounts(ex)
	if err != nil {
		return err
	}
	failed := 0
	for _, f := range ex.Files {
		if f.Error != "" {
			failed++
		}
	}
	fmt.Printf("export written to %s in %s with %d API requests\ncounts: %s\nfiles not downloaded: %d\n",
		*out, time.Since(start).Round(time.Second), c.Requests(), counts, failed)
	return nil
}

// floors is a repeatable KEY=N flag.
type floors map[string]int32

func (f floors) String() string {
	parts := []string{}
	for k, v := range f {
		parts = append(parts, fmt.Sprintf("%s=%d", k, v))
	}
	return strings.Join(parts, ",")
}

func (f floors) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	n, err := strconv.ParseInt(v, 10, 32)
	if !ok || err != nil || n < 1 || k == "" {
		return fmt.Errorf("want TEAM=N, got %q", s)
	}
	f[strings.ToUpper(k)] = int32(n)
	return nil
}

func cmdImport(ctx context.Context, args []string, log *slog.Logger, verifyOnly bool) error {
	name := "import"
	if verifyOnly {
		name = "verify"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	in := fs.String("in", "", "export directory (required)")
	userMap := fs.String("user-map", "", "user map file (required; start from usermap.template.json)")
	org := fs.String("org", "", "org slug (required)")
	dry := fs.Bool("dry-run", false, "import in a transaction and roll it back")
	reportPath := fs.String("report", "", "also write the full report (with every exclusion) as JSON here")
	skipFiles := fs.Bool("skip-files", false, "store no file and leave uploads.linear.app URLs as they are (no S3_* needed)")
	publicBase := fs.String("public-base-url", os.Getenv("PUBLIC_BASE_URL"),
		"API origin the download URLs in texts are written on (default $PUBLIC_BASE_URL; empty writes paths)")
	fl := floors{}
	fs.Var(fl, "counter-floor", "minimum next issue number, TEAM=N (repeatable; default none)")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if *in == "" {
		return usageError{"--in is required"}
	}
	if *org == "" {
		return usageError{"--org is required"}
	}
	if *userMap == "" {
		return usageError{"--user-map is required"}
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return usageError{"DATABASE_URL is required"}
	}
	var store *storage.S3
	if !*skipFiles {
		s, err := storeFromEnv()
		if err != nil {
			return err
		}
		store = s
	}

	ex, err := importer.ReadExport(*in)
	if err != nil {
		return err
	}
	um, err := importer.LoadUserMap(*userMap)
	if err != nil {
		return err
	}
	plan, err := importer.BuildPlan(ex, um, fl)
	if err != nil {
		return err
	}
	var objects importer.ObjectStore
	if *skipFiles {
		plan.WithoutFiles()
	} else {
		objects = store
	}
	pool, err := database.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := importer.CheckSchema(ctx, pool); err != nil {
		return err
	}

	if verifyOnly {
		q := dbq.New(pool)
		o, err := q.GetOrgBySlug(ctx, *org)
		if err != nil {
			return fmt.Errorf("org %q: %w", *org, err)
		}
		rows, err := importer.Verify(ctx, q, o.ID, plan, objects)
		if err != nil {
			return err
		}
		rep := &importer.Report{Verify: rows, Counts: map[string]*importer.Counts{}}
		if err := printVerify(rows); err != nil {
			return err
		}
		if err := writeReport(*reportPath, rep); err != nil {
			return err
		}
		if len(importer.Mismatches(rows)) > 0 {
			return importer.ErrMismatch
		}
		return nil
	}

	rep, runErr := importer.Run(ctx, pool, ex, plan, importer.Options{Org: *org, DryRun: *dry, Log: log,
		Store: objects, ExportDir: *in, PublicBaseURL: *publicBase})
	if rep != nil {
		if err := rep.Print(os.Stdout); err != nil {
			return err
		}
		if err := writeReport(*reportPath, rep); err != nil {
			return err
		}
	}
	return runErr
}

// storeFromEnv is the attachment store from S3_* (the server's variables).
// All four of S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY, S3_SECRET_KEY are
// needed; with none, the import needs --skip-files.
func storeFromEnv() (*storage.S3, error) {
	cfg := storage.Config{
		Endpoint: os.Getenv("S3_ENDPOINT"), Bucket: os.Getenv("S3_BUCKET"), Region: os.Getenv("S3_REGION"),
		AccessKey: os.Getenv("S3_ACCESS_KEY"), SecretKey: os.Getenv("S3_SECRET_KEY"),
	}
	if cfg.Endpoint == "" && cfg.Bucket == "" && cfg.AccessKey == "" && cfg.SecretKey == "" {
		return nil, usageError{"S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY and S3_SECRET_KEY are required to store the files (or pass --skip-files)"}
	}
	s, err := storage.New(cfg)
	if err != nil {
		return nil, usageError{err.Error()}
	}
	return s, nil
}

func printVerify(rows []importer.VerifyRow) error {
	if _, err := fmt.Println("verify only (no write)"); err != nil {
		return err
	}
	for _, r := range rows {
		status := "ok"
		if r.Expected != r.Actual {
			status = "MISMATCH"
		}
		if _, err := fmt.Printf("%-20s expected %6d actual %6d %s\n", r.Entity, r.Expected, r.Actual, status); err != nil {
			return err
		}
	}
	return importer.PrintVerifyRows(os.Stdout, rows)
}

func writeReport(path string, rep *importer.Report) error {
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
