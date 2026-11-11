# E26 — French prompts against English facts, outside E23

Date: 2026-10-25. Dev install with 0057's reranker settings (30 candidates, 128 tokens). Carried from M2 (0055): in E23, 5 of the 9 French prompts where only the reference injected something useful had nothing above backsight's floors. 0056 requires the fix to be evaluated on prompts outside E23's. **V** = observed.

## Data
- E23 took every French prompt of at least 60 characters typed on this machine (its extraction balances languages). Outside E23 there are 12 English prompts before 2026-10-24, and 8 after (E24's pool).
- Paired set:
  - **Prompts**: E19's 11 real prompts that are not among E23's and not from the work repository, each in English and French. One version is the original (3 are French) and the other E19's translation.
  - **Keywords**: E19's 23 agent keyword queries whose French translation differs (made for E26 with E19's translation prompt, `deepseek-v4-flash`, ≈ $0.01).
- Scripts: `E26-scripts/`.

## 1. Calibration on the global scope (**V**, `calib.py`)
For each pair, both versions are recalled (observations, budget low), and the reranker score of each memory both answers return is compared.

| | Shared hits | Gap FR − EN, median | English hits ≥ 0.5 falling under 0.5 in French |
|---|---|---|---|
| all | 490 | +0.000 | 26 / 111 |
| prompts | 95 | +0.002 | 0 / 2 |
| keywords | 395 | +0.000 | 26 / 109 |

On English-strong hits (≥ 0.5) the gap has a median of −0.028 but a long tail (first decile −0.49). French does not shift scores uniformly: specific pairs collapse.

A lower French floor, scored against the English twin's ≥ 0.5 hits as a silver standard:

| French floor | English-strong hits recovered (of 120) | Hits English scored < 0.3 |
|---|---|---|
| 0.5 | 85 | 14 |
| 0.4 | 92 | 17 |
| 0.35 | 96 | 20 |

## 2. On the prompt path (**V**, `path.py`)
Each French version goes through `backsight recall-prompt`, with the floors as they are and lowered by 0.1 when the prompt is detected as French (function-word count, E23's heuristic). The result is identical: 21 prompts with an injection (English twin 27), and 25 of 46 English injections recovered. The cause:
- 18 of the 23 French keyword versions carry too few function words to be detected as French (5 are);
- of the 11 real prompts, only 3 have a hit at all.

## Conclusions
1. With 0057's settings, French queries lose part of the English-strong hits through a tail of collapsed pairs, not through a shift that a floor could correct.
2. A French floor shift recovers little (7 of 120 at 0.4, for 3 more noise hits), and nothing on the prompt path for the data outside E23.
3. **Decision**: no French-specific floor (decision 0058). The prompt event records the prompt's language, and `stats` shows recalls and injections per language. E24 (M4) measures French prompts typed after 2026-10-24 under 0057's settings, and a fix is taken only from evidence there.

## Limits
Few real French prompts exist outside E23. The keyword stratum is translated, not typed.

## Amendments (M3 block 3 review, 2026-10-25)
- **Calibration**: it rests almost entirely on translated keyword queries (109 of 111 English-strong hits, 395 of 490 shared hits), which never reach the prompt hook. "A tail of collapsed pairs, not a shift" is a keyword-query finding.
- **Power**: the prompt-path test has almost none, so "changes nothing" is absence of evidence. E26 is **inconclusive**: no change by default (INDEX row on 0058).
- **Silver standard**: it treats the English twin's score ≥ 0.5 as relevance. It cannot credit a relevant hit that only French finds.
