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
| `(*Memory).Remember(ctx context.Context, agentID, text string) error` | |
| `(*Memory).Recall(ctx context.Context, agentID, query string) ([]string, error)` | |
| `(*Memory).Consolidate(ctx context.Context, agentID string) error` | Runs decay, pruning and configured graph extraction. Generative compaction is disabled. |
| `(*Memory).RememberShared(ctx context.Context, text string) error` | |
| `(*Memory).RecallShared(ctx context.Context, query string) ([]string, error)` | |
| `(*Memory).RecallAll(ctx context.Context, agentID, query string) ([]string, error)` | |
| `(*Memory).Close() error` | |
| `(*Memory).Advanced() AdvancedStore` | Narrow handle for CRUD, listing, raw bbolt access |
| `(*Memory).Config() Config` | |
| `(*Memory).RememberWithOptions`, `RememberSharedWithOptions` | Added in v0.20.0. Accept `context.Context` and `memory.WriteOptions`, return the exact committed `memory.Fact`; normal writes retain automatic consolidation policy |
| `(*Memory).RecallWithOptions`, `RecallExplainWithOptions`, `RecallSharedWithOptions`, `RecallAllWithOptions` | Added in v0.20.0. Accept `context.Context` and `memory.RecallOptions`; configured `TopK` applies |
| `Config` struct — all fields present in v0.1.0 | New fields may be added |
| `DefaultConfig() Config` | |
| `EmbeddingMode` type and constants | |

### `github.com/angelnicolasc/graymatter/pkg/memory`

