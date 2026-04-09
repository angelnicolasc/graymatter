<h1 align="center"> GrayMatter </h1>

<div align="center">
  <img width="1500" height="276" alt="graymatter-banner" src=".github/assets/graymatter1.jpg" />
</div>
<p align="center">
  <a href="https://github.com/angelnicolasc/graymatter/actions/workflows/ci.yml"><img src="https://github.com/angelnicolasc/graymatter/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://pkg.go.dev/github.com/angelnicolasc/graymatter"><img src="https://pkg.go.dev/badge/github.com/angelnicolasc/graymatter.svg" alt="Go Reference"></a>
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
# macOS / Linux
curl -sSL https://github.com/angelnicolasc/graymatter/releases/latest/download/graymatter_$(uname -s)_$(uname -m).tar.gz | tar xz
sudo mv graymatter /usr/local/bin/

# Windows (PowerShell)
iwr https://github.com/angelnicolasc/graymatter/releases/latest/download/graymatter_Windows_x86_64.zip -OutFile graymatter.zip
Expand-Archive graymatter.zip; Move-Item graymatter\graymatter.exe C:\Windows\System32\
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

The full test suite requires no LLM and no network — all tests use
`t.TempDir()` and a keyword embedder or injected stubs:

```bash
CGO_ENABLED=0 go test ./... -count=1 -timeout=120s
```

| Package | Tests | What's covered |
|---------|-------|----------------|
| `pkg/harness` | 16 | Agent file parsing, retry/backoff logic, session recovery |
| `pkg/kg` | 21 | Graph CRUD, entity extraction, weight decay, Obsidian export |
| `pkg/memory` | 6 | Shared memory namespace, RRF deduplication |
| `pkg/plugin` | 10 | Install, list, remove, E2E echo plugin binary |
| `pkg/server` | 9 | All REST endpoints, 400/503 error handling |

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
- [ ] Ollama-backed consolidation LLM (Ollama as summariser, not just embedder)
- [ ] WebSocket streaming for REST API

---

*GrayMatter — april 2026*
