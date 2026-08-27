# Architecture Decision Records

Each record covers one decision that is not obvious from reading the code, and
states the condition under which it should be reversed. The reversal condition
is the part that matters: a decision without one is a preference, and a
preference nobody can argue with is how a codebase ossifies.

Format is [MADR](https://adr.github.io/madr/)-shaped: context, decision,
consequences, reversal condition.

| # | Decision | Status |
|---|---|---|
| [001](001-decay-half-life.md) | Memory decays on a 30-day half-life | Accepted |
| [002](002-bbolt-single-writer.md) | bbolt with a single writer, and a daemon to share it | Accepted |
| [003](003-knowledge-graph-autopopulation.md) | The knowledge graph has a write path but no automatic population | Accepted, partial |
| [004](004-local-first-single-node.md) | Local-first and single-node, deliberately not multi-tenant | Accepted |
| [005](005-embedding-degradation-chain.md) | Embeddings degrade Ollama → OpenAI → Anthropic → keyword | Accepted |
| [006](006-configurable-signal-weights.md) | Retrieval signal weights are configurable | Accepted |
| [007](007-supersede-tombstones.md) | Contradictions are resolved by tombstone, never by delete | Accepted |
| [008](008-knowledge-graph-wiring.md) | Knowledge-graph auto-population ships gated (`--kg` / env) | Accepted |
| [009](009-kg-sentinel-activation.md) | KG activation persists as data-dir state via `init --kg` | Accepted |
| [010](010-pinned-facts.md) | Pinned facts are exempt from decay, pruning and summarisation | Accepted |
| [011](011-consolidation-propose-apply.md) | Consolidation is propose/apply with tombstone receipts; Ollama summarises locally | Accepted |
| [012](012-tool-definition-quality.md) | Tool definitions are engineered against the TDQS rubric and pinned by contract tests | Accepted |
| [013](013-structured-tool-results.md) | Tool results carry structuredContent twins with declared output schemas | Accepted |

## Writing a new one

Copy the shape of an existing record. Two rules:

1. **The reversal condition has to be checkable.** "If it becomes a problem"
   is not a condition. "If p95 recall latency exceeds 50 ms on a 100k-fact
   store" is.
2. **Record what was rejected and why.** The alternatives are most of the
   value; a decision with no alternatives listed reads as though there were
   none.