| Identifier | Notes |
|---|---|
| `Open(cfg StoreConfig) (*Store, error)` | |
| `(*Store).Put(ctx, agentID, text string) error` | |
| `(*Store).PutReturningFact(ctx, agentID, text string) (Fact, error)` | Added after v0.18.0. Concrete `Store` capability, intentionally not part of `AdvancedStore`; returns the exact fact committed so callers can persist its identity without a post-write lookup |
| `(*Store).PutWithOptionsReturningFact(ctx, agentID, text, WriteOptions) (Fact, error)` | Added in v0.20.0. Text, confidence, canonical ID and index maintenance commit in the same fact transaction |
| `(*Store).ReviseWithOptions`, `ReviseFactsWithOptions` | Added in v0.20.0. Conservative omitted confidence and exact replacement identity. Replacement and selected source retirements commit atomically. |
| `(*Store).RecallWithOptions`, `RecallExplainWithOptions`, `RecallSharedWithOptions`, `RecallAllWithOptions`, `BatchRecallWithOptions` | Added in v0.20.0. Per-call options; nonpositive `topK` uses 8. Existing raw Store recall signatures keep their legacy zero-count behavior |
| `(*Store).DefaultConfidenceWeight() float64` | Optional adapter capability added in v0.20.0; advertises the configured default so request routing and negotiated RPC retain the same effective policy |
| `WriteOptions`, `RecallOptions`, `RecallResult`, `RecallExplainResult`, `RetrievalMetadata`, `ConfidenceRanking` | Added in v0.20.0. Optional pointer fields distinguish omission from explicit labels/zero; additive result metadata is omitted for legacy calls |
| `EligibleVectorStore`, `ExhaustiveVectorStore` | Optional capabilities added in v0.20.0; `VectorStore` and `AdvancedStore` require no new methods |
| `(*Store).Delete(agentID, factID string) error` | Explicitly removes the fact row by namespace and ID, including pinned or retired rows |
| `(*Store).List(agentID string) ([]Fact, error)` | |
| `(*Store).ListAgents() ([]string, error)` | |
| `(*Store).Stats(agentID string) (MemoryStats, error)` | |
| `(*Store).UpdateFact(agentID string, f Fact) error` | |
| `(*Store).Recall(ctx, agentID, query string, topK int) ([]string, error)` | Result ordering is deterministic — see below |
| `(*Store).RecallShared(ctx, query string, topK int) ([]string, error)` | |
| `(*Store).RecallAll(ctx, agentID, query string, topK int) ([]string, error)` | |
| `(*Store).RecallExplain(ctx, agentID, query string, topK int) ([]RecallReceipt, error)` | Added in v0.17.0. The same ranking as `Recall` — identical order, dedup and access-metadata side effects — with one receipt per returned fact instead of bare text. KG neighbour enrichment is text-only and has no receipts, so it is not part of the explain result |
| `RecallReceipt` struct (`text`, `weight`, `age_days`, `ranks`, `provenance`, `kg_links`) | Added in v0.17.0. JSON tags are the wire contract for `graymatter recall --explain --json` and the MCP `explained` payload |
| `RecallRanks` struct (`vector_rank`, `keyword_rank`, `recency_rank`, `fused_score`, `k`) | Added in v0.17.0. A rank of 0 means the signal did not rank the fact; the fused score reproduces from the ranks and `k` using the documented RRF arithmetic |
| `RecallProvenance` struct (`fact_id`, `written_at`, `superseded_by`, `confidence`, `pinned`) | Added in v0.17.0. Includes the optional lineage projection described below. |
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
| `ErrConsolidateLLMUnsupported` | Added in v0.10.0. Deprecated in v0.14.0: retained for matching historical errors; kept so `errors.Is` callers keep compiling |
| `SharedAgentID` constant | |
| `ConsolidateConfig` interface | **v0.14.0:** gained `GetOllamaURL()` and `GetOllamaConsolidateModel()`. This is a signature-level change to a stable identifier, taken with the deviation documented in the 0.14.0 changelog compatibility notes instead of a prior-minor deprecation cycle. Callers using `graymatter.Config` are unaffected; hand-rolled implementers add two getters |
| `GraphAccessor` interface | |
| `EntityExtractorAccessor` interface | |
| `TypedEntityExtractor` interface, `EntityRef`, `EntityLink` | Added in v0.12.0 — optional extractor capability preserving label + type and producing co-mention links; consolidation uses it when implemented, legacy ID-only path otherwise |
| `EdgeWriter` interface | Added in v0.12.0 — optional graph capability used by consolidation to persist co-mention edges |
| `AdvancedStore.SetKG(...)` | Exposed in v0.12.0 (mirrors `(*Store).SetKG`) |
| `(*Store).ConsolidationCounters()`, `ReadConsolidationCounters(db)` | Added in v0.14.0 — historical generative cycle and source-consumption totals from the meta bucket, surfaced by `status` and `doctor`. Maintenance cycles leave both counters unchanged. |

### Fact retention and deletion

Revision and retirement exclude the old fact from recall immediately.
Inspection and export can show the retired record while its tombstone remains
in the store. Recall provenance derives revision and consolidation lineage
from surviving tombstone rows. A source's receipt can disappear after that
row is pruned or explicitly deleted.

Unpinned facts decay with a default 30-day half-life. Maintenance can prune
live facts and tombstones with weight below 0.01. A pin protects against
automatic decay, pruning and summarisation.

`Store.Delete(agentID, factID)` removes the canonical row directly, regardless
of weight, age or pin status. The REST endpoint `DELETE /forget/{id}?agent=<id>`
performs that deletion by ID. `DELETE /forget` also accepts an exact ID in the
request body. Query-based deletion on that endpoint requires confirmation.
MCP `memory_reflect` with `forget` and the CLI `graymatter forget` command
retire the fact as a tombstone.

### Recall result ordering

**Recall result ordering is deterministic: descending final score, oldest
first, then ID.**

With zero confidence weight the final score is the existing fused RRF score.
An explicit confidence filter first restricts the corpus. Without a filter and
with zero weight, scores and selection retain the previous arithmetic.

The same query against the same store returns the same facts in the same order,
on every call, on every platform. Facts that score equally are ordered by
`CreatedAt` ascending, and facts created in the same instant by fact ID
ascending, which makes the order total.

