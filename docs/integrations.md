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

1. **Auto-wired** — `graymatter init` adds missing GrayMatter entries and
   preserves existing customized entries and other servers. Re-run it after
   upgrading. Use `--only CLIENT --replace-mcp` to replace only a selected
   GrayMatter entry; no secret-bearing backup is written automatically.
2. **Manual** — copy the snippet below into the client's config file.

`graymatter init --global` still runs the normal auto-wiring in the current
project. The flag additionally writes the managed memory instructions to
Claude Code and OpenCode's home directories; it does not turn project-scoped
MCP configs into global ones. A repository still needs its own client wiring
when the client uses project-scoped MCP config; an existing user-scoped Claude
MCP registration can be reused across repositories. Codex's config is already
home-scoped.

An explicit `--dir`, including `--dir .graymatter`, persists the selected
absolute store path in newly installed MCP and hook commands. With `--dir`
omitted, they continue to resolve the current project's store dynamically.
Codex's home-scoped config with an explicit directory therefore points every
project using that entry at one fixed store. Existing customized entries are
preserved and their runtime routing is not verified by init.

Setup validates all selected files before any write. A malformed file now
fails the command without preparing the store or other clients. A later apply
failure leaves successful earlier actions in place and returns nonzero by
default. `--best-effort` permits exit 0 only for a known partial apply; rerun
after fixing the failure. `init --json` gives a versioned action report, with
`runtime_verified:false`. The interactive Windows wizard asks separately
before changing user PATH and defaults to No; `--no-path` always prevents it.
On Windows, changing any existing setup file (MCP config, instructions or hook
settings) requires readable audit SACL metadata. Ordinary user tokens often
cannot read it, so setup returns `unsupported_metadata` during preflight before
any write. Newly created files and preserved entries are unaffected. Edit the
existing file manually, or run with permission to read its audit SACL, when
that metadata is unavailable. If only an existing instructions file is
blocked, `--skip-instructions` can omit that action.
During an explicit replacement, keep other editors of that file and its
parent directory's ACL idle. The file guard detects competing GrayMatter
initializations, but cannot make external edits atomic with the final rename.

For Claude Code, register the MCP server once at user scope if it is not
already registered:

```sh
claude mcp add --scope user --transport stdio graymatter -- graymatter mcp serve
```

If global instructions and MCP registration already exist, a new project's
local memory directory can be prepared without rewriting either. The
`--store-only` flag is available since v0.20.0:

```sh
graymatter init --store-only --quiet
```

This creates `.graymatter/MEMORY.md` only when there is no regular marker or
`gray.db`. An existing regular database is left alone. The command does not
install hooks or client configuration, change `PATH`, open or validate the
database, or remove old setup files. Global hooks are optional; install them
with `graymatter hooks install --scope global`. They skip unprepared
directories. MCP can create `gray.db` at startup even without a prior init.
For Git-root launch scripts, see
[the Claude Code examples](../examples/claude-global/README.md). The scripts
prepare the selected checkout or worktree before starting Claude; MCP and hooks
must run against that same root. A custom `--dir` requires matching paths in
the MCP and hook commands. A fixed global data path shares one store between
projects.

For MCP stdio and hooks, an omitted `--dir` uses the selected project root's
`.graymatter` directory. `CLAUDE_PROJECT_DIR` takes precedence when present;
hook payload `cwd` is the next choice for hooks. A payload outside the Claude
project must be corrected; a different legacy process-directory store requires
an explicit store selection. A relative `--dir` always uses the process
working directory, even when the project root came from Claude. HTTP uses its
service working directory and does not infer a remote client's project. See
[project routing and migration](runtime-project-routing.md) before reusing a
user-scoped registration across worktrees.

## Provider endpoints

Set the matching environment variable in the CLI or MCP server process.
Set the Go field to configure a specific library instance.

| Go field | Environment variable | Default base | Request path appended |
|-----------|----------------------|--------------|-----------------------|
| `AnthropicBaseURL` | `ANTHROPIC_BASE_URL` | `https://api.anthropic.com` | `v1/messages` |
| `OpenAIBaseURL` | `OPENAI_BASE_URL` | `https://api.openai.com/v1` | `embeddings` |
| `VoyageBaseURL` | `VOYAGE_BASE_URL` | `https://api.voyageai.com/v1` | `embeddings` |

For example, `OPENAI_BASE_URL=https://gateway.example/tenant/v1` sends
embedding requests to `/tenant/v1/embeddings`. An Anthropic base of
`https://gateway.example/tenant` sends Messages requests to `/tenant/v1/messages`.
An Anthropic base ending in `/v1` produces `/v1/v1/messages`.
Use absolute HTTP or HTTPS bases. Provider selection, credentials and model
requirements still apply when using a gateway.

