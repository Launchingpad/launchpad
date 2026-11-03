## What and why

<!-- What does this change do? Why is it needed? One short paragraph. -->

## How you tested it

- [ ] `make docker-test` (in `server/`)
- [ ] The integration lane, for a database change (`make docker-test-integration`)
- [ ] The web checks, for a web change (lint, typecheck, test and build in `web/`)
- [ ] `make compose-check deploy-test deploy-lint`, for a change in `deploy/`
- [ ] A new migration has a `.down.sql`; `make sqlc schema` output is committed

## Checks

- [ ] No secret, token, real host name or personal email address in code, tests, docs or logs
