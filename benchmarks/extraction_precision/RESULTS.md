# Resultados — precisión de extracción (regex extractor)

| | |
|---|---|
| Fecha de medición | 2026-08-23 |
| Comando | `go run ./cmd/graymatter/extractionbench` |
| Corpus | `benchmarks/fixtures/extraction-gold-v1.jsonl` (105 facts etiquetados a mano) |
| Sujeto | `kg.NewExtractor(ExtractorConfig{UseLLM:false})` — regex puro, cero red |
| Predicciones | `PREDICTIONS.md` en este directorio, commiteadas ANTES de la implementación del runner (`bc72eb2` precede a este archivo en la historia) |

## Números medidos

```
ID-level:   TP 77   FP  6   FN 16
Precision   0.928
Recall      0.828
F1          0.875
Strict (ID + tipo correcto): 0.714
GATE: PASS — precision 0.928 >= 0.70
```

| Métrica | Predicción | Medido | Veredicto |
|---|---|---|---|
| Precision (ID) | [0.88, 0.97] | **0.928** | ✅ dentro de banda · gate PASS |
| Recall (ID) | [0.82, 0.93] | **0.828** | ✅ dentro de banda |
| Strict (ID+tipo) | [0.50, 0.80] | **0.714** | ✅ dentro de banda |

## Desglose por tipo

| Tipo | Gold | Typed-ok | Tipo incorrecto | Missed |
|---|---|---|---|---|
| person | 38 | 37 | 1 | 4 |
| organization | 22 | **1** | **21** | 0 |
| date | 10 | 10 | 0 | 0 |
| preference | 4 | 4 | 0 | 0 |
| reference | 2 | 2 | 0 | 2 |
| fact | 1 | 1 | 0 | 4 |
| project | 0 | 0 | 0 | **1** |
| role | 0 | 0 | 0 | **5** |

## Lectura (lo que estos números dicen)

1. **El gate de identificación pasa con margen**: cuando el extractor emite una
   entidad, casi siempre existe (precision 0.93). El grafo no se llenará de ruido inventado.
2. **La fidelidad de TIPO es el problema real**: 21 de 22 organizaciones quedan tipadas como
   `person` o `fact` — exactamente la clase de falla que D2 describió a nivel de upsert y que aquí
   se confirma a nivel clasificación. El grafo funcionaría, pero sus etiquetas mentirían.
3. **Roles son invisibles para el regex** (0/5): CTO todo-mayúsculas no matchea ningún patrón;
   director/manager/advisor en minúscula tampoco.
4. **Nombres con acentos se fragmentan**: 4 personas perdidas por el corte de `[A-Z][a-z]+`
   en el primer carácter no-ASCII.
5. **Dos defectos nuevos descubiertos por el run** (no estaban en las predicciones):
   - *Determinador pegado*: "The Atlas Migration" se captura como una sola entidad
     `"the atlas migration"` → FP + FN simultáneos.
   - *URL con puntuación final*: el regex de URLs traga el punto anterior al fin de oración
     (`"...changelog."`) creando referencias sucias.

## Consecuencia para el wiring (ADR-003)

Condición 1 del Go/No-Go **satisfecha** (precision 0.928 ≥ 0.70). Quedan pendientes las
condiciones 2–4 (EnrichedHitRate multi-hop > baseline, Δp95 ≤ +2ms, Dead = 0%) que se miden
en PR-C sobre las queries q7–q9. La lista de mejoras al extractor que este run justifica —
sufijos organizativos extendidos, stopwords de determinador, Unicode en las clases de
carácter, recorte de puntuación final en URLs — se ejecuta SOLO si PR-C muestra que el
enriquecimiento aporta; si no aporta, el extractor queda donde está y el wiring no ocurre.
