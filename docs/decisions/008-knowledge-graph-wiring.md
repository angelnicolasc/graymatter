# 008 — Knowledge-graph auto-population ships gated, measured, and budgeted

**Status:** Accepted — **Date:** 2026-08-23
**Amends:** [003](003-knowledge-graph-autopopulation.md) (its reversal conditions were met)

## Context

ADR-003 documented the awkward truth: the graph had a write path, but nothing
populated it automatically, and the reversal conditions demanded measurement
before wiring. Two benchmarks now exist and were run in order:

1. `extraction_precision` (105 hand-labeled facts): ID precision 0.928 with
   the v1 extractor; 0.946 after four deterministic fixes (Unicode classes,
   determiner stripping, org-suffix families, role titles). Gate ≥ 0.70 PASS.
2. `retrieval_quality -fixtures fixtures-v2` multi-hop protocol: with a
   corpus whose entities recur across sessions — as real project memory does
   — entity-bridge enrichment answers **67%** of queries that plain keyword
   ranking answers at **0%**, dead rate unchanged, Δp95 within noise.

On corpus-v1 the same enrichment scored 0% vs 0%: its fillers carry no
recurring proper nouns, so no bridge can exist. The lesson is structural —
enrichment value depends on recurrence, which real project memory has and
synthetic uniform corpora do not.

## Decision

Auto-population ships **gated**: `daemon run --kg` or `GRAYMATTER_KG=1`.
Default remains off until a full release cycle confirms field behaviour.

- Consolidation consumes a `TypedEntityExtractor` when the wired extractor
  implements it: nodes keep label + type, co-mentioned pairs become edges.
  Extractors without the capability keep the legacy ID-only path verbatim.
- Recall enrichment is budgeted: at most **3** neighbour labels appended
  after the ranked facts, tombstones respected on every path. Documented as
  an explicit exception to the exactly-topK contract, active only when the
  graph is wired.
- `AdvancedStore.SetKG` is exposed so hosts (CLI, daemon) construct the graph
  and extractor where they own the bbolt handle.

## Consequences

- Issue #24 closes: nodes *and* edges appear from ordinary use, typed and
  traversable, with zero agent effort beyond storing facts.
- The TUI Graph tab, Obsidian export, and future analytics read a populated
  graph instead of an empty one.
- Reversal condition: if any future re-run of these benches shows Dead > 0%,
  EnrichedHitRate ≤ baseline on recurring-entity corpora, or a recall p95
  regression beyond +2ms attributable to enrichment, the default stays off /
  flag is flipped back and ADR-008 gains an Amended note. Extraction
  improvements remain the unlock path, never a silent behaviour change.
