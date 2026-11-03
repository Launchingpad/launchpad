// Package testdb gives each integration test its own freshly migrated
// database, so tests never share rows and can run in parallel.
//
// Integration tests are gated by INTEGRATION=1
// and use TEST_DATABASE_URL, default the compose postgres on :5433. The
// role in that URL must be allowed to CREATE DATABASE.
package testdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/db/migrations"
	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/tracker"
)

// DefaultURL is the compose postgres as seen from the host.
const DefaultURL = "postgres://devops:devops@localhost:5433/devops?sslmode=disable"

// AdminURL is the database the test databases are created from.
func AdminURL() string {
	if u := os.Getenv("TEST_DATABASE_URL"); u != "" {
		return u
	}
	return DefaultURL
}

// Require skips t unless INTEGRATION=1.
func Require(t testing.TB) {
	t.Helper()
	if os.Getenv("INTEGRATION") != "1" {
		t.Skip("requires INTEGRATION=1 and a running Postgres (TEST_DATABASE_URL)")
	}
}

// Fresh creates an empty database and returns its URL. It is dropped when
// the test ends. Nothing is migrated.
func Fresh(t testing.TB) string {
	t.Helper()
	Require(t)
	return create(t, "")
}

// templateLockKey is the advisory lock that serialises the template build and
// every CREATE DATABASE ... TEMPLATE across the test binaries of one run
// (go test runs one binary per package, in parallel).
const templateLockKey = 7305135

// create makes a database, from template when it is not empty, and drops it
// when the test ends.
func create(t testing.TB, template string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, err := pgx.Connect(ctx, AdminURL())
	if err != nil {
		t.Fatalf("connect admin db (is postgres up? TEST_DATABASE_URL=%s): %v", AdminURL(), err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	name := "devops_test_" + hex.EncodeToString(b)
	// name is generated above from hex only, so it is safe to splice.
	stmt := "CREATE DATABASE " + name
	if template != "" {
		stmt += " TEMPLATE " + template
		// Insurance: with the FILE_COPY strategy Postgres can refuse a copy
		// while another session is copying or building the same template
		// (concurrent copies were seen to work on PG 16). Copies take the
		// lock too, so none depends on that.
		if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", templateLockKey); err != nil {
			t.Fatalf("template lock: %v", err)
		}
		defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", templateLockKey) }()
	}
	if _, err := admin.Exec(ctx, stmt); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, AdminURL())
		if err != nil {
			t.Logf("drop %s: connect: %v", name, err)
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		if _, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})
	u, err := url.Parse(AdminURL())
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

var (
	templateOnce sync.Once
	templateName string
	templateErr  error
)

// migrationsHash names the template: a changed migration makes a new one.
func migrationsHash() (string, error) {
	h := sha256.New()
	err := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(migrations.FS, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", path, len(b))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:16], err
}

// ensureTemplate returns the name of a database migrated to the newest
// version, built once per migration set and kept. Migrating takes seconds
// (43 migrations); copying a template takes milliseconds, so every test
// copies it instead of migrating (LAUN-135: the internal/api package took
// over 5 minutes, and far more on a loaded host, mostly in this step).
// A build that dies leaves only a "_building" database, which the next build
// removes; the final name appears by rename, so it is always complete.
func ensureTemplate() (string, error) {
	templateOnce.Do(func() {
		hash, err := migrationsHash()
		if err != nil {
			templateErr = err
			return
		}
		name := "devops_tpl_" + hash
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		admin, err := pgx.Connect(ctx, AdminURL())
		if err != nil {
			templateErr = fmt.Errorf("connect admin db (TEST_DATABASE_URL=%s): %w", AdminURL(), err)
			return
		}
		defer func() { _ = admin.Close(context.Background()) }()
		if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", templateLockKey); err != nil {
			templateErr = err
			return
		}
		defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", templateLockKey) }()
		var exists bool
		if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
			templateErr = err
			return
		}
		if exists {
			templateName = name
			return
		}
		building := name + "_building"
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+building+" WITH (FORCE)"); err != nil {
			templateErr = err
			return
		}
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+building); err != nil {
			templateErr = err
			return
		}
		u, err := url.Parse(AdminURL())
		if err != nil {
			templateErr = err
			return
		}
		u.Path = "/" + building
		if _, err := database.Migrate(u.String()); err != nil {
			templateErr = fmt.Errorf("migrate template: %w", err)
			return
		}
		if _, err := admin.Exec(ctx, "ALTER DATABASE "+building+" RENAME TO "+name); err != nil {
			templateErr = err
			return
		}
		templateName = name
	})
	return templateName, templateErr
}

