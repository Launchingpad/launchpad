# Launchpad documentation

Launchpad is early software (see the [README](../README.md)). These pages
describe what the code does today.

## Start here

- [Quickstart](quickstart.md): install Launchpad in two steps.
- [Development](development.md): run the dev stack on your computer.
- [Self-hosting](self-hosting.md): run Launchpad on a server. The short guide.
- [Configuration](configuration.md): every environment variable.
- [MCP server](mcp.md): connect an AI agent.
- [Themes](themes.md): choose Default, Paper or Terminal, light or dark.

## Reference

- [Deploy runbook](deploy.md): the full production runbook (reverse proxy,
  first run, mail, sign-ups, updates, rollback, backup and restore).
- [Architecture](architecture.md): the data model, the REST API, tenancy,
  login, attachments, GitLab, the importer.
- OpenAPI file: [`../server/api/openapi.yaml`](../server/api/openapi.yaml).

## Project

- [Contributing](../CONTRIBUTING.md)
- [Security policy](../SECURITY.md)
- [Code of conduct](../CODE_OF_CONDUCT.md)
- [`AGENTS.md`](../AGENTS.md): rules for AI coding agents that work on this
  repository.

## Internal design notes

[`internal/`](internal/) holds design notes from the first deployment. They
record why the code is the way it is. They are kept for history and may
name tools and people from that deployment. They are not a user guide.

- [`internal/deploy-orbit.md`](internal/deploy-orbit.md): the parts of the
  deploy runbook that only the first deployment used (its hosts and the
  Linear import cutover).
- `internal/decisions-*.md`: one note for each part of the system (server
  core, MCP server, planning objects, attachments, Linear importer, GitLab
  webhooks, web login, first-run setup, mail settings, agent users, web
  tokens, sign-ups and invites).
- [`internal/web-decisions.md`](internal/web-decisions.md): the web app.
