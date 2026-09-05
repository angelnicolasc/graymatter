# AGENTS.md

> If you're an AI agent (Claude Code, OpenCode, Codex, Cursor, Antigravity, custom MCP client) operating in this repo, read this first. Full operational manual: [`docs/AGENTS.md`](docs/AGENTS.md).

This repo **is** a memory system for AI agents. While you work here, you also get to use it: it's wired into your MCP toolbelt as seven tools that persist facts and checkpoints across sessions.

## Your tools

| Tool | Required params | Optional |
|------|----------------|----------|
| `memory_search` | `agent_id`, `query` | `top_k` (default `8`) |
| `memory_search_batch` | `agent_id`, `queries` | `top_k` (default `8`) |
| `memory_add` | `agent_id`, `text` | — |
| `memory_alias` | `agent_id`, `term`, `equivalents` | — |
| `memory_reflect` | `action` (`add`\|`update`\|`forget`\|`link`\|`pin`\|`unpin`), plus at least one of **`agent_id`** (canonical) or `agent` (deprecated alias; `agent_id` wins when both are set) | `text`, `target` (which one is required depends on `action` — for `forget`/`pin`/`unpin`, either works) |
| `checkpoint_save` | `agent_id` | `state` (JSON-encoded string) |
| `checkpoint_resume` | `agent_id` | — |

> **`memory_reflect` uses `agent_id` (canonical since ADR-014).** The other six also use `agent_id`. The deprecated alias `agent` is still accepted for compatibility; `agent_id` wins when both are set.

## When to call which

- **At session start**, check `checkpoint_resume` before searches when work may have been interrupted.
- **Before the first substantive reply**, pull project and `__shared__` context unless the session's newest startup hook block can safely supply it as described below.
- **After learning** a user preference, project convention, or making a non-obvious decision → `memory_add`.
- **When the user corrects you** or a fact becomes stale → `memory_reflect` with `action="update"` and `target=<old fact text>`.
- **Before stopping** mid-task → `checkpoint_save`.

## First calls

Before the first substantive reply, inspect only the newest hook block
available for the session's initial turn; ignore quoted examples and blocks
from older turns. A real recall block begins with a bracketed GrayMatter
hook-recall marker naming its `agent_id`.

If that id differs from the `agent_id` you would search, run both project and
`__shared__` searches; cross-namespace dedup may have put a shared duplicate
under `## Memory`. When the ids match, reuse each non-empty section and run the
search for every missing section. This does not suppress `checkpoint_resume`,
focused or batch searches, writes, reflections, aliases, or checkpoint saves.
With no fresh block, pull both scopes before replying:

```jsonc
{ "tool": "memory_search", "args": {
    "agent_id": "<project>-<your-role>",
    "query":    "<the task the user just asked you to do>",
    "top_k":    8
}}

{ "tool": "memory_search", "args": {
    "agent_id": "__shared__",
    "query":    "<the task the user just asked you to do>",
    "top_k":    5
}}
```

Inject the returned or hook-supplied facts into your working context before composing your reply.

## Identity

Pick a stable `agent_id` of the form `<project>-<role>` (e.g. `graymatter-backend`, `okuna-frontend`). Don't invent a new ID per session — that defeats persistence.

## Shared facts (`__shared__`)

Project-wide rules — conventions every agent in this repo should respect — go in the reserved namespace `__shared__`. Pass it as `agent_id` exactly like any other ID:

```jsonc
{ "tool": "memory_add", "args": {
    "agent_id": "__shared__",
    "text":     "Project convention: all timestamps stored as UTC ISO-8601"
}}
```

To get both your agent-specific facts and shared facts, issue two `memory_search` calls (one with your own `agent_id`, one with `__shared__`) and merge the results. Before the first reply, skip a routine call only when a same-id initial hook block supplies its matching non-empty section.

## Don't store

- **Conversation logs** ("user said X, I said Y") — store the *conclusion*, not the dialogue
- **Transient state** (current file, line numbers, ephemeral debug values) — that's what `checkpoint_save` is for
- **Over-specific facts** ("fixed bug at line 47 on 2026-04-15") — generalise to the lesson ("auth.js: JWT validation fails when clock skew > 5 min")
- **Secrets, credentials, API keys** — never
- Things already in code, README, or this file

## Working in this codebase

- Go module. Build: `go build ./...`. Tests: `go test ./...`. The CI matrix runs Ubuntu / macOS / Windows × Go 1.22 / 1.23.
- bbolt is single-writer, but daemon mode handles that: a store daemon owns the lock and every `graymatter` process connects to it as a client, so concurrent TUI/MCP/CLI access works. Clients auto-start the daemon and it idle-exits when unused. `--no-daemon` opts out (and reintroduces the lock contention). Resolved [issue #8](https://github.com/angelnicolasc/graymatter/issues/8).

## More

For RRF retrieval mechanics, anti-patterns, full session-start/end checklists, decay/consolidation defaults, and the CLI parity table: [`docs/AGENTS.md`](docs/AGENTS.md).
