# Changelog

All notable changes to this project are documented in this file.  
Format: [Keep a Changelog](https://keepachangelog.com/en/1.0.0/)  
Versioning: [Semantic Versioning](https://semver.org/spec/v2.0.0.html)

---

## [Unreleased]

---

## [0.5.2] – 2026-04-28

### Added

**Agent onboarding kit — `docs/AGENTS.md`**
- New ~650-line operational guide covering when and how to use GrayMatter from Claude Code, Cursor, OpenCode, Codex, Antigravity, and any MCP-compatible host.
- Authoritative MCP tool reference table (exact param names per tool — resolves the `agent` vs `agent_id` inconsistency between `memory_reflect` and the other four tools, see `cmd/graymatter/internal/mcp/server.go:144`).
- Replaces fabricated retrieval-weight numbers with a correct RRF (Reciprocal Rank Fusion) explanation, cross-referenced to `pkg/memory/recall.go:14`.
- Adds CLI parity table, library API pointer (`examples/agent/main.go`), multi-process bbolt lock guidance, and a knowledge-graph `link`-action caveat (only works when the host wires `SetKGLinker`).
- Cross-references `GRAYMATTER_PLAYBOOK.md` (strategy / why) ↔ `docs/AGENTS.md` (operations / how).

**README link**
- New blockquote in the `MCP clients (drop-in)` section pointing readers at `docs/AGENTS.md` for per-tool parameter names and usage patterns.

### Credits

- Original draft contributed by **MikeCase** (`MikeCase/graymatter-agent-patterns`, issue #6). Edited and extended for accuracy against the current codebase.

---

## [0.5.0] – 2026-04-18

### Added

**Auto-grab for every major MCP-compatible client**
- `graymatter init` now wires the MCP server into Claude Code, Cursor, Codex (OpenAI), OpenCode, and Antigravity (Google) with a single command.
- New writers: `writeClaudeCodeProject`, `writeCursorProject`, `writeCodexHome`, `writeOpencodeProject`, `writeAntigravityProject` (`cmd/graymatter/cmd_init_writers.go`).
- Codex support handles the TOML schema at `~/.codex/config.toml` (`[mcp_servers.graymatter]`) and preserves unrelated keys.
- OpenCode writes `opencode.jsonc`; if the existing file uses JSONC comments we fail soft and print the exact snippet to paste.
- Antigravity is opt-in (`--with-antigravity`) since it's still community-documented.
- New flags: `--skip-claudecode`, `--skip-cursor`, `--skip-codex`, `--skip-opencode`, `--with-antigravity`, `--only <csv>`.
- 7 new tests in `cmd/graymatter/cmd_init_writers_test.go` covering first-write, merge-preserving-other-servers, idempotency on second run, TOML round-trip for Codex, JSONC fail-soft for OpenCode, Antigravity opt-in, and `--only` parsing.

### Changed

- `graymatter init` now **merges** MCP entries instead of skipping files that already exist. Pre-existing servers from other tools are preserved; `graymatter` is upserted.
- README hero re-framed around "general-purpose MCP server, zero vendor lock-in" — the old section `Claude Code / Cursor (MCP)` is now `MCP clients (drop-in)` with the full client table.
- `README` and footer bumped to `v0.5.0`.

### Internal

- New dep: `github.com/BurntSushi/toml v1.4.0` (pure-Go, zero transitive deps) for Codex config round-trip.

---

## [0.4.0] – 2026-04-16

### Added

**Token Cost panel in the observability dashboard**
- New `Token Cost · 30d` card on the Stats tab aggregates input / output / cache-read / cache-write tokens per agent, per model, per day, with USD cost computed from the public Anthropic price list (`cmd/graymatter/internal/harness/pricing.go`).
- Cache-hit-rate headline with dynamic colour thresholds (mint ≥ 60%, amber ≥ 30%, rose otherwise) surfaces the real savings from prompt caching.
- Per-model breakdown (top 3 by spend) with a proportional share bar.
- Bucket schema (`token_usage`) is pre-aggregated on write (key `{agent}|{model}|{yyyymmdd}`), so the hot path is a single bbolt `Put` and the read path is bounded by `agents × models × days`.

**Honest empty-state handling**
- Panel renders a friendly hint when the bucket is empty (`No agent runs yet — Tracked automatically on graymatter run`). Unknown models are flagged as `Partial`; the UI never fabricates a cost.

### Changed

**Dashboard grid is now strictly symmetric**
- Width math reworked so the KPI strip, the Agents / (Token Cost + Weight Distribution) row, and the Activity panel share the same outer border columns (1-column gutters accounted for explicitly).
- New height-aware `panelBoxH` helper in `tui_styles.go` lets the right-column stack pad down to the exact line count of the Agents panel, so bottom borders align on a single grid baseline.
- Weight Distribution footer collapsed to a compact `avg · oldest → newest` line to free vertical space for the new Token Cost panel above it.

### Internal

- `harness.RecordTokenUsage(db, agent, model, input, output, cacheRead, cacheWrite)` is invoked best-effort from `runner.go` immediately after every successful `client.Messages.New` call. Accounting failures never break a run.
- Added `TestDashboardRender_WithTokens` and `TestFormatUSD` to lock in panel output and USD formatting.

---

## [0.3.0] – 2026-04-13

Observability dashboard redesign (KPI strip, Agents inventory-vs-activity panel, weight distribution, activity sparkline). See commit `20758a2`.

---

## [0.2.1] – 2026-04-11

### Fixed

**Windows UX — auto-register executable in User PATH**
- `graymatter init` now writes the executable's directory to `HKCU\Environment\Path` via the Windows registry on first run, so users can type `graymatter` from any PowerShell session without prefixing `.\`. The operation is idempotent (no-op if the directory is already present) and best-effort (prints a warning to stderr but never fails the command).
- A `WM_SETTINGCHANGE` broadcast is sent after the registry write so running shells receive the updated PATH without a full logoff/logon cycle.
- No-op on macOS and Linux where the binary is placed in `/usr/local/bin`, which is already on PATH.

**README install commands**
- Pinned binary install URLs to the correct v0.2.1 GoReleaser asset names (`darwin_arm64`, `windows_amd64`).
- Removed the `Move-Item ... C:\Windows\System32` step from the Windows instructions; PATH registration is now handled automatically by `graymatter init`.

---

## [0.2.0] – 2026-04-10

### Added

**Context-propagation API**
- `RememberCtx`, `RecallCtx`, `RememberSharedCtx`, `RecallSharedCtx`, `RecallAllCtx` — context-aware variants of every public memory operation. Callers can now propagate deadlines and cancellation signals end-to-end through the memory subsystem.
- All original methods (`Remember`, `Recall`, etc.) are preserved unchanged and delegate internally to their `Ctx` counterparts, guaranteeing full backward compatibility with existing integrations.

**Pluggable vector backend (`VectorStore` interface)**
- Introduced `pkg/memory.VectorStore`, a stable interface decoupling the memory core from any specific vector database implementation. Methods: `AddDocument`, `Query`, `EnsureCollection`, `Close`.
- Default implementation (`chromemVectorStore`) wraps chromem-go behind the interface; the adapter is transparently selected at `Open()`.
- `StoreConfig.VectorBackend` field allows callers to inject an alternative backend (Qdrant, Weaviate, pgvector, etc.) without modifying `Store` internals.

**Observability**
- `/metrics` endpoint exposing four `expvar` counters: `requests_total`, `request_latency_us`, `facts_total`, `recall_total`. Zero additional dependencies; uses the Go standard library exclusively.
- `StoreConfig.OnRecall` hook: invoked after every `Recall` with elapsed time and result count, enabling integration with external APM systems.
- `StoreConfig.OnPut` hook: invoked after every successful `Put`.
- `StoreConfig.Logger` field: accepts any `*log.Logger`; internal diagnostics route through it instead of the default logger.
- `loggingMiddleware` on the HTTP server: structured per-request log lines with method, path, status code, and latency.

**Embedding dimension guard**
- On `Open()`, the stored embedding dimension is read from a dedicated bbolt meta bucket and compared against the current provider's reported dimension. A mismatch emits a structured warning instead of silently corrupting the vector index with incommensurable embeddings.
- `recordEmbedDimensions` persists the dimension on first use; subsequent opens with a matching provider are a no-op.

**Go workspace (module split)**
- `go.work` workspace file declaring two modules: root (core library) and `cmd/graymatter` (CLI / TUI binary).
- `cmd/graymatter/go.mod` isolates all CLI and TUI dependencies (bubbletea, bubbles, lipgloss, cobra, mcp-go) from the core library. Downstream consumers that import only the core module no longer transitively pull in UI toolkits.
- Verified with `go mod graph`: zero TUI dependencies reachable from the root module.

**Continuous integration**
- Three-platform CI matrix: `ubuntu-latest`, `macos-latest`, `windows-latest`.
- Coverage gate: core library ≥ 70% statement coverage; CLI module ≥ 65%.
- Coverage artefacts uploaded per run for trend analysis.
- Non-blocking benchmark job (`BenchmarkRecall_100`, `BenchmarkRecall_1000`, `BenchmarkTokenize`, `BenchmarkKeywordScore`) with `benchmem` and 3-second bench time.

**Test suite**
- `pkg/memory/fuzz_test.go`: three fuzz targets — `FuzzTokenize`, `FuzzUnmarshalFact`, `FuzzKeywordScore` — each with a seeded corpus for deterministic CI runs. Property assertions cover: no-panic on arbitrary input, token length invariant (≥ 2 characters), lowercase normalisation, valid fact-ID format, and non-negative keyword scores.
- `pkg/memory/store_test.go`: `TestPut_WithEmbedder` and `TestRecall_WithEmbedder` using a fixed-vector `goodProvider`, exercising `addToVector`, `chromemVectorStore.AddDocument`, and `chromemVectorStore.Query` end-to-end. Additional unit tests for `DB()`, `SetKG()`, and `marshalJSON`.
- `cmd/graymatter/internal/server/server_test.go`: `TestConcurrentRememberAndRecall` (10 concurrent writers, 10 concurrent readers, 5 operations each; verifies all 51 facts persisted without data loss) and `TestRequestContext_Cancellation` (pre-cancelled context requests must not produce 5xx responses or leave the server in a degraded state).

### Fixed

**Critical — HTTP server store lifecycle**
- The HTTP server was opening and closing a bbolt database handle on every incoming request. Because bbolt enforces exclusive process-level file locking, concurrent requests would contend on the lock, and the in-process vector index would be rebuilt from disk on each call. The `Store` is now opened once in `New()`, held for the lifetime of the server, and closed in `Shutdown()`. A `storeReady` guard returns HTTP 503 if the store has not yet initialised, preventing nil-dereference panics at startup.

**Critical — Ollama embedding retry body reuse**
- The Ollama embedding client's retry loop was reusing the original `*http.Request`, whose body had already been consumed by the first attempt. Subsequent retries sent an empty body, receiving a 400 from the server and exhausting all attempts without ever embedding the text. Each retry iteration now constructs a fresh `*http.Request` with a new `bytes.Reader`. Backoff: 500 ms after attempt 1, 1 s after attempt 2; respects context cancellation.

**Correctness — consolidate.go error handling**
- `UpdateFact` errors inside the decay loop were silently discarded via blank-identifier assignment (`_ = err`). Errors are now accumulated with `errors.Join` and returned from `MaybeConsolidate` so callers and the `OnConsolidateError` callback receive the full error set.
- `summariseFacts` was calling `Delete` before `Put`. A crash between the two operations would permanently lose the fact. The order is now `Put` then `Delete`, preserving the fact on disk in all failure scenarios.
- `UpsertNode` errors in the knowledge-graph enrichment path were silently ignored; they are now forwarded to `OnConsolidateError`.

### Changed

**Stop-word filtering performance**
- The `stopWords()` function — previously called on every invocation of `tokenize`, allocating a new `map[string]bool` each time — has been replaced with a package-level `var stopWordSet` initialised once at program startup. This eliminates per-call heap allocations on the hot recall path.

**`vectorSearch` return type**
- `vectorSearch` now returns `[]VectorResult` (a stable, interface-aligned type) instead of the previously leaked `[]chromem.Result`. This decouples the recall path from the chromem-go type system and is required for the `VectorStore` abstraction.

**expvar singleton guard**
- `expvar.NewMap` panics if the same name is registered twice within a process (as occurs in test suites that instantiate multiple `Server` instances). A `getOrNewMap` helper now checks `expvar.Get` before calling `NewMap`, preventing test-suite panics without altering production behaviour.

**bbolt cleanup ordering on Windows**
- `t.TempDir()` deferred cleanup was running before `srv.Shutdown()` in server tests, causing bbolt to fail to release its file lock on Windows before directory removal. All shutdown calls now use `t.Cleanup` (which runs in LIFO order before `TempDir` cleanup) instead of `defer`, ensuring correct resource teardown on all platforms.

### API Stability

See [`docs/api-stability.md`](docs/api-stability.md) for the list of stable public identifiers and the compatibility promise for the v0.x series.

---

## [0.1.0] – 2026-04-10

### Added

**Core memory API**
- `Remember` / `Recall` / `Consolidate` / `Close` public API
- `RememberShared` / `RecallShared` / `RecallAll` for shared cross-agent memory
- Hybrid retrieval: vector similarity (chromem-go) + keyword TF-IDF + recency, fused via RRF
- Exponential decay curve with configurable half-life (`DecayHalfLife`, default 30 days)
- bbolt-backed durable storage with chromem-go in-process vector index

**Goroutine lifecycle (fixes)**
- Async consolidation now uses a bounded semaphore (`MaxAsyncConsolidations`, default 2)
- All background goroutines are tracked with a `sync.WaitGroup`; `Close()` drains them before closing the database
- Consolidation errors surfaced via optional `OnConsolidateError` callback instead of being silently discarded
- `context.Background()` replaced with a cancellable shutdown context throughout

**Consistency (fixes)**
- `Open()` runs `reconcileVectors()`: re-indexes any bbolt fact missing from the vector store, repairing divergences caused by crashes between the bbolt commit and the vector write

**Fact extraction primitive**
- `Extract(ctx, llmResponse)` — extracts up to 5 atomic facts from an LLM response using structured JSON output
- `RememberExtracted(ctx, agentID, llmResponse)` — one-call Extract + Remember
- Graceful degradation: without an API key, returns the raw response as a single fact

**Knowledge graph**
- Entity extraction (regex and LLM-based via `pkg/kg`)
- Graph-enriched recall: top-ranked fact neighbours surfaced automatically
- Obsidian-compatible markdown export (`pkg/export`)

**Tooling**
- CLI: `init remember recall consolidate checkpoint export run sessions tui`
- MCP server (stdio + HTTP) with `memory_search`, `memory_add`, `checkpoint_*`, `memory_reflect`
- REST API server
- Bubble Tea TUI (4 views: facts, sessions, graph, stats)
- Plugin system (JSON-line protocol)
- Session checkpointing and recovery (`pkg/session`)

**Embedding backends**
- Auto-detection: Ollama → OpenAI → Anthropic → keyword-only
- Explicit overrides via `EmbeddingMode` config field

### API Stability

See [`docs/api-stability.md`](docs/api-stability.md) for the list of stable public identifiers and the compatibility promise for the v0.x series.

[0.2.1]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.2.1
[0.2.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.2.0
[0.1.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.1.0
