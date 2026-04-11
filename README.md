<h1 align="center"> GrayMatter </h1>

<div align="center">
  <img width="1500" height="276" alt="graymatter-banner" src=".github/assets/graymatter1.jpg" />
</div>
<p align="center">
  <a href="https://github.com/angelnicolasc/graymatter/actions/workflows/ci.yml"><img src="https://github.com/angelnicolasc/graymatter/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://pkg.go.dev/github.com/angelnicolasc/graymatter"><img src="https://pkg.go.dev/badge/github.com/angelnicolasc/graymatter.svg" alt="Go Reference"></a>
  <a href="https://github.com/angelnicolasc/graymatter/releases/tag/v0.2.0"><img src="https://img.shields.io/github/v/release/angelnicolasc/graymatter" alt="Latest Release"></a>
  <img src="https://img.shields.io/badge/coverage-73.5%25-brightgreen" alt="Coverage 73.5%">
  <img src="https://img.shields.io/badge/platforms-linux%20%7C%20macOS%20%7C%20windows-blue" alt="Platforms">
  <img src="https://goreportcard.com/badge/github.com/angelnicolasc/graymatter" alt="Go Report Card">
  <img src="https://img.shields.io/github/license/angelnicolasc/graymatter" alt="License">
</p>
<div align="center">
  <strong>Three lines of code to give your AI agents persistent memory.</strong>
  <br />
  Single Go binary. Zero infra. Works with Claude Code, or any tool that calls the Anthropic Messages API.
  <br /><br />
  Free, offline, no account required.
</div>
<br /><br />

```bash
go get github.com/angelnicolasc/graymatter
```

```go
mem := graymatter.New(".graymatter")
mem.Remember("agent", "user prefers bullet points, hates long intros")
ctx := mem.Recall("agent", "how should I format this response?")
// ["user prefers bullet points, hates long intros"]
```

---

## Why

Every AI agent today is stateless by default. Every run starts from zero.

Mem0, Zep, Supermemory solve this — but in Python or TypeScript, and they
require a server. Go has zero production-ready, embeddable, zero-deps
memory layer for agents. That gap is GrayMatter.

**~90% token reduction** at 100 sessions versus full-history injection.
No Docker. No Redis. No Python. No API key required for storage.

---

## Install

**Binary (recommended):**

```bash
# macOS (Apple Silicon)
curl -sSL -o graymatter.tar.gz https://github.com/angelnicolasc/graymatter/releases/download/v0.2.0/graymatter_0.2.0_darwin_arm64.tar.gz
tar -xzf graymatter.tar.gz
sudo mv graymatter /usr/local/bin/

# Windows (PowerShell)
iwr https://github.com/angelnicolasc/graymatter/releases/download/v0.2.0/graymatter_0.2.0_windows_amd64.zip -OutFile graymatter.zip
Expand-Archive graymatter.zip -DestinationPath .\graymatter_cli
```

**Go install:**

```bash
go install github.com/angelnicolasc/graymatter/cmd/graymatter@latest
```

**Library:**

```bash
go get github.com/angelnicolasc/graymatter
```

---

## Library usage

Three functions. That's the entire API surface.

```go
import "github.com/angelnicolasc/graymatter"

// Open (or create) a memory store in the given directory.
mem := graymatter.New(".graymatter")
defer mem.Close()

// Store an observation.
mem.Remember("sales-closer", "Maria didn't reply Wednesday. Third touchpoint due Friday.")

// Retrieve relevant context for a query.
ctx := mem.Recall("sales-closer", "follow up Maria")
// ctx is a []string ready to inject into a system prompt:
// ["Maria didn't reply Wednesday. Third touchpoint due Friday."]
```

Every method has a context-aware variant that respects deadlines and cancellation signals end-to-end — no wrappers needed:

```go
ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
defer cancel()

if err := mem.RememberCtx(ctx, "agent", "observation"); err != nil { ... }
results, err := mem.RecallCtx(ctx, "agent", "query")
```

### Full agent pattern

```go
mem := graymatter.New(project.Root + "/.graymatter")
defer mem.Close()

// 1. Recall before calling the LLM.
memCtx, _ := mem.Recall(skill.Name, task.Description)

messages := []anthropic.MessageParam{
    {Role: "system", Content: skill.Identity + "\n\n## Memory\n" + strings.Join(memCtx, "\n")},
    {Role: "user",   Content: task.Description},
}

// 2. Call your LLM.
response, _ := client.Messages.New(ctx, anthropic.MessageNewParams{...})

// 3. Remember after the run.
mem.Remember(skill.Name, extractKeyFacts(response))
```

### Config

```go
mem, err := graymatter.NewWithConfig(graymatter.Config{
    DataDir:          ".graymatter",
    TopK:             8,
    EmbeddingMode:    graymatter.EmbeddingAuto,  // Ollama → OpenAI → Anthropic → keyword
    OllamaURL:        "http://localhost:11434",
    OllamaModel:      "nomic-embed-text",
    AnthropicAPIKey:  os.Getenv("ANTHROPIC_API_KEY"),
    OpenAIAPIKey:     os.Getenv("OPENAI_API_KEY"),
    DecayHalfLife:    30 * 24 * time.Hour,        // 30 days
    AsyncConsolidate: true,
})
```

