# Predicciones pre-registradas — precisión de extracción (regex extractor)

| | |
|---|---|
| Fecha de predicción | 2026-08-23 |
| Commit del corpus | `benchmarks/fixtures/extraction-gold-v1.jsonl` (105 facts etiquetados a mano) |
| Sujeto medido | `kg.NewExtractor(ExtractorConfig{UseLLM:false})` — regex puro, cero red |
| Regla de matching primaria | ID canónico (lowercase) exacto entre extraído y gold; el tipo se mide aparte |
| Gate ADR-003 | precision(ID) ≥ 0.70 para wirear |

## Predicciones (escritas ANTES del primer run)

| Métrica | Banda pre-registrada | Racional |
|---|---|---|
| **Precision (ID-level)** | **[0.88, 0.97]** — gate PASS | El extractor es conservador: casi todo lo que emite corresponde a un nombre propio real del texto |
| **Recall (ID-level)** | **[0.82, 0.93]** | Los roles en minúscula (director, manager, advisor) y frases compuestas en minúscula son invisibles para los regex |
| **Precision estricta (ID+tipo)** | [0.50, 0.80] | Organizaciones de 2 palabras se clasifican como *person* (el clasificador solo reconoce sufijos corp/inc/ltd/llc/company); proyectos quedan como *fact* o *person* |

## Clases de fallo previstas (falsables)

1. **Nombres con acentos se fragmentan**: `[A-Z][a-z]+` corta en el primer acento
   ("Sebastián Yañez" → fragmentos tipo "Sebasti/Yañez" parciales). Predicción: 4–6 FPs de este tipo
   y sus FNs correspondientes (x088, x090, x098, x103).
2. **Roles invisibles**: CTO (todo-caps no matchea ningún patrón), director/manager/advisor en
   minúscula → 5 FNs de la clase *role*. Recall por tipo role ≈ 0.
3. **Confusión org→person**: organizaciones de dos palabras sin sufijo reconocido
   (Juniper Labs, Vertex Analytics, Meridian Capital…) se clasifican *person*.
   Predicción: ≥ 60% de las organizaciones terminan con tipo incorrecto (afecta
   métrica estricta y calidad del grafo, NO el gate de ID).
4. **Proyectos ambiguos**: "Atlas Migration" (2 palabras propias) se clasifica *person*, no
   *project*.

## Regla del experimento

Si precision(ID) < 0.70 ⇒ NO se wirea el auto-poblado; primero extractor
(según gate ADR-003). Si alguna banda falla, se publica igual y el resultado
dirige la ingeniería del extractor (stopwords de inicio-de-oración, lista de
sufijos organizativos, soporte Unicode en las clases de caracteres).
