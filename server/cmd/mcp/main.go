// Command mcp is the tracker's MCP server: a stdio program an agent runtime
// (Claude Code or another MCP client) starts, which turns tool calls into REST calls.
//
//	LAUNCHPAD_URL    https://<tracker host> (http only to loopback, unless
//	                     LAUNCHPAD_ALLOW_INSECURE_HTTP=1)
//	LAUNCHPAD_TOKEN  an odt_ personal access token (never logged)
//	LAUNCHPAD_ORG    org slug, e.g. orbit
//	LAUNCHPAD_TEAM   default team key, e.g. ORB (optional)
//
//	LAUNCHPAD_DOWNLOAD_DIR        where download_attachment saves files (default: a 0700
//	                                  folder under the OS temp dir)
//	LAUNCHPAD_FILES_URL           origin of the attachment store's public endpoint; a
//	                                  download may be redirected there or to the API host only
//	LAUNCHPAD_DOWNLOAD_MAX_BYTES  size cap of one download (default 25 MiB, at most 100 MiB)
//
//	mcp [--profile full|reviewer]
//	mcp --version   prints "launchpad-mcp <version>" to stdout and exits
//
// stdout carries only protocol messages; logs go to stderr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"devops/internal/mcp"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "launchpad-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	profileFlag := fs.String("profile", "full", "tool profile: full or reviewer (get_issue, list_comments, get_document only)")
	versionFlag := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *versionFlag {
		// Before any config is read: no token or URL is needed to ask.
		fmt.Println("launchpad-mcp", version)
		return nil
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	profile, err := mcp.ParseProfile(*profileFlag)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	var dlMax int64
	if v := os.Getenv("LAUNCHPAD_DOWNLOAD_MAX_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return errors.New("LAUNCHPAD_DOWNLOAD_MAX_BYTES must be a positive integer")
		}
		dlMax = n
	}
	client, err := mcp.NewClient(mcp.ClientConfig{
		BaseURL:           os.Getenv("LAUNCHPAD_URL"),
		Token:             os.Getenv("LAUNCHPAD_TOKEN"),
		Org:               os.Getenv("LAUNCHPAD_ORG"),
		ReadOnly:          profile == mcp.ProfileReviewer,
		AllowInsecureHTTP: os.Getenv("LAUNCHPAD_ALLOW_INSECURE_HTTP") == "1",
		UserAgent:         "launchpad-mcp/" + version,
		DownloadDir:       os.Getenv("LAUNCHPAD_DOWNLOAD_DIR"),
		FilesURL:          os.Getenv("LAUNCHPAD_FILES_URL"),
		DownloadMaxBytes:  dlMax,
	})
	if err != nil {
		return err
	}
	tools, err := mcp.NewTools(client, os.Getenv("LAUNCHPAD_TEAM"), profile)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	mcp.CheckCredential(checkCtx, client, profile, log)
	cancel()

	log.Info("serving", "profile", profile, "tools", len(tools.Names()))
	err = mcp.NewServer(tools, log, version).Serve(ctx, os.Stdin, os.Stdout)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
