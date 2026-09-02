# Predictions — revision currency suite

Written before the suite was run for the first time, per `benchmarks/RESULTS.md`
convention. The numbers below are commitments, not descriptions.

## What is being measured

The same 600-fact history, twice: once with every correction written as an
independent fact (what an agent produces today), once with the corrections
recorded as revisions through `Store.Revise` (what `graymatter revise` writes).
35 revision families, 19 of them paraphrased.

Endpoint per probe: **A** the current value outranks every retired sibling
**and** **B** it lands in the injected top-8.

## The commitments

| Quantity | Prediction |
|---|---|
| flat arm, A∧B | 6 – 14 of 35 |
| revised arm, A∧B | 22 – 30 of 35 |
| revised arm, **A** | **35/35 exactly** |
| retired facts shown, flat | 35 – 45 |
| retired facts shown, revised | **0 exactly** |
| McNemar exact p | < 0.001 |
| revised arm, paraphrased stratum | below the literal stratum by 20–45 points |

Two of these are stated as exact values rather than ranges, and that is
deliberate — they are structural claims, not measurements. If the revised arm
returns a single retired fact, or misses currency on a single probe, the
tombstone filter is not doing what `pkg/memory/recall.go` says it does, and the
suite has found a bug rather than a number.

## Prior, and why the range is wide

A Python harness over the same 35 families measured 8/35 flat and 26/35 revised
(p = 8e-6) against a store built through the CLI. This suite is not that
harness: the filler is enumerated instead of sampled, the timeline is scripted
rather than wall-clock, and the corpus is rebuilt in Go. The families and the
endpoint are the same, so the shape should hold; the exact counts should not be
assumed to transfer, and the ranges above are set to fail loudly if the
mechanism is weaker than claimed rather than to be safe.

## What would falsify the design, not just the number

- **Revised arm A < 35/35** → currency is not settled by the tombstone filter
  alone, and ranking work to order current above retired is back on the table.
- **Revised arm shows any retired fact** → the filter has a hole.
- **p ≥ 0.05** → the edges do not pay for themselves on this workload, and
  recording revisions is a usability change with no measurable retrieval effect.
- **Paraphrased stratum not below literal** → the residual is not retrieval,
  and blaming the embedder for the remaining failures is wrong.