This is a guarantee callers may rely on within each namespace. It applies to
`Recall` and `RecallShared`, and every configuration of `SignalWeights` and
`MinRelevance`. `RecallAll` combines those input rankings by namespace RRF and
breaks ties by agent-list position, shared-list position, then text. Batch merge
orders by best input rank, number of query hits, then first appearance.

Recall and Explain record activity for live aliases that add a token to the
query. Combined namespace recalls and batches deduplicate alias activity by
namespace and fact ID across the operation. The alias record can be updated
even when the query returns no ranked facts. Frontends use the daemon's
scoped batch capability when available and send scalar recalls to older
daemons. The daemon treats each scalar recall as a separate operation.
Preview and read-only stores leave alias activity unchanged.

For a live alias, maintenance and recorded activation follow its stored source.

| Stored `AliasSource` | Decay and pruning while live and unpinned | On recorded activation |
| --- | --- | --- |
| Empty (authored, including older records) | Exempt | Records access without resetting weight |
| `usage` | Ordinary decay and pruning | Resets weight to `1` while unpinned and records access |
| Any other value | Ordinary decay and pruning | Records access without resetting weight |

Recorded access increments the count and advances the timestamp monotonically.
A pin protects the record from decay and pruning and preserves its weight
during activation. Retired aliases stop expanding queries and follow ordinary
decay and pruning unless pinned. A retained pin leaves the alias retired.
Maintenance checks the current record before pruning, so a completed renewal
can protect a usage alias that was below the pruning threshold earlier in the
cycle.

**Exception, v0.12.0:** when a knowledge graph is wired via `SetKG` (directly,
via `AdvancedStore.SetKG`, or by enabling the daemon's `--kg` /
`GRAYMATTER_KG=1`), `Recall` may append **at most three** neighbour labels
after the ranked facts. The first `topK` entries keep the deterministic order
above; appended entries are enrichment hints, capped and deduplicated, and
never displace a ranked fact. Without a wired graph the exception does not
exist and `Recall` returns at most `topK` distinct eligible facts.

An explicit `min_confidence`, including `unverified`, suppresses this text-only
enrichment because graph labels have no confidence receipts. Retrieval metadata
reports `kg: "suppressed_min_confidence"`; without a filter it describes hints
as `"hints_without_confidence_receipts"`. Explain never adds that hint tail.

### Knowledge graph provenance

The edge's `sources` array is the compatible, capped projection of its fact
IDs. Versioned provenance separately records manual support, whether the
source list is complete or truncated, and exact support receipts identified
by namespace, fact ID and revision digest. An unversioned edge remains
unknown even when it has a short or empty source list. A new source can add
evidence without establishing completeness for an existing unknown edge.

Graph maintenance removes an edge only when its complete provenance proves
that it has no remaining support. Pending edges continue participating in
navigation. The inventory progress describes what has been examined. Its
completion can coexist with pending edges and their reasons. The CLI and
daemon maintain an existing graph even when auto-population is off.

`status --json` exposes the summary under `kg.provenance`.
`doctor --graph --json` exposes the same summary under `provenance`.
`kg inspect --json` includes the edge snapshot and pending reason for each
edge. An older daemon can return graph data with the provenance summary
unavailable.

`kg reconcile evidence.json` applies an explicit proof for one edge. Set
`expected` to the inspected edge object, `manual_support` to the verified
boolean, and `supports` to the full set of verified sources. Each entry has
`namespace`, `fact_id`, `kind`, and `text_sha256`, the lower-case SHA-256
digest of the exact UTF-8 fact text. The stored kind for ordinary facts is the
empty string. Supply an empty array for an edge proven to have only manual
support. The operation checks the edge snapshot and current source content in
the graph transaction. Stale proofs and truncated edges remain pending.



### Review journal

