# Architecture — one page, with the tests that guard each module

The model mental written down, not promised. Each module: one line for what it
is, one for the invariant that matters, and the tests that hold the line when
someone changes the code. Decisions and their reversal conditions live in
[decisions/](decisions/README.md); the security boundary lives in
[threat-model.md](threat-model.md).

```
cmd/graymatter            CLI + TUI + MCP server + REST + daemon host (module cmd/graymatter)
pkg/memory                the engine: store, recall, consolidation (root module)
pkg/embedding             pluggable embeddings: Ollama → OpenAI → Anthropic → keyword
```

## pkg/memory — the engine

| Module | What it is | Invariant | Guarded by |
|---|---|---|---|
| `store.go` | bbolt persistence; facts are JSON docs under per-agent buckets | append-only: `Delete` is the only removal path; `UpdateFact` never creates | `store_test.go`, `reconcile_test.go` |
| `recall.go` | RRF fusion over vector / keyword / recency signals | deterministic total order (score → CreatedAt → ID); superseded facts never score | `tiebreak_test.go`, `golden_test.go` |
| `consolidate.go` | decay → propose/apply summarisation → prune → KG extraction | LLM proposes, apply is deterministic; receipts via tombstones; pinned facts exempt from all three steps (I-1) | `consolidate_ollama_test.go`, `consolidate_test.go`, `pin_test.go`, property: `TestConsolidation_TombstonePropertyAcrossCycles` |
| extraction watermark | SHA-256 text signature per fact | a fact is extracted once per text version; retired facts never reach the graph | `TestExtraction_WatermarkSkipsUnchangedAndSuperseded` |
| `fact.go` | the Fact record and its lifecycle markers | superseded ⇒ out of recall immediately; pinned ⇒ out of decay/prune/batch | `pin_test.go`, `supersede` tests in rpc/mcp |
| quality gates | corpus-backed regression floors | canonical recall@8 must not regress across consolidation | `benchmarks/retrieval_quality/consolidation_test.go`, `golden_test.go` |

## cmd/graymatter/internal/kg

| Module | What it is | Invariant | Guarded by |
|---|---|---|---|
| `extractor.go` | deterministic entity extraction feeding the graph | measured noise classes stay out (stoplist caps, no URL/date nodes); typed fallback is `concept` | golden gate: `extractor_golden_test.go` + `testdata/golden_facts.json`; regressions in `extractor_test.go` |
| `graph.go` | bbolt-backed nodes/edges, wikilink resolution | idempotent upserts; edges keep their source-fact receipts | `graph_test.go`, `graph_link_test.go` |

## cmd/graymatter/internal/{daemon,rpc surface}

| Module | What it is | Invariant | Guarded by |
|---|---|---|---|
| daemon Host | owns the store; serves checkpoints, sessions, KG, audit, token ledger over net/rpc | one writer process; every client connects or spawns it | `host_test.go`, `daemon_integration_test.go` |
| discovery + tokens | `graymatter.addr` (+token) and HTTP bearer files | on Windows both carry protected owner-only DACLs; failure to secure aborts startup | `sock_acl_windows_test.go`, `httpauth/token_acl_windows_test.go`; Unix perms unchanged |
| idle-exit / spawn | clients auto-start the daemon; it exits when unused | lock contention is never user-visible | `unit_test.go`, `rest_through_daemon_test.go` |

## cmd/graymatter/internal/mcp

| Tool surface | Invariant | Guarded by |
|---|---|---|
| `memory_search/add`, `checkpoint_*`, `memory_reflect` (add/update/forget/link/pin/unpin) | parameter names exactly as documented in [AGENTS.md](AGENTS.md); reflect's update/forget tombstone instead of delete | `handlers_test.go`, `supersede_test.go`, `pin_test.go`, `reflect_alias_test.go` |

## cmd/graymatter/internal/export

| Module | Invariant | Guarded by |
|---|---|---|
| Obsidian export | byte-deterministic index; UTF-8-safe truncation; pineados marcados `pinned: true`; wikilinks resuelven | `obsidian_test.go`, `export_graph_test.go` |

## CI as the outer guard

`.github/workflows/ci.yml` runs vet, `-race` tests on 3 OSes × 2 Go versions,
coverage gates (core ≥70%, CLI ≥65%), govulncheck, the standalone-build check
for the nested CLI module (`Version consistency`), and handshake-version
parity. `.github/workflows/fuzz.yml` runs the three fuzz targets nightly and
uploads crash reproducers on failure.
