# Current facts by subject

The `core` MCP profile accepts two optional write intentions. Existing writes
without either field remain independent facts.

- `subject`: a stable name for one topic in the explicit namespace. Writing the
  same subject replaces its live fact in one transaction and returns the ID and
  beginning of the replaced text. A first write has zero revisions.
- `replaces`: a non-empty list of returned fact IDs to retire in the same
  transaction as the new assertion. Combine it with `subject` only for the
  explicit recovery procedure below.

Both fields are available on `memory_add`, including each object in `facts`,
and on `memory_reflect` with `action: "add"`. An explicit reflect `update`
already selects its victims with `target`; it cannot combine a second write
intention. `full` preserves its earlier input schema and rejects these fields
with profile guidance. An unsupported daemon reports an update/restart error
before dispatching the new operation.

```json
{"agent_id":"project","text":"The retention period is 11 days.","subject":"retention policy"}
```

Later, write the current value with the same subject:

```json
{"agent_id":"project","text":"The retention period is 37 days.","subject":"retention policy"}
```

The old version remains retired according to the existing retention policy.
Its citation stays attached to its namespace, fact ID and content hash. The
replacement does not inherit the previous source or quote; supply new evidence
explicitly. Pin and confidence retain the revision rules described in the
[agent manual](AGENTS.md#confidence-options).

## Names and concurrent writes

Subject equality uses Unicode NFC, lowercase and collapsed Unicode whitespace,
followed by NFC again. Names must be valid UTF-8 and at most 256 bytes before and
after normalization. An explicitly empty name is an error. Accents are not
removed: `café` and `cafe` are different subjects. CJK and fullwidth characters
are not reduced to ASCII. Similarity never decides subject equality.

The subject index is scoped to the namespace. Shared writes still require
`agent_id: "__shared__"`. Two simultaneous writes resolve the current head
inside the write transaction: each gets an explicit outcome and one live head
remains. If a reply is lost, do not retry the write automatically; inspect the
store first. An external retry is a new operation.

A legacy revision may have associated several subject names with one fact.
Reads preserve all those associations. Writing just one such subject is
ambiguous and fails visibly. Replacing that concrete ID explicitly preserves
all its existing associations; `replaces` cannot newly merge different subject
heads. If an older writer pruned a head, only exact retained replacement lineage
to one live canonical successor permits resolution. Missing, cyclic or
ambiguous history leaves that subject unresolved.

## Recover an unresolved subject

An older writer can retire a fact without leaving the exact revision history
needed to prove a successor. The subject then becomes unresolved. Searches
still return live facts, including the older writer's replacement as an
independent fact. One warning names up to three affected subjects and gives
the total. Core MCP search returns `subject_issues` with the affected namespace,
subject name and recorded `head_fact_id`. Full MCP search includes the same
warning in its structured `feedback`, and batch responses carry
`subject_issues`. `graymatter doctor` reports affected projects and counts
without modifying the store.

Writes to other subjects and facts continue. A write to the unresolved subject
needs an explicit choice of what to replace. Use `memory_add` in `core` with
the same `subject`, your new assertion, and one full ID in `replaces`.
Choose the live fact you intend to replace. If there is no live fact to replace,
use the recorded `head_fact_id` from the diagnostic instead.

```json
{"agent_id":"project","text":"The deployment window is Thursday.","subject":"deploy window","replaces":["FULL-FACT-ID"]}
```

Recovery installs the new current fact and continues the revision count in
one transaction. The old version remains retired and keeps its own evidence.
Search and maintenance leave the unresolved head in place until an explicit
recovery. Maintenance retains the evidence that the head references.

Earlier maintenance can also expire the exact evidence snapshot of a retired
fact while leaving its canonical row. When the row is inspected or exported,
it stays retired and reports that its exact citation is unavailable.

## Addressing a fact

Core search shows a short ID and the subject on the fact's line. A short ID
is an unambiguous prefix in the fact's namespace at the time of resolution.
Use `explain: true` for full IDs, revision counts and exact citation
references. A citation marker can be fetched with the displayed short ID.

`memory_reflect` accepts a target ID for `update`, `forget`, `pin` and `unpin`.
Automatic resolution first preserves the historical exact-text match,
including all eligible copies, then considers a complete ID or unique prefix.
Use optional `target_type: "id"` or `target_type: "text"` to disambiguate a
literal text that looks like an ID. Ambiguous prefixes fail; they never select
the first match. `link` retains its existing graph-node contract.

An independent write can mention similar live facts as suggestions. Suggestions
use a deterministic lexical comparison in the same namespace, show at most
three candidates and report the total. Computing them does not update access
counts, learn aliases, call an embedding provider or retire anything. Only
declared `subject`, `replaces` or explicit curation retires facts.

## Projections and retention

TUI and exports show every subject association with its revision count as
escaped data. Hooks include subject markers inside their existing budget;
literal quotes are still fetched separately. Ordinary reads do not repair
subject indexes or resolve ambiguity by writing.

Subject heads use a separate version-aware bucket so an older binary cannot
discard them when rewriting a fact. Their metadata and evidence owners count
toward the existing shared evidence quotas; historical evidence follows the
same reachability and grace-period rules, not a separate unlimited history.

The existing JSON `--include-lineage` audit export also contains stored subject
heads, including closed anchors or anchors whose canonical rows were pruned.
Each names its namespace, exact reference, revision count and state. Current
fact projections and these stored anchors are captured in one read transaction;
an anchor is not a claim that its original row is still live.

The JSON projection importer preserves text, confidence and literal citation
for independent facts. It rejects subject projections because importing them
as fresh rows would not restore their associations and historical identities.
Use complete database backup/restore when those identities must be preserved.
