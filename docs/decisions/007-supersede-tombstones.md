# 007 — Contradictions are resolved by tombstone, never by delete

**Status:** Accepted · **Date:** 2026-08-22

## Context

An append-only memory store accumulates contradictions. An agent learns "we use
Lemon Squeezy for billing", the company migrates, the agent learns "we use
Polar" — and now both are stored, both are true of some moment, and only one is
true now.

Retrieval cannot tell them apart. Both match the query, both are plausible, and
the agent receives both with no signal about which is current. This is the
failure mode that makes long-lived memory worse than no memory: not missing
information, but confidently returned stale information sitting next to its own
correction.

`memory_reflect` already had the vocabulary for this — `update` to correct a
fact, `forget` to drop one. Neither worked. Both set `Weight = 0` and reported
success, and `Recall` does not read `Weight`; it ranks on vector, keyword and
recency alone. The retired fact came back on the very next search, and kept
coming back until a consolidation cycle happened to prune it. Consolidation
only fires past `ConsolidateThreshold` facts, so below the threshold it came
back forever.

The tools reported "Fact suppressed for agent". The next search returned it.

## Decision

Add a tombstone to `Fact`:

```go
SupersededBy string `json:"superseded_by,omitempty"`
```

Non-empty means retired. `Recall` drops those facts **before scoring**, so a
tombstoned fact cannot even displace a live one from the top-k. The value
carries the reason: the replacement fact's ID for `update`, or the
`SupersededByAgent` marker for `forget`, so a correction can be followed rather
than merely observed.

### Precedence, stated once

Three mechanisms can end a fact's life. Overlapping them is how contradictory
behaviour gets built, so each owns exactly one question:

| Mechanism | Question it answers | Effect |
|---|---|---|
| **Tombstone** | Is this still true? | Excluded from retrieval immediately, at any weight |
| **Decay** | Does anyone still use this? | Lowers weight over time; keeps running on tombstoned facts |
| **Pruning** | Is this worth storing? | Deletes below 0.01 weight — the only thing that ever deletes |

A tombstone beats any weight, which is what makes a correction take effect at
once instead of waiting for a cycle. Decay does not care about tombstones.
Pruning is still the only deletion path, so a retired fact leaves the store the
same way an unused one does.

### Why a tombstone and not a delete

The README describes storage as append-only, and that is not a technicality —
it is what makes the store auditable. A contradiction is *information*: that a
fact was believed, then corrected, and when. Deleting on contradiction throws
away the history and makes the audit trail describe writes that no longer have
subjects.

So `List`, `export` and the TUI still show retired facts. Only retrieval skips
them.

### Explicit only, for now

Tombstones are written by explicit agent action through `memory_reflect`.
Nothing detects contradictions automatically, and specifically **nothing does
so in `Put`**. Put is the hot path — every `Remember` goes through it — and it
is deliberately cheap and deterministic. Adding contradiction detection there
would mean an embedding comparison, or an LLM call, on every write, and would
break the property the README states outright: *consolidation is the only
"smart" step; everything else is deterministic.*

## Consequences

- `update` and `forget` do what they have always claimed to do. This is a bug
  fix wearing a feature's clothes.
- The field is additive and `omitempty`, so stores written by earlier versions
  load unchanged, with no migration. Asserted against a literal v0.9.0 JSON
  fact, because "it should be fine" is not a test.
- A tombstoned fact still occupies disk and still costs a decay update per
  cycle until pruning collects it. At the default half-life that is months.
  Acceptable: the alternative is deleting, which is the thing being avoided.
- `update` writes the replacement *before* retiring the original. The previous
  order zeroed the weight first, so a failing `Remember` left an agent with a
  retired fact and nothing in its place.
- The tombstone is enforced in `Recall` only. Anything reading facts through
  `List` sees retired ones and must filter for itself — correct for export and
  the TUI, a trap for any future caller that wants "current" facts.

## Reversal condition

Reconsider if tombstoned facts exceed **20%** of a typical store, which would
mean corrections are common enough that carrying the history costs more than
it is worth, and that pruning should collect them on a shorter horizon than
live facts.

Automatic contradiction detection unlocks under both of:

1. It lives inside `Consolidate` — the one step already permitted to be smart
   and to require an LLM — and never in `Put`.
2. It is gated on `ConsolidateLLM` being configured, so a store running
   keyword-only stays fully deterministic.

A false positive here silently deletes a true fact from an agent's working
memory, which is worse than the staleness being fixed. Precision on a
hand-labelled sample must exceed **0.95** before it is enabled by default.

## Alternatives rejected

- **Delete the old fact.** Simplest, and it breaks the append-only property
  and the audit trail with it.
- **Make `Recall` respect `Weight`.** Would have fixed the reported bug with a
  one-line change, and it conflates two questions: a fact can be unused
  (low weight) without being untrue, and untrue without being unused. The
  Lemon Squeezy fact was recent, frequently accessed, and wrong.
- **A separate `tombstones` bucket.** Keeps `Fact` clean at the cost of a
  second lookup on every recall and a consistency problem between two buckets.
- **Automatic detection now.** Latency on the hot path, non-determinism in the
  core, and a failure mode that quietly deletes true facts.
