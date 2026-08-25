# 010 — Pinned facts are exempt from decay, pruning and summarisation

**Status:** Accepted — **Date:** 2026-08-25

## Context

ADR-001's 30-day half-life is the honesty mechanism of the memory: a stale
fact means something changed. But it fails one class of content outright —
standing obligations and architecture decisions that are never touched again.
Decay recomputes from last access, so a fact the agent recalls every session
survives implicitly; a fact in a project that goes dormant for six months is
pruned (~199 days at the default half-life) no matter how important the user
declared it.

`docs/AGENTS.md` used to recommend "pinning" important rules to the
`__shared__` namespace. Nothing pinned: `__shared__` is a visibility scope,
not a retention guarantee, and the advice was the only part of the memory
model that was implicit rather than enforced.

## Decision

A fact can be pinned (`graymatter pin`, `memory_reflect action=pin`) and
unpinned. The exemption is total and covers all three consolidation steps
(invariant I-1):

1. **Decay** — the weight of a pinned fact is never rewritten.
2. **Summarisation** — a pinned fact is never in the batch the LLM consumes
   (`summarisationBatch` filters before the weight sort; pinned facts are
   precisely the rarely-accessed ones that sort surfaces first).
3. **Pruning** — the only code path that ever removes a fact skips pins.

The exemption is **visible**: pinned facts carry a star in the TUI, are
counted by `status` (human line and `pinned` in JSON), and carry
`pinned: true` in exports. Pinning a superseded fact is rejected; unpinning
restores normal decay from the fact's current weight, inheriting the
staleness accumulated while pinned (decay recomputes from last access — a
fact pinned for a year then unpinned is a year stale, which is true and
auditable rather than silently reset).

## Consequences

- A dormant project can no longer silently collect standing obligations or
  architecture decisions. What the user declared permanent stays.
- The escape hatch closes: rules no longer need to migrate to
  CLAUDE.md/AGENTS.md to survive, which is where memory value goes to dilute.
- The stale-fact risk ADR-001 guards against now concentrates exactly where
  the user accepted it: a pinned fact that becomes wrong stays wrong until
  unpinned or superseded. That is why the exemption is loud — surfaces show
  the pin so contradictions are visible, and `memory_reflect update` on a
  pinned fact produces a fresh (unpinned) replacement the user can re-pin.
- Consolidation batches shrink when pins are present; with everything pinned
  the summariser has nothing to consume, which is the configured intent.

## Reversal condition

If pinned facts in real stores are observed contradicting newer evidence
without the users noticing (the visibility surfaces failing at their job),
the design should move to slow decay (e.g. a 50× half-life) instead of total
exemption — retaining the reminder signal while keeping the guarantee in
practice. Measured via `doctor` health rules once W5 lands.
