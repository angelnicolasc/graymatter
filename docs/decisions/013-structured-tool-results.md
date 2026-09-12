# 013 — Tool results carry structuredContent twins with declared output schemas

**Status:** Accepted — **Date:** 2026-08-27

## Context

All five MCP tools returned prose only (`NewToolResultText`). Two costs:

- **No output schema.** MCP clients and registry scorers read `outputSchema`
  from `tools/list`; without it, agents must trust the description's prose for
  what comes back, and tool-selection frameworks discount the tool (TDQS
  relieves a description from documenting returns only when a schema declares
  them).
- **Prose is a parsing tax.** `memory_search` returned a numbered list;
  `checkpoint_resume` a formatted block. Machine consumers (scripts, harnesses,
  other agents) had to parse human formatting, and the not-found case was
  indistinguishable from any other error text.

mcp-go v0.58 provides `NewToolResultStructured(payload, fallbackText)` and
`WithOutputSchema[T]()` (reflection over JSON tags) — the migration surface.

## Decision

Every tool declares an `outputSchema`. Normal single-shape schemas are
generated mechanically from Go types in `internal/mcp/types.go`; [ADR-015](015-checkpoint-resume-empty-result.md)
later documents the explicit `checkpoint_resume` union exception, whose normal
checkpoint branch remains generated while `server.go` composes its absence
branch. Every success handler returns the payload as `structuredContent` with
its **byte-identical pre-existing prose** as text content:

| tool | structured payload |
|------|--------------------|
| `memory_search` | `{agent_id, query, count, facts[]}` — `count: 0, facts: []` on the empty path |
| `memory_add` | `{agent_id, stored: true}` |
| `checkpoint_save` | `{agent_id, checkpoint_id, created_at}` |
| `checkpoint_resume` | `{id, created_at, state?, message_count?}` |
| `memory_reflect` | `{action, agent, ok: true}` |

Errors return `isError=true` with text content only; `structuredContent` is
reserved for successful results matching the declared output schema.

### Amendment — 2026-09-07 (#117)

The original typed `checkpoint_resume` error (`{error: "not_found", agent_id}`)
did not conform to its success-only output schema. Strict clients rejected
the entire response, hiding both the error code and the explanation. The
original test checked that payload against a separate Go type instead of the
schema advertised in `tools/list`, so it protected the defect.

Resume errors now use the same text-only error helper as other tools. This
removes the invalid structured error payload while retaining `isError`, the
historical not-found prose, and the success schema and prose unchanged. It is
an intentional compatibility correction: consumers of the old error object
must use the tool-error result instead. Widening the success schema or turning
absence into success would change a larger, working contract. A future
machine-readable absence result requires a separate contract decision. That
decision is [ADR-015](015-checkpoint-resume-empty-result.md): it takes the
deferred separate decision on an opt-in, staged path — an `on_missing: "empty"`
result with a staged default flip — rather than folding the schema widening
into the #117 correction.

This follows the [MCP tool error and output-schema contract](https://modelcontextprotocol.io/specification/2025-06-18/server/tools):
structured results must match the advertised schema; execution errors can
carry text with `isError=true`.

### Error classification (#118)

Only `errors.Is(err, session.ErrNoCheckpoint)` selects the historical absence
notice. Other failures retain their cause under `checkpoint resume error`.
Checkpoint reads propagate decoding errors instead of silently discarding
records and presenting incomplete history as a successful or empty resume.

The daemon transports absence in an opt-in `NotFound` response field, because
`net/rpc` serializes returned errors as strings and loses sentinel identity.
`ReportNotFound` in the request protects older clients: without the opt-in,
absence still returns an RPC error. A new client talking to an older daemon
preserves its untyped error as an operational failure; restarting that daemon
with the updated binary enables typed absence. No error-text matching or
general-purpose error protocol is introduced.

## Consequences

- Success text-parsing clients are unaffected by construction: the text content is
  unchanged, and the contract tests pin it (`handlers_test.go` asserts the
  same strings as before the migration).
- `structured_contract_test.go` validates every success payload against its
  declared schema (key subset, required presence, primitive/union type match)
  and asserts that resume errors omit structured content on the wire.
- The wire contract this decision introduces — tool names, parameter names,
  schemas, and `structuredContent` keys — is covered by the compatibility
  promise in [api-stability.md](../api-stability.md#mcp-wire-contract-stable-within-the-v0x-series).
  The JSON tags in `types.go` are the wire contract: renaming one is
  major-version territory.
- `checkpoint_resume`'s state maps through as a JSON object; `omitempty`
  fields may be absent, and the schema declares them non-required — clients
  must tolerate absent keys (standard JSON Schema semantics).

## Alternatives rejected

- **Structured-only responses** (`NewToolResultStructuredOnly`): breaks the
  text contract the MCP spec explicitly asks tools to keep
  ("SHOULD also return functionally equivalent unstructured content").
- **A schema-validator dependency for the tests**: at the time of this
  decision, the deterministic key-set/type checks in
  `structured_contract_test.go` covered the contract surface without adding a
  direct dependency on a validator library. [ADR-015](015-checkpoint-resume-empty-result.md)
  later revisited that choice for the explicitly composed `checkpoint_resume`
  union and validates it with `github.com/santhosh-tekuri/jsonschema/v6`.

## Reversal condition

If an MCP client release makes structuredContent mandatory and text content
deprecated for tools with output schemas, drop the prose fallback and collapse
`toolStructured` into a structured-only helper, updating the pinned text
assertions in the same commit. If `memory_reflect` grows per-action result
details worth exposing (e.g. the superseded fact's ID on update), split the
single `reflectResult` into per-action payload types rather than widening one
struct with unused fields.
