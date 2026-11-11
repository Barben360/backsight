# E17 — Does a retro pass add knowledge over session retain under project-only pools?

Date: 2026-10-24. Sandbox 1.1.0, two my-garage sessions (2026-10-04, 2026-10-08), same content written three ways, each in its own project pool (`[["project:<p>"]]`). **V** = observed.

| Pool | Writes | Raw facts | Observations | Notes |
|---|---|---|---|---|
| `e17chat` | session retain, stock `conversation` strategy | 9 | 9 | same core knowledge; **session 1 in English** |
| `e17dur` | session retain, `conversation` mission + the retro rules (language, durable only, typed, absolute dates) | 11 | 11 | French; adds "Décision : la rétention de kube-prometheus-stack n'a aucune importance" and an explicit final state |
| `e17both` | session retain (stock) + retro strategy | 25 | 9 | French; adds one generalised lesson (a stuck active job blocks a CronJob) |

Reading: under project-only pools, consolidation merges both writers, and the retro mostly re-extracts what session retain already holds, for a second extraction cost. Putting the retro's rules (language, durability, types, dates) into the `conversation` strategy gives the same coverage in one pass. Limit: short sessions (2-3 prompts); long sessions chunked at 12 000 chars may lose cross-chunk synthesis that the retro's 30 000-char tranches see — the end-of-project `memory-retro` skill (agent-written) covers that case.

Decision input: no retro job in v1; `conversation` strategy carries the durable/language rules; `kind` labels on every strategy; telemetry compares weekly fact quality with the Python retro during M2-M3 on the author's machine.
