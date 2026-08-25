# Audit: export / Obsidian vault / status reporting hardening

**Date:** 2026-08-24 · **Scope:** `graymatter export` (all formats), the
`--include-graph` Obsidian pipeline, `kg_linkpass`, and the `status` command's
human reporting. Triggered by three user-visible failures found while producing
marketing captures against a real vault (see verification section).

Method: full read of `cmd/graymatter/internal/export/*`, `internal/kg/graph.go`
(ExportObsidian path), `kg_linkpass.go`, `cmd_export.go`, `cmd_status.go`, and
`internal/harness/token_usage.go`; every finding below is backed by a failing
behaviour, not by style preference.

## Findings

### H1 · BLOCKER — `status` prints impossible cache-hit percentages

`cmd_status.go` divides `CacheRead` by `Input + Output`:

```go
inOut := tok.Input + tok.Output
... pct(tok.CacheRead, inOut) ...
```

Cache reads are part of the **input** side. On any cache-heavy workload the
denominator is far smaller than the numerator, so the line prints values like
`cache-read 430%`. The JSON path is unaffected — `harness.TokenUsageSummary`
already computes `CacheHitRate = CacheRead / (Input + CacheRead)` correctly,
which is why the TUI dashboard (84%) and `status` (430%) disagreed for the
same store.

**Fix:** denominator becomes `Input + CacheRead`, matching the harness.
**Test:** unit test on `renderStatus` with a ledger where `CacheRead` is 9x
`Input`; assert the line prints `90%`, not `900%`.

### H2 · BLOCKER — knowledge-graph `## Related` links never resolve in Obsidian

`kg/graph.go` names each entity note after its **label**:

```go
fname := sanitizeFilename(n.Label) + ".md"
```

but links related entities by their **raw node ID**:

```go
line := fmt.Sprintf("- [[%s]] (%s)", e.To, e.Relation)
```

`[[01M0RTKV…]]` matches no note, so every entity-to-entity edge in the graph
is a broken link — and since Obsidian's graph view draws resolved links only,
**the KG edges are invisible in the graph view of every real export**. The
entities MOC already links correctly (`[[sanitizeFilename(label)|label]]`),
which makes the inconsistency internal to the same function.

**Fix:** build an ID→label map from the exported nodes and emit
`[[sanitizeFilename(label)|Label]]`; fall back to the raw ID if a dangling
edge references an unknown node.
**Test:** export a two-node graph whose labels need sanitizing; assert the
`## Related` target file exists on disk and the link uses the label form.

### H3 · BLOCKER — fact notes are named by raw ULID

`export/obsidian.go` writes `<agent>/<ULID>.md` and the index links
`[[agent/<ULID>|preview]]`; `kg_linkpass.go` re-derives the same path
independently. Consequences: Obsidian's graph view and quick-switcher show
`01M0RTKV1182XM89SX4BQF4NCM` instead of the fact's text, and the fact layer
of the graph is unreadable noise.

**Fix:** derive note names from the fact text (sanitized, rune-safe length
clamp, deterministic `-<short-id>` suffix only on collision or empty slug),
computed by **one** function (`export.BuildFactNoteNames`) shared by the
exporter, the index, and the link pass — eliminating the three-implementation
drift that produced H2/H3 in the first place.
**Tests:** names are readable; every index wikilink resolves to a file on
disk; two facts with identical text get distinct files.

### M1 · Moderate — index previews can emit invalid UTF-8

`writeObsidianIndex` truncates previews by byte slicing (`preview[:77]`),
which can split a multi-byte rune and write mojibake for any non-ASCII fact
text. **Fix:** rune-aware truncation. **Test:** fact with accented text
straddling the boundary; assert the index is valid UTF-8 with no replacement
characters.

### M2 · Moderate — the Obsidian index is not deterministic

`writeObsidianIndex` ranges over a `map[string][]Fact`, so section order
changes between runs on the same store. This contradicts the project's own
determinism bar (`context-sync` guarantees "same store state, same block
bytes"). **Fix:** sort agent sections alphabetically. **Test:** two exports
of the same store produce byte-identical `_index.md`.

### L1 · Low — `kg.sanitizeFilename` admits characters Obsidian and Windows reject

The replacer covers `/\:*?"<>|` and spaces but not `#^[]` (Obsidian link
syntax), and does not trim trailing dots/spaces (illegal on Windows). Labels
differing only in case also collide on case-insensitive filesystems.
**Fix:** extend the replacer, trim trailing `. ` characters. Case-collision
dedup is out of scope here (entity labels colliding case-insensitively are
renamed by the user at the source); noted for a future pass.

### L2 · Low — filename logic lived in three places

`export/obsidian.go`, `export` index, and `kg_linkpass.go` each derived note
paths independently; kg_linkpass also duplicated kg's replacer as
`entityNoteFilename`. This drift is the root cause that let H2 and H3 ship.
**Fix:** `kg.SanitizeFilename` becomes exported and is the single entity-name
authority; `export.BuildFactNoteNames` is the single fact-name authority;
`kg_linkpass.go` consumes both and owns no naming logic.

## Verified non-issues

- The JSON `status` payload (uses harness `CacheHitRate`) — correct.
- `graph-canvas.json` — canvas edges reference node IDs and canvas nodes are
  keyed by the same IDs; internally consistent.
- `MarkdownExporter` — per-agent files, no cross-references, unaffected.
- `writeEntitiesMOC` — already links by sanitized label.

## Adversary note

H1–H3 were all caught outside of CI, on a real vault, by a human looking at
the output. The export path had tests for file *existence* but none for link
*resolution* — the property users actually consume. The tests added here pin
resolution (every emitted wikilink must name a file that exists), naming
(readable, collision-free), determinism (byte-identical re-exports), and the
cache-hit arithmetic, so this class of regression cannot silently return.
