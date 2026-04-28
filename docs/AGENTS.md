# AGENTS.md — GrayMatter Memory Guide for AI Agents

> Operational guide for AI agents (Claude Code, Cursor, OpenCode, Codex, Antigravity, custom MCP clients, Go callers) using GrayMatter as long-term memory.
>


---

## Philosophy

GrayMatter is your long-term memory. Unlike conversation context, which disappears at the end of a session, GrayMatter facts persist across sessions, projects, and agent restarts. Use it to accumulate knowledge that makes you more effective over time.

**Key principle**: Store *conclusions*, not *conversations*. A good memory is something you would want injected into your system prompt on day 1 of a new session.

---

## MCP Tool Reference

Five tools are registered by `graymatter mcp serve` (see [`cmd/graymatter/internal/mcp/server.go`](../cmd/graymatter/internal/mcp/server.go)). **Parameter names are not uniform** — check the table before calling.

| Tool | Required params | Optional params | Returns |
|------|----------------|-----------------|---------|
| `memory_search` | `agent_id` (string), `query` (string) | `top_k` (int, default `8`) | Newline-separated fact texts (deduped) |
| `memory_add` | `agent_id` (string), `text` (string) | — | Confirmation string |
| `checkpoint_save` | `agent_id` (string) | `state` (JSON-encoded string) | Checkpoint ID + RFC3339 timestamp |
| `checkpoint_resume` | `agent_id` (string) | — | JSON: `{id, created_at, state}` (latest checkpoint) or empty if none |
| `memory_reflect` | `action` (`add`\|`update`\|`forget`\|`link`), **`agent`** (string), `text` (string) | `target` (string — old fact text for `update`/`forget`; target node ID for `link`) | Confirmation string |

> ⚠️ **`memory_reflect` uses `agent`, not `agent_id`.** The other four tools use `agent_id`. If your client builds tool calls programmatically, branch on tool name.

### Return-shape examples

```jsonc
// memory_search
"User prefers TypeScript with strict mode\nProject uses pnpm, not npm\nAuth tokens live in HttpOnly cookies"

// checkpoint_resume (no checkpoint yet)
""

// checkpoint_resume (latest)
{
  "id": "ckpt_01HW...",
  "created_at": "2026-04-28T13:42:11Z",
  "state": "{\"task\":\"db migration\",\"step\":3}"
}
```

---

## When to Use Memory

### ALWAYS store

- **User preferences** — coding style, communication preferences, tool choices
- **Project conventions** — "this repo uses tabs not spaces", "never use X library"
- **Architecture decisions** — "chose PostgreSQL over MySQL because…"
- **Bug fixes & workarounds** — "fixed by upgrading to v2.3, don't downgrade"
- **Recurring patterns** — "user always asks for TypeScript examples first"
- **Environment quirks** — "needs `NODE_OPTIONS=--max-old-space-size=4096`"
- **Stakeholder info** — "CTO prefers detailed explanations, CEO wants summaries"

### NEVER store

- **Conversation logs** — raw back-and-forth without conclusions
- **Duplicate information** — already in README, AGENTS.md, or code comments
- **Speculative thoughts** — "maybe we should try X" (store after the decision)
- **Secrets or credentials** — use proper secret management
- **Large outputs** — store the insight, not the 500-line stack trace

(Transient session state goes in a checkpoint, not a memory — see Anti-Pattern §5.)

### Decision Framework

```
About to store something?
├── Is it a conclusion / fact / preference?     → YES, store it
├── Is it raw conversation without insight?     → NO, extract insight first
├── Is it already documented in code/README?    → NO, reference docs instead
├── Will this still matter in 10 sessions?      → YES, store it
├── Is it temporary debugging state?            → NO, use checkpoint
└── Is it a secret / credential?                → NO, never store in memory
```

---

## Memory Operations

### `memory_add` — store a clean fact

Use when you have a single, atomic, well-formed fact.

**Good:**
```jsonc
{ "tool": "memory_add", "args": {
    "agent_id": "frontend-agent",
    "text":     "User prefers Tailwind CSS over styled-components"
}}

{ "tool": "memory_add", "args": {
    "agent_id": "backend-agent",
    "text":     "API rate limit: 100 req/min — exceeded returns 429 with Retry-After header"
}}
```

