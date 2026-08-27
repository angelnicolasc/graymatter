# API Stability

## Compatibility promise (v0.x series)

Starting with **v0.1.0**, GrayMatter follows a best-effort compatibility policy for the identifiers listed below:

- **No removals or signature changes** within the v0.x series without a deprecation notice in the prior minor release.
- **Struct fields** listed as stable will not be removed; new fields may be added.
- **Store internals** (unexported fields, internal packages) are not covered — do not embed `Store` or depend on unexported symbols.
- When v1.0.0 is released, full semver guarantees apply.

---

## Stable identifiers

### `github.com/angelnicolasc/graymatter` (root package)

| Identifier | Notes |
|---|---|
| `New(dataDir string) *Memory` | |
| `NewWithConfig(cfg Config) (*Memory, error)` | |
| `(*Memory).Remember(agentID, text string) error` | |
| `(*Memory).Recall(agentID, query string) ([]string, error)` | |
| `(*Memory).Consolidate(ctx context.Context, agentID string) error` | |
| `(*Memory).RememberShared(text string) error` | |
| `(*Memory).RecallShared(query string) ([]string, error)` | |
| `(*Memory).RecallAll(agentID, query string) ([]string, error)` | |
| `(*Memory).Close() error` | |
| `(*Memory).Advanced() AdvancedStore` | Narrow handle for CRUD, listing, raw bbolt access |
| `(*Memory).Config() Config` | |
| `Config` struct — all fields present in v0.1.0 | New fields may be added |
| `DefaultConfig() Config` | |
| `EmbeddingMode` type and constants | |

### `github.com/angelnicolasc/graymatter/pkg/memory`

| Identifier | Notes |
|---|---|
| `Open(cfg StoreConfig) (*Store, error)` | |
| `(*Store).Put(ctx, agentID, text string) error` | |
| `(*Store).Delete(agentID, factID string) error` | |
| `(*Store).List(agentID string) ([]Fact, error)` | |
| `(*Store).ListAgents() ([]string, error)` | |
| `(*Store).Stats(agentID string) (MemoryStats, error)` | |
| `(*Store).UpdateFact(agentID string, f Fact) error` | |
| `(*Store).Recall(ctx, agentID, query string, topK int) ([]string, error)` | Result ordering is deterministic — see below |
| `(*Store).RecallShared(ctx, query string, topK int) ([]string, error)` | |
| `(*Store).RecallAll(ctx, agentID, query string, topK int) ([]string, error)` | |
| `(*Store).PutShared(ctx, text string) error` | |
| `(*Store).MaybeConsolidate(ctx, agentID string, cfg ConsolidateConfig) error` | |
| `(*Store).Consolidate(ctx, agentID string, cfg ConsolidateConfig) error` | |
| `(*Store).Close() error` | |
| `(*Store).SetKG(graph GraphAccessor, extractor EntityExtractorAccessor)` | |
| `(*Store).DB() *bolt.DB` | |
| `Fact` struct — all fields present in v0.1.0 | New fields may be added; `SupersededBy` added in v0.10.0; `Confidence` added in v0.12.0; `Pinned`/`PinnedAt` added in v0.14.0 (zero values reproduce pre-v0.14 behaviour; older stores load as unpinned) |
| `(Fact).IsSuperseded() bool` | Added in v0.10.0 |
| `SupersededByAgent` constant | Added in v0.10.0 |
| `MemoryStats` struct | |
| `StoreConfig` struct — all fields present in v0.1.0 | New fields may be added; `SignalWeights` and `MinRelevance` added in v0.10.0 |
| `SignalWeights` struct | Added in v0.10.0 |
| `DefaultSignalWeights() SignalWeights` | Added in v0.10.0 |
| `ErrConsolidateLLMUnsupported` | Added in v0.10.0. Deprecated in v0.14.0: never returned anymore (Ollama consolidation is implemented); kept so `errors.Is` callers keep compiling |
| `SharedAgentID` constant | |
| `ConsolidateConfig` interface | **v0.14.0:** gained `GetOllamaURL()` and `GetOllamaConsolidateModel()`. This is a signature-level change to a stable identifier, taken with the deviation documented in the 0.14.0 changelog compatibility notes instead of a prior-minor deprecation cycle. Callers using `graymatter.Config` are unaffected; hand-rolled implementers add two getters |
| `GraphAccessor` interface | |
| `EntityExtractorAccessor` interface | |
| `TypedEntityExtractor` interface, `EntityRef`, `EntityLink` | Added in v0.12.0 — optional extractor capability preserving label + type and producing co-mention links; consolidation uses it when implemented, legacy ID-only path otherwise |
| `EdgeWriter` interface | Added in v0.12.0 — optional graph capability used by consolidation to persist co-mention edges |
| `AdvancedStore.SetKG(...)` | Exposed in v0.12.0 (mirrors `(*Store).SetKG`) |
| `(*Store).ConsolidationCounters()`, `ReadConsolidationCounters(db)` | Added in v0.14.0 — lifetime consolidation totals from the meta bucket, surfaced by `status` and `doctor` |

