# 011 — Consolidation is propose/apply with tombstone receipts; Ollama is the offline summariser

**Status:** Accepted — **Date:** 2026-08-25

## Context

Three audit findings converged on one subsystem:

- **A5** — the summarisation step replaced its batch with a **hard delete**
  (`s.Delete`), making consolidation the single code path that violated
  ADR-007's "tombstones, never delete". A summary that dropped a detail the
  user considered key destroyed the evidence of what had been there.
- **A6** — `ConsolidateLLM="ollama"` was accepted by config and rejected by
  runtime (`ErrConsolidateLLMUnsupported`): a store could be configured for
  fully-local intelligence and silently never get any.
- **A7** — every cycle re-extracted every surviving fact into the knowledge
  graph. Idempotent, but O(facts) per cycle forever, and a real cost once the
  extractor is an LLM.

The playbook's constraint that shaped everything else: an LLM proposes,
never applies. And W1's invariant I-1 already keeps pinned facts out of the
batch, so no proposal can ever consume them.

## Decision

**Propose/apply.** The summariser returns a structured proposal:

```json
{"summary": "<paragraph>", "consumes": ["<fact-id>", ...], "contradictions": [...]}
```

Application is deterministic code, not model judgement:

1. The summary enters with `Put`. If that fails, nothing else happens.
2. `consumes` is **clamped to the batch** — the model may only consume what
   it was shown. Hallucinated IDs are ignored; duplicate IDs consume once.
3. Consumed facts become tombstones (`SupersededBy` → the *real* summary
   fact's ID) and keep their post-decay weight. Zeroing their weight instead
   would let the same cycle's prune step delete each receipt milliseconds
   after writing it — receipts must outlive the cycle so `List`, export and
   the TUI stay auditable. Ordinary decay collects them on schedule.
4. A response that is not a valid proposal (malformed JSON after fence
   stripping, empty summary, empty consumes) is **discarded**: hook fires,
   store untouched, counters unmoved. A broken summariser degrades to the
   exact behaviour of `ConsolidateLLM=""`.

The Anthropic path predates structured proposals; whatever it returns is
treated as consuming the whole batch — same semantics as before, now with
receipts.

**Ollama is implemented** as the local summariser: `/api/generate` with
`"format":"json"` (sampler-level JSON constraint, not prompt hope), model
from `GRAYMATTER_OLLAMA_CONSOLIDATE_MODEL` (default `llama3.2`). One retry,
and only for transient failures — transport errors, 5xx, 408, 429. A 4xx is
a deterministic rejection; retrying reproduces it. An unreachable Ollama
surfaces through `OnConsolidateError`; the sentinel
`ErrConsolidateLLMUnsupported` is retired (kept only so callers matching it
still compile). Offline-first holds both ways: users without Ollama see no
difference, users with Ollama need no account and no key.

**Extraction watermark (A7).** The meta bucket records a SHA-256 text
signature per extracted fact. A fact is re-extracted only when its text
changed or its ID is new. Superseded facts are skipped entirely: since they
stopped being deleted, the pass would otherwise feed retired content back
into the graph — the graph mirrors recallable memory. Deleting a fact drops
its signature with it.

**Counters.** Each applied proposal bumps `consolidations` and
`facts_consolidated` in the meta bucket; `status` (human and JSON),
`StoreOverview`, and `doctor`'s store check surface them. What the product
measures it can publish honestly.

## Consequences

- No consolidation path deletes anymore; ADR-007's append-only promise is
  uniform across supersede and consolidate.
- Summarisation quality is now bounded below by "no worse than disabled":
  every failure mode lands on decay+prune-only behaviour.
- Tombstones linger until decay collects them, so `List` overstates live
  memory briefly; every consumer already filters by `IsSuperseded`.
- Local-model output quality varies; the structured contract plus clamping
  means the worst a bad model can do is nothing.

## Reversal condition

If real stores show summaries routinely dropping information users expected
to survive (measurable via the recall-quality gate in
`benchmarks/retrieval_quality/consolidation_test.go` against lossy mock
summarisers), the next step is requiring the proposal to quote consumed fact
IDs *verbatim inside the summary* before apply accepts it — turning the
summary itself into a receipt-checkable artifact.
