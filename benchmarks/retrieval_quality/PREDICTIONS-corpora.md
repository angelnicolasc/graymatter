# Pre-registered predictions — corpora v3 (multilingual-es, long-horizon)

Written and committed BEFORE any measurement run over the new corpora.
Protocol identical to PREDICTIONS-multihop.md: expectations first, actuals
appended afterwards, misses investigated rather than explained away.

## Corpus under test

| corpus | facts | queries | sessions | language |
|---|---|---|---|---|
| multilingual-es | ~150 | 20 | 15 | Spanish with ASCII anchors |
| long-horizon | ~400 | 12 | 50 | English |

Systems: fixed-K GrayMatter (keyword embedder — no vectors in CI) vs sliding
window-8 vs full-history injection, same protocol as the frozen corpus.

## Predictions

### multilingual-es

The keyword tokenizer is ASCII-only (`recall.go` tokenize keeps `[a-z0-9]`
after lowercasing). Spanish text therefore produces keyword tokens only from
its ASCII fragments: product names, numbers, emails, URLs. Queries are split
into two declared classes at corpus-authoring time:

1. **ascii-anchor class** (10 queries): gold shares an explicit ASCII anchor
   with the query (product name, order id, email).
   - Prediction: GrayMatter HitRate ≥ 60%.
   - Window-8 HitRate ≤ 40% (anchors are old; window is recent-first).

2. **pure-es class** (10 queries): matching requires accented or
   Spanish-only tokens (verb stems, accented names).
   - Prediction: GrayMatter HitRate ≤ 10%. **This near-zero is the point**:
   it quantifies, on purpose-built data, exactly what the Unicode tokenizer
   (Playbook A) must recover. A baseline of zero is a requirement spec, not
   an embarrassment.

3. Dead-rate ≤ 5% everywhere: forbidden facts are superseded variants, which
   tombstoning already handles.

### long-horizon

4. GrayMatter fixed-K HitRate ≥ 75% (frozen-v2 measured 83% at a similar
   span; this corpus has heavier distractor clusters, hence the wider band).
5. Window-8 HitRate ≤ 25% (gold lives in sessions 1–5 of 50).
6. Full-history HitRate = 100%, AvgTokens ≈ 6–7k (sanity anchors).
7. GrayMatter AvgTokens ≤ 150.

## Actuals

(appended after measurement — see RESULTS-corpora.md)
