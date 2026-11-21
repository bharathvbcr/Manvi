# Batching-policy study 2: results

Pre-registration: [PREREG.md](PREREG.md), committed (8d5561a7) before the first measured run.
Rows: [results/runs.jsonl](results/runs.jsonl), one per run (metrics and the graded answer, no tool output).
Statistics: [results/stats_prereg.json](results/stats_prereg.json), from `run_batching2.py analyze`.
Setup as pre-registered: Manvi at 8d5561a7 (binary built with CGO_ENABLED=0), four clean detached clones with
`bench/batching/` and all of `bench/batching2/` but `fixtures/` removed, `manvi map build` once in each,
claude-haiku-5-5 on Anthropic via the default anthropic-account account, `manvi run --json --max-steps 60 --timeout 10m`,
4 units in parallel. Arm ids: A batch off; C default; D = C2, identical repeat of C; S default plus the devcouncil_script
nudge; R serial lookups off.

Complete: 120 units, 600 runs, none invalid, none retried, none dropped; `unpriced_steps` 0. Spend $3.597 measured (Manvi's
own pricing, not an invoice) plus $0.1296 pilot = $3.727 of the $6.00 cap; the runner never reached its $5.75 stop.
Arm checks held: no A run made a batch or script call; no R run had a serial-lookup note or refusal.
Correctness: facts right A 719/720, C 719/720, C2 718/720, S 718/720, R 717/720.

## Pre-registered primary results

Input tokens (`turn.usage` input_tokens), paired per unit, x minus y, percentile bootstrap CI at 98.33% (alpha 0.05/3).
neg/pos/zero count units by delta sign.

| | comparison | n | mean delta [98.33% CI] | neg/pos/zero | correct_frac delta | reading |
|---|---|---|---|---|---|---|
| P1 | C - A, all units | 120 | -69,721 [-114,314, -28,875] | 75/45/0 | 0.000 | supported |
| P2 | S - C, all units | 120 | -64,774 [-100,714, -34,607] | 76/44/0 | -0.001 | supported |
| P3 | C - R, chained units | 80 | -32,581 [-101,791, +39,137] | 44/35/1 | +0.005 | not shown |
| gate | C2 - C, all units | 120 | +7,431 [-40,359, +55,882] | 56/64/0 | | holds (CI contains zero) |

The noise-floor gate held, the refusal fired in 15 chained runs of C (at least the 10 required, so P3 is read), and neither arm
check failed, so all three readings stand as pre-registered. P1 and P2 are supported; P3, the refusal rule, is not shown: a
saving of -32.6k tokens on average with a CI from -102k to +39k and a 44/35 sign split.

Arm means over all 120 units: input tokens A 245,117; C 175,395; C2 182,827; S 110,621; R 197,031. Steps A 9.63; C 6.50; C2 6.67;
S 4.69; R 7.08. Cost per run A $0.0068; C $0.0057; C2 $0.0063; S $0.0047; R $0.0064.

## How much each mechanism ran

Runs (of 120 per arm; of 40 trail, 40 other chained, 40 independent) that exercised it, over all valid runs:

| arm | runs with a refusal (refusals) | runs with a serial note | runs that called devcouncil_script (calls) | runs with a batch call |
|---|---|---|---|---|
| A | 0 | 0 | 0 | 0 |
| C | 15 (32) | 33 | 26 (26) | 85 |
| C2 | 13 (32) | 33 | 26 (26) | 76 |
| S | 0 | 3 | 40 (45) | 87 |
| R | 0 | 0 | 20 (20) | 74 |

- The refusal rule fired, which it never did in the first study: in 15 runs of C and 13 of C2. In C, 14 of those were trail runs
  (14 of 40) and 1 was a dependent code-chain run; no independent run was refused. Haiku sent a refused lookup again and it
  ran, as designed, and every one of those runs still answered correctly.
- The script tool was used without any nudge: 26 of 40 trail runs in C and in C2, 19 of 40 in R, never in the independent tasks.
  With the nudge, 38 of 40 trail runs and 2 dependent runs used it, and S needed 4.22 steps on the trails against 9.07 for C.
- The independent tasks used the batch tool (38 to 40 of 40 runs in every batch-on arm) and never the script tool.

## Secondary and exploratory (95% intervals, no multiplicity control)

Input-token deltas by stratum (mean [95% CI], neg/pos); the full set, with cost, steps, output tokens, tool calls and
correctness, is in results/stats_prereg.json.

