# Launchpad server. Targets that run `go` need Go 1.27.3 on the host (CI has
# it). On a Mac with no Go, use the docker-* targets: they run the same
# commands in golang:1.27.3 (Debian, so -race works).
#
# Compose needs a project name (AGENTS.md). In a worktree:
#   make up PROJECT=dev-ops-mytask

.PHONY: build build-mcp docker-build-mcp test test-integration migrate-check sqlc sqlc-check schema schema-check \
        compose-check restore-test deploy-test deploy-lint smoke up down logs docker-test docker-test-integration ci

GO_IMAGE   ?= golang:1.27.3
SQLC_IMAGE ?= sqlc/sqlc:1.30.0
PROJECT    ?=
COMPOSE    := docker compose $(if $(PROJECT),-p $(PROJECT),)

build:
	go build -o bin/server ./cmd/server

# The MCP server (docs/internal/decisions-T2.md). Agents run it on their own machine.
build-mcp:
	go build -o bin/launchpad-mcp ./cmd/mcp

# The unit lane: vet, gofmt and every test with the race detector. The
# INTEGRATION-gated tests SKIP here (no database).
# -tags backuptest builds the test-only local backup driver (T16 #16);
# scripts/check-backuptest.sh proves the normal build has no reference to it.
test:
	go vet -tags backuptest ./...
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	go test -tags backuptest ./... -race -count=1
	scripts/check-backuptest.sh

# Unit + DB-backed tests. Needs postgres and silo (make up, or
# TEST_DATABASE_URL and TEST_S3_ENDPOINT).
test-integration:
	INTEGRATION=1 go test -tags backuptest ./... -race -count=1 -timeout 30m

# Migration up, down, up on a throwaway database.
migrate-check:
	INTEGRATION=1 go test ./internal/database/ -run 'TestMigrateUpDownUp' -count=1 -v

sqlc:
	docker run --rm -v "$(CURDIR)":/src -w /src $(SQLC_IMAGE) generate

# Fails when internal/dbq is not what sqlc would generate.
sqlc-check:
	docker run --rm -v "$(CURDIR)":/src -w /src $(SQLC_IMAGE) diff

schema:
	scripts/schema.sh --write

schema-check:
	scripts/schema.sh --check

# Every port docker-compose.yml publishes is on 127.0.0.1 (reads the
# rendered `docker compose config`; starts nothing).
# Also the production file: only web and silo publish, nothing
# mounts the Docker socket, images are pinned (deploy/check-compose.sh),
# and the self-test of those rules on mutated renders.
compose-check:
	scripts/check-compose-bindings.sh
	../deploy/check-compose.sh
	../deploy/test-compose-check.sh

# Backup + restore of a real dump into a scratch database, with row counts,
# and the restore of an encrypted, signed copy from a fake remote
# (it builds the server binary in a golang container). Needs only
# docker; publishes no port.
restore-test:
	../deploy/test-restore.sh
	../deploy/test-restore-remote.sh

# deploy/.env is never deleted or changed by a failed run, and a failed or
# killed first run leaves no secrets file behind. test-env.sh needs
# docker and starts no container of any stack; test-offsite.sh (LAUN-72) runs the
# offsite role grants and backup.sh against a throwaway postgres container.
deploy-test:
	../deploy/test-env.sh
	../deploy/test-offsite.sh
	../scripts/test-export.sh
	../scripts/test-audit.sh
	../scripts/test-install-mcp.sh
	../scripts/test-launchpad-plugin.sh

# shellcheck on the deploy scripts, in docker (no host install needed).
SHELLCHECK_IMAGE ?= koalaman/shellcheck:v0.10.0@sha256:2097951f02e735b613f4a34de20c40f937a6c8f18ecb170612c88c34517221fb
deploy-lint:
	docker run --rm -v "$(abspath $(CURDIR)/..)":/mnt -w /mnt $(SHELLCHECK_IMAGE) -x \
		deploy/deploy.sh deploy/restore.sh deploy/test-restore.sh deploy/test-restore-remote.sh deploy/test-env.sh deploy/test-offsite.sh scripts/install-mcp.sh scripts/test-install-mcp.sh scripts/test-launchpad-plugin.sh plugins/launchpad/scripts/launchpad-mcp plugins/launchpad/lib/install-mcp.sh deploy/check-compose.sh deploy/test-compose-check.sh deploy/lib.sh deploy/backup/backup.sh scripts/export-public.sh scripts/audit-public.sh scripts/test-export.sh scripts/test-audit.sh

# BASE_URL and TOKEN: see scripts/smoke.sh.
smoke:
	scripts/smoke.sh

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs --tail=50 app

# --- Go in docker (no host Go) -----------------------------------------------

DOCKER_GO = docker run --rm -v "$(CURDIR)":/src -w /src \
	-v dev-ops-gomod:/go/pkg/mod -v dev-ops-gocache:/root/.cache/go-build

docker-test:
	$(DOCKER_GO) $(GO_IMAGE) sh -c 'go vet -tags backuptest ./... && test -z "$$(gofmt -l .)" && go test -tags backuptest ./... -race -count=1 && scripts/check-backuptest.sh'

# The MCP binary for this Mac (no host Go). MCP_GOOS / MCP_GOARCH pick
# another target.
MCP_GOOS   ?= darwin
MCP_GOARCH ?= arm64
docker-build-mcp:
	$(DOCKER_GO) -e CGO_ENABLED=0 -e GOOS=$(MCP_GOOS) -e GOARCH=$(MCP_GOARCH) $(GO_IMAGE) \
		go build -trimpath -o bin/launchpad-mcp ./cmd/mcp

# Joins the compose network, so the stack must be up (make up PROJECT=...).
docker-test-integration:
	@test -n "$(PROJECT)" || { echo "set PROJECT=<compose project>"; exit 2; }
	$(DOCKER_GO) --network $(PROJECT)_default -e INTEGRATION=1 \
		-e TEST_DATABASE_URL='postgres://devops:devops@postgres:5432/devops?sslmode=disable' \
		-e TEST_S3_ENDPOINT='http://silo:9000' \
		$(GO_IMAGE) go test -tags backuptest ./... -race -count=1 -timeout 30m

# What CI's test job runs after postgres is up.
ci: test test-integration sqlc-check schema-check compose-check restore-test deploy-test deploy-lint
