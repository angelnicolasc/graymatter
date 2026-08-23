# 005 — Embeddings degrade Ollama → OpenAI → Anthropic → keyword

**Status:** Accepted · **Date:** 2026-08-22

## Context

Hybrid retrieval wants vector similarity, and vectors need an embedding model.
Every way of getting one costs something a local-first tool would rather not
spend: an API key, a network round trip per write, a running local service, or
retrieval quality.

The requirement that decides it: **`graymatter init` has to work on a laptop
with no API key and no internet, and the tool has to be useful afterwards.** An
embedding provider that is mandatory makes that impossible.

## Decision

Embeddings are optional, and the provider is detected at runtime in a fixed
order. `EmbeddingAuto` — the default — tries:

1. **Ollama** — local HTTP, no key, no data leaves the machine, `nomic-embed-text`.
2. **OpenAI** — `text-embedding-3-small`, if `OPENAI_API_KEY` is set.
3. **Anthropic** — if `ANTHROPIC_API_KEY` is set.
4. **Keyword-only** — TF-IDF over stored text, plus recency. No vectors at all.

Local first, then whatever credential is present, then a floor that always
works. `EmbeddingMode` pins any single provider explicitly.

Step 4 is the load-bearing one. Keyword-only is not an error state or a
degraded mode that warns; it is a supported configuration. Retrieval still
fuses keyword relevance and recency through RRF, the entire test suite runs on
it, and so does the benchmark — which is why the published numbers are
reproducible by anyone with no key.

### Consolidation does not follow this chain

Summarisation is a separate setting with a separate resolution, and its rules
differ:

- `ConsolidateLLM` resolves to `"anthropic"` if `ANTHROPIC_API_KEY` is set,
  otherwise `""` (disabled).
- Ollama is **excluded from auto-detection** here, unlike for embeddings.
  Detecting it means probing an HTTP endpoint, and paying 500 ms+ on every
  process start — including every `graymatter recall` — to discover a service
  that usually is not running is not a trade worth making. Set
  `ConsolidateLLM = "ollama"` explicitly instead.
- Setting it explicitly does not work yet: **Ollama summarisation is not
  implemented.** As of v0.10.0 that path returns
  `ErrConsolidateLLMUnsupported` through `OnConsolidateError`. Before v0.10.0
  it returned an empty summary and a nil error, so a store configured this way
  ran decay and pruning forever, never summarised, and gave no indication why.

## Consequences

- Retrieval quality varies with the environment. The same store answers the
  same query differently on a machine with Ollama than on one without, and
  nothing in the tree measures how much — vector-versus-keyword recall quality
  is unmeasured (`docs/benchmarks.md`).
- Switching providers mid-store mixes embeddings from different models in one
  index. The dimension guard catches a changed vector width and warns; it
  cannot catch two models that happen to share dimensions, whose vectors are
  simply not comparable.
- Facts written while an embedder was unavailable have no vectors. The pending
  queue and the reconciler exist for this, and `PendingVectorCount()` reports
  it.
- Auto-detection makes the active provider a runtime fact rather than a
  configured one. `graymatter doctor` prints which one is in use, because
  otherwise nobody can tell.

## Reversal condition

Reconsider the ordering if any of these is measured:

1. Keyword-only recall scores below **70%** of Ollama-backed recall on a
   quality benchmark. That would make the floor too weak to call supported,
   and would argue for warning loudly rather than degrading quietly.
2. Ollama detection latency exceeds **200 ms** at p95 in ordinary use, making
   the default probe worse than requiring configuration.
3. A local embedding model good enough to ship *inside the binary* becomes
   practical. That would collapse the chain into one provider and delete this
   decision outright — the best outcome available.

Implementing Ollama summarisation is not a reversal of this record, only the
removal of a gap in it.

## Alternatives rejected

- **Require an embedding provider.** Kills offline use, and the reproducible
  benchmark with it.
- **Bundle a model.** Adds tens of megabytes to a binary whose selling point is
  being one file, for quality still below a hosted model.
- **Ask the user to choose at init.** A question most people cannot answer
  before using the tool, and the auto-chain gets it right for nearly everyone.
- **Probe Ollama for consolidation too, symmetrically.** Consistent, and it
  taxes every CLI invocation to discover a service that is usually absent.
