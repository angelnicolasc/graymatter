# 001 — Memory decays on a 30-day half-life

**Status:** Accepted · **Date:** 2026-08-22

## Context

A memory store that only accumulates becomes a liability. Retrieval quality
falls as the corpus grows, because more candidates compete for the same eight
slots, and the oldest material is usually the least true. Something has to
decide what stops mattering.

The options were to keep everything and rely on ranking, to expire facts on a
fixed TTL, or to weight them on a decay curve and prune what falls through the
floor.

## Decision

Every fact carries a `Weight` in [0, 1], starting at 1.0. `Consolidate` decays
it exponentially against time since last access:

```
weight *= exp(-ln(2) / halfLife * hoursSinceAccessed)
```

`DecayHalfLife` defaults to **720h (30 days)**. Facts below **0.01** are
pruned — roughly seven half-lives untouched, about seven months at the
default.

Decay is driven by `AccessedAt`, not `CreatedAt`. A fact recalled regularly
stays alive indefinitely regardless of age; a fact nothing ever asks for
fades whether it was written yesterday or last year. Recall is what keeps
memory alive, which is the property worth having: usage is the only evidence
of relevance the store actually has.

30 days was chosen against the rhythm of the work this is for. A month is
about one project cycle: long enough that a fact from the start of a piece of
work is still weighted when the work finishes, short enough that a decision
reversed two quarters ago is not competing for a slot with a decision made
last week. It is a judgement, not a measurement, and it is exposed as
`DecayHalfLife` precisely because it is a judgement.

### The threshold is read twice, deliberately

`ConsolidateThreshold` gates two different things with two different
comparisons, which looks like an off-by-one and is not:

- `MaybeConsolidate` runs a cycle at **>= N** facts.
- `Consolidate` summarises at **> N** facts.

At exactly N facts a cycle runs decay and pruning but does not summarise.
Decay and pruning are arithmetic on data already in hand — cheap, local,
reversible in effect. Summarisation costs an API call and *destroys the batch
it replaces*. The expensive, lossy step waits until the store is unambiguously
over the line; the cheap one does not need to.

## Consequences

- A fact nobody recalls for ~7 months is deleted. There is no undo, and no
  archive. Anything that must never be lost does not belong in a decaying
  store — that is what `graymatter export` is for.
- Decay only advances when `Consolidate` runs. A store that never crosses
  `ConsolidateThreshold` never decays. Weights are therefore a lagging
  measure, not a live one.
- Two classes of fact are actively mismodelled by any decay curve:
  **standing obligations** (licence terms, compliance constraints, security
  invariants) and **architecture decisions that are still in force**. Both are
  most valuable precisely when nobody has asked about them in months. The
  store has no way to mark them, and this is the sharpest known limitation of
  the decay model. Keep them in the instruction file, not in memory.
- Time-to-prune scales with the half-life, so raising it does not just slow
  forgetting, it delays every eviction by the same factor.

## Reversal condition

Revisit if any of these becomes measurable:

1. Recall of a fact known to be relevant fails because that fact was pruned,
   in more than **2%** of a measured sample. Currently unmeasurable — nothing
   in the tree measures retrieval quality (`docs/benchmarks.md`), so this
   condition depends on building that first.
2. Stores in ordinary use grow past **50k facts per agent**, which would mean
   pruning is not keeping pace and the floor, not the half-life, is wrong.
3. Users are found routinely setting `DecayHalfLife` to the same non-default
   value, which would make 30 days the wrong default rather than the wrong
   idea.

Any of the three argues for tuning the constants. None of them argues against
decay itself. The decision that would actually be reversed — weighting rather
than expiring — needs evidence that recency-weighted ranking is worse than a
fixed TTL, which no one has produced.

## Alternatives rejected

- **Fixed TTL.** Simpler, and wrong for the same reason a cache TTL is wrong
  for a knowledge base: it cannot tell a fact nobody needs from a fact nobody
  has needed *yet*.
- **Keep everything, rank harder.** Defensible while a store is small. It
  moves the entire cost onto retrieval, and retrieval runs on every prompt
  while pruning runs occasionally.
- **LLM-judged retention.** Better decisions, at the cost of making the store
  non-deterministic and unusable offline. Consolidation is deliberately the
  only step in this system allowed to be smart.
