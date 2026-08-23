# 003 — The knowledge graph has a write path, but nothing populates it automatically

**Status:** Accepted, partial · **Date:** 2026-08-22

## Context

GrayMatter ships a knowledge graph: a schema, bbolt-backed storage, entity
extraction, an Obsidian export, and a TUI view. It is also the feature most
likely to be misdescribed, including by this project's own README, which said
until v0.10.0 that *nodes have no write path in shipped builds*.

That is not what the code does, and the imprecision cuts both ways: it
undersells what works and hides what does not.

## What is actually true

Verified against the tree, file by file:

**The write path exists and runs in shipped builds.**

- `memory_reflect` with `action=link` creates edges: `handlers.go` → the
  backend's `KGLink`.
- In daemon mode the daemon opens a real `kg.Graph` and `kg.GraphAdapter`
  (`internal/daemon/daemon.go`) and serves `KGLink` and `KGUpsert` over RPC
  (`internal/daemon/host.go`).
- With `--no-daemon` the direct store does the same in-process
  (`store_handle.go`).

So an agent *can* write to the graph, and the graph is queryable and
exportable. What it cannot do is populate itself.

**Automatic population is implemented and never activated.** `Store.SetKG`
wires a graph and an entity extractor into the memory store. Two features
depend on those fields being non-nil:

- `Consolidate` step 4 — entity extraction over surviving facts, upserting a
  node per entity.
- `Recall` — enrichment of results with graph neighbours of the top-ranked
  fact.

Both are written, both are tested, and neither has ever run outside a test,
because **`SetKG` is not called anywhere in the shipped binaries.** The fields
stay nil and both blocks are skipped in silence.

The accurate statement is therefore: *entity extraction and graph enrichment
are implemented but not wired; the graph is populated only by explicit agent
action.*

## Decision

Leave the automatic population disconnected for now, and say so precisely.

Wiring `SetKG` is a two-line change, which is exactly why it has not been
done: the cost is not in the wiring, it is in what the wiring turns on.
Extraction would then run over every surviving fact on every consolidation
cycle, and `Recall` would start appending graph neighbours to results — facts
the ranking never selected, on every recall, with no budget and no relevance
check. Turning that on without a way to measure retrieval quality means
shipping a change to what every agent reads and having no way to tell whether
it made things better or worse. Nothing in the tree measures relevance today
(`docs/benchmarks.md`).

The extractor is also heuristic — capitalised-token matching, not entity
resolution. On prose it produces nodes for names, and on the rest it produces
nodes for the first word of a sentence.

## Consequences

- The TUI graph view is empty for anyone who has not used
  `memory_reflect action=link`, which reads as broken and is technically
  correct behaviour. This is the most user-visible cost.
- The Obsidian export works and has almost nothing to export.
- Dead-but-tested code sits in `Consolidate` and `Recall`. It is covered, so
  it does not rot, but coverage of a path production never takes is a weaker
  guarantee than it looks.
- The honest description is longer than "not implemented", which is why the
  short wrong version kept getting written. README and roadmap were corrected
  alongside this record.

## Reversal condition

Wire `SetKG` in the daemon and the direct store when **both** hold:

1. Retrieval quality is measurable — a benchmark that scores whether recalled
   facts answer the query, so enrichment can be shown to help rather than
   assumed to.
2. Enrichment is bounded: a cap on appended neighbours and a relevance floor
   they must clear, so graph output cannot crowd out ranked results.

Extraction quality gates it separately: on a hand-labelled sample of 100
facts, precision below **0.7** means the graph fills with noise and wiring it
makes recall worse. Measure before wiring, not after.

## Alternatives rejected

- **Wire it now.** Two lines, and it silently changes what every agent reads,
  with no measurement to catch a regression.
- **Delete the graph.** Removes the dead paths and a working agent-driven
  feature with them, and `#24` describes something people want.
- **Leave the README as it was.** The cheapest option and the most expensive
  one. A project whose main asset is being trusted about its own numbers
  cannot afford a description of its own features that is wrong in both
  directions.
