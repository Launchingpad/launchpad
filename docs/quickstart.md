# Quickstart

You need a running Docker daemon (Compose v2.20+), `git`, `curl` and `python3`.

## 1. Run the deploy script

```bash
git clone https://github.com/kraetos10/launchpad.git
cd launchpad
deploy/deploy.sh
```

The first run takes several minutes (it builds images). Enter takes the default in `[ ]`.

- **Bind address**: keep `127.0.0.1` unless a proxy on another machine must
  reach the ports. Then use the LAN address ([`self-hosting.md`](self-hosting.md)).
- **Web host** and **Files host**: two different names, for example
  `launchpad.example.com` and `launchpad-files.example.com`. Type your own:
  they have no default.
- **Web port** and **Files port**: Enter takes `4001` and `4002`.

Email is not asked: set it later under **Settings**, then **Platform**. The
warning "web login needs mail" is expected. Keep `deploy/.env` safe (secrets).

The end shows `== Launchpad is up` and then the setup link. That is one
command. If stdout is not a terminal (a pipe, a file, CI), the script prints
no link and shows a hint: run `deploy/deploy.sh --setup-link` in a terminal.

If it stops with an error, fix the cause the message names and run it again.
A failed first run leaves no partial `deploy/.env`. A re-run asks nothing and
keeps every value.

## 2. Open the setup link

Paste the link into your browser (it works once, for 24 hours). Make the
first user, the organisation and the first team. The page shows the first
admin token once: save it.

## Trying it on one computer

`deploy/deploy.sh` is for a server with TLS in front. It does not work out
of the box for a trial on a laptop over plain HTTP:

- The stack runs as production (`LOG_ENV: production`). The setup link and
  the file URLs use `https://` and the host names with no port.
- The session and CSRF cookies are `Secure` and carry the `__Host-` prefix.
  A browser can refuse them over plain HTTP (Safari is expected to; not
  measured).
- Login codes are not written to the log, so sign-in needs mail or the
  admin token.

For a trial, use one of these:

- **The dev stack.** It uses plain HTTP on `127.0.0.1` and logs login
  codes. Start the stack first, then run first-run setup with your own
  values. The script defaults (org `orbit`, team `ORB`, `SETUP_EMAIL`
  `admin@example.com`) are generic and fine for a throwaway stack. Set your own if you want. Follow steps 1 to 4 of
  [`development.md`](development.md).
- **A TLS proxy in front of `deploy/deploy.sh`.** This is possible with a
  proxy that ends TLS for both host names ([`self-hosting.md`](self-hosting.md)).
  It is not documented or tested for a laptop.

## More

- [Self-hosting](self-hosting.md): the server, the proxy, mail and updates.
- [Agents and MCP](mcp.md): connect an AI agent. For Claude Code run
  `claude plugin marketplace add kraetos10/launchpad` and
  `claude plugin install launchpad@launchpad-mcp`. For other clients, one
  command installs or updates the MCP binary from a release:
  `curl -fsSLO https://github.com/kraetos10/launchpad/releases/latest/download/install-mcp.sh && less install-mcp.sh && sh install-mcp.sh`.
- [Development](development.md): run the dev stack and the tests.
