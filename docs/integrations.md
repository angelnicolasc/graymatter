# Client integrations

GrayMatter is a general-purpose MCP server (`graymatter mcp serve` over
stdio, `graymatter mcp serve --http ADDR` for StreamableHTTP). Any MCP
client works. This page documents a verified config per client, because
every client wants a different shape — and gets it wrong silently:

- VS Code uses a root `servers` key (not `mcpServers`)
- Codex uses TOML in `~/.codex/config.toml`
- OpenCode and Kilo Code take `command` as an **array**
- Goose calls servers "extensions"
- Zed nests them under `context_servers`

Two ways to wire a client:

1. **Auto-wired** — `graymatter init` writes and maintains the config for
   you (merge-never-overwrite, idempotent, `.bak` on rewrite). Re-run
   `graymatter init` after upgrading.
2. **Manual** — copy the snippet below into the client's config file.

Command: `graymatter` (must be on PATH — check with
`graymatter doctor`); args: `mcp serve`.

## Status legend

| Mark | Meaning |
|------|---------|
| auto-wired | `graymatter init` writes and upserts this config; verified against v0.17 |
| verified | config below tested against the client's published schema; verified against v0.17 |
| community | contributed config — verify the client's current docs; PRs welcome per client |

## Client matrix

| Client | Config file | Status |
|--------|-------------|--------|
| Claude Code | `.mcp.json` (project) / `~/.claude.json` | auto-wired |
| Claude Desktop | `claude_desktop_config.json` | verified |
| Cursor | `.cursor/mcp.json` | auto-wired |
| Codex CLI | `~/.codex/config.toml` | auto-wired |
| OpenCode | `opencode.jsonc` | auto-wired |
| Antigravity | `mcp_config.json` | auto-wired (opt-in: `init --with-antigravity`) |
| Windsurf | `.windsurf/mcp.json` | auto-wired |
| VS Code (Copilot) | `.vscode/mcp.json` | auto-wired |
| Gemini CLI | `.gemini/settings.json` | community |
| Goose | `~/.config/goose/config.yaml` | community |
| Crush | `.crush/crush.json` | community |
| Amp | `~/.amp/mcp-settings.json` | community |
| Amazon Q | `~/.aws/amazonq/mcp.json` | community |
| Qwen Code | `.qwen/settings.json` | community |
| Junie | `.junie/mcp.json` | community |
| Warp | Warp MCP settings | community |
| Zed | `.zed/settings.json` | community |
| JetBrains AI Assistant | IDE MCP settings | community |
| Trae | `.trae/mcp.json` | community |
| Cline | VS Code extension MCP config | community |
| Roo Code | VS Code extension MCP config | community |
| Kilo Code | VS Code extension MCP config | community |
| Continue | `~/.continue/config.yaml` | community |
| Pi | `~/.pi/mcp.json` | community |

## Verified configs

### Claude Code (auto-wired)

`.mcp.json` at the project root:

```json
{
  "mcpServers": {
    "graymatter": {
      "command": "graymatter",
      "args": ["mcp", "serve"]
    }
  }
}
```

Claude Code also supports GrayMatter's **hooks** for automatic per-turn
memory injection — see `graymatter hooks install` and the hooks section in
the README. The MCP server and the hooks are independent: either works
alone, both work together.

### Claude Desktop (verified)

`claude_desktop_config.json` (Claude Desktop → Settings → Developer →
Edit Config):

```json
{
  "mcpServers": {
    "graymatter": {
      "command": "graymatter",
      "args": ["mcp", "serve"]
    }
  }
}
```

### Codex CLI (auto-wired)

`~/.codex/config.toml` — TOML, not JSON:

```toml
[mcp_servers.graymatter]
command = "graymatter"
args = ["mcp", "serve"]
```

### OpenCode (auto-wired)

`opencode.jsonc` — note `command` is an **array** and `type` is required:

```json
{
  "mcp": {
    "graymatter": {
      "type": "local",
      "command": ["graymatter", "mcp", "serve"],
      "enabled": true
    }
  }
}
```

### Antigravity (auto-wired, opt-in)

`mcp_config.json`:

```json
{
  "mcpServers": {
    "graymatter": {
      "command": "graymatter",
      "args": ["mcp", "serve"]
    }
  }
}
```

### Windsurf (auto-wired)

`.windsurf/mcp.json`:

```json
{
  "mcpServers": {
    "graymatter": {
      "command": "graymatter",
      "args": ["mcp", "serve"]
    }
  }
}
```

### VS Code / Copilot CLI (auto-wired)

`.vscode/mcp.json` — VS Code uses a root **`servers`** key:

```json
{
  "servers": {
    "graymatter": {
      "command": "graymatter",
      "args": ["mcp", "serve"]
    }
  }
}
```

## Community configs

These follow each client's published MCP schema as of August 2026. Each
snippet targets the stdio transport with the `graymatter` binary on PATH.
If a client ships its own schema changes, the fix is a one-line PR here —
that is the point of this section.

### Gemini CLI

`.gemini/settings.json`:

```json
{
  "mcpServers": {
    "graymatter": {
      "command": "graymatter",
      "args": ["mcp", "serve"]
    }
  }
}
```

### Goose

`~/.config/goose/config.yaml` — Goose calls servers **extensions**:

```yaml
extensions:
  graymatter:
    command: graymatter
    args: [mcp, serve]
    enabled: true
```

### Zed

`.zed/settings.json` — Zed nests servers under **`context_servers`**:

```json
{
  "context_servers": {
    "graymatter": {
      "command": {
        "path": "graymatter",
        "args": ["mcp", "serve"]
      }
    }
  }
}
```

### Cline / Roo Code / Kilo Code

VS Code extension settings (each extension has its own MCP panel; the
shape is the same) — `command` is an **array**:

```json
{
  "mcpServers": {
    "graymatter": {
      "command": ["graymatter", "mcp", "serve"],
      "disabled": false
    }
  }
}
```

### Continue

`~/.continue/config.yaml`:

```yaml
mcpServers:
  - name: graymatter
    command: graymatter
    args: [mcp, serve]
```

### JetBrains AI Assistant / Junie / Trae / Qwen Code / Crush / Amp / Amazon Q / Warp / Pi

The remaining clients in the matrix accept the standard stdio shape
(`command` string or array per client, `args: ["mcp", "serve"]`) in their
respective config files listed above. As each is verified against its
current release, its exact snippet moves up to the **verified** section —
send the PR with the client version you tested against.

## Windows notes

- If `graymatter` is not on PATH, use the absolute path to the binary in
  `command`. `graymatter init` registers the executable's directory on the
  user PATH for you (refusable with `--no-path`).
- Quoting: paths with spaces must be quoted. The hooks installer does this
  for you (`graymatter hooks install`).

## Verifying a wiring

```sh
graymatter doctor        # data dir, binary on PATH, store health
graymatter hooks doctor  # hooks registration, binary path, latency
```

Every MCP client can also be smoke-tested directly:

```sh
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}' \
  | graymatter mcp serve
```

A JSON-RPC response with the server name and tool capabilities means the
wiring is good before you involve the client at all.