---

## CLI

```bash
graymatter init                                    # create .graymatter/ + .mcp.json
graymatter remember "agent" "text to remember"    # store a fact
graymatter remember --shared "text"               # store in shared namespace (all agents)
graymatter recall   "agent" "query"               # print context
graymatter recall   --all "agent" "query"         # merge agent + shared memory
graymatter checkpoint list    "agent"             # show saved checkpoints
graymatter checkpoint resume  "agent"             # print latest checkpoint as JSON
graymatter mcp serve                              # start MCP server (Claude Code / Cursor)
graymatter mcp serve --http :8080                 # HTTP transport
graymatter export --format obsidian --out ~/vault # dump to Obsidian vault
graymatter tui                                    # 4-view terminal UI
graymatter run agent.md [--background]            # run a SKILL.md agent file
graymatter sessions list                          # list managed agent sessions
graymatter plugin install manifest.json           # install a plugin
graymatter server --addr :8080                    # REST API server
```

Global flags: `--dir` (data dir), `--quiet`, `--json`

---

## Observability

The REST server (`graymatter server`) exposes a `/metrics` endpoint powered by Go's standard `expvar` package — zero extra dependencies.

```
GET /metrics
```

```json
{
  "requests_total":     {"remember": 120, "recall": 340, "healthz": 5},
  "request_latency_us": {"remember": 4200, "recall": 1800},
  "facts_total":        {"stored": 120},
  "recall_total":       {"served": 340}
}
```

For library users, `memory.StoreConfig` exposes hooks for APM integration:

```go
store, err := memory.Open(memory.StoreConfig{
    DataDir:       ".graymatter",
    DecayHalfLife: 30 * 24 * time.Hour,

    // Called after every Recall with agent ID, query, result count, and latency.
    OnRecall: func(agentID, query string, n int, d time.Duration) {
        metrics.RecordHistogram("graymatter.recall.latency", d.Seconds())
    },

    // Called after every successful Put with agent ID, fact ID, and latency.
    OnPut: func(agentID, factID string, d time.Duration) {
        metrics.Increment("graymatter.facts.stored")
    },

    // Routes internal log events to any standard logger.
    Logger: slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),

    // Swap the vector backend entirely — bring your own Qdrant, pgvector, etc.
    VectorBackend: myQdrantAdapter,
})
```

---

## Claude Code / Cursor (MCP)

```bash
graymatter init     # creates .mcp.json automatically
```

Claude Code detects `.mcp.json` automatically. Five tools become available:

| Tool | What it does |
|------|-------------|
| `memory_search` | Recall facts for a query |
| `memory_add` | Store a new fact |
| `checkpoint_save` | Snapshot current session |
| `checkpoint_resume` | Restore last checkpoint |
| `memory_reflect` | Add / update / forget / link memories (agent self-edit) |

Or add manually to your project's `.mcp.json`:

```json
{
  "mcpServers": {
    "graymatter": {
      "command": "graymatter",
      "args": ["mcp", "serve"]
    }
  }
}
```

---

## Storage

| Layer | Tech | What it holds |
|-------|------|--------------|
| KV store | bbolt (pure Go, ACID) | Sessions, checkpoints, facts, metadata, KG |
| Vector index | chromem-go (pure Go) | Semantic embeddings, hybrid retrieval |
| Export | Markdown files | Human-readable, git-friendly, Obsidian-compatible |

Single file: `~/.graymatter/gray.db`  
Single folder: `.graymatter/vectors/`

No migrations. No schema versions. Append-only with decay-based eviction.

---

## Embeddings

GrayMatter degrades gracefully. It works without any embedding model.

| Mode | When |
|------|------|
| **Ollama** (default) | Machine has Ollama running with `nomic-embed-text` |
| **OpenAI** | `OPENAI_API_KEY` set, Ollama not available |
| **Anthropic** | `ANTHROPIC_API_KEY` set, Ollama and OpenAI not available |
| **Keyword-only** | No embedding available — TF-IDF + recency, zero deps |

Auto-detection order in `EmbeddingAuto` mode: Ollama → OpenAI → Anthropic → keyword.

```bash
# Pull the embedding model once (Ollama):
ollama pull nomic-embed-text

# Or set an API key (OpenAI or Anthropic):
export OPENAI_API_KEY=sk-...
export ANTHROPIC_API_KEY=sk-ant-...
```

---

## Memory lifecycle

```
Recall(agent, task)          ← hybrid: vector + keyword + recency → top-8 facts
    ↓
Inject into system prompt    ← your 3 lines of code
    ↓
Agent runs
    ↓
Remember(agent, observation) ← store key facts during/after run
    ↓
Consolidate() [async]        ← summarise + decay + prune (LLM optional)
```

