# Pre-registered predictions — extraction precision (regex extractor)

| | |
|---|---|
| Prediction date | 2026-08-23 |
| Corpus commit | `benchmarks/fixtures/extraction-gold-v1.jsonl` (105 hand-labeled facts) |
| Subject under test | `kg.NewExtractor(ExtractorConfig{UseLLM:false})` — pure regex, zero network |
| Primary matching rule | Exact canonical ID (lowercased) between extracted and gold; type scored separately |
| ADR-003 gate | precision(ID) ≥ 0.70 to wire auto-population |

## Predictions (written BEFORE the first run)

| Metric | Pre-registered band | Rationale |
|---|---|---|
| **Precision (ID-level)** | **[0.88, 0.97]** — gate PASS | The extractor is conservative: nearly everything it emits corresponds to a real proper noun in the text |
| **Recall (ID-level)** | **[0.82, 0.93]** | Lowercase roles (director, manager, advisor) and lowercase compound phrases are invisible to the regexes |
| **Strict precision (ID+type)** | [0.50, 0.80] | Two-word organizations classify as *person* (the classifier only recognizes corp/inc/ltd/llc/company suffixes); projects land as *fact* or *person* |

## Anticipated failure classes (falsifiable)

1. **Accented names fragment**: `[A-Z][a-z]+` cuts at the first accent
   ("Sebastián Yañez" → partial fragments like "Sebasti/Yañez"). Prediction: 4–6 FPs of
   this kind plus their corresponding FNs (x088, x090, x098, x103).
2. **Invisible roles**: CTO (all-caps matches no pattern), director/manager/advisor in
   lowercase → 5 FNs of the *role* class. Per-type role recall ≈ 0.
3. **org→person confusion**: two-word organizations without a recognized suffix
   (Juniper Labs, Vertex Analytics, Meridian Capital…) classify as *person*.
   Prediction: ≥ 60% of organizations end with the wrong type (affects the strict
   metric and graph quality, NOT the ID gate).
4. **Ambiguous projects**: "Atlas Migration" (two proper words) classifies as *person*,
   not *project*.

## Experiment rule

If precision(ID) < 0.70 ⇒ auto-population is not wired; extractor work comes first
(per the ADR-003 gate). If any band fails, results are published anyway and the
outcome directs extractor engineering (sentence-initial stopwords, organizational
suffix list, Unicode support in character classes).
