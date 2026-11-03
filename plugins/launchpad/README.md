# Launchpad plugin for Claude Code

Adds the Launchpad MCP server (issues, comments, documents, attachments) and
the skill `launchpad-tickets`.

```bash
claude plugin marketplace add kraetos10/launchpad
claude plugin install launchpad@launchpad-mcp
```

Then open Claude Code and run `/plugin configure launchpad@launchpad-mcp`.
Give your Launchpad URL (for example `https://launchpad.example.com`), the
organisation slug (for example `acme`) and an agent token. The team key and the
files URL (`https://launchpad-files.example.com`) are optional. The server does
not appear until the required values are set. Restart the session after.

On the first start of a plugin version, the plugin downloads the matching
`launchpad-mcp` binary from the GitHub Release and checks its sha256. A
background hook does this when a session starts. Full notes, troubleshooting and
the manual setup for other MCP clients: `docs/mcp.md` in the repository.
