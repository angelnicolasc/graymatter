# Retrieval quality — predictions and results

**Predictions in this file were committed before the benchmark existed.** The
check is `git log --follow benchmarks/RESULTS.md`: the commit adding the
predictions section precedes the commit adding the numbers. A prediction
written after seeing the data is not a prediction.

**One of the three predictions failed.** It is reported below in the same
detail as the two that held, because that was the deal.

| | |
|---|---|
| Predictions written | 2026-08-23 |
| Results measured | 2026-08-23 |
| Command | `go run ./benchmarks/retrieval_quality` |
| Fixtures | `benchmarks/fixtures/corpus-v1.jsonl`, `benchmarks/fixtures/queries-v1.jsonl` |
| Environment | keyword embedder, no network, no LLM, no API key |
| Reproducibility | 3 consecutive runs, every quality metric identical |

---

## Why this benchmark exists

`benchmarks/token_count` measures one thing — tokens saved against
full-history injection — and `docs/benchmarks.md` is explicit that this is the
weakest baseline available and that nothing in the repository measures
retrieval quality at all. A system returning eight facts at random scores the
same 90% reduction.

This benchmark measures the thing that actually matters, against the baseline
production actually uses.

## What is compared

| System | Description |
|---|---|
| `full-history` | Every stored fact injected. The weak baseline, kept for continuity with `token_count`. |
| `window-8` | A real sliding window: the last 8 facts by insertion order, nothing else. Implemented directly, not simulated. |
| `graymatter-fixed-k` | `Recall` with `TopK=8`, every knob at its default. |
| `graymatter-adaptive` | `Recall` with `TopK=8` and `MinRelevance=0.5`. |
| `graymatter-recency-only` | `SignalWeights{Vector:0, Keyword:0, Recency:1}`, `TopK=8`. |

`graymatter-recency-only` is an **internal cross-check, not a baseline**.
ADR-006 claims a sliding window is the special case of this ranking with all
weight on recency. That claim is testable precisely because `window-8` is
implemented independently.

## Protocol

Two modes, never mixed in one comparison:

- **fixed-K** — `TopK=8` for GrayMatter against `window-8`, which holds 8
  facts. Equal budget. All headline comparisons are in this mode.
- **adaptive** — `MinRelevance=0.5`, which returns a variable number of facts.
  Reported separately. Comparing an adaptive run against a fixed-K baseline
  would measure the budget, not the ranking.

Corpus: 78 facts across three domains (infra 30, product 24, team 24), written
in session order 1 to 99. Queries: 6 across the same three domains, all asked
at session 100. Five **gold** facts are planted at sessions 2 to 4. One
**stale** fact (session 8) is contradicted by a **replacement** (session 71),
and is tombstoned before the queries run.

Both fixtures are frozen. Extending means adding `corpus-v2`, never editing v1.

## Metrics

| Metric | Definition |
|---|---|
| **HitRate** | Fraction of queries where at least one gold fact appears in the returned set. |
| **Dead** | Fraction of queries returning a fact the store already knows is superseded. |
| **Tokens/q** | `words × 1.33` over the returned set — the same approximation `token_count` uses, from the same shared function, so the two benchmarks are numerically comparable. |
| **p50 / p95** | Wall-clock per `Recall` call. |

On the term *hallucination*: GrayMatter cannot hallucinate in the generative
sense, because it only ever returns strings it stored. The meaningful analogue
is returning a fact the store knows to be superseded, which is the **Dead**
column. Naming it "hallucination" would borrow credibility from a metric this
benchmark does not measure.

---

## Results

### fixed-K protocol — equal budget

| System | HitRate | Dead | Tokens/q | Facts/q | p50 | p95 |
|---|---|---|---|---|---|---|
| `full-history` | 100% | **17%** | 1141 | 78.0 | 0s | 18µs |
| `window-8` | **0%** | 0% | 95 | 8.0 | 0s | 0s |
| `graymatter-fixed-k` | **83%** | **0%** | 114 | 8.0 | 605µs | 1.57ms |
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

---

## Predictions, scored

### P1 — Tokens: GrayMatter will not beat a sliding window — **FAILED**

