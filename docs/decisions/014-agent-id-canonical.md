# 014 — agent_id is the canonical agent parameter on memory_reflect

**Status:** Accepted — **Date:** 2026-08-27

## Context

Issue #77 recorded a naming asymmetry: `memory_reflect` spelled its agent
parameter `agent` while the other four tools use `agent_id`. Models
generalize parameter names across a toolset, so the asymmetry was a known
silent-failure class; the runtime alias added in v0.15.0 absorbed the calls
but was invisible to clients reading `tools/list`. Step 3 of the issue's plan
— flipping the canonical spelling — was deferred pending a release cycle of
real-world signal; that cycle (v0.15.0 → this change) completed with no
report of the flip causing friction, and every host in practice relays
whatever parameter names the schema advertises.

## Decision

`agent_id` is canonical; `agent` is a deprecated alias:

- The input schema expresses the true contract as an `anyOf` over two
  required-lists — at least one of the spellings must be present (both at once
  are allowed; `agent_id` wins when both arrive), and neither belongs in a
  flat `required` (requiring either one would schema-invalid the caller class
  the other spelling serves). mcp-go's typed builders cannot
  express `anyOf`, so `memory_reflect` carries a hand-authored raw input
  schema; every property, the enum, and `additionalProperties: false` are
  preserved in it.
- Runtime precedence follows the canonical: `agent_id` wins when both
  spellings arrive with different values. Only a caller sending *both* with
  *different* agents observes the change — a pathological input, and the new
  behaviour is the documented one.
- The `agent` description carries the deprecation and the precedence rule in
  the schema itself, so a client never has to consult external docs.

## Consequences

- Callers sending one spelling — either one — are schema-valid and
  behaviourally identical to before. No caller breaks at this step; the
  class that could observe a difference (both spellings, different values)
  is not known to exist.
- The at-least-one rule moves from prose into machine-checkable schema, and
  `reflect_schema_test.go` pins the anyOf shape, the deprecation marker, and
  both precedence directions on the real `tools/list` payload.
- The wire contract tables in `docs/api-stability.md` and the parameter
  guide in `docs/AGENTS.md` teach `agent_id` from now on; `agent` examples
  remain valid code but are no longer the taught form.

## Reversal condition

If field reports show hosts whose strict schema validators reject `anyOf`
over required-lists (a subset of JSON Schema some tooling skips), flatten
back to `required: ["action", "agent_id"]` with `agent` as a
runtime-accepted alias and note the relaxation in the changelog — the
handler accepts both spellings either way, so the reversal is a schema-only
commit. If models overwhelmingly send `agent` despite the deprecation
marker, keep the alias indefinitely; deprecation without a removal date is
documentation, not a deadline.