```go
cfg := graymatter.DefaultConfig()
cfg.DataDir = ".graymatter"
cfg.OpenAIBaseURL = "https://gateway.example/tenant/v1"
mem, err := graymatter.NewWithConfig(cfg)
```

A nonempty field wins over the environment. An empty field can use its matching
environment variable, and an empty variable uses the official base. Invalid
active bases produce a configuration error. Invalid URLs for inactive embeddings
providers do not prevent a keyword-only or Ollama store from opening.

`DefaultConfig` captures the current values. Each `Memory` keeps its effective
bases for its lifetime. Standalone graph extractors and `graymatter run` capture
their Anthropic URL when prepared. A running daemon uses its own startup
environment. Stop and restart it with the new environment to change its destinations.

Anthropic, OpenAI and Voyage follow only HTTP 307 and 308 redirects to the same
origin, preserving POST. This policy applies to official and custom URLs.

## Consolidation results

Run `graymatter consolidate AGENT --json` to get a versioned result for the
summary phase. `summary.status` distinguishes applied, skipped, failed, partial,
uncertain and not-attempted outcomes. `summary.summary_fact_id` identifies a
confirmed summary commit, and `summary.sources_retired` counts confirmed source
retirements. With uncertain retirement, that count is a lower bound. Decay,
pruning and graph maintenance can run independently of the summary phase.

The command exits 0 for an applied or skipped summary without an operation
error. Other reported outcomes exit 1. An unconfirmed storage operation keeps
its known report. A lost daemon response returns `summary: null` with an
`outcome_unknown` error. Inspect the store before retrying either case.
`--quiet` suppresses successful human output. `--json` still writes its result.
On Unix, a broken output pipe can terminate the process with `SIGPIPE`.

With a daemon, update and restart the daemon when updating the CLI if the
running daemon lacks consolidation reporting. During a mixed-version upgrade,
a new hook can still save its session checkpoint while its detached
consolidation command is rejected by the older daemon. Direct operation remains
available with `--no-daemon` or `GRAYMATTER_NO_DAEMON=1`.

## Prepared-store admission for MCP (opt-in)

The default `graymatter mcp serve` can create a valid missing store. To require
preparation first, use `--no-create` in a new Claude user-scoped registration:

```sh
claude mcp add --scope user --transport stdio graymatter -- graymatter mcp serve --no-create
cd /path/to/selected/project
graymatter init --store-only --quiet
```

Reconnect Claude's MCP client after preparing each checkout or worktree; a
rejected connection does not wait and retry. `init` does not append
`--no-create` to existing custom entries. A custom store needs the same
absolute `--dir` in the preparation, MCP, and hook commands.

The guard admits an existing regular `gray.db`, or a regular `MEMORY.md` when
the database is absent. It does not validate database health or make the
runtime read-only: a marker-only store may acquire a database and other files
after connection. Hooks and MCP reject a symbolic link, directory, or other
nonregular `gray.db` even when `--no-create` is omitted; a regular marker does
not override that rejection. HTTP keeps bearer authentication by default and
allows `--no-auth` only on loopback. An unprepared `--no-create` HTTP server
does not create a token, start a daemon, or bind a listener. Remove
`--no-create` from opted-in registrations before using an older binary; an
unknown flag fails visibly rather than falling back to eager creation.

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
alone, and both work together. When a hook actually injects recalled facts it
adds a marker naming the namespace it queried. The agent reuses only matching,
non-empty sections from the initial hook block before its first reply. An ID
mismatch reruns both project and `__shared__` searches so cross-namespace
deduplication cannot hide a shared fact; missing sections also fall back to
MCP. Focused searches, writes, corrections, aliases, and checkpoints remain
available.

`init --store-only` prepares the local store and is independent of this MCP
registration. It does not tell you which client configuration takes precedence
or whether the database is healthy.

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
graymatter doctor        # read-only data dir, PATH identity, store and config observations
graymatter hooks doctor  # hooks registration, binary path, latency
```

The standard `doctor` command does not write a probe, start a daemon, run a
binary found on PATH, or test data directory writability. Its MCP config check
detects text references; it does not establish which settings, arguments, or
environment a client actually loads. JSON reports `diagnostic_mode: read_only`,
`readiness: not_evaluated`, and `data_dir_writability: not_tested`; `ok` means
no diagnostic check failed. `doctor --health`, `--graph`, `--audit`,
`--embeddings`, and `hooks doctor` have separate contracts. In particular,
graph rendering can write an HTML file.

Every MCP client can also be smoke-tested directly:

```sh
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}' \
  | graymatter mcp serve
```

A JSON-RPC response with the server name and tool capabilities means the
wiring is good before you involve the client at all.
