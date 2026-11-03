# Contributing

Launchpad is early software and a small project. Thank you for your
interest. Please read this page before you open a pull request.

By taking part you agree to follow the [code of conduct](CODE_OF_CONDUCT.md).
To report a security problem, follow [`SECURITY.md`](SECURITY.md). Do not use
a public issue for it.

## Before you start

- For a bug, open an issue with the steps to repeat it.
- For a new feature or a large change, open an issue first and say what you
  want to do. The project is young and the maintainers may not want every
  feature.
- Keep a pull request small and about one thing.
- Open a pull request into `master`. It is squash-merged, so each change
  becomes one commit on `master`. Write the pull request title and
  description as you want the final commit message to read.

## Licence of your contribution

Launchpad uses the Apache License 2.0. A contribution that you send for
inclusion in the project is under the same licence, as section 5 of the
licence says.

## Set up

The server is written in Go (1.27.3). You do not need Go on your host: the
`make docker-*` targets run Go in Docker.

```bash
cd server
make docker-test        # vet, gofmt, unit tests with -race
```

Tests that need a database are named `*_integration_test.go`. They are
skipped unless `INTEGRATION=1`, so a plain run does not prove anything about
the database. To run them, start Postgres and silo, then run the integration
target. Use your own compose project name:

```bash
cd server
docker compose -p launchpad-dev up -d postgres silo
make docker-test-integration PROJECT=launchpad-dev
```

With Go on your host, use `make test` and `make test-integration`
(Postgres on `TEST_DATABASE_URL`, default `localhost:5433`; silo on
`TEST_S3_ENDPOINT`, default `http://localhost:9100`).

The web app needs Node 22 or newer:

```bash
cd web
npm ci
npm run lint
npm run typecheck
npm test
```

See [`docs/development.md`](docs/development.md) to run the whole stack.

## Rules for changes

- Every database change is a new numbered migration in
  `server/db/migrations` (`NNNN_name.up.sql` and a matching `.down.sql`).
  Never edit a migration that has shipped.
- After you change a migration or a query in `server/db/queries`, run
  `make sqlc` and `make schema` in `server/` and commit the changed
  `server/internal/dbq` and `server/db/schema.sql`. `make sqlc-check
  schema-check` must pass. Never edit those two by hand.
- A new route goes in `server/internal/api/router.go` and in
  `server/api/openapi.yaml`. Tests fail if the two differ. After an OpenAPI
  change, regenerate the web client: `cd web && npm run openapi`.
- Every tenant table has an `org_id`, and every query filters by it. Add a
  test that shows another org's data is `404` for each new resource.
- Never log, store or emit a secret (a token, a login code, a session).
- Add or change tests with your change. Run `make docker-test` (and the
  integration lane for a database change) before you push.
- If you change a script in `deploy/`, run `make compose-check`,
  `make deploy-test` and `make deploy-lint`.

`AGENTS.md` has the full set of project rules. It is written for AI coding
agents, but people can read it too.

## Pull requests

- Describe what changed and why. Say how you tested it.
- Make sure the checks above pass.
- Expect review comments. Maintainers may ask for changes or decline a
  change that does not fit the project.