### Recall result ordering

**Recall result ordering is deterministic: descending fused score, oldest
first, then ID.**

The same query against the same store returns the same facts in the same order,
on every call, on every platform. Facts that score equally are ordered by
`CreatedAt` ascending, and facts created in the same instant by fact ID
ascending, which makes the order total.

This is a guarantee callers may rely on. It applies to `Recall`, `RecallShared`
and `RecallAll`, and to every configuration of `SignalWeights` and
`MinRelevance`.

**Exception, v0.12.0:** when a knowledge graph is wired via `SetKG` (directly,
via `AdvancedStore.SetKG`, or by enabling the daemon's `--kg` /
`GRAYMATTER_KG=1`), `Recall` may append **at most three** neighbour labels
after the ranked facts. The first `topK` entries keep the deterministic order
above; appended entries are enrichment hints, capped and deduplicated, and
never displace a ranked fact. Without a wired graph the exception does not
exist and `Recall` returns exactly `topK`.

Before v0.11.0 the ordering of equal-scoring facts was unspecified in practice:
the three signal rankings were sorted with a comparator that read only the
score, and `sort.Slice` is not stable, so tied facts received arbitrary ranks
which the fusion then read. Nothing about the scores has changed — only the
resolution of ties.

### Additions in v0.10.0

Three additions, all fields or new identifiers, no signature changes — so the
compatibility promise above holds and no caller needs to do anything.

Each new field's zero value reproduces the previous behaviour exactly, which is
enforced rather than asserted:

| Field | Zero value | Behaviour at zero |
|---|---|---|
| `Fact.SupersededBy` | `""` | Fact is live. Stores written before v0.10.0 have no `superseded_by` key and load as live — checked against a literal v0.9.0 JSON fact |
| `StoreConfig.SignalWeights` | `nil` | `DefaultSignalWeights()` — vector 1.0, keyword 1.0, recency 0.5, the values that were hardcoded before v0.10.0. It is a pointer precisely so the zero value cannot be confused with "all signals off" |
| `StoreConfig.MinRelevance` | `0` | No relevance floor; `Recall` returns exactly `topK`, the pre-v0.10.0 contract |

`TestRankingDefaults_MatchV09Behaviour` is the gate: with the ranking fields
unset, results must be identical to the v0.9.0 ranking.

One default did change, and it is a behaviour change rather than an addition:
**the REST server's default `k` moved from 5 to 8**, matching
`DefaultConfig().TopK` and every other entry point. `GET /recall` with no `k`
now returns 8 facts. Pass `?k=5` for the old count.

---

## MCP wire contract (stable within the v0.x series)