`(*Store).ReviewAnalyze` explicitly submits one namespace's eligible facts to
the configured provider. `ReviewList` returns summary rows and `ReviewInspect`
returns the preserved evidence and recorded decisions. `ReviewApply`
records reject, resolve, dismiss, or purge actions. The cancellable
`review-journal-v1` RPC capability covers the same operations. Analysis uses
a 90-second RPC limit or an earlier caller deadline. The other review calls
keep the ordinary inspection deadline. Credentials stay in the store owner.

The journal allows up to 12 sources of at most 16 KiB UTF-8 each. The encoded
request and provider response are each limited to 64 KiB. A response can
declare up to 12 findings, with conflict notes of at most 2 KiB. One journal
record is limited to 128 KiB. Each namespace has a 256-record limit and an
8 MiB logical payload limit, including retained history and snapshots.
Rejecting or resolving a record retains its payload. An explicit purge of a
closed record releases its evidence ownership. Unshared snapshots become
eligible for collection after a 30-day grace period.

Snapshots identify exact text, kind, and declared confidence in the
versioned content hash. A source's current, retired, changed, or missing
state is independent of an explicit resolution or dismissal. Journal
inspection and status/doctor projections use read transactions.

Ordinary and batch recall can include bounded `conflict_notices` metadata
in option-aware Go results and MCP structured output. Explain attaches a
notice to the first affected receipt in its result. A notice contains the
conflict ID, affected returned source IDs, and source state. Provider notes
remain in the journal. Legacy string-only Go methods retain their fact
text shape. The same evidence never gains active-memory eligibility from a
journal operation.

### Memory version lineage

`Fact` and `RecallProvenance` can carry `content_hash`, `historical_hash`,
`replaces`, `derived_from`, `legacy_sources`, and `lineage_status`. Each
version reference identifies a fact ID and content hash in the same namespace.
The hash covers exact text, kind, and declared confidence. Status can be
`versioned`, `content_changed`, `legacy_incomplete`, `missing`, or `cycle`.
`content_changed` reports the current hash and the historical hash separately.
Ambiguous legacy source IDs remain under `legacy_sources`, with a hash only
when exact source bytes have been retained. `supersedes` remains the ID list
for older receipt clients.

`(*Store).InspectLineage(ctx, namespace, factID, contentHash)` reads one
version. An omitted hash selects the canonical row or its last known lineage
head. Use the exact hash for pruned source-only evidence and to inspect a
historical version of changed content. Read-only inspection
keeps access counters and recall eligibility unchanged.

`graymatter recall --explain` and MCP `memory_search` with `explain: true`
show these fields in both text and structured receipts. REST `GET /facts`
includes the namespace-scoped projection. Authenticated
`GET /lineage?agent=<namespace>&fact_id=<id>&content_hash=<hash>` inspects an
exact version. The hash is optional, and an unavailable requested version
returns HTTP 404. Successful REST `DELETE /forget` responses retain
`status` and `deleted_id` and add `deletion_scope` and `lineage_retention`.
Deletion removes the fact record. Evidence ownership by lineage or review
continues to govern snapshot retention.

`graymatter export --format json --include-lineage` writes
`memory-lineage.json`, a versioned audit archive of fact rows, retained
snapshots, and relationships. `--agent` scopes the archive to one namespace.
Large archives can use `--no-daemon` after the daemon has released the store.
Plain JSON (`memories.json`), Markdown, and Obsidian export fact projections.
For full database recovery, `(*Store).Backup` writes a consistent bbolt
image, and `RestoreLineageBackup` validates the image before installing it
at an unused destination. The bbolt image includes the evidence ownership
needed to preserve the retained version graph.

<a id="confidence-options-unreleased"></a>

### Confidence options

These APIs and MCP/CLI options are available since v0.20.0 and absent from
v0.19.1. Labels are writer declarations, not probabilities or verification by
GrayMatter. New writes accept exactly `verified`, `inferred` or `unverified`.
Absent/empty historical labels are effectively inferred; unknown historical
labels are preserved and treated as unverified. Explicit invalid/null options
are rejected before effects.

