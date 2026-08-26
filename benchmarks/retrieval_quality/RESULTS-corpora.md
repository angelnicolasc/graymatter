# RESULTS — corpora v3 (multilingual-es, long-horizon)

Protocol and predictions committed beforehand: see PREDICTIONS-corpora.md.
Keyword embedder everywhere (no LLM, no network). Wilson 95% intervals now
rendered by the runner itself.

## multilingual-es — 126 facts / 15 queries / 15 sessions

| System | HitRate [95% CI] | Dead | Tokens/q |
|---|---|---|---|
| full-history | 100% | 0% | 2965 |
| window-8 | 27% [11–52] | 0% | 199 |
| **graymatter-fixed-k** | **60% [36–80]** | **0%** | **193** |
| graymatter-recency-only | 27% [11–52] | 0% | 199 |

Per declared query class:

| Class | n | GrayMatter hits | Prediction | Verdict |
|---|---|---|---|---|
| es-ascii-* (anchor reachable) | 10 | 7 (**70%**) | ≥60% | **met** |
| es-puro-* (accent-dependent) | 5 | 2 (**40%**) | ≤10% | **MISSED — investigated below** |

### The missed prediction, and what it taught

We predicted near-zero on accent-dependent queries because the tokenizer
keeps only `[a-z0-9]`. Reality: 2 of 5 hit anyway, for two reasons that a
prediction written from the tokenizer's spec missed:

1. Accent-splitting is crude stemming. `teléfono` tokenizes as `tel` + `fono`;
   both fragments recombine identically in query and fact, so accented words
   still match when the split lands in a distinctive syllable.
2. Spanish function words are largely ASCII-clean (`llamen`, `correo`,
   `aumento`, `reenviados`, `semana`). A "Spanish" sentence carries plenty of
   intact tokens.

Consequence for Playbook A (Unicode tokenizer + stemming): its ES win is not
binary reachability — fragments already provide that, badly — but precision
and recall on words whose ASCII fragment is ambiguous or empty (`integración`
→ `integraci`, `n` alone; single-letter fragments are dropped entirely).
The corpus stands as A's before/after measurement either way; class
expectations will be re-registered against the new tokenizer.

## long-horizon — 421 facts / 8 queries / 50 sessions

Late paraphrase variants are genuinely tombstoned (kind=variant superseded by
the oldest same-domain decision), so Dead measures real receipts.

| System | HitRate [95% CI] | Dead | Tokens/q |
|---|---|---|---|
| full-history | 100% | 100% | 7696 |
| window-8 | 0% [0–32] | 0% | 146 |
| **graymatter-fixed-k** | **100% [68–100]** | **0%** | **145** |
| graymatter-recency-only | 0% [0–32] | 0% | 144 |

All four predictions met: HitRate 100% ≥ 75%; window 0% ≤ 25%; full-history
anchors hold; tokens 145 ≤ 150. Recency-only scoring returns none of the
early decisions - the fusion's relevance channels, not recency, carry the
96%-style long-horizon behaviour this corpus was built to exercise.

## What changed in the runner

- Multi-hop loading is optional per fixture set (it needs corpus-specific
  bridge queries); missing file skips that suite instead of failing.
- `applySupersede` gained the `kind=variant` rule for long-horizon-style
  corpora: tombstone points at the oldest same-domain decision.
- HitRate now prints its Wilson 95% interval everywhere, including the
  machine-checked frozen tables.

Frozen-corpus gate: unchanged and green (TestReadmeQualityTableMatchesMeasurement).
