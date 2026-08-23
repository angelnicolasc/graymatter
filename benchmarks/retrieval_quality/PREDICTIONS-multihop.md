# Predicciones pre-registradas — multi-hop / EnrichedHitRate (P1.2)

| | |
|---|---|
| Fecha de predicción | 2026-08-23 |
| Queries | `benchmarks/fixtures/queries-multihop-v1.jsonl` (q7–q9 sobre golds f002/f031/f055) |
| Sistemas comparados | `graymatter-fixed-k` (baseline) vs `graymatter-enriched` (expansión por entidades co-mencionadas, cap 3, post-ranking) |
| Dato diagnóstico previo (disclosed) | Scan del extractor regex sobre corpus-v1: **1 de 78 facts** produce entidades extraíbles (`f024`: secrets manager[role], vault[fact]) — ninguna adyacente a un gold |

## Predicciones (escritas ANTES del primer run enriquecido)

| Métrica | Banda pre-registrada | Racional |
|---|---|---|
| HitRate baseline (q7–q9) | **0%** exacto | Cero solapamiento superficial por construcción del corpus |
| **HitRate enriched (q7–q9)** | **[0%, 33%]** — punto estimado 0% | El índice de puentes cubre ~1.3% de los facts; ningún gold es alcanzable por puente |
| Δp95 (enriched − fixed-k) | ≤ +2ms | La expansión opera sobre un índice casi vacío |
| Dead rate ambos sistemas | 0% mantenido | Tombstones ya pineados |
| **GATE condición 2 (Enriched > baseline)** | **FAIL esperado** → No-Go wiring este ciclo | Sin cobertura de extracción no hay puentes que recorrer |

## Implicancia si la predicción se confirma

El wiring (PR-D) queda **bloqueado por el propio gate** que el playbook exige.
La condición bloqueante NO es el algoritmo de expansión sino la cobertura del
extractor sobre prosa técnica real (1.3%). Camino de desbloqueo documentado:

1. Mejoras deterministas al extractor (ya listadas en
   `benchmarks/extraction_precision/RESULTS.md`: sufijos organizativos,
   stopwords de determinador, Unicode, puntuación final de URLs).
2. Re-corrida de AMBOS benchmarks (precisión + multi-hop).
3. Solo con EnrichedHitRate > baseline medido: PR-D procede.
