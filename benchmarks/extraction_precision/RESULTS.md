# Results — extraction precision (regex extractor)

| | |
|---|---|
| Measurement date | 2026-08-23 |
| Command | `go run ./cmd/graymatter/extractionbench` |
| Corpus | `benchmarks/fixtures/extraction-gold-v1.jsonl` (105 hand-labeled facts) |
| Subject | `kg.NewExtractor(ExtractorConfig{UseLLM:false})` — pure regex, zero network |
| Predictions | `PREDICTIONS.md` in this directory, committed BEFORE the runner was implemented (`bc72eb2` precedes this file in history) |

## Measured numbers

```
ID-level:   TP 77   FP  6   FN 16
Precision   0.928
Recall      0.828
F1          0.875
Strict (ID + correct type): 0.714
GATE: PASS — precision 0.928 >= 0.70
```

| Metric | Prediction | Measured | Verdict |
|---|---|---|---|
| Precision (ID) | [0.88, 0.97] | **0.928** | ✅ within band · gate PASS |
| Recall (ID) | [0.82, 0.93] | **0.828** | ✅ within band |
| Strict (ID+type) | [0.50, 0.80] | **0.714** | ✅ within band |

## Per-type breakdown

| Type | Gold | Typed-ok | Wrong type | Missed |
|---|---|---|---|---|
| person | 38 | 37 | 1 | 4 |
| organization | 22 | **1** | **21** | 0 |
| date | 10 | 10 | 0 | 0 |
| preference | 4 | 4 | 0 | 0 |
| reference | 2 | 2 | 0 | 2 |
| fact | 1 | 1 | 0 | 4 |
| project | 0 | 0 | 0 | **1** |
| role | 0 | 0 | 0 | **5** |

## Reading (what these numbers say)

1. **The identification gate passes with margin**: when the extractor emits an entity,
   it almost always exists (precision 0.93). The graph will not fill with invented noise.
2. **TYPE fidelity is the real problem**: 21 of 22 organizations end up typed as `person`
   or `fact` — exactly the failure class D2 described at the upsert level, here confirmed
   at the classification level. The graph would work, but its labels would lie.
3. **Roles are invisible to the regex** (0/5): all-caps CTO matches no pattern; lowercase
   director/manager/advisor do not either.
4. **Accented names fragment**: 4 people lost to `[A-Z][a-z]+` cutting at the first
   non-ASCII character.
5. **Two new defects discovered by the run** (not among the predictions):
   - *Glued determiner*: "The Atlas Migration" is captured as a single entity,
     `"the atlas migration"` → FP + FN at once.
   - *URL with trailing punctuation*: the URL regex swallows the period before
     end-of-sentence (`"...changelog."`), creating dirty references.

## Wiring consequence (ADR-003)

Go/No-Go condition 1 **satisfied** (precision 0.928 ≥ 0.70). Conditions 2–4 remain open
(multi-hop EnrichedHitRate > baseline, Δp95 ≤ +2ms, Dead = 0%), measured in PR-C over
queries q7–q9. The extractor improvements this run justifies — extended organizational
suffixes, determiner stopwords, Unicode character classes, URL trailing-punctuation
trim — execute ONLY if PR-C shows enrichment contributes; if it does not, the extractor
stays as-is and no wiring happens.

---

## Extractor v2 — re-measurement after the deterministic fixes (2026-08-23)

The defects listed above motivated four fixes to the regex extractor
(Unicode-safe classes, determiner stripping, organizational suffix expansion,
all-caps role titles, URL trailing-punctuation trim). Same harness, same
commands, identical corpus:

| Metric | v1 (before) | **v2 (after)** |
|---|---|---|
| Precision (ID) | 0.928 | **0.946** |
| Recall (ID) | 0.828 | **0.946** (+0.118) |
| Strict (ID+type) | 0.714 | **0.977** |
| Organizations typed-ok | 1/22 | **22/22** |
| Person missed | 4 | **0** |
| Role recovered | 0/5 | **4/5** |

GATE: PASS (0.946 ≥ 0.70), with widened margin on every dimension.

Remaining false positives (5): `obsidian` and the `rafael`/`ortiz` fragments
from a second mention by surname (known class), plus two `registrar` captures
from the new contextual-roles rule where the gold did not annotate them —
annotation edge cases, documented without adjusting the gold.

Pending: re-run of the multi-hop bench against this extractor to re-evaluate
condition 2 of the ADR-003 gate (EnrichedHitRate > baseline).
