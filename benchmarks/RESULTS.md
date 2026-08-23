# Retrieval quality — pre-registered predictions

**Status: predictions only. No benchmark has been run against this corpus yet.**

This file is committed before the benchmark that fills it in. That ordering is
the point, and `git log --follow benchmarks/RESULTS.md` is how you check it: the
commit adding these predictions must precede the commit adding results. A
prediction written after seeing the numbers is not a prediction.

The rule for what follows: **whatever the run produces gets published here,
including results that contradict what is predicted below.** A failed
prediction is the most informative outcome available — it points at the part of
the design that is wrong, which is worth more than a table confirming what was
already believed.

Written: 2026-08-23.

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
| `graymatter-adaptive` | `Recall` with `TopK=8` and `MinRelevance > 0`. |
| `graymatter-recency-only` | `SignalWeights{Vector:0, Keyword:0, Recency:1}`, `TopK=8`. |

`graymatter-recency-only` is an **internal cross-check, not a baseline**. ADR-006
claims a sliding window is the special case of this ranking with all weight on
recency. That claim is testable precisely because `window-8` is implemented
independently: if the two disagree, the ADR is wrong, and a real window baseline
is what the comparison rests on either way. Substituting the ablation for the
real baseline would make the claim unfalsifiable.

## Protocol

Two modes, never mixed in one comparison:

- **fixed-K** — `TopK=8` for GrayMatter against `window-8`, which holds 8 facts.
  Equal budget, apples to apples. All headline comparisons are in this mode.
- **adaptive** — `MinRelevance > 0`, which returns a variable number of facts.
  Reported separately as a quality-versus-budget curve. Comparing an adaptive
  run against a fixed-K baseline would be measuring the budget, not the ranking.

Corpus: `benchmarks/fixtures/corpus-v1.jsonl` — 78 facts across three domains
(infra 30, product 24, team 24), written in session order 1 to 99. Queries:
`benchmarks/fixtures/queries-v1.jsonl` — 6 queries across the same three
domains, all asked at session 100.

Five **gold** facts are planted early (sessions 2, 2, 3, 3, 4) and are the
answer to a query asked at session 100. One **stale** fact (f006, session 8) is
contradicted by a **replacement** (f024, session 71); the stale one is
superseded through the tombstone mechanism before the contradiction queries run.

Both fixtures are frozen. Extending means adding `corpus-v2`, never editing v1,
or every result published against v1 becomes unreproducible.

Determinism: keyword embedder, no network, no LLM, no API key. Insertion order
follows the corpus file. Timestamps come from the corpus session index through
the store's clock seam, so the run does not depend on when it happens.

## Metrics

| Metric | Definition |
|---|---|
| **HitRate@budget** | Fraction of queries where at least one gold fact appears in the returned set. |
| **Dead-fact rate** | Fraction of queries returning a fact the store already knows is superseded. |
| **Tokens/query** | `words × 1.33` over the returned set — the same approximation `token_count` uses, so the two benchmarks are numerically comparable. |
| **Recall latency** | p50 and p95 wall-clock per `Recall` call. |

On the term *hallucination*: GrayMatter cannot hallucinate in the generative
sense, because it only ever returns strings it stored. The meaningful analogue
is returning a fact the store knows to be superseded — a true-looking answer
that is wrong — which is the dead-fact rate above. Naming it "hallucination"
would be borrowing credibility from a metric this benchmark does not measure.

---

## Pre-registered predictions

### P1 — Tokens: GrayMatter will not beat a sliding window

At equal budget, `graymatter-fixed-k` and `window-8` will be within **±15%** on
tokens per query.

*Reasoning:* both return 8 facts of comparable length. There is no mechanism by
which the same number of facts of the same kind costs materially different
tokens. **If GrayMatter is cheaper here by more than 15%, that is a corpus
artefact, not a win, and it will be reported as such.**

### P2 — HitRate: the window loses old facts by construction

- `window-8`: HitRate **≈ 0%**
- `graymatter-fixed-k`: HitRate **> 70%**

*Reasoning:* the gold facts are at sessions 2 to 4 and the queries are asked at
session 100. A window holding the last 8 insertions cannot reach them — this
part is arithmetic, not a hypothesis. The GrayMatter side is the real
prediction: hybrid retrieval has to surface a 96-session-old fact on keyword
relevance while the recency signal actively pushes against it.

*Primary suspicion if P2 fails:* the recency component of the RRF fusion
dominates too strongly at the default weight of 0.5. The diagnostic is already
built: `graymatter-recency-only` isolates that signal, so if the default run
scores close to the recency-only ablation, recency is doing the ranking and the
default weight is wrong.

### P3 — Contradictions: zero dead facts after supersede

`graymatter-fixed-k` will return the superseded fact in **0** queries.

*Reasoning:* the tombstone filter in `Recall` drops superseded facts before
scoring, so this should be exact rather than approximate. `window-8` and
`full-history` have no notion of supersede and will return it whenever it falls
in range, which is the asymmetry the whole design rests on.

**P3 is the only prediction that claims a categorical win.** P1 predicts a tie
and P2 predicts a win the baseline is structurally incapable of contesting.
That is the honest shape of this comparison: against a sliding window,
GrayMatter's case is not that it costs less, it is that a window cannot recall
an old fact and cannot know a fact is dead.

---

## Results

*Not yet run. This section is filled in by the commit that adds the benchmark,
whatever the numbers turn out to be.*
