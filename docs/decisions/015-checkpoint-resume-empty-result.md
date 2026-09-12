# 015 — checkpoint_resume returns a successful, machine-readable absence result

**Status:** Accepted — **Date:** 2026-09-12

## Context

Since #117, `checkpoint_resume` with no checkpoint returns a text-only
`isError: true` result. That fixed a real defect — the older typed error
payload (`{"error": "not_found", "agent_id"}`) did not conform to the tool's
success-only `outputSchema`, and strict clients rejected the entire response —
but it left absence undetectable without parsing prose. An agent booting with
`checkpoint_resume` cannot programmatically distinguish "you have no saved
state" from "the store is broken", which is exactly the distinction a session
start needs.

The [ADR-013 amendment](013-structured-tool-results.md) deferred a
machine-readable absence result to a separate contract decision. This is that
decision.

**Client review evidence.** OpenCode 1.18.29's bundled MCP client uses ajv's
`compile` and validates `structuredContent` against the tool's declared
`outputSchema` whenever it is present, regardless of `isError`. It also
rejects a successful (non-error) result that omits `structuredContent` for a
tool that declares an `outputSchema` ("has an output schema but did not return
structured content"). Consequences:

- A text-only **success** result is not viable for any tool with an
  `outputSchema`; a machine-readable absence result must carry
  `structuredContent`.
- `_meta` is not a fallback: it is not the validated field, and clients that
  enforce the schema reject the result before any `_meta` is read.
- Emitting `structuredContent` on the **error** path (the #117 design) must
  satisfy the advertised schema — which is why absence could not simply stay
  an error with a typed payload.

## Decision

Add an optional `on_missing` parameter to `checkpoint_resume`, enum
`"error" | "empty"`, default `"error"`:

- `default` / `"error"`: unchanged from v0.19.1 — text-only `isError: true`,
  historical prose, no `structuredContent`.
- `"empty"`: success (`isError` absent/false) with `structuredContent`
  `{"found": false, "agent_id": "<id>"}` (wire type `checkpointResumeEmpty`,
  `found` without `omitempty` so `false` cannot vanish) and text
  `No checkpoint saved for agent "<id>" yet.`
- Real failures (storage, daemon, corrupt records, uninitialized store) stay
  prose-only `isError: true` in both modes; only the `ErrNoCheckpoint` sentinel
  selects the absence result.

The output schema becomes a root-object `oneOf` union: the generated
`checkpointResumeResult` branch (built mechanically, never hand-written) plus
an absence branch `{"type":"object","additionalProperties":false,"required":
["found","agent_id"],"properties":{"found":{"type":"boolean","enum":[false]},
"agent_id":{"type":"string"}}}`. The union root declares no
`properties`/`required`/`additionalProperties` of its own — a root-level
`additionalProperties: false` applies to every branch and rejects both
payloads.

**Staged migration.** `"empty"` is opt-in in v0.20.0. The default flips to
`"empty"` in v0.21.0, announced in the v0.20.0 changelog and in
`docs/api-stability.md` (the prior-minor notice the deprecation rule
requires). `"error"` remains accepted as the legacy behaviour throughout
v0.x, so no caller breaks at either step.

**Fallback if a real client rejects `oneOf`.** Collapse the output schema to a
single permissive object declaring all six keys (`id`, `created_at`, `state`,
`message_count`, `found`, `agent_id`), none required,
`additionalProperties: false`, while keeping `structuredContent` on both
paths. The wire payloads do not change; only the schema's validation strength
does. The release gate is a real OpenChamber/OpenCode smoke run against this
schema — not just the contract tests — and it must pass before v0.20.0 ships,
because the union is validated on every structured result from that release.

## Consequences

- A session-start caller can opt into `{"found": false}` and stop treating
  "no checkpoint yet" as a failure, without a prose parser.
- Existing callers are unaffected by construction: the default path is
  byte-identical to v0.19.1, and the union schema accepts the success payload
  unchanged.
- The union root means `structured_contract_test.go`'s hand-rolled key/type
  checks cannot validate this tool; checkpoint resume payloads are validated
  with `github.com/santhosh-tekuri/jsonschema/v6` (already in the module graph
  via mcp-go, promoted to a direct test dependency).
- The zero-value trap is pinned twice: `checkpointResumeEmpty.Found` must not
  carry `omitempty`, and a wire test asserts `"found": false` is present in
  the serialized response.

## Alternatives rejected

- **Typed error `structuredContent`** — the #117 payload
  (`{"error": "not_found", "agent_id"}` with `isError: true`). Rejected: it
  violates the success-only output schema, which is the defect #117 fixed.
- **Text-only success** — rejected: a schema-bearing tool's successful result
  that omits `structuredContent` is rejected outright by the reviewed client.
- **`_meta` marker** — rejected: not schema-validated, not surfaced to
  callers, and rejected before it can be read when `structuredContent` is
  missing.
- **A companion `checkpoint_status` tool** — rejected: adds an eighth tool
  and a second round trip to answer a question the resume call already has,
  and clients select tools by description, so "call this first" is a request
  the protocol cannot enforce.
- **Direct flip to `empty` without notice** — rejected: turning absence from
  an error into a success is observable behaviour; the v0.x stability promise
  requires a prior-minor notice and the staged path gives real deployments a
  release to opt in first.

## Reversal condition

The real-client gate must pass before v0.20.0 ships: the union schema ships in
that release and strict clients validate it on every structured result. If the
OpenChamber/OpenCode smoke run (or field reports from other strict clients)
shows `oneOf` output schemas being rejected or mis-handled, apply the
permissive single-object fallback above in the next release after detection
(at latest with the v0.21.0 default flip), and record the client/version
evidence in this ADR. Keep `"error"` as the default until a smoke run re-proves
the schema on a real client. If no such report appears, flip the default in
v0.21.0 as announced. If the flip then produces reports of behavioural
breakage that opt-in adoption did not reveal, keep `"error"` as the default
for the remainder of v0.x and reopen the contract question with the breakage
evidence rather than extending the flip.
