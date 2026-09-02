# GrayMatter Token Benchmarks

Every number on this page is printed by one command. If a figure here and a
figure the command prints disagree, CI fails
(`benchmarks/token_count/main_test.go`) — the table is parsed out of this
markdown and compared against a live run.

```bash
go run ./benchmarks/token_count
```

## What is measured

| | |
|---|---|
| **Baseline** | Full-history injection — every stored observation concatenated into the prompt |
| **GrayMatter** | `Recall()` with hybrid retrieval, `topK=8` |
| **Corpus** | 100 paragraph-length agent observations (~50–70 words each), sales domain |
| **Session** | One stored observation. "30 sessions" = 30 observations in the store |
| **Query** | `"follow up with prospects and close pending deals this week"` — one fixed query |
| **Embedder** | Keyword-only (TF-IDF + recency). No LLM, no network, deterministic |
| **Insertion order** | Shuffled with a fixed seed (42) so ranking is not biased by recency |
| **Tokenizer** | `words × 1.33`, an approximation of GPT-4-class tokenization — within ±10% of tiktoken for English prose. Not a real BPE tokenizer |

## Results

| Sessions | Full injection | GrayMatter | Reduction |
|----------|---------------|------------|-----------|
| 1        | ~80 tokens    | ~80 tokens  | 0% |
| 10       | ~630 tokens   | ~550 tokens | 12% |
| 30       | ~1,880 tokens | ~550 tokens | 71% |
| 100      | ~6,960 tokens | ~670 tokens | **90%** |

**90% is the canonical figure**, and it means one specific thing: at 100
stored observations, against full-history injection, on this corpus, with this
tokenizer. It is not a claim about any other baseline.

The reduction is a function of how much history exists. At one session there
is nothing to cut and the number is 0%. The curve is the result, not the
100-session row.

## What this benchmark does not measure

Stated plainly, because the omissions are larger than the result:

- **Relevance.** This benchmark never checks whether the 8 recalled
  observations are the *right* 8. A system that returned 8 facts at random
  would score an identical 90% reduction here. That is measured separately now
  — see [retrieval quality](../benchmarks/RESULTS.md).
- **A realistic baseline.** Full-history injection is the weakest possible
  comparison. Production systems truncate. **Against a sliding window,
  GrayMatter does not win on tokens** — measured, not estimated: at an equal
  budget of 8 facts it costs *more*, because it returns the facts that answer
  the query and those are the longer ones. The differentiator is what a window
  cannot do at any price: recall a fact planted 96 sessions ago, and refuse to
  return a fact that has been superseded. Both are measured in
  [RESULTS.md](../benchmarks/RESULTS.md).
- **Multiple queries or domains.** One fixed query, one domain. The quality
  benchmark uses six queries across three.
- **Vector embeddings.** Keyword-only, so the numbers are reproducible without
  an API key. Vector recall changes precision; it is not measured here.
- **Consolidation.** Runs with consolidation untriggered.

Earlier revisions of this page published a token table nothing produced, and a
relevance score no code computed. Both are gone. The tests named at the top
exist so that neither can come back quietly.

## The other benchmark

Token count is half the question. [`benchmarks/RESULTS.md`](../benchmarks/RESULTS.md)
holds the other half: whether the facts that come back are the right ones,
measured against a real sliding window rather than against full-history
injection, with the predictions committed before the run.

```bash
go run ./benchmarks/retrieval_quality
```

One of its three pre-registered predictions failed, and it is written up there
in full.

## Revision currency

Retrieval quality asks whether the right facts come back. This asks the
question that only shows up across sessions: after a value was stated,
corrected, and sometimes corrected again, does the caller get the one that
holds?

```bash
go run ./benchmarks/revision_currency
```

The same 600-fact history is built twice - once with every correction written
as an independent fact, once with the corrections recorded through
`graymatter revise` - and both arms are measured on a compound endpoint: the
current value must outrank every retired sibling **and** land in the injected
top-8. Reporting only the first is gameable, and was gamed during development.

Recording the revision settles currency on 35 of 35 probes and shows the caller
zero retired facts, against 11/35 and 35 for the flat arm (McNemar exact,
paired, p = 1.5e-5). What it does not move is retrieval: the ten probes still
missed are ones where the corrected value never reaches the top-8 at all.
Numbers, strata and the committed predictions:
[revision_currency/RESULTS.md](../benchmarks/revision_currency/RESULTS.md).

## The retrieval-quality harness: three corpora

Relevance runs through a separate harness with its own gates - this document
deliberately publishes none of its numbers. What that harness now covers:

| Corpus | Facts | Queries | Purpose |
|---|---|---|---|
| frozen-v2 | 78 | 6 | Canonical English benchmark, byte-checked since v0.10 |
| multilingual-es | 126 | 15 | Spanish retrieval, per declared query class |
| long-horizon | 421 | 8 | Decisions planted early, queried at session 50, late paraphrases genuinely tombstoned |

Hit rates publish Wilson confidence intervals; predictions are committed
before runs and misses investigated in writing. Full tables and per-query
grids: [RESULTS.md](../benchmarks/RESULTS.md) and
[RESULTS-corpora.md](../benchmarks/RESULTS-corpora.md).

## Reproducing

```bash
go run ./benchmarks/token_count
go run ./benchmarks/retrieval_quality
go run ./benchmarks/retrieval_quality -fixtures benchmarks/fixtures/multilingual-es
go run ./benchmarks/retrieval_quality -fixtures benchmarks/fixtures/long-horizon
```

No API key, no network, no LLM. The store is created in a temporary directory
and deleted on exit. Runs in well under a second.

To verify the published tables against a fresh measurement the way CI does:

```bash
go test ./benchmarks/token_count/
```