An omitted add label retains the legacy empty representation. An omitted revise
label becomes `min(inferred, lowest effective confidence of the validated live
targets)`. An explicit label overrides it. LLM-extracted facts are unverified.
Generative compaction is disabled. A revision commits the replacement and
selected source retirements in one transaction. Changed sources, write
failures, and cancellation before commit roll back the entire revision. A
successful result identifies the committed replacement, even when cancellation
follows the commit. The replacement starts unpinned. Narrow lifecycle mutations
preserve unrelated confidence metadata.

`RecallOptions.MinConfidence` filters before document frequencies, signal ranks,
recency, fusion, `MinRelevance`, deduplication and top-k. An optional finite
`ConfidenceWeight` in `[0, 0.5]` multiplies base RRF score `B` by
`1 + weight * c`, where verified has `c = +1`, inferred/legacy `c = 0` and
unverified/unknown `c = -1`. `ConfidencePolicy` is `confidence-v1`.
`ranks.fused_score` remains `B`; the additive receipt `ranking` object identifies
the final score that controls selection. Explicit zero weight preserves filters.
Query options do not mutate global configuration.
`Config.ConfidenceWeight` and `StoreConfig.ConfidenceWeight` set the configured
request default within the same finite range. Omitted per-call weights use that
default; explicit zero overrides it. An omitted RPC weight uses the configured
default advertised by the actual server connection, unless the client supplies
`DialOptions.DefaultConfidenceWeight`. A server that does not advertise a default
uses the client's product default. Adapters send the resolved value explicitly;
batch resolves it once after connection preflight and before fan-out, keeping
one policy if the daemon restarts during the batch. Reconnection renegotiates
the default for later requests.

Both product and low-level defaults remain zero in v0.20.0.
A future positive product default requires explicit notice in a published
minor release and promotion in a subsequent minor release after the frozen
quality gates pass. v0.20.0 does not announce a positive default or close
issue #126. Explicit zero remains the legacy ranking opt-out and preserves
any requested filter.

New RPC methods negotiate `confidence-write-v1`, `confidence-recall-v1`,
`confidence-lifecycle-v1` and `confidence-shared-write-v1`. The separate shared
write capability provides `PutSharedWithOptionsReturningFact` and preserves
`RememberShared`'s policy of scheduling no automatic consolidation. A generic
write with `agent_id = "__shared__"` retains the ordinary `Remember` consolidation
policy. Existing RPC methods retain zero confidence preference.
Compatible reads and lifecycle operations can use an older daemon. Typed RPC
requests that can trigger maintenance, including ordinary writes and explicit
consolidation, require the daemon to advertise `maintenance-only-v1`. New
confidence options or an active positive default also require the applicable
confidence capability. Unsupported operations return an update/restart error
before dispatch. Reconnection renegotiates capabilities. An uncertain write is
never replayed.
Pin, unpin and forget remain available against an older daemon: after negotiating
the absence of `confidence-lifecycle-v1`, the client reads each target's current
metadata and uses the historical `UpdateFact` endpoint once. This compatibility
path retains the old daemon's read-modify-write concurrency limitations; upgraded
daemons use transactional field patches. An error after sending a mutation never
switches endpoints or replays that mutation. Typed revision clients require
`atomic-revision-v1` and `inspection-context-v1` before dispatch. The store
daemon can serve older wire requests using the same atomic revision path.
An acknowledged precommit cancellation reports rollback. A lost response
after dispatch reports an unknown outcome and requires inspection before
another attempt. Custom RPC backends advertise atomic revision only when
their implementation provides the same transaction guarantee.
The REST API retains its legacy endpoints and zero preference. Authenticated
MCP over HTTP includes the new options.

Filtered vector retrieval requires the native eligible-ID capability or an
optional custom capability that proves exhaustion. `VectorStore.Query` promises
only at-most-n results; a short list cannot prove completeness. Progressive
queries reuse one embedding and reject incomplete/no-progress prefixes.