Predicted: within **±15%** of `window-8`.
Measured: **114 vs 95 tokens/query — GrayMatter is 20% more expensive.**

The prediction failed, and it failed in the direction unfavourable to
GrayMatter: it costs *more* than a window at the same fact budget, and by
enough to fall outside the band.

The cause is visible in the corpus. Both systems return 8 facts; a window
returns whatever is newest, while GrayMatter returns whatever is most relevant,
and on this corpus the relevant facts are the longer ones — the planted gold
facts are full explanatory sentences, and the newest 8 include several short
ones. Eight facts is not eight equal-sized facts.

This does not overturn the design, but it does kill a claim that could have
been made carelessly: **at equal fact count, GrayMatter is not the cheaper
option.** If tokens are the only thing being optimised, a window wins on this
corpus. The adaptive mode is the interesting reply — it reaches the same
HitRate at 64 tokens/query, below the window's 95, by returning 4 facts instead
of 8. That is a real result, and it lives in a different protocol, so it is not
quoted as a fixed-K win.

### P2 — HitRate: the window loses old facts by construction — **HELD**

Predicted: `window-8` ≈ 0%, `graymatter-fixed-k` > 70%.
Measured: **0%** and **83%**.

The window scores 0 on every query, which is arithmetic: the gold facts are at
sessions 2 to 4 and the window holds sessions 92 to 99. It cannot reach them.

The GrayMatter side is the part that was a real prediction, and it cleared the
threshold. The stated primary suspicion — that recency dominates the RRF fusion
at its default weight of 0.5 — is **refuted** by the ablation in the same
table: `graymatter-recency-only` scores 0%, identical to the window, while the
default configuration scores 83%. Recency is not driving the default ranking.

The one miss is q2, *"how do i roll back a bad deploy"*, whose gold fact reads
*"Rollbacks are performed with argo rollouts undo…"*. The query says "roll
back"; the fact says "Rollbacks". The keyword scorer has no stemming, so the
two do not match, and the fact is reachable only through terms the query does
not use. That is a concrete, actionable retrieval limitation rather than a
mystery, and it is the single reason this is 83% instead of 100%.

### P3 — Contradictions: zero dead facts after supersede — **HELD**

Predicted: 0 queries returning the superseded fact.
Measured: **0**, against **17% for `full-history`**.

`full-history` hands the agent the dead fact on the one query that asks about
it, which is the failure mode the tombstone exists to prevent. `window-8` also
scores 0 here, and it is worth being precise about why: not because a window
knows the fact is dead, but because the dead fact is at session 8 and falls
outside the window entirely. It avoids the contradiction by having forgotten
everything, which is the same reason it scores 0% on HitRate. Those two numbers
are the same fact about a window, read twice.

---

## Bonus: ADR-006 checked empirically

ADR-006 claims a sliding window is the special case of this ranking with all
weight on recency. The benchmark tests it: `window-8` is implemented
independently, and for **every query**, `SignalWeights{0,0,1}` returned exactly
the same set of facts.

```
CONFIRMED: for every query, SignalWeights{0,0,1} returns exactly the
facts an independently implemented sliding window returns.
```

Identical HitRate, identical dead rate, identical token count, identical facts.
The claim in ADR-006 is now demonstrated rather than asserted, and the check
runs on every invocation, so it will start failing if that stops being true.

---

## What this benchmark does not measure

- **Vector retrieval.** Keyword embedder only, so the numbers are reproducible
  without an API key. Vector recall would likely fix q2, since "roll back" and
  "rollbacks" are close in embedding space and far apart in token space. That
  is a hypothesis, not a result.
- **Consolidation.** Runs with summarisation disabled.
- **Scale.** 78 facts. Latency at 78 facts says nothing about 100k.
- **Multi-hop questions.** Every query is answerable from a single fact.
- **Query realism.** Six queries written by the same person who planted the
  facts. That is the most obvious weakness in this design, and the reason the
  corpus is versioned: a v2 written by someone else, or drawn from real
  sessions, would be worth more than any amount of tuning against v1.

Latency figures are coarse: the measurements run on Windows, where the timer
granularity is around 1ms, and every value here is within a few multiples of
that floor. They establish that recall is sub-millisecond-ish at this scale and
nothing finer.