The Go package that implements the MCP server, `cmd/graymatter/internal/mcp`,
is internal and carries no Go-level guarantee (see [Internal / unstable
packages](#internal--unstable-packages)). The **wire contract it serves** is
different: any MCP client — in any language, often with no Go dependency at
all — compiles against the payloads below. Within the v0.x series:

- **Tool names, parameter names, and required parameters** will not be removed or renamed.
- **`outputSchema` objects are authoritative**: a success result's
  `structuredContent` conforms to the tool's declared schema, which sets
  `additionalProperties: false`. New keys arrive only through a schema
  revision, never silently.
- **Text content** remains functionally equivalent to `structuredContent` per
  the MCP compatibility guidance. Exact prose wording is best-effort and may
  be reworded; clients should read the structured payload.
- Changes that would break a conforming client follow the same deprecation
  rule as the Go identifiers above: notice in the prior minor release.
- Tool calls on one connection **may execute concurrently** (the stdio server
  runs a worker pool): a response guarantees only its own effects, and
  clients must not rely on cross-request ordering within a stream. A client
  that needs write-then-read visibility must wait for the write's response
  first.
- Enforcement is mechanical, not aspirational: `tdqs_contract_test.go` (tool
  set, titles, descriptions, input schemas), `structured_contract_test.go`
  (payload ↔ `outputSchema` agreement), and `annotations_test.go` (safety
  hints) fail CI on drift.

The tables below reflect the definitions served by `graymatter mcp serve`,
verified against a live `tools/list` exchange at the time of writing.

### Tools

| Tool | Required parameters | Optional parameters |
|---|---|---|
| `memory_search` | `agent_id`, `query` | `top_k` (default `8`) |
| `memory_add` | `agent_id`, `text` | — |
| `checkpoint_save` | `agent_id` | `state` (string containing a JSON object) |
| `checkpoint_resume` | `agent_id` | — |
| `memory_reflect` | `action`, plus exactly one of `agent_id` (canonical) or `agent` (deprecated alias) | `text`, `target` |

`memory_reflect.action` is an enum: `add`, `update`, `forget`, `link`, `pin`,
`unpin`. The agent parameter on `memory_reflect` is expressed as a schema
`anyOf` (exactly one of the two spellings is required); `agent` is deprecated,
and when both spellings arrive `agent_id` wins.

### `structuredContent` payloads

| Tool | Success payload | Notes |
|---|---|---|
| `memory_search` | `{"agent_id", "query", "count", "facts"}` | `facts` is nullable in the schema; the empty result is `count: 0, facts: []` with a "No memories found" text notice |
| `memory_add` | `{"agent_id", "stored"}` | `stored` is `true` on success |
| `checkpoint_save` | `{"agent_id", "checkpoint_id", "created_at"}` | `created_at` is RFC3339 |
| `checkpoint_resume` | `{"id", "created_at", "state"?, "message_count"?}` | `state` is the persisted JSON object; keys marked `?` may be absent when empty |
| `memory_reflect` | `{"action", "agent", "ok"}` | `ok` is `true` on success |

One error carries a typed payload: `checkpoint_resume` with no checkpoint
returns `isError: true` with `{"error": "not_found", "agent_id"}`. All other
errors (validation, storage, unavailable features) are prose-only `isError`
results and carry no machine-readable contract — their wording may change.

---

## Provisional (may change before v0.2.0)

| Identifier | Reason |
|---|---|
| `memory.ExtractFacts` | New in v0.1.0; prompt and output format may be tuned |
| `memory.ExtractConfig` | Interface may gain methods |
| `(*Memory).Extract` | New in v0.1.0 |
| `(*Memory).RememberExtracted` | New in v0.1.0 |
| `(*Store).LaunchAsyncConsolidate` | Internal scheduling; may be unexported |

---

## Internal / unstable packages

The following packages are implementation details and provide no stability guarantee:

- `cmd/graymatter/internal/kg` — knowledge graph and entity extraction
- `cmd/graymatter/internal/session` — session checkpointing
- `cmd/graymatter/internal/harness` — agent runner
- `cmd/graymatter/internal/mcp` — MCP server handlers. The Go package is internal and may change freely; the **wire contract it serves** is stable — see [MCP wire contract](#mcp-wire-contract-stable-within-the-v0x-series)
- `cmd/graymatter/internal/server` — REST API server
- `cmd/graymatter/internal/plugin` — plugin protocol
- `cmd/graymatter/internal/export` — Obsidian / markdown export
- `pkg/embedding` — embedding backend adapters (public; see the stable table for its provider interface)
- `cmd/` — CLI command implementations