Consolidation is the only "smart" step. Everything else is deterministic.
Without consolidation, GrayMatter still works — it just doesn't compress over time.

Consolidation auto-enables when `ANTHROPIC_API_KEY` is set. To use Ollama:

```go
cfg := graymatter.DefaultConfig()
cfg.ConsolidateLLM = "ollama"
```

---

## Token efficiency

Numbers produced by `go run ./benchmarks/token_count` — real Recall calls,
keyword embedder, no LLM required:

| Sessions | Full injection | GrayMatter | Reduction |
|----------|---------------|------------|-----------|
| 1        | ~80 tokens    | ~80 tokens | 0% |
| 10       | ~630 tokens   | ~550 tokens | 12% |
| 30       | ~1,880 tokens | ~550 tokens | 71% |
| 100      | ~6,960 tokens | ~670 tokens | **90%** |

Each "session" = one paragraph-length agent observation (~60 words).
GrayMatter always injects only the top-8 most relevant observations for the query.
With vector embeddings the recall precision improves, maintaining similar reduction ratios.

Reproduce locally:

```bash
go run ./benchmarks/token_count
```

---

## Build from source

```bash
git clone https://github.com/angelnicolasc/graymatter
cd graymatter
CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=dev" -o graymatter ./cmd/graymatter
```

Output: single static binary, ~10 MB, no runtime dependencies.

---

## Testing

The full test suite requires no LLM and no network — every test uses
`t.TempDir()` with a keyword embedder or injected stubs. Runs clean on
Linux, macOS, and Windows in CI.

```bash
# Core library
go test -count=1 -timeout=120s ./pkg/memory/...

# CLI / server / plugins
cd cmd/graymatter && go test -count=1 -timeout=120s ./internal/...
```

| Package | Tests | What's covered |
|---------|-------|----------------|
| `pkg/memory` | 42 unit tests + 3 fuzz targets | Store lifecycle, hybrid recall, RRF fusion, decay math, semaphore, concurrent writes, vector paths, dimension guard |
| `internal/harness` | 21 | Agent file parsing, retry/backoff, session recovery |
| `internal/kg` | 21 | Graph CRUD, entity extraction, weight decay, Obsidian export |
| `internal/server` | 11 | All REST endpoints, concurrent remember/recall, cancelled-context requests |
| `internal/plugin` | 10 | Install, list, remove, E2E echo plugin binary |

**Fuzz targets** (`pkg/memory`): `FuzzTokenize`, `FuzzUnmarshalFact`, `FuzzKeywordScore` — each with a seeded corpus so they run deterministically in CI and can be extended with `go test -fuzz`.

**Core library coverage: 73.5%** (CI gate: ≥ 70%). Measured without mocks — real bbolt + chromem-go instances in a temp directory.

Token-reduction benchmark (also zero deps):

```bash
go run ./benchmarks/token_count
```

---

## What GrayMatter is NOT

- Not a framework. Not an agent runner. Not a replacement for your existing tooling.
- Not a hosted service. Not a SaaS. Not a cloud product.
- Not a knowledge base UI. Not Notion. Not Obsidian.
- Not trying to win the enterprise memory market.

It is exactly one thing: **the missing stateful layer for Go CLI agents**,
packaged as a library you import in two lines.

---

## Roadmap

- [x] Library: `Remember` / `Recall` / `Consolidate`
- [x] bbolt + chromem-go storage
- [x] Ollama + OpenAI + Anthropic + keyword-only embedding
- [x] Hybrid retrieval (vector + keyword + recency, RRF fusion)
- [x] CLI: `init remember recall checkpoint export run sessions plugin server`
- [x] MCP server (Claude Code / Cursor) + `memory_reflect` self-edit tool
- [x] Knowledge graph (entity extraction, node/edge linking, Obsidian export)
- [x] Shared memory across agents (`--shared`, `--all` flags, `__shared__` namespace)
- [x] REST API server mode (`graymatter server --addr :8080`)
- [x] Plugin system (JSON line protocol, `graymatter plugin install/list/remove`)
- [x] 4-view Bubble Tea TUI (Memory / Sessions / Knowledge Graph / Stats)
- [x] Context-propagation API (`RememberCtx`, `RecallCtx`, `RecallAllCtx`, …)
- [x] Pluggable `VectorStore` interface (swap chromem-go for Qdrant, pgvector, etc.)
- [x] expvar `/metrics` endpoint — zero-dep, stdlib-only observability
- [x] `OnRecall` / `OnPut` / `Logger` hooks for APM integration
- [x] Embedding dimension guard — warns on provider switch instead of silent corruption
- [x] go.work workspace — core library imports zero TUI/CLI dependencies
- [x] Three-platform CI (Linux, macOS, Windows) + 73.5% coverage gate
- [x] Fuzz testing: `FuzzTokenize`, `FuzzUnmarshalFact`, `FuzzKeywordScore`
- [ ] Ollama-backed consolidation LLM (Ollama as summariser, not just embedder)
- [ ] WebSocket streaming for REST API

---

*GrayMatter — v0.2.0 — April 2026*
