---
title: MCP clients
description: Wiring GrayMatter into Claude Code, Cursor, Codex, OpenCode, Antigravity, Windsurf, VS Code, and any other MCP-compatible client.
---

`graymatter init` auto-wires every supported client at once. Existing entries
from other MCP servers are merged, never overwritten.

`graymatter init --global` still initializes the current project. The flag
also installs home-scoped agent instructions; it does not globalize the
project-scoped MCP configs below. Projects using those configs still need
client wiring, while an existing user-scoped Claude MCP registration can be
reused across repositories. Codex's MCP config is already home-scoped.

| Client | Config file | Scope |
|--------|-------------|-------|
| Claude Code | `.mcp.json` | project |
| Cursor | `.cursor/mcp.json` | project |
| Codex (OpenAI) | `~/.codex/config.toml` | home |
| OpenCode | `opencode.jsonc` | project |
| Antigravity (Google) | `mcp_config.json` | opt-in |
| Windsurf | `.windsurf/mcp.json` | project |
| VS Code Copilot Agent | `.vscode/mcp.json` | project |

**Also works out of the box:** Pi (reads `.mcp.json` natively), Zed, Cline,
and any MCP-compatible client — point them at `graymatter mcp serve`.

## Claude Code with a user-scoped server

Register GrayMatter with Claude once at user scope if you have not already:

```sh
claude mcp add --scope user --transport stdio graymatter -- graymatter mcp serve
```

Global memory instructions can be installed with `graymatter init --global`,
which also performs ordinary setup in the current project. Global hooks are
optional: `graymatter hooks install --scope global` keeps their guard against
unprepared directories.

With MCP and instructions already available, prepare only the current
project's store before a session with `--store-only`, available since v0.20.0:

```sh
graymatter init --store-only --quiet
```

The command creates `.graymatter/MEMORY.md` only when neither that marker nor
a regular `gray.db` exists. It does not write client configs, instructions or
hooks, open the DB, or check runtime health. If neither leaf is regular,
symlink or other nonregular entries are rejected; use `--dir` to select the
real data directory. MCP itself can create `gray.db`
when it starts, even without this command. For scripts that locate the Git
checkout or worktree root before launching Claude, see the
[Claude Code launcher examples](https://github.com/angelnicolasc/graymatter/tree/main/examples/claude-global).
Launch MCP and hooks against the same root. A custom `--dir` must also be
configured on each MCP and hook invocation; a single fixed path shares its
store between projects.

## Manual wiring

Any MCP client that accepts a stdio server works with:

```jsonc
{
  "mcpServers": {
    "graymatter": {
      "command": "graymatter",
      "args": ["mcp", "serve"]
    }
  }
}
```

## HTTP transport

For shared setups (multiple agents, one store), run a single server over HTTP:

```bash
graymatter mcp serve --http 127.0.0.1:8080
```

The HTTP transport requires a bearer token; it lives in
`<data-dir>/graymatter.http-token`. Network surfaces bind loopback-only.

## Troubleshooting a missing tool

GrayMatter registers seven MCP tools, including `memory_reflect`. A tool can
appear in the server's `tools/list` response but be absent from the client's
loaded catalog. Claude Code can exclude a tool whose input schema has a root
combinator when schema rewriting is unavailable; other tools stay usable.
See [Claude Code's schema guidance](https://code.claude.com/docs/en/mcp#tool-input-schemas-with-a-root-level-combinator).

1. Inspect the effective MCP registration: executable or HTTP endpoint,
   arguments, transport, and config scope. Run `--version` on the exact
   configured executable, rather than assuming the `graymatter` found in your
   terminal is the same binary. For HTTP, check the binary running the server.
   Record the client version as well (`claude --version` for Claude Code).
2. Compare the server's `tools/list` names and `memory_reflect.inputSchema`
   with the tools the client actually loaded. The corrected input schema is
   a flat object with `required: ["action"]` and no root `anyOf`, `oneOf`, or
   `allOf`. Identity remains explicit and mandatory at runtime: supply a valid
   `agent_id`, or the deprecated `agent` alias. Every supplied identity field
   must be valid, and `agent_id` wins when both are valid.
3. Check the client's MCP/server log for an exclusion or schema-validation
   reason. In Claude Code, inspect the server in `/mcp`; use the debug log if
   the tool list and loaded catalog differ.
4. Upgrade the configured binary and restart the process serving
   `graymatter mcp serve`. Reconnect the MCP client or start a fresh session
   to reload the descriptions and schemas. For HTTP clients that cache
   discovery, refresh or disable that cache during verification. The MCP
   frontend serves tool discovery, while the data daemon handles storage
   requests. Confirm all seven tools in the refreshed catalog.

If it still fails, report the server and client versions, transport, redacted
effective config, `tools/list` names/schema, and the relevant exclusion log.
Remove tokens, credentials, and private paths from those excerpts; memory
databases are not needed to investigate discovery.

## One store, many processes

GrayMatter persists to bbolt, a single-writer embedded DB — only one process
holds the write lock at a time. Normal CLI, TUI and MCP processes connect to
one store daemon, which owns that lock. Clients can operate concurrently and
start the daemon when needed. `--no-daemon` opts into direct access, where
competing processes can encounter the database lock.

`graymatter tui --read-only` requires an existing store and a running daemon;
it does not start one. Explicit `--no-daemon --read-only` opens the existing
store directly without creating it, but cannot coexist with a direct writer.
See the [agent guide](/reference/agents-guide/) for the access contracts.
