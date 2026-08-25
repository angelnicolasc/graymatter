# Changelog

All notable changes to this project are documented in this file.  
Format: [Keep a Changelog](https://keepachangelog.com/en/1.0.0/)  
Versioning: [Semantic Versioning](https://semver.org/spec/v2.0.0.html)

---

## [Unreleased]

---

## [0.14.0] - 2026-08-25

### What users get

- **Pinned facts: what you declare permanent stays.** `graymatter pin <agent> <text>` and
  `memory_reflect action=pin|unpin` exempt a fact from decay, pruning *and* the summarisation
  batch (ADR-010) — a project that goes dormant no longer collects standing obligations or
  architecture decisions. The exemption is visible everywhere: star in the TUI, `pinned` count
  in `status`, `pinned: true` in the Obsidian export, reported by `doctor`.
- **Consolidation runs fully local with Ollama — no account, no API key.**
  `ConsolidateLLM="ollama"` used to be accepted by config and rejected at runtime; it now works
  end to end against `/api/generate` (`GRAYMATTER_OLLAMA_CONSOLIDATE_MODEL`, default
  `llama3.2`). The model proposes; the application is deterministic (ADR-011): an invalid or
  unreachable response degrades to decay+prune-only behaviour, never to data loss.
- **`graymatter doctor --health` audits your store the way `bench` audits published numbers.**
  Four deterministic rules — supersede loops, dumping bursts, critical-looking facts near prune
  (with pin suggestions), duplicate density. Same store, byte-identical JSON, every run.
- **A living documentation site** ships at graymatter.nickcerutti.workers.dev (Starlight),
  deployed by CI instead of hand-published.

### Changed

- **Consolidation keeps receipts instead of deleting.** The consumed batch becomes tombstones
  pointing at the summary fact (ADR-007 finally holds on every path): they leave recall
  immediately, stay listed/exportable/auditable, and ordinary decay collects them. Code that
  iterates raw `List` output will now observe tombstones where deletes used to be silent;
  every shipped consumer already filters superseded facts.
- **The knowledge graph got an extraction floor.** Stopword entities (`The`), URL/date nodes
  and their meaningless co-mention cliques are gone; role matching uses word boundaries; the
  ambiguous fallback type is `concept`, not `fact`; institution suffixes expanded. All five
  noise classes are pinned by a golden corpus gate that runs in CI
  (`internal/kg/extractor_golden_test.go`). Graph artifacts change shape accordingly.
- **`ErrConsolidateLLMUnsupported` is never returned.** Ollama consolidation is implemented,
  so the sentinel is retired (kept for `errors.Is` compatibility). An unreachable Ollama now
  surfaces as the underlying transport error via `OnConsolidateError`.
- **Extraction is incremental.** A text-signature watermark makes consolidation's graph pass
  O(changes) per cycle instead of re-extracting every fact forever; retired facts never reach
  the graph.

### Security

- **Windows token files are now actually private.** A `0600` mode maps to nothing there, so
  the daemon's discovery file and the HTTP bearer-token file inherited whatever their
  directory granted — every local user in team-shared trees. Both now receive a protected
  owner-only DACL (current user + SYSTEM + Administrators) at write time, and failing to
  secure aborts startup/minting rather than running with an exposed credential.
  `docs/threat-model.md` documents the mechanism and the residual caveat (`gray.db` still
  follows directory ACLs).

### Fixed

- `status` printed cache-read >100% by dividing by the wrong denominator.
- Obsidian export: fact notes named by ULID are now readable titles; previews truncate by rune
  so accented text stays valid UTF-8; the index is deterministic.
- KG export `## Related` links resolve to entity note filenames instead of raw IDs, making the
  graph navigable in Obsidian.
- TUI: the graph tab no longer overflows its viewport; done sessions render as done, not
  pending; the detail pane follows the highlighted node.

### Compatibility notes

- `memory.ConsolidateConfig` gained two methods (`GetOllamaURL`,
  `GetOllamaConsolidateModel`) to carry the local summariser settings. Callers using
  `graymatter.Config` get them automatically. Hand-rolled implementers must add two one-line
  getters — the compiler names them. This deviates from the deprecation-notice rule in
  [api-stability.md](docs/api-stability.md); recorded here deliberately instead of silently.
- `Fact.Pinned` / `Fact.PinnedAt` follow the v0.10.0 pattern: zero values reproduce previous
  behaviour exactly, and stores written before this release load as unpinned.
- `status --json` adds `pinned`, `consolidations` and `facts_consumed` (additive).
- A new `kg_extracted` bucket records the extraction watermark; no migration is needed — old
  stores simply behave as fully unextracted for one cycle.

---

## [0.13.1] - 2026-08-24

### Fixed

- **v0.13.0 has no downloadable binaries; this release does.** Its tag was
  published, so `go install` and the Go module proxy serve v0.13.0 normally,
  but the release job failed before uploading anything: GoReleaser rejected a
  malformed `brews` block, and the retry then died on `tag already exists`
  because the CLI submodule tag from the first attempt was already public.
  Every archive link for v0.13.0 returns 404. The code here is v0.13.0's plus
  the pipeline fixes below.
- **A failed release can now be retried.** Publishing the `cmd/graymatter/`
  submodule tag is idempotent: once the Go proxy has served that version the
  tag is immutable, so a re-run accepts the published one instead of trying to
  recreate it.
- **Homebrew, Scoop and Nix publishing is wired to a credential that can
  reach its repositories.** The three taps live in separate repos, which the
  job's default `GITHUB_TOKEN` cannot write to; each publisher now takes a
  dedicated token, and the release refuses to start without it rather than
  failing after the binaries are already up.

---

## [0.13.0] - 2026-08-24

### What users get

This release attacks the first five minutes: the gap between "installed" and
"the agent actually remembers something".

- **Agents get briefed by the protocol, not just by markdown.** The MCP
  server now sends session instructions in the initialize handshake, so
  clients that never load CLAUDE.md still call the memory tools — the
  failure mode behind issues #3 and #14 loses its biggest blind spot.
- **Self-edits stopped asking for permission.** `memory_reflect` no longer
  advertises itself as destructive; hosts that gate destructive tools were
  prompting on every preference an agent tried to save. The real guardrails
  moved into the handler where they are testable (exact-match forget,
  tombstones, audit trail).
- **`graymatter status` answers "is this thing working?"** in one screen:
  facts and recalls per agent, KG state with its enable command when off,
  the 30-day token ledger (labelled for what it measures), and what a recall
  costs today against your own store.
- **`graymatter bench` makes the published numbers yours.** The suites that
  gate this README run from the installed binary — no Go toolchain, no clone
  — and `--store` measures your actual memory instead of the synthetic
  corpus.
- **Knowledge-graph population is one flag away.** `graymatter init --kg`
  persists the choice so daemons spawned by MCP clients honour it too;
  doctor reports the state either way ([ADR-009](docs/decisions/009-kg-sentinel-activation.md)).
- **Setup says one restart, once.** The Windows PATH note folds into the
  restart step it belongs to, and doctor's restart hint survives a CLI
  `remember` having created the database early.
- **Install via Homebrew or Scoop.** goreleaser generates a formula and a
  manifest on every release — `brew install angelnicolasc/tap/graymatter`
  or `scoop install graymatter`.
- **Two more MCP clients auto-wired.** Windsurf (`.windsurf/mcp.json`) and
  VS Code Copilot Agent (`.vscode/mcp.json`) join the existing five; Pi
  reads `.mcp.json` natively ([#15](https://github.com/angelnicolasc/graymatter/issues/15),
  [#18](https://github.com/angelnicolasc/graymatter/issues/18)).
- **mcp-go upgraded from v0.32.0 to v0.58.0.** The server negotiates MCP
  2025-11-25 with every client, inheriting twenty-six releases of security
  patches and protocol improvements from the library.

---

## [0.12.1] - 2026-08-23

### Added

- **`graymatter doctor --graph`** — knowledge-graph analytics in-binary:
  hubs by degree, articulation points and bridges (Tarjan), orphans, and a
  declared connectivity ratio. Human + JSON output.
- **TUI Graph tab v2** — stats header (entities / edges / orphans) above the
  node list; honest empty-state pointing at `daemon run --kg`.
- **`Fact.Confidence` (verified|inferred|unverified)** — epistemic metadata
  declared at write time via `Store.PutConfident`; surfaced in the Obsidian
  export frontmatter and the TUI fact detail. Display-only: never affects
  ranking, decay or pruning.
- **Edge provenance receipts** — consolidation attributes every co-mention
  edge to the fact that produced it (`sources`, capped at ten); Obsidian
  entity notes print receipt counts.
- **`benchmarks/fixtures-v2`** — recurring-entity corpus for multi-hop
  measurement; enriched recall answers 67% vs 0% baseline there (RESULTS.md).

---

## [0.12.0] - 2026-08-23

### Added

- **Knowledge-graph auto-population ships gated (`daemon run --kg` / `GRAYMATTER_KG=1`)** —
  consolidation now consumes a typed extractor capability that preserves each
  entity's label and classification, and persists co-mention edges between
  them, so the graph fills itself from ordinary use (issue #24). Recall
  enrichment is budgeted: at most three neighbour labels appended after the
  ranked facts, tombstones respected on every path — documented as an explicit
  exception to exactly-topK in `docs/api-stability.md`. Gated by measured
  results per [ADR-008](docs/decisions/008-knowledge-graph-wiring.md),
  amending ADR-003: extraction precision 0.946 on a 105-fact labeled corpus,
  and entity-bridge enrichment answering 67% of multi-hop queries that plain
  ranking answers at 0%.
- **`graymatter export --include-graph`** — the Obsidian export now writes the
  knowledge graph alongside facts: one entity note per node (frontmatter with
  type, first/last seen, weight) plus an Obsidian canvas. Works through the
  daemon too; only the destination path crosses the wire. Requires
  `--format obsidian`.
- **`Graph.Link` never leaves dangling edges** — endpoints that do not exist
  are auto-upserted as placeholder nodes (`EntityType: "unknown"`) inside the
  same transaction, so every edge is traversable no matter when the agent
  links relative to extraction.
- **Wiring contract tests** pin what shipped builds do and do not do with the
  knowledge graph: defaults never auto-wire extraction, explicit `SetKG`
  drives node upserts during consolidation, and today's engine has no path
  that creates edges (documented before the wiring round changes it).

---

## [0.11.1] - 2026-08-23

### Added

- **`graymatter doctor --audit [path]`** — free auditor for instruction
  documents, no store or adoption required. Reports approx token cost per
  prompt (tokenizer declared in every output), near-duplicate paragraphs
  (word-5-shingle Jaccard ≥ 0.8), staleness buckets from git blame
  (≤30d / 31–90d / >90d / uncommitted), size alerts at declared thresholds,
  and structural conflicts in managed blocks only: unterminated or duplicate
  graymatter regions, nesting across kinds, and context-block hashes that no
  longer verify. Semantic contradiction detection is out of scope by design.
  Human and JSON output; exit code 1 only on failure-level findings.

### Fixed

- **`doctor <path>` without `--audit` is now a loud error** instead of
  silently auditing the working directory while ignoring the path. A
  positional path only means something under `--audit`; input that changes
  nothing and says so to nobody is the failure mode this project fixes
  everywhere else.
- **`doctor --audit` no longer reports marker syntax quoted inside fenced
  code blocks.** A document that teaches or documents the managed-block
  syntax inside ``` fences — this repository's own docs do it — produced
  failure-level findings and exit code 1 for markers that are quoted text,
  not active regions. Fenced regions are now blanked before marker scanning;
  duplication, size and staleness still read the full file.
- **`doctor --audit` staleness states why it cannot measure instead of
  reporting false freshness.** When git is missing, the path is outside any
  repository, or the file is not tracked, blame returns a reason with no
  error — and the report printed `available: true` with every bucket at zero
  and a median of 0 days: unmeasurable content presented as fresh. The
  reason now reaches the report verbatim (`unavailable (…)`), as its own
  documentation always claimed.
- **Fact text can no longer forge instructions markers into projected
  blocks.** A stored fact quoting the instructions briefing verbatim was
  sanitized against context-kind markers only; the projected body carried
  live `graymatter:instructions:` markers, which made `doctor --audit` see a
  nested instructions region that does not exist (failure finding, exit 1)
  on a structurally healthy file. Sanitization neutralizes both marker
  families.
- **`context-sync` idempotence, backup and manual-edit detection are pinned
  by tests** alongside the audit's duplication threshold (near-boundary
  pairs, not just identical text), staleness buckets with mixed commit ages
  under a fixed clock, splice behaviour when an orphaned begin marker
  precedes a real block, and the `doctor --audit` exit-code contract.

---

## [0.11.0] - 2026-08-23

### Added

- **`graymatter context-sync` (opt-in)** — projects the highest-weight live
  facts into a managed block inside CLAUDE.md / AGENTS.md under an explicit
  token budget (default 512), regenerated on every run. Safety model:
  content outside the markers is never touched; every rewrite keeps the
  previous file as `<file>.bak`; hand edits are detected against the SHA-256
  recorded in the block header and reported by `doctor` and by
  `context-sync --check` — warned, then overwritten, never silently merged.
  The projection is deterministic given a store state, tombstoned facts are
  excluded immediately, and `FuzzRenderBlock` (the fourth fuzz target) holds
  fact text to the same structure rules. The engine is untouched: `Recall`
  never reads the block.

### Fixed

- **Recall breaks score ties deterministically (oldest first); previously
  arbitrary.** The three signal rankings were sorted by score alone, and
  `sort.Slice` is not stable, so facts that scored equally received arbitrary
  ranks — and those ranks are what the RRF fusion reads. Six facts written in
  the same instant produced all six rotations of the result, both across
  repeated calls on one store and across freshly built stores. The order is now
  total: descending fused score, then `CreatedAt` ascending, then fact ID.
  Scores are unchanged; only the resolution of ties. The contract is recorded
  in [docs/api-stability.md](docs/api-stability.md).

---

## [0.10.0] - 2026-08-23

A release about claims. Every number this project published, and every
behaviour it described, checked against what the code does — and where they
disagreed, the code fixed or the claim withdrawn. Three of the disagreements
were bugs, and one had been shipping since the feature existed.

Nothing here requires action from a caller. Every addition is a new field whose
zero value reproduces v0.9.0 exactly, enforced by
`TestRankingDefaults_MatchV09Behaviour`. One default changes, listed below.

### Fixed

**Forgetting a fact did not stop it being recalled.** `memory_reflect` with
`action="forget"` or `"update"` set the fact's weight to 0 and answered *"Fact
suppressed for agent"*. `Recall` never reads weight — it ranks on vector,
keyword and recency — so the fact came back on the very next search. An agent
that had just corrected itself received both versions:

```
Found 2 relevant memories for agent "billing-agent":
1. Billing runs through Polar
2. Billing runs through Lemon Squeezy
```

with nothing to indicate which was current. Nothing cleared it up until a
consolidation cycle happened to prune the zero-weight fact, and consolidation
only fires past `ConsolidateThreshold` facts — so on a smaller store, never.

`Fact` gains `SupersededBy`. Non-empty means retired, and `Recall` drops those
before scoring, so a tombstoned fact cannot even displace a live one from the
top-k. The value is the replacement fact's ID for `update`, or
`SupersededByAgent` for `forget`, so a correction can be followed rather than
merely noticed. Nothing is deleted: `List`, `export` and the TUI still show
retired facts, and pruning by decay remains the only thing that removes
anything. Precedence between tombstone, decay and pruning is now stated and
tested — see [ADR-007](docs/decisions/007-supersede-tombstones.md).

`update` also stopped retiring the old fact before storing the replacement. The
previous order zeroed the weight first, so a failing `Remember` left an agent
with a retired fact and nothing in its place.

**Decay ran on a half-life of one consolidation cycle, not 30 days.**
`Consolidate` computed `weight *= exp(-lambda * hoursSinceAccessed)` using the
full elapsed time on every run — nothing recorded that a fact had already been
decayed, so each cycle re-applied the whole period. A fact one half-life stale,
across five cycles in the same millisecond: 0.5, 0.25, 0.125, 0.0625, 0.031.
With `AsyncConsolidate` on, consolidation fires after `Remember`, so a busy
agent could take a month-old fact below the 0.01 prune floor in minutes. Every
statement this project made about 30-day decay described a model the code did
not implement.

Weight is now recomputed from staleness rather than multiplied into:
`weight = min(weight, exp(-lambda * hoursSinceAccessed))`. Idempotent, so
consolidating more often no longer forgets faster. The `min` keeps decay from
handing weight back, which also stops a zeroed tombstone being resurrected by
its own recent access time.

**An unimplemented consolidation LLM failed silently.** Ollama is a supported
embedding backend, so `ConsolidateLLM = "ollama"` looks reasonable and config
accepts it — but summarisation through Ollama is not implemented, and the code
returned an empty summary and a nil error. Such a store ran decay and pruning
forever, never summarised, and gave no indication why. It now returns
`ErrConsolidateLLMUnsupported` through `OnConsolidateError`, as do
summarisation errors in general, which were being discarded at that call site.

### Changed

- **`GET /recall` returns 8 facts by default, not 5.** The REST server had its
  own constant while the library, CLI, MCP tools and TUI all used
  `DefaultConfig().TopK`. Same store, same query, a different amount of context
  depending on which door you came through. `?k=5` restores the old count. The
  test reads the library default rather than repeating a number, so the two
  cannot drift apart again.
- The MCP server announces version `0.10.0`.

### Added

- **`StoreConfig.SignalWeights`** — how much vector similarity, keyword
  relevance and recency each contribute to the ranking. A pointer: `nil` means
  `DefaultSignalWeights()` (1.0, 1.0, 0.5 — the values previously hardcoded),
  and the indirection exists so the zero value cannot be mistaken for "all
  signals off". Also on the root `Config`.

  This makes available an honest version of a claim that was not previously
  available at all: with all weight on recency, retrieval degenerates into
  "return the K most recent facts", which is a sliding window. A test runs it
  over a corpus where the relevant facts are the old ones and shows the window
  returning none of them. Before this, no configuration of GrayMatter ranked
  that way, so the claim described a system that did not exist.
- **`StoreConfig.MinRelevance`** — drops results below a fraction of the best
  score in the same result set. Default 0, meaning no cut and exactly `topK`
  returned, as before. The threshold is relative because RRF scores depend on
  how many facts were ranked, so an absolute cutoff would change meaning as a
  store grew.
- **`Fact.SupersededBy`, `Fact.IsSuperseded()`, `SupersededByAgent`** — see
  Fixed. `omitempty` and additive: stores written by earlier versions load
  unchanged, checked against a literal v0.9.0 JSON fact.
- **[`docs/decisions/`](docs/decisions/README.md)** — seven architecture
  decision records, each with a measurable condition for reversing it. Why
  decay is 30 days and which two classes of fact that model gets wrong; why
  there is a daemon; why not multi-tenant; the embedding chain; the signal
  weights; the tombstones; and the true state of the knowledge graph.

### Documentation

**`docs/benchmarks.md` published a table nothing produced.** It claimed
~40,000 tokens of full injection against ~1,200 recalled at 100 sessions;
`go run ./benchmarks/token_count` measures ~6,959 against ~666. The
100-session row was 475% off. It also published a "Relevance@8 vs full
context" score, and no code in this repository measures relevance at all.

The README was correct throughout, which is the uncomfortable part: the v0.9.0
sweep grepped for "97" and this page never said 97, so a five-fold error
survived a hygiene pass whose whole purpose was removing indefensible numbers.

The check is now mechanical. `benchmarks/token_count/main_test.go` parses the
tables out of README.md and docs/benchmarks.md and compares every cell against
a live benchmark run — 2% tolerance on token counts, none at all on the
reduction percentage — and rejects any relevance or precision figure in the
benchmark docs until something computes one.

The rewritten page states what the benchmark does not measure, which is more
than what it does: it never checks whether the 8 recalled facts are the right
8, so a system returning 8 at random scores the same 90%; and full-history
injection is the weakest possible baseline, since a sliding window of the last
8 observations costs roughly 560 tokens against our ~550–670. **Against the
baseline production actually uses, this benchmark shows no token win to
claim.**

**The README described the knowledge graph wrongly in both directions.** It
said nodes have no write path in shipped builds. They do — `memory_reflect
action=link` creates edges, the daemon opens a real graph and serves
`KGUpsert`/`KGLink`, and the direct store does the same. What is missing is
*automatic* population: `Store.SetKG` is never called outside tests, so entity
extraction during consolidation and graph enrichment during recall are
implemented, tested, and have never run. Corrected in the README, the roadmap
and [ADR-003](docs/decisions/003-knowledge-graph-autopopulation.md) together.

**`docs/AGENTS.md` carried four claims that had stopped being true.** That
`update` leaves the old fact for consolidation to prune; that RRF has "no
tunable percentage weights to fiddle with"; that `link` requires a
`SetKGLinker` that no longer exists, returning an error string that no longer
exists, citing a line that now holds something else; and that an unreferenced
fact is pruned in ~60 days, when the arithmetic gives 6.64 half-lives — 199
days. Also a link to `GRAYMATTER_PLAYBOOK.md`, a file with no history in this
repository.

Smaller: the benchmark's header described a corpus of 5 observations per
session where the loop stores 1, and `ConsolidateThreshold`'s two different
comparisons (`>=` to trigger a cycle, `>` to summarise) are now explained at
both sites rather than left looking like an off-by-one.

---

## [0.9.0] - 2026-08-22

### Security

A security audit against v0.8.0 raised 17 findings; all 17 were re-verified
against the tree and remediated, each with a regression test that fails without
its fix. [`docs/threat-model.md`](docs/threat-model.md) is new and states both
what GrayMatter defends and what it does not.

Four defaults change. Everything else here is invisible unless you were relying
on the hole.

| Before | Now | Migration |
|---|---|---|
| `graymatter server --addr :8080` (every interface) | `--addr 127.0.0.1:8080` | Pass `--addr :8080` explicitly; startup warns |
| REST and MCP-HTTP served anyone | Bearer token on every route but `/healthz` | Send the header, or `--no-auth` (loopback only) |
| `DELETE /forget` deleted the closest match | Needs `"confirm": true`, or use `DELETE /forget/{id}` | Add the field, or switch to the exact form |
| Plugin manifests had no checksum | `sha256` is required and verified | Add the digest; a wrong one prints the right one |

**The two network listeners served the whole store to anyone who could reach
the port.** `graymatter server` bound every interface by default — `:8080` is
not localhost — and checked no credential on any route: read, write and delete
on every agent's memory, from the LAN, unauthenticated. `graymatter mcp serve
--http` was the same hole over MCP, where the tool surface includes
`memory_add` and `memory_reflect`; the `Mcp-Session-Id` looked like a barrier
but the server hands one to every caller during `initialize`.

Both now bind `127.0.0.1` and require `Authorization: Bearer <token>`, compared
in constant time. The token is 256 bits (the same `rpc.GenerateToken` the
daemon uses), generated on first run and stored in
`<data-dir>/graymatter.http-token`; `--token` and `GRAYMATTER_HTTP_TOKEN`
override it. `/healthz` stays open so liveness probes keep working. `/metrics`
does not — it lists every agent ID the server has seen. `--no-auth` restores
the old behaviour but is refused on any address that is not loopback: no
credential plus reachable from the network is the combination that made this
critical.

**Two agents writing at once could kill the daemon.** `chromemVectorStore` kept
its collections in a map with no mutex, while `Store.Put` reaches it outside
the store lock and the daemon serves every RPC on its own goroutine. The result
was `fatal error: concurrent map read and map write` — unrecoverable, taking
every client's access to memory with it. The map is now behind a mutex, and the
lookup-or-create returns the handle so `AddDocument` and `Query` no longer
re-read it either.

**`plugin remove ../../../anything` deleted that directory.** `filepath.Join`
cleans a path; it does not contain it. `Install` had the same defect through
`manifest.Name`, which is chosen by whoever published the plugin. Both ends now
validate the name against a whitelist and verify the resolved path really is
under the plugins directory, before any filesystem call.

**Installing a plugin no longer takes the manifest's word for anything.**
`sha256` is a required field, verified against the executable before the
install is recorded and again before every call. The executable is copied into
`<data-dir>/plugins/<name>/bin/` and the stored manifest points there, so what
runs later is the reviewed bytes rather than whatever sits at an external path
by then. Manifests are fetched over HTTPS only (`--insecure` for local
testing), and `plugin install` shows name, version, tools and digest and asks
first — `--yes` for scripts, and EOF on stdin is a refusal rather than assumed
consent.

**Recalled memory reaches the model as data, not as more system prompt.**
`graymatter run` used to concatenate facts under a bare `## Memory` heading, at
the same authority as the operator's own instructions — so anything that could
write a fact could plant instructions that survive restarts. Facts now go
inside a `<memory>` fence with an explicit note that the contents carry no
authority, flattened to one line each with the delimiters neutralised so a
stored fact cannot close the block and continue as prompt. Framing is
mitigation, not a guarantee; the durable control is who can write a fact, which
is what the authentication above is for.

**`/metrics` was an unbounded allocation and a target list.** Keys came from the
request path, the request method and the agent ID, and `expvar` entries are
permanent. Route and method keys now come from fixed sets and agent IDs are
capped at 1000 distinct buckets, with the rest folded into `other`.

**The REST surface is harder to abuse in small ways.** Request bodies are capped
at 1 MiB (413 past that). Every response carries `Cache-Control: no-store` and
`X-Content-Type-Options: nosniff`. Internal failures answer `internal error` and
log the detail instead of returning `err.Error()`, which carried absolute
filesystem paths, PIDs and daemon state to whoever could reach the port;
validation errors stay detailed, because those describe the caller's own input.

**CI and release stopped trusting mutable references.** Every action is pinned
by commit SHA — `@v4` is whatever commit its owner last moved that tag to, and
the release job runs with a token that can publish binaries people install with
`sudo mv /usr/local/bin`. goreleaser is pinned to `v2.17.1` rather than
`latest`, and both workflows default to `permissions: contents: read`.

**Builds move to Go 1.26.7.** `govulncheck` reported 14 standard-library
advisories reachable from this code on go1.26.1; release builds were on 1.22,
worse still. All are fixed at 1.26.6 or earlier. The test matrix keeps 1.23 —
the minimum `go.mod` declares — and adds 1.26.7 beside it.
`cmd/graymatter/go.mod` declares `toolchain go1.26.7` so `go install ...@latest`
produces a patched binary even on an older Go; the root module deliberately
does not, since forcing a toolchain download on library consumers is not this
project's call. A blocking `govulncheck` job now scans both modules.

**`forget` now sticks.** Recall bumps the access counter of every fact it
returns from a detached goroutine, and bolt's `Put` does not care whether the
key still exists — so a writeback landing after a delete brought the fact back,
*after* the store had reported it gone. Every deletion path was affected, since
all of them recall or list before they delete: `graymatter forget`, both
`DELETE /forget` routes, and `memory_reflect`'s forget and update. `UpdateFact`
now refuses to write a key that is no longer there; nothing creates a fact
through it, so the guard costs nothing. Caught by CI on one matrix entry, not
by the local runs.

**Smaller holes closed.** The `kg_audit` bucket is capped at 10 000 entries
(it grew forever) and `audit.Write` returns its error instead of discarding it.
`sessions logs` refuses a log path that resolves outside `<data-dir>/logs`; the
path comes back out of bbolt, so it is only as trustworthy as whatever wrote the
record. The three embedding providers bound how much of an error body they read
into a message. On Unix, the fallback socket moves from a predictable temp-dir
path into a `0700` per-UID directory whose mode, ownership and symlink status
are all verified. `SessionKill` cross-checks the PID against the file
graymatter wrote when it spawned the session, so a made-up session record is no
longer a way to have the daemon terminate an arbitrary process. `init --no-path`
skips the PATH entry.

Known gaps, stated rather than implied: there is no namespace isolation between
agents, facts carry no provenance, there is no rate limiting, and any process
running as you can read the daemon token. All four are in the threat model.

### Documentation

- **`CLAUDE.md` named the wrong Windows transport.** It described daemon
  clients connecting over "a local socket or named pipe". There is no named
  pipe: `pkg/memory/rpc/sock_windows.go` binds TCP loopback on a
  kernel-assigned port, because Go's standard library has no portable named-pipe
  support. Corrected, along with what follows from it — on Windows the 256-bit
  token in the discovery file is the only access control, and the `0600` passed
  to `os.WriteFile` buys nothing there, since Windows ignores POSIX modes and
  the file inherits its parent directory's ACL. Verified on Windows 10: the
  discovery file carries zero non-inherited ACEs, and its ACL is byte-identical
  to its parent's.
- **Added a "How it compares" section to the README** covering code graphs and
  context compressors, after [#23](https://github.com/angelnicolasc/graymatter/issues/23)
  showed the existing text does not say *what* the knowledge graph is a graph of.

---

## [0.8.0] - 2026-08-10

A release about surfaces that reported something they had not checked: an
`init` writing files nobody reads, and a `doctor` calling a project healthy on
the strength of a marker rather than the briefing behind it. All three defects
trace to the same cause — the table that knows which agent reads which file
existed in three copies, and only one of them was kept current.

### Changed

Two CLI contracts move. Neither is covered by the compatibility promise in
[docs/api-stability.md](docs/api-stability.md) — `cmd/` carries no stability
guarantee — but both are observable from a script, so they are called out here
rather than left inside the fixes below.

- **`init --only <agent>` and `--skip-<agent>` write fewer files.** Only the
  instruction file each selected agent actually reads. A provisioning script
  that assumed `--only opencode` left a `CLAUDE.md` behind will no longer find
  one. Plain `graymatter init` is unchanged, byte for byte.
- **`init --only <unknown-id>` now exits 1** instead of 0. It used to wire
  nothing, write both instruction files, and report success.

`doctor` exit codes are unchanged in every state — healthy, outdated block,
nothing wired, global-only. Automation gating on them is unaffected; only the
text it prints is different.

### Fixed

**`init` writes only the instruction files the agents you wire actually read (issue #13)**
- `graymatter init --only opencode` created a `CLAUDE.md` alongside the `AGENTS.md` it needed, and every other value had the same problem pointing the other way: `--only claudecode` wrote an `AGENTS.md`, `--only cursor` and `--only codex` wrote a `CLAUDE.md`. Claude Code reads `CLAUDE.md` and does not read `AGENTS.md`, so the extra file is not harmless redundancy — it is a file nobody reads, checked into the repo.
- The interactive wizard already had the per-agent mapping; the flag path kept its own list of writers and wrote both files regardless. `knownAgents` is now the one table `init`, the wizard and `doctor` all read.
- `--only` rejects an unrecognised id instead of silently wiring nothing. That was cosmetic while both files were written unconditionally; now that the files follow the selection, a typo would produce a run that writes nothing and still exits 0.
- Plain `graymatter init` is unchanged, byte for byte. `--skip-claudecode` no longer writes a `CLAUDE.md` nobody asked for.

**The block writer stops damaging files it updates**
- It always spliced LF. With `core.autocrlf=true` a checkout hands the file back as CRLF, so the managed block came back LF and the rest stayed CRLF, leaving the file with both — and because the change comparison then never matched, every `init` rewrote the file and dirtied the tree. The block now follows the file's dominant line ending, by majority, so one pasted CRLF line in an LF file does not drag the block across.
- It replaced only the first marker pair, so a duplicate could keep the old text while the file still satisfied anything that read the first pair and stopped. All managed blocks are now collapsed into one. An orphaned begin marker is left alone rather than paired with a later block's terminator, which would delete every line in between.

**`doctor` checks the block that is there, and who it reaches (issues #14, #17)**
- Instruction coverage is resolved per wired agent. A project relying on `init --global` was told nothing tells the model to use the memory tools, by the same check whose hint recommends `--global`. Crediting the global block to everyone would have been worse: it reaches Claude Code and OpenCode, not Cursor or Codex.
- A project still carrying the pre-0.7.0 briefing — the one that gated the search on "when prior context might matter" — reported "Everything looks good", because the check accepted any file containing the marker or the word "graymatter". It now compares the block against the text we ship, with line endings normalised so a CRLF checkout is not mistaken for drift. A briefing you wrote yourself is recognised and never called stale.
- `graymatter init` migrates a stale block in place, as before: markers are unchanged and anything outside them is untouched.

### Docs

**`CONTRIBUTING.md` states the Go version we actually need, and the `-race` gap**
- Setup said Go 1.22+. The workspace declares `go 1.23.0`, so 1.22 either pulls 1.23 down or, with `GOTOOLCHAIN=local`, refuses to build at all. It also listed "no CGO" among the requirements, which reads as a constraint on your machine; it is how the release artifact is built (`CGO_ENABLED=0` in `.goreleaser.yml`).
- The documented test commands did not pass `-race`, which CI runs on every package — following the file to the letter got you a green local run and a red pull request. The `-race` commands are now written down beside them, with the C-compiler requirement that comes with the detector.

**README**
- Dropped the Go Report Card badge. The service was sunset and its endpoint now serves a badge reading "retired" for every repository, at HTTP 200 — so no link checker catches it.

---

## [0.7.1] - 2026-08-10

Follow-up to 0.7.0. Every item here is a surface that reported something it had
not checked: a price looked up under the wrong key, a dashboard drawing zeros it
could not read, a setup step that never mentioned the one action that makes it
work.

### Fixed

**Token cost stops reporting the wrong price with confidence**
- Model lookup was a map range over `strings.Contains`, so a short key shadowed any newer model whose ID extends it: Haiku 4.5 resolved to the Haiku 4 row and under-reported cost 4x, Opus 4.8 resolved to Opus 4 and over-reported 3x. Both returned a hit, so the panel's "partial" flag never fired. The result was a confident, incorrect number on the one panel whose job is reporting money, and because Go map iteration is unordered it could differ between runs.
- Lookup now takes the longest matching key, deterministically, and the table carries the current generation. Cache rates follow the published multipliers (read 0.1x input, 5-minute write 1.25x) rather than being transcribed per row, and a test pins that relationship so a mistyped rate fails rather than ships.
- Models with no row still contribute zero and still flag the total as partial. A missing model is the safe failure; a stale one is not.

**The TUI survives a daemon restart, and stops drawing zeros it cannot read**
- Every handle from `openStore` now reconnects, so the TUI, the REST server and anything else long-lived keep working across `graymatter daemon stop`, a crash or an upgrade. Short-lived commands never noticed; a dashboard left open did, permanently.
- An error no longer replaces the whole UI with "Error: … Press q to quit." Nothing ever cleared that state, so one momentary failure ended the session even after the store recovered. Errors now show in the header, the rest of the UI stays usable, `r` still retries, and the next successful load clears it.
- The Stats panel reported "store unreachable" as a dashboard full of zeros, which is indistinguishable from a project that has stored nothing. It now says what happened and points at `graymatter doctor`. An empty project still renders as empty, so the warning cannot become background noise.
- Sessions, knowledge-graph and per-agent fact counts stop swallowing read failures. An empty store does not error on any of those paths, so anything that reaches them is a real failure and is reported as one.

**Setup tells you to restart your client**
- MCP clients launch their servers at startup, so the memory tools do not exist in the session that ran `init`. Nothing said so where it mattered: `init` sent you straight to `doctor` and `remember`, and the one mention lived in a README subsection about flags. A correct install therefore looked broken, which is very likely part of what fed the reports in #14.
- `init` now leads its next steps with the restart, both the plain and interactive paths through one shared helper, and `doctor` carries the hint while the store is still empty. It disappears once a single fact exists, so it cannot become noise.

### Changed

**Docs address the agent doing the install, not just the human reading**
- New "Hand it to your agent" section at the top of the README: a prompt to paste, then a numbered setup procedure written in the second person for the agent to execute.
- `CLAUDE.md` rewritten around the three situations an agent can be in (installing GrayMatter for a user, working in a project that uses it, contributing to it) instead of build and branching instructions, which duplicated `CONTRIBUTING.md`.
- `CONTRIBUTING.md`: test commands use `./...` rather than enumerating subtrees, matching the CI fix, since the old form silently skipped the root package and `cmd/graymatter`. Adds the convention that a status surface must check the thing it reports on and needs a test proving it goes red when that dependency is gone.

---

## [0.7.0] - 2026-08-10

### Added

**`graymatter init -i` — interactive setup wizard**
- New `-i`/`--interactive` flag: prompts for which agents to wire instead of auto-detecting everything.
- Per-agent instructionFile mapping: each known agent declares which instruction file it reads (`CLAUDE.md`, `AGENTS.md`, or none), so only the relevant files are written — an OpenCode-only user finally gets just `AGENTS.md` without `CLAUDE.md`.
- Dedup: when multiple selected agents share the same instruction file (e.g., Cursor + OpenCode both read `AGENTS.md`), it's written only once.
- 16 tests covering all agent combinations, input parsing edge cases, dedup, and idempotency.
- New `knownAgents` table (`cmd_init_interactive.go`) as a single source of truth for agent → {writer, instructionFile} mapping.

**`graymatter init --global` (issue #17)**
- Writes the managed memory block into `~/.claude/CLAUDE.md` and `~/.config/opencode/AGENTS.md`, the home-scoped files agents read no matter which project they are working in. Global installs no longer need `init` run in every repo just to get the instructions. Works with `-i` too.
- `XDG_CONFIG_HOME` is honoured for the OpenCode path. OpenCode resolves its global config through it, so hardcoding `~/.config` would have written where nothing reads. The directory is `~/.config/opencode` on every platform, Windows included; OpenCode does not use `%APPDATA%`.
- Same marker-based upsert as the project files, so it stays idempotent and leaves your own global instructions untouched.
- The block guards on the tools actually being present, since a global install reaches projects that never wired GrayMatter. It is a capability check, not a judgement call, so it cannot become the hedge that made the old block ignorable.
- Deliberately not a `SKILL.md`. OpenCode loads skills lazily, advertising only name and description and leaving the body to the model's discretion, which puts the same "the model decides whether to bother" failure one level up. Instructions that must run before the first reply belong in a file that is always loaded.

### Fixed

**Agent instructions now read as a procedure (issue #14)**
- The generated block described the five tools and told the model to search "when prior context might matter", a condition it can resolve to false every single time. Reported by several users as "init and doctor are green but nothing is ever stored".
- Rewritten as an unconditional session protocol plus a trigger table, matching the briefing this repo already dogfoods in its own `AGENTS.md`.
- `agent_id` is now derivable instead of invented: it is the repository root directory name. The old `<project>-<role>` template produced a different id per session, which scattered facts across namespaces and looked exactly like memory being broken.
- Existing files migrate in place. The markers are unchanged, so re-running `init` replaces the old block rather than stacking a second one.

**`graymatter doctor` notices a project that was set up and never used (issue #14)**
- Every other check can be green while the agent has never called a single tool, and the summary still read "Everything looks good". That is why this failure produced so few reports: a fresh install and a week of silence looked identical, so people quietly gave up instead of filing anything.
- A project initialised more than 24 hours ago that still holds no facts is now a warning with an actionable hint. The age is read from `MEMORY.md`, which `init` writes once and nothing else touches.

**REST server reaches the store through the daemon (issue #19)**
- `graymatter server` was the only command still opening bbolt directly. Clients spawn the daemon on first use and it lingers afterwards, so that is the normal state of the system rather than an edge case: every data route answered 503 while `/healthz` still reported ok. The server now takes a store handle, the same one every other command uses, and never touches the lock.
- It fails to start when the store cannot be opened, rather than coming up and serving nothing but errors.
- `/healthz` reports readiness. It round-trips to the store and answers 503 when that fails, with the reason going to the log rather than to whoever can reach the probe.
- The handle reconnects. A CLI command's store lives for milliseconds; the server holds one for as long as it runs, so `daemon stop`, a crash or an upgrade would otherwise leave it returning 500 forever. A dead connection is re-dialled once per call, and because that re-dial spawns a daemon when none is running, the usual outcome is a served request. Only connection loss is retried: an error that reached the daemon is reported, never replayed.
- `/consolidate` runs with the store owner's policy instead of a config the REST layer assembled itself, which also drops a hardcoded model id. Its `ANTHROPIC_API_KEY` gate is gone: consolidation is mostly decay and pruning, which need no LLM, and once the work moved behind the daemon that check was reading the wrong process's environment. It rejected requests the daemon could have served and admitted ones where the daemon had no key. The endpoint now returns 200 and does its real work with no provider configured.

**MCP tools no longer advertise themselves as destructive**
- All five tools inherited mcp-go's defaults (`readOnlyHint: false`, `destructiveHint: true`, `openWorldHint: true`), so `memory_search` and `checkpoint_resume`, both pure reads, were announced to clients as destructive open-world calls. Clients use those hints to choose between auto-approving a call and prompting for it.
- Each tool now declares what it actually does, and a test pins the annotations so a dependency bump cannot quietly reset them.

### Internal

**CI runs the root and CLI entrypoint packages**
- Both coverage steps enumerate packages by hand, and neither list included the root package or `cmd/graymatter`. The public API surface and the whole CLI entrypoint (`init`, `doctor`, the TUI, the store handles) had never been executed by CI on any platform, so tests living there were reported green by a workflow that never ran them.
- Both now run under `-race` as their own steps, deliberately outside the coverage profiles so the existing gates keep measuring what they always measured.

### Credits

- **MikeCase** — built the interactive `init` wizard (PR #16), and the `AGENTS.md` he rewrote by hand and posted in issue #14 is what pointed at the real cause of the activation failure after a wrong first diagnosis.
- **meyverick** — reported the activation failure (#14) and spotted that `--only opencode` still wrote both instruction files (#13).
- **jtoronto** — independently confirmed #14, which is what made it clear the reports were not a one-off setup mistake.
- **wizziLalev** — asked for a global install (#17), which became `init --global`.

---

## [0.6.0] - 2026-06-13

### Added

**Daemon mode — concurrent store access (issue #8, closes #4 and #9)**
- bbolt is single-writer: until now a second `graymatter` process (TUI + MCP server, two agents' MCP servers, `run` + TUI, opencode + TUI) targeting the same `--dir` timed out on the lock. Daemon mode fixes this structurally.
- One process owns the store and serves it over a local endpoint (Unix domain socket on POSIX, TCP loopback on Windows); every other process — TUI, MCP server, one-shot CLI commands, the `run` harness — connects as a thin client. Transport is `net/rpc` + JSON (`pkg/memory/rpc`), **stdlib only, zero new dependencies** — the single-binary/zero-infra story is intact (stripped binary ≈ 18 MB).
- **Launch-on-connect**: clients spawn the daemon on first use and it idle-exits after 2 min with no clients, so one-shot commands stay snappy and nothing lingers. No manual `serve` step.
- **No silent degradation**: the daemon opens the store strict-write (`StrictWrite`) — if it can't own the lock it fails loudly rather than coming up read-only under its clients. The spawn race is resolved by the bbolt lock itself: only the winner writes the discovery file; losers exit and every client converges on the winner via dial-retry.
- **Local-only auth**: the daemon mints a 256-bit token per run, written to a `0600` discovery file; clients present it as a connection preamble (constant-time compared). On Windows loopback this is the access control; on Unix the socket is also `0600`.
- New `graymatter daemon run|status|stop`. New global `--no-daemon` (and `GRAYMATTER_NO_DAEMON=1`) for in-process debugging/air-gapped inspection.
- `graymatter doctor` is daemon-aware: it reports store health *through* the daemon when one is running, and only flags a lock when a non-daemon process holds it.
- Integration tests cover spawn-on-connect, four concurrent clients writing through one daemon (the #4/#9 scenario), idle-exit, and graceful stop.

**`graymatter doctor` — end-to-end setup diagnosis (issue #3)**
- New command checks the full chain that makes agent memory work: binary on `PATH`, data dir writable, store health + lock state (single-writer detection with actionable hints), MCP wiring per client config, and agent-instruction files.
- `--json` for scripting; exit code 1 only on hard failures.

**`graymatter init` writes agent instructions (issue #3)**
- `init` now upserts a managed memory block into `CLAUDE.md` and `AGENTS.md` (markers `graymatter:instructions:begin/end`), so the model is actually told to call the memory tools — an MCP connection alone only makes tools *available*.
- Idempotent: re-runs replace only the managed block; user content above/below is never touched. Opt out with `--skip-instructions`.
- README gained a troubleshooting section covering the two failure modes this closes ("MCP connected but nothing stored", orphaned manual `mcp serve`).

### Fixed

**`memory_reflect` forget action matches its documentation (PR #10)**
- The docs promised `text` and `target` were equivalent for `forget`, but the handler required `target` and the schema marked `text` globally required — the documented text-only call failed. `text`/`target` are now validated per action; `forget` accepts either (`target` wins when both are set), and no empty-`text` placeholder is needed.
- Regression tests cover every documented call shape.

**TUI lock handling (PR #7)**
- Clear "gray.db is locked by another process" error instead of a raw bbolt timeout; automatic read-only fallback where the OS lock allows it; `--read-only` flag; `⊘ read-only` header badge with `d`/`k` disabled; `ErrStoreReadOnly` sentinel at the store level.

### Credits

- **MikeCase** — reported the `memory_reflect` forget/`target` mismatch with a precise repro (PR #10) and the original TUI lock report (issue #4).
- **Ferroman** — pushed for `init` updating `CLAUDE.md` so agents learn about the memory driver (issue #3).
- **live-sound** — original "How should it work?" report that became the doctor command (issue #3).

---

## 0.5.2 - 2026-04-28 — never tagged

No `v0.5.2` tag exists. This section is kept because it is the only record of the work, which first shipped in [0.6.0].

### Added

**Agent onboarding kit — `docs/AGENTS.md`**
- New ~650-line operational guide covering when and how to use GrayMatter from Claude Code, Cursor, OpenCode, Codex, Antigravity, and any MCP-compatible host.
- Authoritative MCP tool reference table (exact param names per tool — resolves the `agent` vs `agent_id` inconsistency between `memory_reflect` and the other four tools, see `cmd/graymatter/internal/mcp/server.go:144`).
- Replaces fabricated retrieval-weight numbers with a correct RRF (Reciprocal Rank Fusion) explanation, cross-referenced to `pkg/memory/recall.go:14`.
- Adds CLI parity table, library API pointer (`examples/agent/main.go`), multi-process bbolt lock guidance, and a knowledge-graph `link`-action caveat (only works when the host wires `SetKGLinker`).
- Cross-references `GRAYMATTER_PLAYBOOK.md` (strategy / why) ↔ `docs/AGENTS.md` (operations / how).

**README link**
- New blockquote in the `MCP clients (drop-in)` section pointing readers at `docs/AGENTS.md` for per-tool parameter names and usage patterns.

### Credits

- Original draft contributed by **MikeCase** (`MikeCase/graymatter-agent-patterns`, issue #6). Edited and extended for accuracy against the current codebase.

---

## [0.5.1] - 2026-04-19

### Fixed

- **`go install` works for anyone outside the local workspace.** The `replace` directive in `cmd/graymatter/go.mod` pointed at the checkout next door, so `go install github.com/angelnicolasc/graymatter/cmd/graymatter@latest` failed for every user who was not building from a clone with the sibling module present.

---

## [0.5.0] - 2026-04-18

### Added

**Auto-grab for every major MCP-compatible client**
- `graymatter init` now wires the MCP server into Claude Code, Cursor, Codex (OpenAI), OpenCode, and Antigravity (Google) with a single command.
- New writers: `writeClaudeCodeProject`, `writeCursorProject`, `writeCodexHome`, `writeOpencodeProject`, `writeAntigravityProject` (`cmd/graymatter/cmd_init_writers.go`).
- Codex support handles the TOML schema at `~/.codex/config.toml` (`[mcp_servers.graymatter]`) and preserves unrelated keys.
- OpenCode writes `opencode.jsonc`; if the existing file uses JSONC comments we fail soft and print the exact snippet to paste.
- Antigravity is opt-in (`--with-antigravity`) since it's still community-documented.
- New flags: `--skip-claudecode`, `--skip-cursor`, `--skip-codex`, `--skip-opencode`, `--with-antigravity`, `--only <csv>`.
- 7 new tests in `cmd/graymatter/cmd_init_writers_test.go` covering first-write, merge-preserving-other-servers, idempotency on second run, TOML round-trip for Codex, JSONC fail-soft for OpenCode, Antigravity opt-in, and `--only` parsing.

### Changed

- `graymatter init` now **merges** MCP entries instead of skipping files that already exist. Pre-existing servers from other tools are preserved; `graymatter` is upserted.
- README hero re-framed around "general-purpose MCP server, zero vendor lock-in" — the old section `Claude Code / Cursor (MCP)` is now `MCP clients (drop-in)` with the full client table.
- `README` and footer bumped to `v0.5.0`.

### Internal

- New dep: `github.com/BurntSushi/toml v1.4.0` (pure-Go, zero transitive deps) for Codex config round-trip.

---

## [0.4.0] - 2026-04-16

### Added

**Token Cost panel in the observability dashboard**
- New `Token Cost · 30d` card on the Stats tab aggregates input / output / cache-read / cache-write tokens per agent, per model, per day, with USD cost computed from the public Anthropic price list (`cmd/graymatter/internal/harness/pricing.go`).
- Cache-hit-rate headline with dynamic colour thresholds (mint ≥ 60%, amber ≥ 30%, rose otherwise) surfaces the real savings from prompt caching.
- Per-model breakdown (top 3 by spend) with a proportional share bar.
- Bucket schema (`token_usage`) is pre-aggregated on write (key `{agent}|{model}|{yyyymmdd}`), so the hot path is a single bbolt `Put` and the read path is bounded by `agents × models × days`.

**Honest empty-state handling**
- Panel renders a friendly hint when the bucket is empty (`No agent runs yet — Tracked automatically on graymatter run`). Unknown models are flagged as `Partial`; the UI never fabricates a cost.

### Changed

**Dashboard grid is now strictly symmetric**
- Width math reworked so the KPI strip, the Agents / (Token Cost + Weight Distribution) row, and the Activity panel share the same outer border columns (1-column gutters accounted for explicitly).
- New height-aware `panelBoxH` helper in `tui_styles.go` lets the right-column stack pad down to the exact line count of the Agents panel, so bottom borders align on a single grid baseline.
- Weight Distribution footer collapsed to a compact `avg · oldest → newest` line to free vertical space for the new Token Cost panel above it.

### Internal

- `harness.RecordTokenUsage(db, agent, model, input, output, cacheRead, cacheWrite)` is invoked best-effort from `runner.go` immediately after every successful `client.Messages.New` call. Accounting failures never break a run.
- Added `TestDashboardRender_WithTokens` and `TestFormatUSD` to lock in panel output and USD formatting.

---

## [0.3.0] - 2026-04-13

Observability dashboard redesign (KPI strip, Agents inventory-vs-activity panel, weight distribution, activity sparkline). See commit `20758a2`.

---

## [0.2.1] - 2026-04-11

### Fixed

**Windows UX — auto-register executable in User PATH**
- `graymatter init` now writes the executable's directory to `HKCU\Environment\Path` via the Windows registry on first run, so users can type `graymatter` from any PowerShell session without prefixing `.\`. The operation is idempotent (no-op if the directory is already present) and best-effort (prints a warning to stderr but never fails the command).
- A `WM_SETTINGCHANGE` broadcast is sent after the registry write so running shells receive the updated PATH without a full logoff/logon cycle.
- No-op on macOS and Linux where the binary is placed in `/usr/local/bin`, which is already on PATH.

**README install commands**
- Pinned binary install URLs to the correct v0.2.1 GoReleaser asset names (`darwin_arm64`, `windows_amd64`).
- Removed the `Move-Item ... C:\Windows\System32` step from the Windows instructions; PATH registration is now handled automatically by `graymatter init`.

---

## [0.2.0] - 2026-04-10

### Added

**Context-propagation API**
- `RememberCtx`, `RecallCtx`, `RememberSharedCtx`, `RecallSharedCtx`, `RecallAllCtx` — context-aware variants of every public memory operation. Callers can now propagate deadlines and cancellation signals end-to-end through the memory subsystem.
- All original methods (`Remember`, `Recall`, etc.) are preserved unchanged and delegate internally to their `Ctx` counterparts, guaranteeing full backward compatibility with existing integrations.

**Pluggable vector backend (`VectorStore` interface)**
- Introduced `pkg/memory.VectorStore`, a stable interface decoupling the memory core from any specific vector database implementation. Methods: `AddDocument`, `Query`, `EnsureCollection`, `Close`.
- Default implementation (`chromemVectorStore`) wraps chromem-go behind the interface; the adapter is transparently selected at `Open()`.
- `StoreConfig.VectorBackend` field allows callers to inject an alternative backend (Qdrant, Weaviate, pgvector, etc.) without modifying `Store` internals.

**Observability**
- `/metrics` endpoint exposing four `expvar` counters: `requests_total`, `request_latency_us`, `facts_total`, `recall_total`. Zero additional dependencies; uses the Go standard library exclusively.
- `StoreConfig.OnRecall` hook: invoked after every `Recall` with elapsed time and result count, enabling integration with external APM systems.
- `StoreConfig.OnPut` hook: invoked after every successful `Put`.
- `StoreConfig.Logger` field: accepts any `*log.Logger`; internal diagnostics route through it instead of the default logger.
- `loggingMiddleware` on the HTTP server: structured per-request log lines with method, path, status code, and latency.

**Embedding dimension guard**
- On `Open()`, the stored embedding dimension is read from a dedicated bbolt meta bucket and compared against the current provider's reported dimension. A mismatch emits a structured warning instead of silently corrupting the vector index with incommensurable embeddings.
- `recordEmbedDimensions` persists the dimension on first use; subsequent opens with a matching provider are a no-op.

**Go workspace (module split)**
- `go.work` workspace file declaring two modules: root (core library) and `cmd/graymatter` (CLI / TUI binary).
- `cmd/graymatter/go.mod` isolates all CLI and TUI dependencies (bubbletea, bubbles, lipgloss, cobra, mcp-go) from the core library. Downstream consumers that import only the core module no longer transitively pull in UI toolkits.
- Verified with `go mod graph`: zero TUI dependencies reachable from the root module.

**Continuous integration**
- Three-platform CI matrix: `ubuntu-latest`, `macos-latest`, `windows-latest`.
- Coverage gate: core library ≥ 70% statement coverage; CLI module ≥ 65%.
- Coverage artefacts uploaded per run for trend analysis.
- Non-blocking benchmark job (`BenchmarkRecall_100`, `BenchmarkRecall_1000`, `BenchmarkTokenize`, `BenchmarkKeywordScore`) with `benchmem` and 3-second bench time.

**Test suite**
- `pkg/memory/fuzz_test.go`: three fuzz targets — `FuzzTokenize`, `FuzzUnmarshalFact`, `FuzzKeywordScore` — each with a seeded corpus for deterministic CI runs. Property assertions cover: no-panic on arbitrary input, token length invariant (≥ 2 characters), lowercase normalisation, valid fact-ID format, and non-negative keyword scores.
- `pkg/memory/store_test.go`: `TestPut_WithEmbedder` and `TestRecall_WithEmbedder` using a fixed-vector `goodProvider`, exercising `addToVector`, `chromemVectorStore.AddDocument`, and `chromemVectorStore.Query` end-to-end. Additional unit tests for `DB()`, `SetKG()`, and `marshalJSON`.
- `cmd/graymatter/internal/server/server_test.go`: `TestConcurrentRememberAndRecall` (10 concurrent writers, 10 concurrent readers, 5 operations each; verifies all 51 facts persisted without data loss) and `TestRequestContext_Cancellation` (pre-cancelled context requests must not produce 5xx responses or leave the server in a degraded state).

### Fixed

**Critical — HTTP server store lifecycle**
- The HTTP server was opening and closing a bbolt database handle on every incoming request. Because bbolt enforces exclusive process-level file locking, concurrent requests would contend on the lock, and the in-process vector index would be rebuilt from disk on each call. The `Store` is now opened once in `New()`, held for the lifetime of the server, and closed in `Shutdown()`. A `storeReady` guard returns HTTP 503 if the store has not yet initialised, preventing nil-dereference panics at startup.

**Critical — Ollama embedding retry body reuse**
- The Ollama embedding client's retry loop was reusing the original `*http.Request`, whose body had already been consumed by the first attempt. Subsequent retries sent an empty body, receiving a 400 from the server and exhausting all attempts without ever embedding the text. Each retry iteration now constructs a fresh `*http.Request` with a new `bytes.Reader`. Backoff: 500 ms after attempt 1, 1 s after attempt 2; respects context cancellation.

**Correctness — consolidate.go error handling**
- `UpdateFact` errors inside the decay loop were silently discarded via blank-identifier assignment (`_ = err`). Errors are now accumulated with `errors.Join` and returned from `MaybeConsolidate` so callers and the `OnConsolidateError` callback receive the full error set.
- `summariseFacts` was calling `Delete` before `Put`. A crash between the two operations would permanently lose the fact. The order is now `Put` then `Delete`, preserving the fact on disk in all failure scenarios.
- `UpsertNode` errors in the knowledge-graph enrichment path were silently ignored; they are now forwarded to `OnConsolidateError`.

### Changed

**Stop-word filtering performance**
- The `stopWords()` function — previously called on every invocation of `tokenize`, allocating a new `map[string]bool` each time — has been replaced with a package-level `var stopWordSet` initialised once at program startup. This eliminates per-call heap allocations on the hot recall path.

**`vectorSearch` return type**
- `vectorSearch` now returns `[]VectorResult` (a stable, interface-aligned type) instead of the previously leaked `[]chromem.Result`. This decouples the recall path from the chromem-go type system and is required for the `VectorStore` abstraction.

**expvar singleton guard**
- `expvar.NewMap` panics if the same name is registered twice within a process (as occurs in test suites that instantiate multiple `Server` instances). A `getOrNewMap` helper now checks `expvar.Get` before calling `NewMap`, preventing test-suite panics without altering production behaviour.

**bbolt cleanup ordering on Windows**
- `t.TempDir()` deferred cleanup was running before `srv.Shutdown()` in server tests, causing bbolt to fail to release its file lock on Windows before directory removal. All shutdown calls now use `t.Cleanup` (which runs in LIFO order before `TempDir` cleanup) instead of `defer`, ensuring correct resource teardown on all platforms.

### API Stability

See [`docs/api-stability.md`](docs/api-stability.md) for the list of stable public identifiers and the compatibility promise for the v0.x series.

---

## [0.1.0] - 2026-04-10

### Added

**Core memory API**
- `Remember` / `Recall` / `Consolidate` / `Close` public API
- `RememberShared` / `RecallShared` / `RecallAll` for shared cross-agent memory
- Hybrid retrieval: vector similarity (chromem-go) + keyword TF-IDF + recency, fused via RRF
- Exponential decay curve with configurable half-life (`DecayHalfLife`, default 30 days)
- bbolt-backed durable storage with chromem-go in-process vector index

**Goroutine lifecycle (fixes)**
- Async consolidation now uses a bounded semaphore (`MaxAsyncConsolidations`, default 2)
- All background goroutines are tracked with a `sync.WaitGroup`; `Close()` drains them before closing the database
- Consolidation errors surfaced via optional `OnConsolidateError` callback instead of being silently discarded
- `context.Background()` replaced with a cancellable shutdown context throughout

**Consistency (fixes)**
- `Open()` runs `reconcileVectors()`: re-indexes any bbolt fact missing from the vector store, repairing divergences caused by crashes between the bbolt commit and the vector write

**Fact extraction primitive**
- `Extract(ctx, llmResponse)` — extracts up to 5 atomic facts from an LLM response using structured JSON output
- `RememberExtracted(ctx, agentID, llmResponse)` — one-call Extract + Remember
- Graceful degradation: without an API key, returns the raw response as a single fact

**Knowledge graph**
- Entity extraction (regex and LLM-based via `pkg/kg`)
- Graph-enriched recall: top-ranked fact neighbours surfaced automatically
- Obsidian-compatible markdown export (`pkg/export`)

**Tooling**
- CLI: `init remember recall consolidate checkpoint export run sessions tui`
- MCP server (stdio + HTTP) with `memory_search`, `memory_add`, `checkpoint_*`, `memory_reflect`
- REST API server
- Bubble Tea TUI (4 views: facts, sessions, graph, stats)
- Plugin system (JSON-line protocol)
- Session checkpointing and recovery (`pkg/session`)

**Embedding backends**
- Auto-detection: Ollama → OpenAI → Anthropic → keyword-only
- Explicit overrides via `EmbeddingMode` config field

### API Stability

See [`docs/api-stability.md`](docs/api-stability.md) for the list of stable public identifiers and the compatibility promise for the v0.x series.

[Unreleased]: https://github.com/angelnicolasc/graymatter/compare/v0.13.1...HEAD
[0.12.1]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.12.1
[0.12.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.12.0
[0.11.1]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.11.1
[0.11.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.11.0
[0.10.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.10.0
[0.9.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.9.0
[0.8.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.8.0
[0.7.1]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.7.1
[0.7.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.7.0
[0.6.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.6.0
[0.5.1]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.5.1
[0.13.1]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.13.1
[0.13.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.13.0

[0.5.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.5.0
[0.4.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.4.0
[0.3.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.3.0
[0.2.1]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.2.1
[0.2.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.2.0
[0.1.0]: https://github.com/angelnicolasc/graymatter/releases/tag/v0.1.0