// New returns a pool on a fresh database migrated to the newest version.
func New(t testing.TB) (*pgxpool.Pool, string) {
	t.Helper()
	Require(t)
	tpl, err := ensureTemplate()
	if err != nil {
		t.Fatalf("migrated template database: %v", err)
	}
	dsn := create(t, tpl)
	pool, err := database.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, dsn
}

// MustExec runs sql and fails the test on error.
func MustExec(t testing.TB, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, fmt.Errorf("%w", err))
	}
}

// Fixture values: the data the old boot seed made, which the integration
// tests are written against (a server never makes it).
const (
	OrgSlug   = "orbit"
	OrgName   = "Orbit"
	TeamKey   = "ORB"
	TeamName  = "Orbit"
	ChenEmail = "chen@example.com"
)

// Data is what Fixture made. Chen and Claude are token secrets pinned to the
// org: chen read/write/admin (org admin, human), claude read/write (member,
// agent).
type Data struct {
	OrgID        uuid.UUID
	Chen, Claude string
}

// Fixture fills a fresh database with org `orbit`, team ORB (with the eight
// default states), the default labels, users chen and claude, and one
// token each. Neither user is a platform admin.
func Fixture(t testing.TB, pool *pgxpool.Pool) Data {
	t.Helper()
	ctx := context.Background()
	var d Data
	err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		org, err := q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: OrgSlug, Name: OrgName})
		if err != nil {
			return fmt.Errorf("org: %w", err)
		}
		d.OrgID = org.ID
		if _, err := tracker.CreateTeamTx(ctx, q, org.ID, TeamKey, TeamName, ""); err != nil {
			return fmt.Errorf("team: %w", err)
		}
		if err := tracker.InsertDefaultLabelsTx(ctx, q, org.ID); err != nil {
			return err
		}
		email := ChenEmail
		for _, su := range []struct {
			username, name, role string
			agent                bool
			email                *string
			scopes               []string
			out                  *string
		}{
			{"chen", "Chen", "admin", false, &email, []string{"read", "write", "admin"}, &d.Chen},
			{"claude", "Claude", "member", true, nil, []string{"read", "write"}, &d.Claude},
		} {
			u, err := q.InsertUser(ctx, dbq.InsertUserParams{Username: su.username, Name: su.name, Email: su.email, IsAgent: su.agent})
			if err != nil {
				return fmt.Errorf("user %s: %w", su.username, err)
			}
			if err := q.InsertOrgMember(ctx, dbq.InsertOrgMemberParams{OrgID: org.ID, UserID: u.ID, Role: su.role}); err != nil {
				return fmt.Errorf("member %s: %w", su.username, err)
			}
			nt, err := auth.GenerateToken()
			if err != nil {
				return err
			}
			if _, err := q.InsertAPIToken(ctx, dbq.InsertAPITokenParams{
				UserID: u.ID, OrgID: &org.ID, Kind: auth.KindPAT, Name: "fixture", TokenHash: nt.Hash,
				Prefix: nt.Prefix, Scopes: su.scopes,
			}); err != nil {
				return fmt.Errorf("token %s: %w", su.username, err)
			}
			*su.out = nt.Plaintext
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return d
}

// AddMember makes a human user and an org membership with role (direct
// inserts), team_access 'all'. It returns the user id.
func AddMember(t testing.TB, pool *pgxpool.Pool, orgID uuid.UUID, username, role string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	q := dbq.New(pool)
	u, err := q.InsertUser(ctx, dbq.InsertUserParams{Username: username, Name: username})
	if err != nil {
		t.Fatalf("user %s: %v", username, err)
	}
	if err := q.InsertOrgMember(ctx, dbq.InsertOrgMemberParams{OrgID: orgID, UserID: u.ID, Role: role}); err != nil {
		t.Fatalf("member %s: %v", username, err)
	}
	return u.ID
}

// MakeRestricted sets team_access = 'selected' for a member (direct update)
// and puts the member in teamIDs. No API can do this yet (T20 #19).
func MakeRestricted(t testing.TB, pool *pgxpool.Pool, orgID, userID uuid.UUID, teamIDs ...uuid.UUID) {
	t.Helper()
	MustExec(t, pool, `UPDATE org_members SET team_access = 'selected' WHERE org_id = $1 AND user_id = $2`, orgID, userID)
	PutInTeams(t, pool, orgID, userID, teamIDs...)
}

// PutInTeams inserts team_members rows (direct inserts; it does not touch the flag).
func PutInTeams(t testing.TB, pool *pgxpool.Pool, orgID, userID uuid.UUID, teamIDs ...uuid.UUID) {
	t.Helper()
	for _, id := range teamIDs {
		MustExec(t, pool, `INSERT INTO team_members (org_id, user_id, team_id) VALUES ($1, $2, $3)`, orgID, userID, id)
	}
}
