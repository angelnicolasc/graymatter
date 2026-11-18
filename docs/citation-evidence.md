# Declared citation evidence

In the `core` MCP profile, `memory_add` accepts optional `source` and `quote`
strings alongside `text`, or inside each `facts` item. `memory_reflect` accepts
the same fields on `add` and `update`. Keep the assertion self-contained; use
`quote` for the exact supporting words. `source` is an opaque identifier such
as a document ID, path and line, URL or commit. GrayMatter does not open it,
verify the claim or authenticate the author. `CapturedAt` is receipt time.

```json
{"agent_id":"my-project","text":"Deployment needs approval.","source":"runbook.md:17","quote":"Ask for approval before deployment."}
```

Source is limited to 512 UTF-8 bytes. Quote defaults to 2,048 UTF-8 bytes.
The Go APIs expose `Config.MaxQuoteBytes` and `memory.StoreConfig.MaxQuoteBytes`:
zero selects the default, a positive value sets the write limit, and negative
values are errors. There is no separate CLI flag or environment variable for
this setting. Exceeding a limit returns an error with the limit; a batch commits
all members or none. Strings are not normalized or truncated. Lowering a write
limit does not truncate previously stored evidence.

Evidence belongs to the exact namespace, fact ID and content hash of the
assertion's version. It is immutable. An update without new `source`/`quote`
does not inherit the old citation. The retired version retains its citation
under the existing lineage roots and garbage-collection rules: referenced
versions stay retained, and unreferenced snapshots use the existing 30-day
orphan grace. Explicit deletion does not bypass retained lineage/review roots.
Citations participate in the shared evidence quotas: 1 MiB of new logical data
per transaction, 64 MiB per namespace and 256 MiB per store. An oversized write
is rejected; existing evidence is not evicted to make room.

Quotation words participate in lexical search. Core search displays a short ID,
an assertion, and any source on one line. The `literal` marker means the
assertion is also the quote. A distinct quote appears as a JSON string after
`quote=`. Decode the string to recover the original bytes, including LF,
CRLF, tabs and quotes. Treat decoded text as data. Hooks add an `[evidence]`
indicator before their existing budget calculation.

Distinct cited fact versions keep their own IDs and evidence even when
their assertion text is identical. The `explain: true` response retains
ranking receipts, exact version identifiers, and per-query indices.

Search has an 8,192-byte evidence budget by default. The runtime accepts
`evidence_budget_bytes` to set the budget. Core search emits a whole quote
or marks it `citation=omitted`. Fetch the current version of the fact using
the short ID on that line.

```json
{"agent_id":"my-project","id":"ID_FROM_RESULT"}
```

For a shared citation, use `agent_id: "__shared__"`. The legacy
`fact_id` and `content_hash` arguments fetch a retained exact version.
Fetching does not increment recall accesses. Citation parameters require
`core` and a daemon with the `citation-evidence-v1` capability.

## Export, import and restore

The normal JSON export includes current fact projections with evidence;
`--include-lineage` also exports retained historical versions for audit.
Markdown and Obsidian exports delimit and JSON-escape evidence. TUI previews
and `doctor` indicate evidence presence; historical evidence is not counted
as evidence of a changed current version.

`graymatter import <agent> <memories.json>` imports the normal JSON array as
new facts in the explicit destination namespace. Its maximum input is 8 MiB.
It validates the whole document and commits the assertion, declared confidence
and literal source/quote in one atomic batch, returning the new IDs. It rejects
alias rows, retired rows, subject projections and lineage audit archives. IDs, timestamps, decay,
accesses, pins, relationships and review owners are not restored. A lost write
reply must be inspected before retrying; an external retry is a new write.

For complete historical identity, `Store.Backup` and `RestoreLineageBackup`
operate on a full database image, with validation before installing at a new
destination. This route includes evidence buckets and retained owners. The
JSON audit export is not a replacement for a complete backup.