A revision's durable vector work may finish after its canonical transaction.
While its namespace has pending vector work, recall uses canonical keyword
and recency ranking and skips the external vector query. Option-bearing
receipts can report `retrieval.vector_fallback` as `pending_vector_index`,
`vector_index_changed`, or `vector_index_unavailable`. Doctor and inspection
health expose `vector_search_degraded` while the durable queue is nonempty.
After reconciliation, a backend that cannot prove complete eligible results
can use the vectors stored on the canonical facts for revised namespaces.

A native index can lose a file even after acknowledging delivery. Reopening
checks canonical live embeddings in revised namespaces and queues any
missing entries. If the native index is unreadable and the store has
revision history or pending writes, writable recovery queues the live
canonical embeddings before quarantining and rebuilding the derived index.
Read-only opens and repairs that cannot proceed serve canonical recall in
degraded mode. A subsequent writable open can resume recovery.
Keyword-only search remains supported without a vector capability.
Concurrent confidence updates cannot mix eligibility with different receipt
metadata. Once a query embedding exists, an overlapping alias edit that changes
the effective query returns an explicit retry error before another embedding.

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
  `structuredContent` conforms to the tool's declared schema. Each result
  shape sets `additionalProperties: false` — on a `oneOf` union, each branch,
  since a root-level constraint would apply to every branch. New keys arrive
  only through a schema revision, never silently.
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
| `memory_search` | `agent_id`, `query` | `top_k` (default `8`), `explain` (boolean, default `false`), `min_confidence`, `confidence_weight` (since v0.20.0) |
| `memory_search_batch` | `agent_id`, `queries` | `top_k` (default `8`), `min_confidence`, `confidence_weight` (since v0.20.0) |
| `memory_add` | `agent_id`, `text` | `confidence` (since v0.20.0) |
| `memory_alias` | `agent_id`, `term`, `equivalents` | — |
| `checkpoint_save` | `agent_id` | `state` (string containing a JSON object) |
| `checkpoint_resume` | `agent_id` | `on_missing` (`"error"` \| `"empty"`, default `"error"`) |
| `memory_reflect` | `action`, plus at least one explicit valid `agent_id` (canonical) or `agent` (deprecated alias; `agent_id` wins when both are valid) | `text`, `target` |

`memory_reflect.action` is an enum: `add`, `update`, `forget`, `link`, `pin`,
`unpin`. Its input schema is a flat object with `required: ["action"]`, no
root `anyOf`, `oneOf`, or `allOf`, and `additionalProperties: false`. Both
identity spellings remain schema-optional so canonical-only and alias-only
callers can pass client validation. At least one explicit identity is required
at runtime for both stdio and HTTP; no identity is derived from the working
directory. An identity-free object passes the schema but returns a tool error
before any backend call.

Every supplied `agent_id` or `agent` must be a non-empty string that is not
entirely whitespace. Null, wrong types, empty strings, and whitespace-only
strings are rejected even when the other field is valid. When both fields are
valid, `agent_id` wins. Valid identities are not trimmed or normalized:
`" project "` and `"project"` address distinct namespaces; Unicode and
`__shared__` are accepted. This hardens malformed inputs that previously could
fall back to the alias or ignore an invalid alias without changing valid
callers. See [ADR-014](decisions/014-agent-id-canonical.md).

Since v0.20.0, `confidence` is accepted on reflect `add`/`update` only. Search
options on writes, confidence on other tools/actions, explicit null, wrong
types, unknown labels and non-finite/out-of-range weights are errors. The runtime
validates these new options explicitly; it does not enable global strict MCP
argument validation as a side effect.

### `structuredContent` payloads

