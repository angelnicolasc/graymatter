---
title: Knowledge graph
description: Typed entities, co-mention edges, and Obsidian export — the graph builds itself from ordinary use.
---

Your agent doesn't just remember facts — it builds a map of how they connect.

```bash
graymatter daemon run --kg    # that's it
```

Every consolidation cycle extracts typed entities (person, organization,
project) and links the ones that appear together. No manual tagging. No
configuration. The graph builds itself from ordinary use.

Every edge carries **receipts**: the fact IDs that produced it.

## Persist activation

```bash
graymatter init --kg
```

Persists KG activation via a sentinel file so future daemons build the graph
without the flag. See [ADR-009](/reference/decisions/009-kg-sentinel-activation/).

## Manual links

`memory_reflect` with `action=link` connects two entities explicitly:

```jsonc
{ "tool": "memory_reflect", "args": {
    "action": "link",
    "agent":  "backend-agent",
    "text":   "depends_on",
    "target": "user-database"
}}
```

Automatic population ships gated and measured — the design history is in
[ADR-003](/reference/decisions/003-knowledge-graph-autopopulation/) and
[ADR-008](/reference/decisions/008-knowledge-graph-wiring/).

## Inspect the graph

```bash
graymatter doctor --graph
```

Hubs by degree, articulation points, orphans, and a declared connectivity
ratio — printed or emitted as JSON.

## Export to Obsidian

```bash
graymatter export --format obsidian --include-graph
```

Entities become notes, connections become wikilinks, and the whole graph
renders natively in Obsidian's graph view.