**Bad:**
```jsonc
// Too vague
{ "agent_id": "agent", "text": "user likes things" }

// Conversation log
{ "agent_id": "agent", "text": "User: Can you help? Agent: Sure, what do you need?" }

// Duplicate (already in README)
{ "agent_id": "agent", "text": "Project uses React" }
```

### `memory_search` — retrieve relevant context

Always search before acting on ambiguous requests. Phrase the query as the *task you're trying to do*, not as keywords.

**Good queries:**
```jsonc
{ "agent_id": "frontend-agent",
  "query":    "how should I style this component",
  "top_k":    5 }

{ "agent_id": "backend-agent",
  "query":    "authentication middleware patterns for this project",
  "top_k":    8 }
```

**How retrieval works**

GrayMatter ranks facts via **Reciprocal Rank Fusion (RRF)** over three independent signals (see [`pkg/memory/recall.go:14`](../pkg/memory/recall.go)):

1. **Vector similarity** (cosine, pluggable `VectorStore`) — when embeddings are available
2. **Keyword relevance** (TF-IDF approximation over bbolt facts)
3. **Recency** (exponential decay from `CreatedAt`)

Each signal produces an independent ranking; RRF fuses the ranks (not the scores) into a single ordered list. Returns top-K, deduplicated by text. Access metadata is updated asynchronously (`AccessCount++`, `AccessedAt = now`). If a knowledge graph is wired, neighbour entities of the top hits are appended.

> RRF means **rank position matters, not raw scores**. There are no tunable percentage weights to fiddle with — that's the whole point of RRF. If you want stronger recency bias, lower `DecayHalfLife`; for stronger keyword bias, configure `EmbeddingMode = EmbeddingKeyword`.

**Query strategies:**

```jsonc
// Strategy 1: Broad context gathering at session start
{ "agent_id": "agent", "query": "<current task description>", "top_k": 8 }

// Strategy 2: Focused lookup mid-task
{ "agent_id": "agent", "query": "<specific question>", "top_k": 3 }

// Strategy 3: Multi-query fusion for ambiguous topics
// Issue 2-3 related queries, dedupe results yourself.
```

### `memory_reflect` — self-curation

The most powerful tool. Use it to maintain memory quality over time.

> ⚠️ Parameter is **`agent`** (not `agent_id`). The asymmetry is intentional historical scar tissue and will not be flipped without a major version bump.

| Action | Param meaning of `text` | Param meaning of `target` |
|--------|-------------------------|---------------------------|
| `add` | The new fact | (unused) |
| `update` | The corrected fact | The old fact text to supersede |
| `forget` | The fact to remove | (or, equivalently, pass via `target`) |
| `link` | Description of the edge | Target node ID in the knowledge graph |

**Update workflow:**
```jsonc
// 1. Find the old fact
{ "tool": "memory_search", "args": {
    "agent_id": "backend-agent", "query": "API base URL", "top_k": 3
}}

// 2. Supersede it
{ "tool": "memory_reflect", "args": {
    "action": "update",
    "agent":  "backend-agent",
    "text":   "API base URL is https://api.v2.example.com",
    "target": "API base URL is https://api.v1.example.com"
}}
```

The old fact is dropped to weight 0; consolidation prunes it on the next pass.

**Forget workflow:**
```jsonc
{ "tool": "memory_reflect", "args": {
    "action": "forget",
    "agent":  "backend-agent",
    "text":   "Workaround for Node 14 bug (project now on Node 18)"
}}
```

**Link workflow (knowledge graph):**

> ⚠️ The `link` action only works when the host has wired `SetKGLinker(...)` on the MCP server (`cmd/graymatter/internal/mcp/server.go:55-57`). If the linker isn't wired, the tool returns an error like `knowledge graph not configured`. Agents should call `link` opportunistically and gracefully degrade if it fails — don't make `link` a hard prerequisite for any workflow.

```jsonc
{ "tool": "memory_reflect", "args": {
    "action": "link",
    "agent":  "backend-agent",
    "text":   "depends_on",
    "target":  "user-database"
}}
```

### `checkpoint_save` / `checkpoint_resume` — session continuity

