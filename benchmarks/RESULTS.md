# Retrieval quality — predictions and results

Predictions in this file were committed before the benchmark existed.
`git log --follow benchmarks/RESULTS.md` shows the commit adding the
predictions preceding the commit adding the numbers.

All three predictions are scored below against the measurement, including the
one that landed outside its stated band.

| | |
|---|---|
| Predictions written | 2026-08-23 |
| Results measured | 2026-08-23 |
| Command | `go run ./benchmarks/retrieval_quality` |
| Fixtures | `benchmarks/fixtures/corpus-v1.jsonl`, `benchmarks/fixtures/queries-v1.jsonl` |
| Environment | keyword embedder, no network, no LLM, no API key |
| Reproducibility | 3 consecutive local runs and one CI run, every quality metric identical |

---

## Scope

`benchmarks/token_count` measures tokens saved against full-history injection.
It does not measure whether the returned facts answer the query: a system
returning eight facts at random scores the same 90% reduction.

This benchmark measures retrieval quality, against a sliding window as well as
against full-history injection.

## Systems compared

| System | Description |
|---|---|
| `full-history` | Every stored fact injected. Kept for continuity with `token_count`. |
| `window-8` | A sliding window: the last 8 facts by insertion order. Implemented directly, not simulated. |
| `graymatter-fixed-k` | `Recall` with `TopK=8`, every knob at its default. |
| `graymatter-adaptive` | `Recall` with `TopK=8` and `MinRelevance=0.5`. |
| `graymatter-recency-only` | `SignalWeights{Vector:0, Keyword:0, Recency:1}`, `TopK=8`. |

`graymatter-recency-only` is a cross-check, not a baseline. ADR-006 states that
a sliding window is the special case of this ranking with all weight on
recency; `window-8` is implemented independently so that statement can be
tested rather than assumed.

## Protocol

Two modes, not mixed within a single comparison:

- **fixed-K** — `TopK=8` for GrayMatter against `window-8`, which holds 8
  facts. Equal fact budget.
- **adaptive** — `MinRelevance=0.5`, which returns a variable number of facts.
  Reported separately, since a comparison against a fixed-K baseline measures
  the budget as well as the ranking.

Corpus: 78 facts across three domains (infra 30, product 24, team 24), written
in session order 1 to 99. Queries: 6 across the same three domains, all asked
at session 100. Five **gold** facts are planted at sessions 2 to 4. One
**stale** fact (session 8) is contradicted by a **replacement** (session 71),
and is tombstoned before the queries run.

Both fixtures are frozen. Extending means adding `corpus-v2` rather than
editing v1, so results published against v1 stay reproducible.

## Metrics

| Metric | Definition |
|---|---|
| **HitRate** | Fraction of queries where at least one gold fact appears in the returned set. |
| **Dead** | Fraction of queries returning a fact the store records as superseded. |
| **Tokens/q** | `words × 1.33` over the returned set — the same approximation `token_count` uses, from the same shared function, so the two benchmarks are numerically comparable. |
| **p50 / p95** | Wall-clock per `Recall` call. |

The **Dead** column is named for what it measures: a returned fact that the
store records as superseded. The generative sense of "hallucination" does not
apply, since retrieval returns only stored strings.

---

## Results

### fixed-K protocol — equal fact budget

| System | HitRate | Dead | Tokens/q | Facts/q | p50 | p95 |
|---|---|---|---|---|---|---|
| `full-history` | 100% | 17% | 1141 | 78.0 | 0s | 18µs |
| `window-8` | 0% | 0% | 95 | 8.0 | 0s | 0s |
| `graymatter-fixed-k` | 83% | 0% | 114 | 8.0 | 605µs | 1.57ms |
| `graymatter-recency-only` | 0% | 0% | 95 | 8.0 | 531µs | 1.61ms |

### adaptive protocol — reported separately

| System | HitRate | Dead | Tokens/q | Facts/q | p50 | p95 |
|---|---|---|---|---|---|---|
| `graymatter-adaptive` | 83% | 0% | 64 | 4.0 | 1.00ms | 1.00ms |

