# Changelog

All notable changes to this project are documented in this file.  
Format: [Keep a Changelog](https://keepachangelog.com/en/1.0.0/)  
Versioning: [Semantic Versioning](https://semver.org/spec/v2.0.0.html)

---

## [Unreleased]

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

[0.2.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.2.0
[0.1.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.1.0
