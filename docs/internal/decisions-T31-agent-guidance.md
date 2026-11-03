# Decisions T31: agent guidance (LAUN-200)

Admins of an org write working guidance for the agents that work in it. Agents
read it with one MCP tool. The default form was chosen by the orchestrator;
the project owner was not asked. Change it with a new record.

1. **Data.** Table `agent_guidance` (migration 0030): one org-wide row
   (`team_id` NULL) and at most one row per team. Composite FK
   `(org_id, team_id)`; deleting a team deletes its row. The text is markdown,
   stored as plain text, 1 to 8192 UTF-8 bytes (a CHECK and the API both).
2. **REST.** `GET /orgs/{org}/agent-guidance[?team=]` is read scope, so any
   token, an agent token too. `PUT` (body `{text, team?}`) and
   `DELETE [?team=]` are admin scope, so an agent cannot write. No
   `Idempotency-Key`: both are idempotent by shape. An empty text is `400`;
   removal is DELETE. A team of another org is `404 not_found`.
3. **Events.** A change emits `agent_guidance.updated` last (entity type `org`,
   payload `{team, cleared}`). The payload never holds the text. A write of the
   same text and a delete of a missing entry write nothing and emit nothing.
4. **MCP.** `get_guidance` (optional `team`, default `LAUNCHPAD_TEAM`; none:
   every entry) calls the REST route only. The answer adds a fixed `note`. The
   server instructions are static: they name the tool and never fetch. The
   reviewer profile does not have the tool.
   The text may not hold bidirectional control characters (U+202A to U+202E,
   U+2066 to U+2069, U+200E, U+200F) or zero-width characters (U+200B, U+200C,
   U+200D, U+2060, U+FEFF): `400` with a fixed sentence. They could hide a line
   from the next admin. The MCP answer puts the fixed `note` before the `items`
   (an ordered struct; a test pins it).
5. **It is data, not a grant.** The text is written by people of the org. The
   tool description and the `note` say so: working conventions, no permission
   change, no override of the agent's own instructions. Nothing in the server
   reads the text.
6. **Web.** Settings, Administration, "Agent guidance": one editor for the
   org-wide text and one per team, with a byte counter, Save and Remove.
