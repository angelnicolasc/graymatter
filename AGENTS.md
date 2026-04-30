# AGENTS.md

> If you're an AI agent (Claude Code, OpenCode, Codex, Cursor, Antigravity, custom MCP client) operating in this repo, read this first. Full operational manual: [`docs/AGENTS.md`](docs/AGENTS.md).

This repo **is** a memory system for AI agents. While you work here, you also get to use it: it's wired into your MCP toolbelt as five tools that persist facts and checkpoints across sessions.

## Your tools

| Tool | Required params | Optional |
|------|----------------|----------|
| `memory_search` | `agent_id`, `query` | `top_k` (default `8`) |
| `memory_add` | `agent_id`, `text` | — |
| `memory_reflect` | `action` (`add`\|`update`\|`forget`\|`link`), **`agent`**, `text` | `target` |
| `checkpoint_save` | `agent_id` | `state` (JSON-encoded string) |
| `checkpoint_resume` | `agent_id` | — |

> ⚠️ **`memory_reflect` uses `agent`, not `agent_id`.** The other four use `agent_id`. Don't mix them up — the call will silently fail with a parameter-validation error.

## When to call which

- **Before answering** any question that depends on prior context → `memory_search` first.
- **After learning** a user preference, project convention, or making a non-obvious decision → `memory_add`.
- **When the user corrects you** or a fact becomes stale → `memory_reflect` with `action="update"` and `target=<old fact text>`.
- **At the start of a session** that may resume a long task → `checkpoint_resume`. **Before stopping** mid-task → `checkpoint_save`.

## Identity

Pick a stable `agent_id` of the form `<project>-<role>` (e.g. `graymatter-backend`, `okuna-frontend`). Don't invent a new ID per session — that defeats persistence.

For project-wide rules (conventions everyone in the repo should follow), use the reserved namespace `__shared__` as your `agent_id`.

## Don't store

- Conversation logs, transient state, current-file pointers (use `checkpoint_save` for those)
- Secrets, credentials, API keys
- Things already in code, README, or this file

## Working in this codebase

- Go module. Build: `go build ./...`. Tests: `go test ./...`. The CI matrix runs Ubuntu / macOS / Windows × Go 1.22 / 1.23.
- bbolt is single-writer — running two `graymatter` processes against the same `--dir` will fight over the lock. Tracked in [issue #8](https://github.com/angelnicolasc/graymatter/issues/8); structural fix in v0.6.0.

## More

For RRF retrieval mechanics, anti-patterns, full session-start/end checklists, decay/consolidation defaults, and the CLI parity table: [`docs/AGENTS.md`](docs/AGENTS.md).
