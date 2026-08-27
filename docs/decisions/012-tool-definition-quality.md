# 012 — Tool definitions are engineered against the TDQS rubric and pinned by contract tests

**Status:** Accepted — **Date:** 2026-08-27

## Context

Glama's Tool Definition Quality Score (TDQS) is an open framework
([spec](https://github.com/glama-ai/tool-definition-quality-score)) that scores
what an MCP client sees from `tools/list` — the only surface an agent reads
when deciding which tool to call. Glama ranks tools by TDQS in its directory
search, and their published study (arXiv 2602.18914) measures ~260% higher
selection for well-described tools in competitive settings.

First scores for graymatter's five tools: **3.1/5.0 (tier B)**, with
Usage Guidelines at 2/5 and Behavioral Transparency at 1–2/5. The server-level
rollup (`0.6 × mean + 0.4 × min`) means the worst tool caps the whole server's
description-quality score.

An empirical pass over the handlers also surfaced **documentation drift**:
`docs/AGENTS.md` claimed return shapes that no handler produces
(`memory_search` as newline-separated text, `checkpoint_resume` returning an
empty string or a JSON object). Any description written from those docs would
have embedded false claims — the descriptions were written from the handlers,
and the docs were corrected in the same pass.

## Decision

Tool definitions are treated as a scored contract surface, engineered against
the TDQS rubric's 5-point anchors and pinned by tests:

1. **Purpose Clarity (25%)** — every description opens with one specific,
   per-tool verb (Search / Store / Persist / Read / Curate) and differentiates
   the tool from its siblings.
2. **Usage Guidelines (20%)** — every description carries an explicit
   when-to-use cue and names the sibling tool for the cases it excludes
   (`memory_add` ↔ `memory_reflect`, `checkpoint_save` ↔ `checkpoint_resume`).
3. **Behavioral Transparency (20%)** — annotations already carry the safety
   hints (ADR-tested in `annotations_test.go`); the description adds what
   annotations cannot express: return shape, empty/error path, and the
   tombstone/decay semantics of writes. Every claim is verified against the
   handler code, not against prose docs.
4. **Parameter Semantics (15%)** — schema description coverage stays at 100%,
   `top_k` declares `default: 8`, `action` keeps its enum. The description
   only adds cross-parameter semantics the schema cannot express per-property
   (`update` exact-match rule, `target`-wins precedence).
5. **Titles** — every tool declares a meaningful `title` (differs from the
   name, longer than the name), the TDQS `titleIsMeaningful` signal.
6. **Contract tests** — `tdqs_contract_test.go` pins the client-visible
   `tools/list` payload: name set, title rules, description shape (opening
   verb, ≤ 800 chars, sibling mention, `__shared__` mention, return/error
   disclosure, anti-tautology), exact parameter sets, required lists, enum
   values, and schema coverage. Content stays free to evolve; the shape may
   not drift.

## Alternatives rejected

- **`outputSchema` + `structuredContent`** — mcp-go v0.58 supports it, but the
  handlers return human-readable text via `NewToolResultText`. Declaring an
  output schema the results do not structurally satisfy would break
  validating clients and misdescribe the wire. That is a wire-format migration
  (tracked in the structured-results issue), not a metadata edit.
- **Renaming `memory_reflect`'s `agent` parameter to `agent_id`** — a breaking
  schema change for every existing caller. The alias already mitigates;
  tracked as its own issue with a deprecation path.
- **Writing descriptions from `docs/AGENTS.md`** — the docs were wrong about
  return shapes (now fixed). Handlers are the source of truth for behavior
  claims; docs are corrected to match, in the same commit family.

## Consequences

- Re-scoring happens automatically: any description edit changes the tool's
  `inputHash`, Glama's sweep re-scores the changed tools, and a server sync
  picks the new definitions up. No release is required for the *score*, only
  for the *binary* users run.
- The contract tests make TDQS regression a CI failure rather than a
  slow drift: editing a description without its opening verb, sibling
  reference, or return disclosure now breaks `go test`.
- Scores remain an LLM-judged rubric: the tests pin the structural anchors the
  rubric scores against, not the judge's output. Expected outcome from the
  re-score is 4.5–5.0 per dimension on the six-dimension rubric (tier A),
  with residual variance belonging to the judge, not the definitions.

## Reversal condition

If Glama retires TDQS, or materially reweights the rubric (e.g. penalising
five-sentence descriptions on two-parameter tools under a stricter Conciseness
anchor), re-run the lint assertions against the new anchors: the per-tool
verb map, the sibling map, and the 800-char budget are the three knobs to
rebalance. If graymatter migrates to `structuredContent` outputs, the
return-disclosure sentences should move into the output schema and the
description requirement is relaxed accordingly.
