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
| `(*Memory).Store() *memory.Store` | |
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
| `(*Store).Recall(ctx, agentID, query string, topK int) ([]string, error)` | |
| `(*Store).RecallShared(ctx, query string, topK int) ([]string, error)` | |
| `(*Store).RecallAll(ctx, agentID, query string, topK int) ([]string, error)` | |
| `(*Store).PutShared(ctx, text string) error` | |
| `(*Store).MaybeConsolidate(ctx, agentID string, cfg ConsolidateConfig) error` | |
| `(*Store).Consolidate(ctx, agentID string, cfg ConsolidateConfig) error` | |
| `(*Store).Close() error` | |
| `(*Store).SetKG(graph GraphAccessor, extractor EntityExtractorAccessor)` | |
| `(*Store).DB() *bolt.DB` | |
| `Fact` struct — all fields present in v0.1.0 | |
| `MemoryStats` struct | |
| `StoreConfig` struct — all fields present in v0.1.0 | |
| `SharedAgentID` constant | |
| `ConsolidateConfig` interface | |
| `GraphAccessor` interface | |
| `EntityExtractorAccessor` interface | |

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

- `pkg/kg` — knowledge graph and entity extraction
- `pkg/session` — session checkpointing
- `pkg/harness` — agent runner
- `pkg/mcp` — MCP server handlers
- `pkg/server` — REST API server
- `pkg/plugin` — plugin protocol
- `pkg/export` — Obsidian / markdown export
- `pkg/embedding` — embedding backend adapters
- `cmd/` — CLI command implementations
