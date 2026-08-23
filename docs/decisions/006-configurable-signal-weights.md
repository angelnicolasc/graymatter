# 006 — Retrieval signal weights are configurable

**Status:** Accepted · **Date:** 2026-08-22

## Context

`Recall` fuses three signals through Reciprocal Rank Fusion: vector
similarity, keyword relevance, and recency. Until v0.10.0 their weights were
compile-time constants — 1.0, 1.0 and 0.5 — with no way to reach any other
ranking.

That blocked a claim worth being able to make honestly.

The natural comparison for this kind of system is a **sliding window**: keep
the last K observations, drop the rest. It is what production systems actually
do, it costs nothing to implement, and it is the baseline a sceptical reader
has in mind. The appealing framing is that a sliding window is a *special case*
of this ranking — the one where all the weight sits on recency.

With hardcoded weights that framing was false. Not an overstatement: false.
There was no configuration of GrayMatter that ranked by recency alone, so the
claim described a system that did not exist. Stating it would have been exactly
the kind of unfalsifiable assertion this project has spent a release removing.

## Decision

Make the weights configuration, so the claim becomes a testable property
instead of a description.

```go
type SignalWeights struct {
    Vector  float64
    Keyword float64
    Recency float64
}

func DefaultSignalWeights() SignalWeights {
    return SignalWeights{Vector: 1.0, Keyword: 1.0, Recency: 0.5}
}
```

`StoreConfig.SignalWeights` is a **pointer**. `nil` means defaults; a
non-nil value is used exactly as given. The indirection is load-bearing: with
a plain struct, the zero value — what every existing caller passes — would
mean "all three signals off", turning a compatible addition into a silent
catastrophe. A pointer distinguishes *unset* from *deliberately zero*, which
is the whole reason a caller would want `{0, 0, 1}` in the first place.

`k = 60` stays fixed. It is the constant from Cormack, Clarke & Buettcher
(SIGIR 2009) and it damps the gap between adjacent ranks; with a single signal
enabled it cannot change the ordering at all, so exposing it would add a knob
with no reachable effect.

### The claim, and its test

`TestSignalWeights_RecencyOnlyEmulatesSlidingWindow` runs recall with
`{Vector: 0, Keyword: 0, Recency: 1}` over a corpus built so that the facts
answering the query are the *oldest* ones. It asserts two things: the result is
exactly the K most recent facts, and none of them answers the query.

That is a sliding window, produced by configuring this ranking rather than by
implementing a second retrieval path. The claim is now demonstrable by
ablation inside the model.

Recency defaults to 0.5 rather than parity for a reason worth recording: it is
a tie-breaker, meant to put fresh context ahead of equally-relevant stale
context. Weighted at parity on a store with a steady write rate, it drowns
relevance out — which is precisely what the sliding-window test demonstrates
when it is turned up to 1.0 alone.

## Consequences

- A window baseline can be measured without writing one. Any comparison
  against truncation reconfigures the same code path, so there is no second
  implementation whose fairness a reader has to take on trust.
- Weights are a supported surface now, so a bad combination is a supported way
  to get bad results. `{0, 0, 0}` scores everything zero and returns an
  arbitrary K. Nothing validates this, and nothing should — a caller asking
  for no signals is asking for exactly that.
- Anything published about how GrayMatter ranks is now checkable against a
  configuration, which is the point.
- One more thing to keep compatible. `TestRankingDefaults_MatchV09Behaviour`
  is the gate: with the field unset, results must match the hardcoded ranking.

## Reversal condition

Remove the knob if, over a release cycle, **no** measurement or issue uses a
non-default value — that would mark it as speculative generality, and it would
go the way any unused abstraction should.

Change the defaults only against a retrieval-quality benchmark, never against
intuition. Specifically: a different weighting must beat `(1.0, 1.0, 0.5)` on
HitRate at equal budget by more than **5%** across at least three query
domains. Nothing in the tree measures that yet, so the current defaults stand
on the fact that they are what shipped and what everything was tuned against —
which is a weaker justification than it sounds, and is why it is written down
rather than implied.

## Alternatives rejected

- **Keep them hardcoded and make the claim anyway.** Marketing. The exact
  failure this release exists to remove.
- **Keep them hardcoded and write a separate window baseline.** Honest and
  worse: two retrieval paths to keep in step, and a reader still has to trust
  that the baseline was implemented fairly.
- **A plain struct instead of a pointer.** Ambiguous zero value; see above.
- **Expose k too.** A knob that cannot change a single-signal ordering.
- **Presets (`ModeRecent`, `ModeRelevant`) instead of numbers.** Friendlier,
  and it hides the ablation that makes the sliding-window equivalence
  demonstrable.