### Per query

```
System                      q1    q2    q3    q4    q5    q6
──────────────────────────────────────────────────────────────
full-history                ·     ·     ·     ·     ·     ·D
window-8                    x     x     x     x     x     x
graymatter-fixed-k          ·     x     ·     ·     ·     ·
graymatter-recency-only     x     x     x     x     x     x
```

`·` hit · `x` miss · `D` returned a superseded fact

| Query | Domain | Text |
|---|---|---|
| q1 | infra | what order do migrations and the api rollout go in |
| q2 | infra | how do i roll back a bad deploy |
| q3 | infra | are release tags required to be signed for deploy |
| q4 | product | what blocks enterprise customers from signing |
| q5 | team | how many approvals does a billing change need |
| q6 | infra | where are production secrets stored |

---

## Predictions, scored

### P1 — tokens relative to a sliding window

Predicted: `graymatter-fixed-k` within **±15%** of `window-8`.
Measured: **114 against 95 tokens/query, +20%** — outside the stated band.

Cause: at equal fact count both systems return 8 facts, and the facts selected
by relevance are longer than the 8 most recent ones. The planted gold facts are
full explanatory sentences; the newest 8 include several short ones. Token cost
at a fixed fact count therefore tracks the length of the selected facts, not
the count.

The adaptive mode reaches the same HitRate at 64 tokens/query by returning 4
facts instead of 8. That measurement belongs to the adaptive protocol and is
not a fixed-K comparison.

### P2 — HitRate on facts planted early

Predicted: `window-8` ≈ 0%, `graymatter-fixed-k` > 70%.
Measured: **0%** and **83%** — both within the predicted ranges.

The window result is structural: the gold facts are at sessions 2 to 4 and the
window holds sessions 92 to 99, so they are outside it.

The stated candidate explanation for a low GrayMatter result — recency
dominating the RRF fusion at its default weight of 0.5 — is not supported by
the measurement. `graymatter-recency-only` scores 0%, identical to the window,
while the default configuration scores 83%, so recency is not determining the
default ranking.

The one miss is q2, *"how do i roll back a bad deploy"*, whose gold fact reads
*"Rollbacks are performed with argo rollouts undo…"*. The query contains "roll
back"; the fact contains "Rollbacks". The keyword scorer applies no stemming,
so the two do not match, and the fact is reachable only through terms the query
does not use. This accounts for the difference between 83% and 100%.

### P3 — superseded facts after tombstoning

Predicted: 0 queries returning the superseded fact.
Measured: **0**, against **17% for `full-history`**.

`full-history` returns the superseded fact on the one query that asks about it.
`window-8` also measures 0 here, for a structural reason rather than a
supersede mechanism: the superseded fact is at session 8 and falls outside the
window. The same property produces its 0% HitRate.

---

## ADR-006 cross-check

ADR-006 states that a sliding window is the special case of this ranking with
all weight on recency. `window-8` is implemented independently, and for **every
query** `SignalWeights{0,0,1}` returned exactly the same set of facts.

```
CONFIRMED: for every query, SignalWeights{0,0,1} returns exactly the
facts an independently implemented sliding window returns.
```

Identical HitRate, dead rate, token count and fact set. The check runs on every
invocation, so it fails if that stops holding.

---

## Out of scope for this benchmark

- **Vector retrieval.** Keyword embedder only, so results are reproducible
  without an API key. Whether vector recall resolves q2 — "roll back" and
  "rollbacks" being close in embedding space and distant in token space — is
  untested.
- **Consolidation.** Runs with summarisation disabled.
- **Scale.** 78 facts. Latency at this size does not extrapolate.
- **Multi-hop questions.** Every query is answerable from a single fact.
- **Query provenance.** The six queries were written alongside the corpus. A
  v2 corpus with queries drawn from recorded sessions would test generalisation
  that v1 cannot.

Latency figures are coarse: measurements ran on Windows, where timer
granularity is about 1ms, and every value is within a small multiple of that
floor. They establish sub-millisecond recall at this corpus size and no finer
resolution than that.
