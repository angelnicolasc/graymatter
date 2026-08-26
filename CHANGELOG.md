# Changelog

All notable changes to this project are documented in this file.  
Format: [Keep a Changelog](https://keepachangelog.com/en/1.0.0/)  
Versioning: [Semantic Versioning](https://semver.org/spec/v2.0.0.html)

---

## [Unreleased]


### Added

- **`graymatter doctor --embeddings` makes the silent keyword-only fallback visible.**
  `Put` degrades a fact to keyword-only whenever the embedder errors and still returns nil,
  so a broken backend was indistinguishable from an empty store. The store now records the
  lifetime degradation count and the last error (`Store.EmbeddingHealth`, `Store.CountEmbeddings`),
  and the new audit reports vector coverage over live facts, degraded writes, the retry
  backlog, and which of the three honest states you are in: healthy channel, supported
  keyword-only (ADR-005), or a failing backend hiding behind that silence. Deterministic like
  `doctor --health` — reads only store bytes, byte-identical output per store, works with the
  daemon down.

### Fixed

- **`RecallAll` now performs the RRF fusion its documentation always claimed.** The
  implementation concatenated agent-first and truncated, so the shared namespace starved
  whenever the agent list filled its topK — while `docs/api-stability.md` listed `RecallAll`
  under the deterministic-ordering guarantee and the doc-comment promised Reciprocal Rank
  Fusion. The two namespace rankings are now fused with the same k=60 constant Recall uses
  internally (a shared fact ranked 1 beats an agent fact ranked 8), identical texts across
  namespaces accumulate both contributions and appear once, and ties resolve through a total
  order so repeated calls return identical output.

- **`memory_reflect` forget/update receipts survive consolidation.** The MCP surface
  zeroed a retired fact's weight on top of tombstoning it, which drops it under the prune
  floor (<0.01) — so the next consolidation cycle collected the receipt milliseconds after
  writing it, destroying the audit trail ADR-007 keeps tombstones for. The engine's own
  supersede path argued exactly this and kept the weight; the two paths now agree: the
  tombstone keeps whatever weight decay left it, recall still skips it from the next query
  onward, and ordinary decay collects it in due course.

- **The embeddings chain's third slot works.** It dialled
  `api.anthropic.com/v1/embeddings` — an endpoint that does not exist; Anthropic has never
  offered an embeddings API — so every call failed, and because `Put` swallows embedder
  errors, anyone relying on that slot silently ran keyword-only memory while believing they
  had vectors. The slot now targets Voyage AI (`api.voyageai.com/v1/embeddings`, model
  `voyage-3`, 1024 dims unchanged so existing stores stay valid) keyed off `VOYAGE_API_KEY`.
  `EmbeddingAnthropic`/`ModeAnthropic` remain accepted as deprecated aliases: with
  `VOYAGE_API_KEY` set they resolve to the Voyage provider; without one they resolve to
  keyword directly instead of constructing a provider guaranteed to fail on every call.
  ADR-005 amended.

- **The Obsidian entity export no longer loses entities to filename collisions.** Two
  distinct labels that sanitize identically ("Acme Corp" / "Acme_Corp") wrote the same note
  file — the second silently overwrote the first, taking its Related links and MOC entry
  with it. `EntityNoteNames` is now the single naming authority for an export: colliding
  names get a deterministic `-2`/`-3` suffix in canonical-ID order, and node files, Related
  links and the entities index all resolve through it.

- **Ollama auto-detection requires an actual Ollama.** The probe counted any HTTP status
  below 500 as "reachable", so a captive portal or corporate proxy answering 404 for
  `/api/tags` made AutoDetect select the Ollama provider on networks without Ollama; every
  embedding failed and Put's silent degradation turned that into keyword-only memory with a
  latency tax on every write.
