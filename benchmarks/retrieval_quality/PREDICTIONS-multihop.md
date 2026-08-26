# Pre-registered predictions — multi-hop / EnrichedHitRate (P1.2)

| | |
|---|---|
| Prediction date | 2026-08-23 |
| Queries | `benchmarks/fixtures/queries-multihop-v1.jsonl` (q7–q9 over golds f002/f031/f055) |
| Systems compared | `graymatter-fixed-k` (baseline) vs `graymatter-enriched` (co-mention entity expansion, cap 3, post-ranking) |
| Prior diagnostic (disclosed) | Regex-extractor scan over corpus-v1: **1 of 78 facts** yields extractable entities (`f024`: secrets manager[role], vault[fact]) — none adjacent to a gold |

## Predictions (written BEFORE the first enriched run)

| Metric | Pre-registered band | Rationale |
|---|---|---|
| Baseline HitRate (q7–q9) | exactly **0%** | Zero surface overlap by corpus construction |
| **Enriched HitRate (q7–q9)** | **[0%, 33%]** — estimated point 0% | The bridge index covers ~1.3% of facts; no gold is reachable through a bridge |
| Δp95 (enriched − fixed-k) | ≤ +2ms | Expansion operates over a nearly empty index |
| Dead rate, both systems | held at 0% | Tombstones already pinned |
| **GATE condition 2 (Enriched > baseline)** | **FAIL expected** → No-Go wiring this cycle | Without extraction coverage there are no bridges to walk |

## Implication if the prediction holds

Wiring (PR-D) is **blocked by the very gate the playbook requires**. The blocking
condition is NOT the expansion algorithm but extractor coverage over real technical
prose (1.3%). Documented unblock path:

1. Deterministic extractor improvements (already listed in
   `benchmarks/extraction_precision/RESULTS.md`: organizational suffixes,
   determiner stopwords, Unicode, URL trailing punctuation).
2. Re-run BOTH benchmarks (precision + multi-hop).
3. Only with EnrichedHitRate > baseline measured does PR-D proceed.