| comparison | all (120) | chained (80) | trail (40) | independent (40) |
|---|---|---|---|---|
| C - A | -69.7k [-104.7k, -36.0k] 75/45 | -101.6k [-151.8k, -51.9k] 56/24 | -189.8k [-275.4k, -99.8k] 29/11 | -6.0k [-28.2k, +14.4k] 19/21 |
| S - C | -64.8k [-92.8k, -39.1k] 76/44 | -84.6k [-125.9k, -48.2k] 49/31 | -158.1k [-232.7k, -92.0k] 25/15 | -25.0k [-39.8k, -11.0k] 27/13 |
| C - R | -21.6k [-60.1k, +16.5k] 63/56 | -32.6k [-91.1k, +24.8k] 44/35 | -69.3k [-180.5k, +48.5k] 26/14 | +0.3k [-16.0k, +16.9k] 19/21 |
| S - A | -134.5k [-170.3k, -100.7k] 94/26 | -186.2k [-236.2k, -139.5k] 68/12 | -347.8k [-409.1k, -286.4k] 39/1 | -31.1k [-49.3k, -14.4k] 26/14 |
| R - A | -48.1k [-80.9k, -17.0k] 65/55 | -69.0k [-115.9k, -23.5k] 46/34 | -120.5k [-208.5k, -32.6k] 24/16 | -6.3k [-28.4k, +13.4k] 19/21 |
| C2 - C | +7.4k [-31.5k, +47.1k] 56/64 | +10.6k [-47.9k, +68.6k] 38/42 | +17.6k [-98.6k, +133.5k] 20/20 | +1.2k [-17.7k, +18.8k] 18/22 |

Steps, C - A: all -3.13 [-4.08, -2.25], trail -7.88 [-9.72, -5.90]. Steps, S - C: all -1.81 [-2.60, -1.10], trail -4.85.
Cost (USD x1e-3), C - A all -1.12 [-2.54, -0.14]; S - C all -0.97 [-1.46, -0.53]; C - R all -0.71 [-2.02, +0.22].

Readings, with their limits stated:

- The savings are concentrated where lookups depend on each other. On the two trail tasks C is 190k tokens and 7.9 steps below A;
  on the independent tasks the C - A CI contains zero (-6.0k [-28.2k, +14.4k]), as in the first study.
- The nudge (S - C) lowers tokens on the trails, where the script tool is used. It also lowers them on the independent tasks
  (-25.0k [-39.8k, -11.0k], 27/13) where no run in S or in C called devcouncil_script, so on those tasks the saving is not the
  script tool. The nudge paragraph is the only difference between S and C there; this was not a pre-registered comparison and
  is one of 24 unadjusted intervals, so it is a lead to test, not a finding.
- R - A is below zero too (all -48.1k [-80.9k, -17.0k]): R has the batch tool, the script tool and no refusal, so much of
  P1's saving comes from the tools and not from the refusal.
- Refusal (P3 and C - R): no effect is shown, in either direction. The refusal fired in 15 of the 80 chained C runs, the other 65
  were not affected by it, which dilutes any effect on the stratum mean. C - R on the trails (-69.3k [-180.5k, +48.5k], 26/14) leans
  toward a saving and is far from conclusive.
- The noise floor is wide. C2 - C has CI [-40.4k, +55.9k] at the primary level, and two identical arms differed by more than 20k
  input tokens in 94 of 120 units and by more than 40k in 67 (median absolute difference 58.8k; signs split 56/64). A mean
  difference of a few tens of thousands of tokens between two arms is not distinguishable from run-to-run variation here.

## What the pre-registered power claim got wrong

The plan assumed a paired-delta standard deviation of 33.6k tokens (from the first study, whose tasks were all light) and reported
80% power for a 10k-token saving at 120 units. The delta spreads observed here, recovered from the primary CIs the same way, are
about 196k (P1), 220k (C2 - C) and 263k (P3), several times larger, because trail runs use between 39k and 886k input tokens each. At the
observed spread the minimum detectable effect at 120 units is about 58k tokens, not 10k; P1 and P2 were detected because their
effects (-70k, -65k) are larger than that, and a real effect of the 10k that the plan was sized for would most likely have gone
undetected. P3's null is therefore not evidence of no effect: its interval spans -102k to +39k.

## Limits

One model (Haiku 5.5), one repository at one commit, six tasks that repeat 20 times each, so the 120 units are not independent
samples of tasks. The trail fixture is synthetic and was built to make lookups depend on each other; real work may rarely have
that shape. The trails dominate the totals (a trail run uses 2.5 to 4 times the input tokens of an independent-task run, by arm: 272k
against 110k in C, 462k against 116k in A), so the all-units means mostly describe the trails. The nudge is one wording. Percentile bootstrap intervals on 120 heavy-tailed paired deltas are
approximations, and the 24 secondary intervals are unadjusted. The first-study caveats on one model and on constant lookups
that favour batching still apply to the independent tasks.