Use for long-running tasks that might span multiple sessions or be interrupted.

**What checkpoints capture:**
- An arbitrary JSON-encoded state string (your choice of structure)
- An ID + RFC3339 timestamp

**What they DON'T capture:**
- Memory facts (separate system — use `memory_add`)
- Filesystem state
- External-service state

**Pattern: task-progress tracking**
```jsonc
// Before starting
{ "tool": "checkpoint_save", "args": {
    "agent_id": "migration-agent",
    "state":    "{\"task\":\"db migration\",\"step\":0,\"tables_done\":[]}"
}}

// After each step
{ "tool": "checkpoint_save", "args": {
    "agent_id": "migration-agent",
    "state":    "{\"task\":\"db migration\",\"step\":3,\"tables_done\":[\"users\",\"orders\"]}"
}}

// On session start
{ "tool": "checkpoint_resume", "args": { "agent_id": "migration-agent" } }
// → parse the returned `state` JSON, continue from step
```

`state` is a **string** at the MCP layer — encode/decode JSON yourself. The CLI (`graymatter checkpoint resume`) does the same.

---

## Memory Hygiene

### Fact-quality checklist

Before storing, verify the fact:

- [ ] **Atomic** — one idea per fact, not a paragraph
- [ ] **Timeless** — still true in 3 months
- [ ] **Actionable** — helps future-you make better decisions
- [ ] **Specific** — "prefers tabs", not "has preferences"
- [ ] **Self-contained** — readable without conversation context

### Decay & consolidation

Facts decay. A fact you never recall will eventually be pruned.

**Mechanics** (defaults from [`config.go`](../config.go)):
- Initial weight = `1.0`
- Exponential decay based on time since last access
- Half-life = `30 days` (`DecayHalfLife = 720h`)
- Pruned when weight `< 0.01`
- Recall resets the decay clock for that fact
- Consolidation triggers when an agent has ≥ `ConsolidateThreshold` (default `20`) facts; runs async unless `AsyncConsolidate = false`; up to `MaxAsyncConsolidations` (default `2`) goroutines concurrently

**Implications:**
```jsonc
// Anti-pattern: store once, never reference → pruned in ~60 days
{ "tool": "memory_add", "args": { "agent_id": "agent", "text": "Critical security policy: …" }}
// Then never search for it.

// Better: keep important facts warm by including them in routine context-gathering.

// Best: pin truly permanent rules to the shared namespace (see "Shared memory" below).
```

### Cleanup schedule

Every 10–20 sessions, sweep:

```bash
# 1. List everything for an agent
graymatter recall <agent_id> "*" --all

# 2. Identify low-quality entries (vague, outdated, duplicate)
# 3. Clean up via memory_reflect (forget / update)
```

---

## Shared Memory (`__shared__`)

GrayMatter reserves the agent ID `__shared__` (the constant `SharedAgentID` in [`pkg/memory/store.go:40`](../pkg/memory/store.go)) for facts every agent in this workspace should see — project conventions, team rules, security policies.

There is **no magic routing** at the MCP layer. To write or read shared memory, just pass `__shared__` as the `agent_id` parameter exactly like any other agent ID:

```jsonc
// Write a project-wide rule
{ "tool": "memory_add", "args": {
    "agent_id": "__shared__",
    "text":     "Project convention: all timestamps stored as UTC ISO-8601 strings"
}}

// Read it
{ "tool": "memory_search", "args": {
    "agent_id": "__shared__",
    "query":    "timestamp conventions",
    "top_k":    5
}}
```

