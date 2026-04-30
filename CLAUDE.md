# CLAUDE.md

> Project-specific instructions for Claude Code working in this repo.

## What this repo is

GrayMatter — a single-binary Go memory system for AI agents. Library + CLI + MCP server + TUI. Persists facts to bbolt with optional vector embeddings; ~97% reduction in context tokens versus full-history injection.

## Two facts that change how you work here

1. **You have memory tools available** via this repo's own MCP server. See [`AGENTS.md`](AGENTS.md) for the per-tool param reference (notably: `memory_reflect` takes `agent`, the other four take `agent_id` — don't mix them up).
2. **bbolt is single-writer**. If `graymatter tui`, `graymatter serve`, an MCP client child, or a test all run simultaneously against the same `--dir`, they fight over the lock. The structural fix is tracked in [issue #8](https://github.com/angelnicolasc/graymatter/issues/8) and lands in v0.6.0 (daemon mode).

## Codebase basics

- **Build**: `go build ./...`
- **Test**: `go test ./...` (use `-race` if you have CGO; CI runs the race matrix)
- **Lint**: standard `go vet ./...`; format with `gofmt -s -w .`
- **Module split**: root module = core library; `cmd/graymatter/` = CLI + TUI binary (separate `go.mod` to keep TUI deps out of library consumers)
- **Public API surface**: see [`docs/api-stability.md`](docs/api-stability.md) — `graymatter.Memory` is stable; internal packages are not

## Conventions

- Branches off `main`. PRs are opened when external review is wanted; otherwise direct commits to `main` are fine.
- CHANGELOG follows Keep a Changelog format with `### Added / Changed / Fixed / Internal / Credits` sections.

## Where deep docs live

- [`AGENTS.md`](AGENTS.md) — agent operator brief (this file points to it for the MCP tools)
- [`docs/AGENTS.md`](docs/AGENTS.md) — full memory-system manual (RRF mechanics, anti-patterns, CLI parity, etc.)
- [`GRAYMATTER_PLAYBOOK.md`](GRAYMATTER_PLAYBOOK.md) — the strategic *why* (gap analysis, ecosystem positioning)
- [`docs/api-stability.md`](docs/api-stability.md), [`docs/benchmarks.md`](docs/benchmarks.md), [`docs/plugin-protocol.md`](docs/plugin-protocol.md) — specialised references
