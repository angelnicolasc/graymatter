---
title: Observability
description: The TUI dashboard, doctor audits, and bench — you can't improve what you can't see.
---

## Live dashboard

```bash
graymatter tui
```

Opens a live terminal dashboard with everything your agent memory is doing —
no extra setup required. Auto-refreshes every 5 seconds. Press `1–4` to switch
tabs, `r` to force refresh, `q` to quit.

**What you get at a glance:**

- **Facts** — total stored, distributed across agents
- **Memory cost** — KB on disk (text + embeddings), not tokens
- **Recalls** — cumulative access count across all sessions
- **Health** — percentage of facts above relevance threshold (weight > 0.5)
- **Token cost (30d)** — real spend breakdown by model, with cache hit rate
- **Agent activity** — facts vs recalls per agent, side by side
- **Weight distribution** — how consolidated your memory is over time
- **Activity timeline** — facts created per day, last 30 days

If another process holds the write lock, open the TUI in read-only mode
explicitly: `graymatter tui --read-only`.

## Audit an instruction file

```bash
graymatter doctor --audit [path]
```

Measures tokens, duplicates, staleness, and marker conflicts in any
instruction file (CLAUDE.md, AGENTS.md, …). Free auditor, no LLM required.

## Graph analytics

```bash
graymatter doctor --graph
```

Hubs by degree, articulation points, orphans, and a declared connectivity
ratio for the knowledge graph.

## Status and audit of published numbers

```bash
graymatter status    # facts, recalls, KG state, injection estimate
graymatter bench     # audit the published benchmark figures from the binary
```

Every figure on the [benchmarks page](/reference/benchmarks/) is
machine-checked against a live run in CI.