**Per-agent + shared in one shot**: issue two calls (one with the agent's own ID, one with `__shared__`) and merge the results. The Go library exposes a `RecallAll(agentID, query)` helper that does this for you ([`graymatter.go`](../graymatter.go)) — there is no MCP equivalent.

**Shared-memory best practices:**
- Store **project-wide** conventions, not agent-specific preferences
- Prefix shared facts with intent: `"Project convention: …"`, `"Team rule: …"`, `"Security policy: …"`
- Keep it small and high-signal (≲ 50 facts)
- The CLI `--shared` flag on `graymatter remember` / `graymatter recall` writes/reads this namespace directly

---

## Session Continuity Patterns

### Pattern 1: memory-first boot

```jsonc
// 1. Was I interrupted?
{ "tool": "checkpoint_resume", "args": { "agent_id": "my-agent" } }

// 2. Pull relevant memories for the current task
{ "tool": "memory_search", "args": {
    "agent_id": "my-agent",
    "query":    "<current task description>",
    "top_k":    8
}}

// 3. Pull shared context
{ "tool": "memory_search", "args": {
    "agent_id": "__shared__",
    "query":    "<current task description>",
    "top_k":    5
}}

// 4. Concatenate into the system prompt and proceed.
```

### Pattern 2: continuous learning

After significant interactions, extract atomic conclusions and `memory_add` them. Don't store the conversation; store what you *learned*.

### Pattern 3: multi-agent coordination

```jsonc
// Agent-A discovers a convention
{ "tool": "memory_add", "args": { "agent_id": "agent-a",
    "text": "Use async/await, not callbacks" }}

// Promote it to shared so Agent-B sees it on their next recall
{ "tool": "memory_add", "args": { "agent_id": "__shared__",
    "text": "Project convention: use async/await, not callbacks" }}

// Agent-B picks it up via shared search
{ "tool": "memory_search", "args": {
    "agent_id": "__shared__",
    "query":    "async patterns" }}
```

---

## CLI Parity

Every memory operation is also available from the terminal — useful for scripts, CI hooks, and debugging.

| MCP tool | CLI equivalent |
|----------|----------------|
| `memory_add` | `graymatter remember <agent_id> "<text>"` (or `--shared` for `__shared__`) |
| `memory_search` | `graymatter recall <agent_id> "<query>"` (`--all` to dump every fact, `--shared` to query `__shared__`) |
| `checkpoint_save` | (library/MCP only — no CLI) |
| `checkpoint_resume` | `graymatter checkpoint resume <agent_id>` (lists most recent) |
| — | `graymatter checkpoint list <agent_id>` (history) |
| `memory_reflect` | (MCP only — no CLI) |

Other useful subcommands:

| Command | Purpose |
|---------|---------|
| `graymatter init` | Wire MCP into Claude Code, Cursor, Codex, OpenCode, Antigravity (see [README.md](../README.md)) |
| `graymatter mcp serve` | Start the MCP server (stdio default, `--http :8080` for HTTP) |
| `graymatter tui` | 4-view terminal dashboard (live observability) |
| `graymatter export --format obsidian --out vault/` | Dump all memories to a Markdown vault |
| `graymatter run <skill.md>` | Execute a SKILL.md agent file |
| `graymatter sessions list` | List managed agent sessions |
| `graymatter plugin {install,list,remove}` | Manage local plugins |

---

## Library API (Go callers)

If you're embedding GrayMatter directly in a Go program (not via MCP), see [`examples/agent/main.go`](../examples/agent/main.go) for the canonical pattern:

1. `graymatter.Open(graymatter.DefaultConfig())` — open the store
2. `mem.Recall(ctx, agentID, query, topK)` — pull context before the LLM call
3. Inject the recalled facts into the system prompt
4. After the LLM responds, `mem.Remember(ctx, agentID, conclusion)` (or `RememberExtracted` to let GrayMatter pull atomic facts via Anthropic Haiku)
5. `defer mem.Close()` to flush + release the bbolt lock

For the public API surface and stability promises, see [`docs/api-stability.md`](api-stability.md).

---

## Multi-Process Gotcha (bbolt write lock)

GrayMatter persists to bbolt, a single-writer embedded DB. **Only one process may hold the write lock at a time.** This shows up the moment you run two MCP-aware agents in the same workspace (e.g. Claude Code + OpenCode + the `graymatter tui` dashboard).

What happens in v0.5.x:

- The `graymatter` CLI and TUI auto-detect a held lock and **fall back to read-only mode**. You can still recall, but `remember` / `checkpoint save` will refuse with a clear error rather than block forever.
- MCP servers spawned by separate clients will fight over the lock. The **second one to start fails fast**, not silently.
- Workarounds:
  - Run a single shared `graymatter mcp serve --http :8080` and point all clients at it (most robust)
  - Quit one agent's MCP integration before working from the other
  - Use the `tui` in `--read-only` mode explicitly when you only want to inspect

If you're an agent and `memory_add` returns a lock error, **degrade gracefully**: keep the fact in your in-context working memory, surface the error to the user, suggest closing competing processes — don't retry in a loop.

---

## Anti-Patterns

### 1. The Dumping Ground

```jsonc
// BAD
{ "agent_id": "agent", "text": "User said hello" }
{ "agent_id": "agent", "text": "User asked about weather" }
{ "agent_id": "agent", "text": "I responded with the forecast" }
// → 1000 low-signal facts, important ones buried

// GOOD
{ "agent_id": "agent",
  "text":     "User is planning outdoor event, needs weather updates" }
```

### 2. The Self-Fulfilling Prophecy

```jsonc
// BAD: never updating
{ "agent_id": "agent", "text": "User likes X" }
// User changes preference; you keep recalling and acting on the stale fact.

// GOOD: update on change
{ "tool": "memory_reflect", "args": {
    "action": "update",
    "agent":  "agent",
    "text":   "User now prefers Y (changed from X)",
    "target": "User likes X" }}
```

### 3. The Orphaned Fact

```jsonc
// BAD: no context
{ "agent_id": "agent", "text": "Blue" }

// GOOD: contextual
{ "agent_id": "agent", "text": "User's preferred UI theme: blue" }
```

### 4. The Over-Specific Fact

```jsonc
// BAD: rotting timestamp & location
{ "agent_id": "agent",
  "text":     "On 2026-04-15 at 3:42pm, fixed bug in line 47 of auth.js" }

// GOOD: generalised learning
{ "agent_id": "agent",
  "text":     "auth.js: JWT validation fails when clock skew > 5 minutes" }
```

### 5. The Memory Leak (transient state as a fact)

```jsonc
// BAD
{ "tool": "memory_add", "args": {
    "agent_id": "agent",
    "text":     "Current file being edited: src/components/Button.tsx" }}

// GOOD: that's checkpoint territory
{ "tool": "checkpoint_save", "args": {
    "agent_id": "agent",
    "state":    "{\"current_file\":\"src/components/Button.tsx\"}" }}
```

### 6. Ignoring Shared Memory

```jsonc
// BAD: every agent stores the same convention
{ "agent_id": "agent-a", "text": "Use TypeScript" }
{ "agent_id": "agent-b", "text": "Use TypeScript" }
{ "agent_id": "agent-c", "text": "Use TypeScript" }

// GOOD: write once, all agents see it
{ "agent_id": "__shared__",
  "text":     "Project convention: use TypeScript" }
```

### 7. Treating `link` as Mandatory

`memory_reflect` `link` only works when the host has wired a knowledge-graph linker. If your agent loop *requires* `link` to function, it'll break in stock deployments. Treat it as optional enrichment, not infrastructure.

---

## Performance Considerations

### Token budget

| Sessions | Full history | GrayMatter | Savings |
|---------:|-------------:|-----------:|--------:|
| 1 | ~80 | ~80 | 0% |
| 10 | ~630 | ~550 | 12% |
| 30 | ~1,880 | ~550 | 71% |
| 100 | ~6,960 | ~670 | **~90%** |

GrayMatter pays off after roughly 10 sessions. For one-shot agents, the overhead may not be worth it. See [`docs/benchmarks.md`](benchmarks.md) for the full methodology.

### Latency

| Operation | Typical | Notes |
|-----------|--------:|-------|
| `memory_add` | 5–20 ms | bbolt write + optional vector upsert |
| `memory_search` | 10–50 ms | Keyword + vector + RRF fusion |
| `checkpoint_save` | 5–15 ms | Single bbolt transaction |
| `checkpoint_resume` | 5–10 ms | Direct key lookup |

Safe to call multiple times per turn. No need to batch.

### Storage growth

```
Per fact: text_bytes + (embedding_dim × 4 bytes)
With nomic-embed-text (768-dim): ~3 KB / fact
1000 facts: ~3 MB on disk
```

Even very large memory stores stay tiny. Don't pre-optimise for storage.

---

## Configuration Quick Reference

### Environment variables

```bash
# Embedding providers (auto-detected in this order: Ollama → OpenAI → Anthropic → keyword)
export OPENAI_API_KEY=sk-...           # OpenAI embeddings
export ANTHROPIC_API_KEY=sk-ant-...    # Anthropic embeddings + consolidation LLM

# Or run Ollama locally (default, recommended)
ollama pull nomic-embed-text
export GRAYMATTER_OLLAMA_URL=http://localhost:11434     # optional override
export GRAYMATTER_OLLAMA_MODEL=nomic-embed-text         # optional override
export GRAYMATTER_OPENAI_MODEL=text-embedding-3-small   # optional override
```

### Key config fields ([`config.go`](../config.go))

| Field | Default | When to tune |
|-------|---------|--------------|
| `DataDir` | `.graymatter` | Move out of the workspace if you don't want it tracked |
| `TopK` | `8` | ↑ to 12 for very dense memory; ↓ to 5 if facts are highly specific |
| `EmbeddingMode` | `EmbeddingAuto` | Force `EmbeddingKeyword` to skip vector search entirely |
| `DecayHalfLife` | `720h` (30 d) | ↓ to 7 d for fast-changing domains; ↑ to 90 d for stable conventions |
| `ConsolidateThreshold` | `20` | ↓ to 10 for aggressive consolidation; ↑ to 50 for retention |
| `AsyncConsolidate` | `true` | Set `false` only in tests / deterministic CI |
| `MaxAsyncConsolidations` | `2` | Concurrency cap on background consolidation |
| `ReadOnly` | `false` | Set `true` to open the store without taking the write lock |

---

## Quick Decision Trees

### Should I store this?

```
Is it a conclusion / decision / preference?
├── YES → Is it already in code/README?
│   ├── YES → Don't store (reference docs instead)
│   └── NO  → Store it
└── NO  → Is it temporary state?
    ├── YES → Use checkpoint
    └── NO  → Don't store
```

### Which tool?

```
Need to store a fact?
├── Atomic fact ready              → memory_add
├── Long LLM response, multiple    → graymatter.RememberExtracted (Go) or extract yourself
│   insights inside                  before calling memory_add
├── Fix / replace existing fact    → memory_reflect action=update
├── Remove a bad fact              → memory_reflect action=forget
└── Connect two entities (KG)      → memory_reflect action=link  (host must wire SetKGLinker)

Need to retrieve context?
├── Agent-specific only            → memory_search (agent_id=<your-id>)
├── Shared only                    → memory_search (agent_id="__shared__")
├── Both merged                    → two calls, merge yourself  (or use RecallAll in Go)
└── Resume after interruption      → checkpoint_resume
```

### Session-start checklist

- [ ] `checkpoint_resume` — was I interrupted?
- [ ] `memory_search` agent-specific (top_k=8) — relevant memories
- [ ] `memory_search` `__shared__` (top_k=5) — shared context
- [ ] Concatenate into system prompt
- [ ] Proceed with task

### Session-end checklist

- [ ] Extract key learnings from the session
- [ ] `memory_add` for each atomic insight
- [ ] `memory_reflect action=update` for any preferences that changed
- [ ] `checkpoint_save` if the task is incomplete
- [ ] `memory_reflect action=forget` for any temporary / transient facts that slipped in

---

## Resources

- **GrayMatter GitHub**: <https://github.com/angelnicolasc/graymatter>
- **Go docs**: <https://pkg.go.dev/github.com/angelnicolasc/graymatter>
- **Releases**: <https://github.com/angelnicolasc/graymatter/releases>
- **Strategy / why**: [`GRAYMATTER_PLAYBOOK.md`](../GRAYMATTER_PLAYBOOK.md)
- **API stability**: [`docs/api-stability.md`](api-stability.md)
- **Benchmarks**: [`docs/benchmarks.md`](benchmarks.md)
- **Plugin protocol**: [`docs/plugin-protocol.md`](plugin-protocol.md)
- **Canonical Go integration**: [`examples/agent/main.go`](../examples/agent/main.go)

---

*Good memory makes good agents. Store conclusions, not conversations.*

_Adapted and extended from a draft by [MikeCase](https://github.com/MikeCase/graymatter-agent-patterns)._
