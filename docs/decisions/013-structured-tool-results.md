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

Every tool declares an `outputSchema` generated from a Go type in
`internal/mcp/types.go`, and every success handler returns the payload as
`structuredContent` with its **byte-identical pre-existing prose** as text
content:

| tool | structured payload |
|------|--------------------|
| `memory_search` | `{agent_id, query, count, facts[]}` — `count: 0, facts: []` on the empty path |
| `memory_add` | `{agent_id, stored: true}` |
| `checkpoint_save` | `{agent_id, checkpoint_id, created_at}` |
| `checkpoint_resume` | `{id, created_at, state?, message_count?}` |
| `memory_reflect` | `{action, agent, ok: true}` |

The one error type worth a contract gets a typed payload: `checkpoint_resume`
with no checkpoint returns `isError=true` with
`{error: "not_found", agent_id}` as structured content and the historical
prose as text. Validation errors stay prose-only: they are misuse messages,
not part of the result contract, and typing every misuse path would freeze
error strings into a public schema for no consumer benefit.

## Consequences

- Text-parsing clients are unaffected by construction: the text content is
  unchanged, and the contract tests pin it (`handlers_test.go` asserts the
  same strings as before the migration).
- `structured_contract_test.go` validates every success payload against its
  declared schema (key subset, required presence, primitive/union type match)
  and the typed not-found payload — schema drift or payload drift is a CI
  failure.
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
- **A schema-validator dependency for the tests**: the deterministic
  key-set/type checks in `structured_contract_test.go` cover the contract
  surface without adding a direct dependency on a validator library.

## Reversal condition

If an MCP client release makes structuredContent mandatory and text content
deprecated for tools with output schemas, drop the prose fallback and collapse
`toolStructured` into a structured-only helper, updating the pinned text
assertions in the same commit. If `memory_reflect` grows per-action result
details worth exposing (e.g. the superseded fact's ID on update), split the
single `reflectResult` into per-action payload types rather than widening one
struct with unused fields.
