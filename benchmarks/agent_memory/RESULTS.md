# Inference cost across three-session tasks

GrayMatter’s optional `core` MCP profile and an organized notes folder each carried the same task through three sessions. Each new session started without the conversation history of the preceding session. The agent could use its assigned memory system to recover earlier decisions.

## Study design

The holdout comprised 120 synthetic tasks in eight families. Forty started with empty memory. A separate set of 80 started with 1,000 prior facts. Within each pair, both arms received the same task, model (Claude Haiku 5.5 at medium effort), prompts, and limits on Linux with prompt caching disabled. The seeded folder organized the same assertions, sources, and quotes into 20 topic files. GrayMatter used keyword retrieval. Two families allowed a bounded amount of automatic startup context in both arms. The other six used on-demand retrieval.

The primary analysis excluded a pair if either arm ended in a provider refusal. This left 39 empty-memory pairs and 74 seeded pairs. Inference cost counts model usage at the study’s list prices. The dollar amounts below scale the observed means to 1,000 tasks. The study did not run 1,000 tasks per arm.

## Results

| Starting memory | Valid pairs | GrayMatter USD per 1,000 tasks | Notes folder USD per 1,000 tasks | Lower mean cost (95% CI) | Pairs cheaper with GrayMatter | Tasks solved, GrayMatter / folder |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Empty | 39 | $4.32 | $4.72 | 8.4% (5.4–11.8%) | 34 | 15 / 10 |
| 1,000 prior facts | 74 | $4.82 | $5.38 | 10.4% (8.0–12.9%) | 60 | 35 / 34 |

GrayMatter solved 50 of 113 tasks and the folder solved 44. The paired difference was +5.3 percentage points (95% CI −3.5 to +14.2), meeting the prespecified noninferiority margin of −10 points. Counting refusals as failures retained all 120 pairs. Mean inference cost remained lower in both groups and the completion difference was +5.8 points (95% CI −2.5 to +14.2).

Intervals use 20,000 paired bootstrap resamples of tasks within each family and starting-memory group. The cost percentages and their interval endpoints express the paired absolute cost differences as a share of the observed mean folder cost. Task completion uses an absolute percentage-point difference.

## What the README chart shows

Each entry is the mean USD saved with GrayMatter per 1,000 tasks of that family. Family means describe the observed pairs. The prespecified cost comparisons used the full group at each starting-memory size.

| Task family | Empty pairs | USD saved per 1,000 | Seeded pairs | USD saved per 1,000 |
| --- | ---: | ---: | ---: | ---: |
| Partial updates | 5 | 0.248 | 9 | 0.174 |
| Project conventions | 5 | 0.584 | 9 | 0.356 |
| Vocabulary and aliases | 5 | 0.403 | 8 | 0.472 |
| Corrected requirements | 5 | 0.121 | 8 | 0.574 |
| Missing evidence | 5 | 0.091 | 10 | 0.638 |
| Decision rationale | 5 | 0.429 | 10 | 0.659 |
| Project environment | 5 | 0.849 | 10 | 0.663 |
| Stable configuration | 4 | 0.471 | 10 | 0.882 |

The [revision benchmark](../revision_currency/RESULTS.md) tests a separate question with a deterministic fixture of 600 facts and 35 revision queries.
