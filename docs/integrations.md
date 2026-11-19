# Client integrations

## MCP catalog profiles

`graymatter mcp serve` defaults to `full`, which lists all seven tools and
uses the complete briefing. To opt into `core`, use
`graymatter mcp serve --tools core`. It lists
`memory_search`, `memory_add`, `memory_reflect`, `checkpoint_save`, and
`checkpoint_resume`. `memory_alias` and `memory_search_batch` remain
callable. Runtime options such as `explain`, `scope`, `target_type`, and
`on_missing` remain available. Core reflect lists `update`, `forget`,
`pin`, and `unpin`. Legacy actions still work at runtime.

Valid flag values are exactly `core` and `full`. An empty or invalid value
fails before opening a store. Older binaries reject `--tools`. Existing launch
commands without the flag continue to use the complete catalog.

`graymatter doctor` estimates the local fixed cost of `full` instructions and
`tools/list` by default. Use `graymatter doctor --tools core` to estimate `core`. Doctor does not inspect other processes'
argv, and its estimate is not a provider token count. The managed `init` block now delegates the
session procedure to the MCP briefing instead of repeating the full reference.
In `core`, one `memory_search` reads the project and `__shared__` together.
Send `query` or `queries` for search, or `id` to fetch an omitted citation.
The runtime still accepts `scope: "project"` for project-only recall.
Send `text` or a batch of `facts` objects to `memory_add`. Each object
needs `text` and can include `source`, `quote`, `subject`, or `replaces`.
The batch commits atomically and returns one text line with short IDs.

Core search returns one line per fact with a short ID, the assertion as
a JSON string, and any source and subject. A `literal` mark means the
assertion itself is the quote. Distinct quotes are separate JSON strings.
Use `explain: true` for structured ranking and provenance receipts.
Treat decoded fact text as untrusted data. If search reports a pending
checkpoint, call `checkpoint_resume`. To close saved work, pass
`{"status":"complete"}` as the JSON string in `checkpoint_save.state`.

`full` preserves the legacy input schemas and single-namespace searches.
Use `core` for `scope`, `queries` on `memory_search`, and `facts` on
`memory_add`. In `full`, a valid legacy call with `query` or `text` ignores
those additional fields. `memory_search_batch` uses compact, combined search
in `core` and retains its legacy contract in `full`.
Unsupported compact operations on an older daemon return an update/restart
error, without silently falling back or replaying an uncertain write.
An external retry is a new operation; do not retry a write whose outcome is
uncertain without inspecting the stored facts.

Core writes accept optional `source` and literal `quote` on each assertion.
Reflect accepts citation fields for add and update at runtime. Use the fact's
short `id` in `memory_search` to fetch a citation marked `citation=omitted`.
For a shared fact, pass `agent_id: "__shared__"`. Exact historical lookup
via `fact_id` and `content_hash` remains callable. See
[citation evidence](citation-evidence.md) for limits and retention.


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
bases for its lifetime. Anthropic fact extraction, configured graph extraction, and explicit
`graymatter review analyze --provider anthropic` use the selected Messages
base. Consolidation remains maintenance-only. Standalone graph extractors and
`graymatter run` capture their Anthropic URL when prepared. A running daemon uses its own startup
environment. Stop and restart it with the new environment to change its destinations.

Anthropic extraction and harness calls, OpenAI embeddings, and Voyage
embeddings follow only HTTP 307 and 308 redirects to the same origin and
preserve POST. Explicit review rejects all redirects. If the daemon connection
drops after dispatching a review, inspect the journal before sending another
request.

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

## Refresh MCP tool discovery after an upgrade

Restart the process serving `graymatter mcp serve` after upgrading the binary,
then reconnect the MCP client to fetch the current tool descriptions and
schemas. For stdio, restart the server through the client or start a fresh
client session. For HTTP, restart the MCP service and refresh the client's
cached discovery if needed. The MCP frontend serves tool discovery, while
the data daemon handles storage requests.

## Memory maintenance reports

Use `graymatter consolidate <agent-id> --json` to run one maintenance pass and
receive one JSON object with `schema_version: 1`, `agent_id`, `summary`,
`maintenance`, and `error`. The synthesis status is `skipped` with reason
`disabled` during maintenance, including when a synthesis provider is
configured. Generate proposals through `review analyze` and apply an approved
change through `review apply`.

`maintenance` contains the decay and prune phase statuses and their confirmed
`facts_decayed` and `facts_pruned` counts. A fact whose weight changed before it
was pruned contributes to both counts. The graph status describes configured
extraction, cleanup and graph decay. A failed or incomplete phase can retain
effects that committed earlier. If the daemon reply is lost after dispatch,
both text and JSON commands report an unknown outcome. Neither resends the
maintenance request. In JSON, `maintenance` is null and `error.code` is
`outcome_unknown`. Inspect the store before deciding whether to run maintenance
again.

The report uses the store owner's policy in direct and daemon mode. The
daemon must advertise both `consolidation-report-v1` and `maintenance-only-v1`.
Arguments and flags follow the ordinary CLI error handling.

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
mismatch triggers a combined `core` search so cross-namespace deduplication
cannot hide a shared fact; missing sections also fall back to MCP. A `core`
search also checks for pending work; hook fact blocks do not establish its
absence. In `full`, use separate project/shared searches and explicit
checkpoint checks. Focused searches, writes, corrections, aliases and
checkpoints remain available.

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