| Tool | Success payload | Notes |
|---|---|---|
| `memory_search` | `{"agent_id", "query", "count", "facts", "feedback"?}` | `facts` is nullable in the schema; the empty result is `count: 0, facts: []` with a "No memories found" text notice. `feedback` is optional: it carries the weak-match vocabulary block when the query's vocabulary barely overlaps the store's (v0.18.0), omitted otherwise |
| `memory_search` with `explain: true` | `{"agent_id", "query", "count", "facts", "explained"?}` | Added in v0.17.0. `explained` carries one `RecallReceipt` per fact (same JSON shape as the Go type); `facts` is present but empty so the payload conforms to the declared schema. The ranking is identical to `explain: false` — explain only reads it out |
| `memory_search_batch` | `{"agent_id", "count", "merged", "per_query"}` | `count` is the number of distinct facts in `merged`; `per_query[]` carries `{"query", "facts", "error"?}` — a per-query failure is reflected in `per_query[].error` while the other query results still return successfully |
| `memory_add` | `{"agent_id", "stored"}` | `stored` is `true` on success |
| `memory_alias` | `{"agent_id", "term", "equivalents", "stored"}` | `stored` is `true` on success |
| `checkpoint_save` | `{"agent_id", "checkpoint_id", "created_at"}` | `created_at` is RFC3339 |
| `checkpoint_resume` | `{"id", "created_at", "state"?, "message_count"?}` | `state` is the persisted JSON object; keys marked `?` may be absent when empty |
| `checkpoint_resume` with `on_missing: "empty"` | `{"found": false, "agent_id"}` | Added in v0.20.0. The successful absence result; `found` is always `false` and is declared non-optional. The output schema declares this shape and the success shape under `oneOf` |
| `memory_reflect` | `{"action", "agent", "ok"}` | `ok` is `true` on success |

Confidence calls added in v0.20.0 preserve these keys and add declared optional
schema fields. Explicit-label add/update results carry `fact_id` and
`confidence`. Search, explain and batch add optional `retrieval` with effective
`min_confidence`, `confidence_weight`, `policy` and `kg`, including empty results.
Explain receipts add optional `ranking` with `base_score`, `final_score`,
`factor`, `effective_confidence`, `confidence_weight` and `policy`. Text reports
the same policy and identifies final scores. Legacy calls omit the new metadata.
Batch applies one global policy to all input queries and its merged view.

### Checkpoint resume absence and default

Latest resume selects the newest resumable checkpoint in the agent namespace.
It skips historical PreCompact and SessionEnd markers that carry only hook
provenance. These entries remain visible when listing checkpoints or loading
by ID. A namespace with only these markers has the same resume result as one
with no checkpoints.

The `graymatter run --resume latest` harness restores saved conversation
messages. State saved through MCP remains available to checkpoint readers and
is separate from harness message history. The harness starts a new run on typed
absence and returns other recovery failures before using memory or the model.

Errors return text-only `isError: true` results without `structuredContent`;
their wording may change. `checkpoint_resume` with no checkpoint returns such
an error **by default**; the optional `on_missing` parameter (`"error"` — the
default — or `"empty"`) lets a caller opt into the successful
`{"found": false, "agent_id"}` result above instead. This is an additive
option in v0.20.0, not a behaviour change for existing callers. **Advance
notice:** a default change to `"empty"` in **v0.21.0** is conditional on a
successful real OpenChamber/OpenCode smoke and publication of this prior-minor
notice and the matching changelog notice, per the deprecation rule above.
If the client rejects the union schema, apply and validate the documented
fallback before publishing
v0.20.0, keep `"error"` as the default, and defer the default change.
Callers that depend on absence being an error should pass `on_missing: "error"`
explicitly; it remains accepted as the legacy behaviour throughout v0.x.
Storage, daemon, and corrupt-record failures stay prose-only in both modes. The
former `{"error": "not_found", "agent_id"}` payload violated the declared
success schema and was removed to prevent strict clients from rejecting the
entire response (#117; [ADR-013 amendment](decisions/013-structured-tool-results.md));
the machine-readable absence result that amendment deferred is
[ADR-015](decisions/015-checkpoint-resume-empty-result.md).

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
