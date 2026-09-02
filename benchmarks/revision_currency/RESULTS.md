# Revision currency — results

Run: `go run ./benchmarks/revision_currency`. Gate: `go test ./benchmarks/revision_currency`.
Keyword embedder, scripted timeline, enumerated filler. No network, no LLM, no API key, no randomness.
Predictions in [PREDICTIONS.md](PREDICTIONS.md) were committed before the first run.

## First run — 600 facts, 35 revision families, top-k 8

| arm | A∧B | 95% CI | A | B | useful@8 |
|---|---|---|---|---|---|
| flat — no supersede edges | 8/35 | [0.12, 0.39] | 11/35 | 25/35 | 4.71 |
| revised — edges via `Store.Revise` | **25/35** | [0.55, 0.84] | **35/35** | 25/35 | 3.54 |

McNemar exact, paired: **b=17, c=0, p = 0.0000153**.
Retired facts shown to the caller: **35 → 0**.

| stratum | flat | revised |
|---|---|---|
| paraphrased (n=19) | 26% | 53% |
| literal (n=16) | 19% | 94% |

## Reading it

**A = 35/35 is the result.** Recording the revision settles currency on every
probe, of every shape, in both strata — the current value outranks every
retired sibling, and no retired fact reaches the caller. That is a structural
property of the tombstone filter, not a tuned number, which is why the CI gate
asserts it exactly rather than as a threshold.

**B = 25/35 is unchanged between the arms, and it is what caps the endpoint.**
The ten probes the revised arm still misses are retrieval failures: the current
value never reaches the top-8, so there is nothing for currency to order. Every
one of them has keyword rank 0 — the correction shares no content word with the
question (`fortnight` for "how long is a pager rotation?", `NATS` for "what
message broker do we use?"). The paraphrased stratum carries nine of the ten,
which is why it sits at 53% against the literal stratum's 94%.

So the two failure modes are separable and now separately measured: **currency
is solved; retrieval is not.** A better embedder moves B and nothing else.

**useful@8 falls from 4.71 to 3.54** because forty facts left the live set. That
is the intended trade — the block is shorter and none of it is wrong — but it
is reported rather than hidden, because a block that shrinks is a block with
fewer chances to contain the answer.

## Predictions vs. outcome

| Committed | Outcome | |
|---|---|---|
| flat A∧B in 6–14 | 8 | ✓ |
| revised A∧B in 22–30 | 25 | ✓ |
| revised A exactly 35/35 | 35/35 | ✓ |
| retired shown, flat: 35–45 | 35 | ✓ |
| retired shown, revised: exactly 0 | 0 | ✓ |
| McNemar p < 0.001 | 0.0000153 | ✓ |
| paraphrased below literal by 20–45 points | 41 points | ✓ |

Seven of seven. The prediction file also earned its keep during development:
the first run reported A = 25/35 against a committed 35/35, which turned out to
be a bug in the harness — it requested only the top-8 and so could not tell
"outranked by a retired fact" from "not returned at all". A run without a
committed prediction would have published 25 and called it the answer.

## What this does not measure

- **An external benchmark.** LongMemEval-S `knowledge-update` is the standard
  for this capability and needs an LLM judge; it is not offline and is not run
  here.
- **Whether agents record revisions.** The suite measures what happens when
  they do. Making them do it is a separate problem — instructions, hooks, or
  automatic contradiction detection.
- **Embedding-backed retrieval.** The keyword embedder is deliberate: it is the
  default a user gets with no account and no network, and the published number
  should be the one they can reproduce.

## Cross-check

An independent Python harness over the same 35 families, driving the released
binary through `graymatter revise` on the CLI rather than the library, measured
8/35 flat and 26/35 revised, A = 35/35, retired-shown 0, p = 8e-6, and the same
53%/94% stratum split. Two implementations, two write paths, one answer.
