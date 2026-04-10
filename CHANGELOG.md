# Changelog

All notable changes to this project are documented in this file.  
Format: [Keep a Changelog](https://keepachangelog.com/en/1.0.0/)  
Versioning: [Semantic Versioning](https://semver.org/spec/v2.0.0.html)

---

## [Unreleased]

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

[0.1.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.1.0
