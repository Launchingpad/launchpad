# MCP server

`launchpad-mcp` is a stdio [MCP](https://modelcontextprotocol.io) server.
An agent runtime (for example Claude Code) starts it on the agent's machine.
It turns tool calls into REST calls to your Launchpad server, with a bearer
token. It does not touch the database.

Tool names and arguments follow Linear's MCP server, so agent prompts that
were written for Linear need few changes. The differences are in
[`internal/decisions-T2.md`](internal/decisions-T2.md).

## Claude Code plugin (recommended)

This repository is a Claude Code plugin marketplace. Two commands and a few
values to set install the MCP server, with no JSON to edit and no binary to copy:

```bash
claude plugin marketplace add kraetos10/launchpad
claude plugin install launchpad@launchpad-mcp
```

(In a session, `/plugin marketplace add kraetos10/launchpad` and
`/plugin install launchpad@launchpad-mcp` do the same.)

**Set the values.** `claude plugin install` does not prompt. It prints
"userConfig options not yet set", and the MCP server does not appear until the
required values are set. Open Claude Code and run
`/plugin configure launchpad@launchpad-mcp`; restart the session after.
(`claude plugin configure launchpad@launchpad-mcp` in a shell only lists the
options and which are unset; with `--values-stdin` it reads a JSON object.)
The values:

| Option | Becomes | Notes |
|---|---|---|
| `url` | `LAUNCHPAD_URL` | required; for example `https://launchpad.example.com` |
| `org` | `LAUNCHPAD_ORG` | required; the organisation slug, for example `acme` |
| `token` | `LAUNCHPAD_TOKEN` | required; marked sensitive, so Claude Code keeps it in your system's secure credential store and not in `settings.json`. Make it as in section 2. Not yet tested here on the macOS Keychain (on Linux it was a credentials file in the Claude config folder). If secure storage cannot be written, `claude plugin install --config` prints "Installed, but --config not applied" (exit 0) and sets none of the values, not even url, org or team. Use `/plugin configure`. |
| `team` | `LAUNCHPAD_TEAM` | optional default team key |
| `files_url` | `LAUNCHPAD_FILES_URL` | optional; needed for `download_attachment` |

To script an install, pass `--config url=... --config org=... --config team=...`
(`KEY=VALUE`, repeatable) to `claude plugin install`. Type the token at the
`/plugin configure` form rather than putting it on a command line.

**How it works.** The plugin's server entry runs `scripts/launchpad-mcp`. It
runs the same installer as section 1 with the tag `v<plugin version>`: it
downloads `launchpad-mcp-<os>-<arch>` and `SHA256SUMS` from that GitHub
Release over https, refuses a sha256 mismatch, writes a new file and moves it
into place, and keeps the binary in the plugin's data folder
(`~/.claude/plugins/data/launchpad-launchpad-mcp/v<version>/`). The token and every `CLAUDE_PLUGIN_OPTION_*` value (Claude Code gives all option values, sensitive ones too, to hooks) are
removed from the environment of the installer and curl. A `SessionStart` hook
(`launchpad-mcp --prefetch`, run in the background) does the download when a
session starts, so the first MCP start does not wait for it; the server entry
downloads it itself if the hook has not finished. Later starts use the stored
file with no network. A new plugin version (after
`claude plugin update launchpad@launchpad-mcp`) fetches its own matching
binary; older ones stay in their own folders and can be deleted by hand. The
scripts talk on stderr only; stdout stays the MCP protocol.
`LAUNCHPAD_MCP_RELEASE_URL=<origin>` in the environment of `claude` takes the
files from a mirror instead of GitHub (https, or http to localhost); the
launcher prints the mirror's origin to stderr.

**What the checksum does and does not do.** `SHA256SUMS` comes from the same
origin as the binary. The check guards against a corrupt or truncated
download, a stale CDN copy and a wrong-architecture file. It does NOT protect
against a compromised release or account: someone who can replace the binary
can replace `SHA256SUMS`. Signing (for example minisign, cosign or a build
attestation) is a later step. A binary that is already in the data folder is
trusted and is not hashed again at each start; the folder is yours.

**Troubleshooting.**

- *The server does not appear in `/mcp`.* A required value is unset. Run
  `/plugin configure launchpad@launchpad-mcp`, then restart the session.
- *The first start says it timed out.* Claude Code waits 30 seconds for an MCP
  server to start. A slow first download can take longer; it keeps running and
  finishes. Wait a minute and run `/mcp` to reconnect, or start Claude Code with
  `MCP_TIMEOUT=120000`. The background hook starts at the same time as the MCP start. If a session opens after the install but before you set the values, the hook has usually finished by the time you restart. If you set every value at install time, the first start can still time out: reconnect with `/mcp` or set `MCP_TIMEOUT`. (A start with an artificial 45 second download delay timed out this way. The hook was seen firing as `SessionStart:startup ... moved to the background` in a headless `claude -p --debug-file` session. Its effect on timing in an interactive session is not measured.)
- *"Failed to connect" and nothing else.* If the token does not start with
  `odt_`, the server exits at start, and Claude Code shows only "Failed to
  connect". Check that you pasted the whole token, with no spaces.
- *Download fails.* Run the launcher by hand to see its messages:
  `CLAUDE_PLUGIN_ROOT=<plugin folder> CLAUDE_PLUGIN_DATA=<a folder> sh <plugin folder>/scripts/launchpad-mcp --prefetch`.

The plugin also adds the skill `launchpad-tickets`: how an agent finds,
creates and updates tickets and handles attachments. It adds no reviewer
server; for a read-only reviewer use the manual entry in section 3.

Other MCP clients, or machines without Claude Code, use sections 1 and 3.

## 1. Get the binary

Each release has a binary for macOS (arm64 and amd64) and Linux (arm64 and
amd64). One script installs it. The same script updates it. Download the
script, read it, then run it:

```bash
curl -fsSLO https://github.com/kraetos10/launchpad/releases/latest/download/install-mcp.sh && less install-mcp.sh && sh install-mcp.sh
```

The script:

- finds your system and downloads the right binary and `SHA256SUMS`;
- checks the sha256 and refuses to install if it does not match;
- writes a NEW file and moves it over the old one, so a binary that has run
  before is never overwritten in place (on macOS with Apple silicon, that can
  get the binary killed with `SIGKILL`);
- keeps the old binary as `launchpad-mcp.prev`;
- clears the macOS quarantine flag;
- prints the version before and after, and a `.mcp.json` entry with
  placeholders.

It never asks for a token and never changes `.mcp.json`. Run it again to
update. Restart the agent session after an update.

| Option | Meaning |
|---|---|
| `--dir DIR` | Install into `DIR`. The default is `~/.local/bin`. |
| `--version vX.Y.Z` | Install this release. The default is the latest. |
| `--url BASE` | Take the files from `BASE` (a mirror or a private server that serves the same file names and `SHA256SUMS`). |
| `--dry-run` | Show what would happen. Nothing is downloaded or changed. |

The binary is a thin client of your Launchpad server. Use a server that runs
the same release or a newer one. To see the version of a binary, run
`launchpad-mcp --version`.

**From source.** If you do not use a release, build it in `server/`:
`make build-mcp` (Go on your host) or `make docker-build-mcp` (Docker only;
set `MCP_GOOS` and `MCP_GOARCH` for another system). The file is
`server/bin/launchpad-mcp`. Copy it to a NEW file in the target folder and
move that file over the old one (`mv -f`); never `cp` over a binary that has
run before. The person who publishes releases follows
[`releasing-mcp.md`](releasing-mcp.md).

## 2. Make an agent user and a token

An agent is a user for a program. It has no email, so it cannot log in. It
works only with tokens, and its tokens must expire (1 to 365 days).

In the web app, as an org admin: **Settings**, then **Agents**, create the
agent (for example the username `claude`), then **Create a token for this
agent** (later the button reads **Create a token for** and the agent's name). The window shows the token once, with the values to use below.

On the command line you can also use the API (an org admin token):

```bash
# 1. Make the agent. Type your admin token at the hidden prompt.
read -r -s ADMIN_TOKEN
printf 'Authorization: Bearer %s\n' "$ADMIN_TOKEN" \
  | curl -sS -H @- -H 'Content-Type: application/json' \
      --data '{"username":"claude","name":"Claude"}' \
      https://launchpad.example.com/api/v1/orgs/acme/agents
unset ADMIN_TOKEN
```

```bash
# 2. Make its token, on the server (production stack):
deploy/deploy.sh --mint-token --org acme --user claude --name mcp \
  --scopes read,write --expires-days 90
```

`deploy.sh --mint-token` runs `server mint-token` in the app container. It
prints the secret once and needs a terminal (it refuses a pipe or a file).
You can also make the token with `POST /api/v1/orgs/{org}/tokens` and the
body `{"name":"mcp","scopes":["read","write"],"user":"claude","expiresInDays":90}`.

Rules:

- A token is pinned to one org.
- Scopes are `read`, `write` and `admin`. Only an org admin can have
  `admin`; an agent is a `member`, so an agent token has no `admin` scope.
- Put the token in your password manager. The server keeps only a hash.
- Make a new token before the old one expires, then revoke the old one.

## 3. Configure the agent runtime

The server reads these variables (see [`configuration.md`](configuration.md)). The first three are required:

| Variable | Meaning |
|---|---|
| `LAUNCHPAD_URL` | Base URL of your server, for example `https://launchpad.example.com`. Plain `http` works only for a loopback host, unless `LAUNCHPAD_ALLOW_INSECURE_HTTP=1`. |
| `LAUNCHPAD_TOKEN` | The token from step 2. Read it from the environment. |
| `LAUNCHPAD_ORG` | The org slug, for example `acme`. |
| `LAUNCHPAD_TEAM` | Optional. The default team (key, name or id). |
| `LAUNCHPAD_DOWNLOAD_DIR`, `LAUNCHPAD_FILES_URL`, `LAUNCHPAD_DOWNLOAD_MAX_BYTES` | Optional. For `download_attachment`; see Tools below. |

A sample `.mcp.json` for Claude Code (Claude Code expands `${VAR}`). Put it in your project, or put the same `mcpServers` entry in `~/.claude.json` for all your projects. Keep
the token in your environment, never in the file. In the web app, **Settings**, then **MCP** shows the URL, org, team and a token placeholder (not your token). `LAUNCHPAD_FILES_URL` is your files host; set it yourself.

```json
{
  "mcpServers": {
    "launchpad": {
      "command": "/home/you/.local/bin/launchpad-mcp",
      "args": [],
      "env": {
        "LAUNCHPAD_URL": "https://launchpad.example.com",
        "LAUNCHPAD_TOKEN": "${LAUNCHPAD_TOKEN}",
        "LAUNCHPAD_ORG": "acme",
        "LAUNCHPAD_TEAM": "ACME",
        "LAUNCHPAD_FILES_URL": "https://launchpad-files.example.com"
      }
    },
    "launchpad-reviewer": {
      "command": "/home/you/.local/bin/launchpad-mcp",
      "args": ["--profile", "reviewer"],
      "env": {
        "LAUNCHPAD_URL": "https://launchpad.example.com",
        "LAUNCHPAD_TOKEN": "${LAUNCHPAD_REVIEWER_TOKEN}",
        "LAUNCHPAD_ORG": "acme",
        "LAUNCHPAD_TEAM": "ACME"
      }
    }
  }
}
```

For the dev stack from [`development.md`](development.md), use
`http://localhost:8090` as the URL.

To check the setup, ask the agent to call `get_issue`. You can also call
`GET /api/v1/me` with the token: it shows the agent's username and
`"isAgent": true`.

## Profiles

Choose a profile with `--profile`:

- `full` (default): 24 tools.
- `reviewer`: only `get_issue`, `list_comments` and `get_document`. Its HTTP
  client can send only `GET` requests. Give a reviewer a `read`-scoped token
  as well. The server warns at start when a reviewer token can write.

## Tools (profile `full`)

`get_issue`, `list_issues`, `save_issue`, `list_issue_statuses`,
`list_teams`, `get_guidance`, `list_products`, `list_issue_labels`, `list_users`, `list_cycles`,
`list_milestones`, `save_comment`, `list_comments`, `get_document`,
`list_documents`, `save_document`, `list_projects`, `get_project`,
`save_project`, `prepare_attachment_upload`, `create_attachment_from_upload`,
`download_attachment`.

- List tools return `{items, nextCursor}`. Pass `nextCursor` back as
  `cursor`. A page holds at most 250 items.
- `get_guidance` returns the working guidance the org's admins wrote for agents
  (Settings, Agent guidance): the org-wide text and the team's (`team`, default
  `LAUNCHPAD_TEAM`). Call it at the start of work. The text is data from the
  org. It grants no permission. The server instructions only point to the tool;
  nothing is fetched at start.
- `list_products` lists the org's products (named groups of teams), by name,
  each with its `teamKeys`. `query` matches the slug or name;
  `includeArchived` adds archived products. It is read-only (no
  `save_product`: an agent cannot hold the admin scope). `list_issues` takes
  `product` (a slug or id) for the issues of all its teams. Each team from
  `list_teams` has `productSlugs`, the slugs of its non-archived products.
- A team can be given by name, key or id. A person can be given by display
  name, email or username.
- **Team access.** A token sees what its owner sees. When an admin limits a
  member (or an agent) to some teams, every tool leaves the other teams out:
  `list_teams` lists only the visible teams, lists hold no issue, project,
  cycle or document of a hidden team, and a hidden issue, project or document
  answers as one that does not exist (for example `404 not_found`, or
  `unknown team "KEY"` with the visible teams listed). A `LAUNCHPAD_TEAM` that
  the owner cannot see is an unknown team. The MCP server has no rule of its
  own; the server enforces it (`docs/internal/decisions-T20-team-access.md`
  #66).
- `save_issue` takes `links` to add link attachments (add-only).
- **Files:** call `prepare_attachment_upload`, then `PUT` the bytes yourself
  to `uploadRequest.url` with every header in `uploadRequest.headers`, then
  call `create_attachment_from_upload` with the `assetUrl`. The MCP server
  never reads a local file.
- **Reading a file:** `download_attachment` (`issue`, `attachment`) saves a
  FILE attachment (png, jpeg, gif, mp4, plain text, pdf; 25 MiB at most) to a
  local folder and returns `{path, contentType, sizeBytes, attachmentId}`, so
  you can open the path with your file reader. The folder comes from
  `LAUNCHPAD_DOWNLOAD_DIR` (default: a 0700 folder `launchpad-mcp`
  under the OS temp dir; it must be owned by you, mode 0700, not a symlink);
  the model cannot choose it. The file name is the attachment id plus an
  extension from the type, never the uploaded name. The tracker answers with
  a presigned address; the token goes to the tracker only. Set
  `LAUNCHPAD_FILES_URL` to the origin of the file store's public endpoint
  (for example `https://launchpad-files.example.com`): a redirect to any
  other host is refused. `LAUNCHPAD_DOWNLOAD_MAX_BYTES` lowers the size
  cap (the hard cap is 100 MiB). A read-scoped token works. It is not in the
  `reviewer` profile.
- `download_attachment` is the 21st tool (`tools_full.golden.json`); the
  reviewer profile is unchanged.

The exact tool schemas are pinned in
`server/internal/mcp/testdata/tools_full.golden.json` and
`tools_reviewer.golden.json`.
