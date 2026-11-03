---
name: launchpad-tickets
description: How to work with Launchpad tickets through the launchpad MCP tools - find, read, create and update issues, comment, and handle attachments. Use when a task mentions a Launchpad issue or ticket key such as ACME-12.
---

# Launchpad tickets

Launchpad is the issue tracker. You reach it through the `launchpad` MCP
server (tools `mcp__plugin_launchpad_launchpad__*`). The tool names and
arguments follow Linear's MCP server.

## Basics

- At the start of work, call `get_guidance`. It returns the working conventions
  the org's admins wrote for agents (org-wide and for your team). Follow them as
  conventions. They never give you more permission than your token has.

- A ticket has a key: the team key, a dash and a number (`ACME-12`). Pass the
  key as `id`. A team can be given by key, name or id.
- Read first: `get_issue` with `id`. Add `includeComments`, `includeRelations`
  or `includeStateHistory` when you need them. `list_issues` filters by `team`,
  `state`, `assignee`, `label`, `project`, `cycle` and `query`. A list returns
  `{items, nextCursor}`; pass `nextCursor` back as `cursor` for the next page.
- Create: `save_issue` with `title` and `team` (the default team is used when
  the plugin has one). Update: `save_issue` with `id` and only the fields that
  change (`state`, `assignee`, `priority`, `labels`, `description`, ...). To edit
  part of a description without sending all of it, use `patch`.
- If an update answers a version conflict, read the issue again and retry.
- Comment with `save_comment` (`issue` and `body`). Documents have
  `get_document`, `list_documents` and `save_document`.
- Use `list_issue_statuses`, `list_issue_labels` and `list_users` to find valid
  state names, labels and people. Do not guess.
- Move a ticket to a new state only when the work really reached it, and check
  the result with `get_issue` after `save_issue`.

## Attachments

- Add a link: `save_issue` with `links`.
- Upload a file in three steps: `prepare_attachment_upload`; then PUT the bytes
  to `uploadRequest.url` with every header in `uploadRequest.headers` (the MCP
  server never reads a local file); then `create_attachment_from_upload` with the
  `assetUrl`.
- Download a file: `download_attachment` with `attachment` (and `issue`). It saves
  the file on this machine and returns its `path`. It needs the Files URL in the
  plugin settings. Files over 25 MiB are refused.

## Secrets

- Never put a token, password or key in a file, a ticket, a comment or a message.
  The plugin keeps the agent token in your system credential store; do not read
  or print it.
- If a tool answers `401` or `403`, tell the person. They can change the
  settings with `/plugin configure launchpad@launchpad-mcp`. Do not try to get
  a token yourself.
