---
title: MCP clients
description: Wiring GrayMatter into Claude Code, Cursor, Codex, OpenCode, Antigravity, Windsurf, VS Code, and any other MCP-compatible client.
---

`graymatter init` auto-wires every supported client at once. Existing entries
from other MCP servers are merged, never overwritten.

`graymatter init --global` still initializes the current project. The flag
also installs home-scoped agent instructions; it does not globalize the
project-scoped MCP configs below, so wire each repository separately with
`init` or manual configuration. Codex's MCP config is already home-scoped.

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

## One store, many processes

GrayMatter persists to bbolt, a single-writer embedded DB — only one process
holds the write lock at a time. The CLI and TUI fall back to read-only mode
when the lock is held; a second MCP server fails fast instead of blocking.

Most robust setup: one shared `graymatter mcp serve --http 127.0.0.1:8080`
pointed at by every client. Details and failure modes in the
[agent guide](/reference/agents-guide/).
