# Batching-policy benchmark: results

Pre-registration: [PREREG.md](PREREG.md) (committed before any measured run).
Raw rows: [results/runs.jsonl](results/runs.jsonl) (metrics and the graded answer only; no tool output).
Statistics: [results/stats_prereg_n12.json](results/stats_prereg_n12.json), [results/stats_pooled.json](results/stats_pooled.json).
Setup: Manvi at f29f2d9, binary built with CGO_ENABLED=0, clean detached checkout with a built map,
claude-haiku-5-5 on Anthropic via the default anthropic-account account,
`manvi run --json --max-steps 60 --timeout 10m`. 4 read-only lookup tasks (tasks.json, 4-5 constants each).
Arms: A batch tool off; B batch on, serial lookups off; C batch on, serial lookups default (refuse).
Reproduce: `python3 run_batching.py run ...` then `analyze` (see the driver's docstring); `python3 test_batching.py`.

Deltas are paired per task-rep: first arm minus second, so negative favours the first arm.
Intervals are percentile bootstrap 95% CIs on the mean delta (mh.stats.bootstrap_ci, 10000 resamples).
"neg/pos/zero" counts task-reps by delta sign. Input tokens are Manvi's `turn.usage` input_tokens, which already includes cache reads and writes.

## Pre-registered batch: 12 task-reps (reps 0-2 of 4 tasks), 36 runs, $0.162

| Arm | input tokens mean / median | cost (USD) mean / median | steps | tool calls | batch calls (inner) | refusals | facts right |
|---|---|---|---|---|---|---|---|
| A | 97979 / 91266 | 0.0046 / 0.0044 | 4.58 / 5.00 | 7.50 / 8.00 | 0 (0) | 0 | 57/57 |
| B | 95895 / 92228 | 0.0047 / 0.0046 | 3.92 / 4.00 | 3.00 / 3.00 | 28 (92) | 0 | 57/57 |
| C | 89668 / 83467 | 0.0042 / 0.0044 | 4.00 / 4.00 | 3.33 / 3.00 | 32 (101) | 0 | 57/57 |

| Delta (paired) | input tokens: mean [95% CI], neg/pos/zero | cost (USD x1e-3) | steps |
|---|---|---|---|
| B-A | -2085 [-22049, 16011] 4/8/0 | 0.05 [-0.67, 0.85] 6/6/0 | -0.67 [-1.08, -0.25] 6/0/6 |
| C-A | -8311 [-20933, 3627] 8/4/0 | -0.39 [-1.03, 0.20] 8/4/0 | -0.58 [-1.08, -0.08] 7/2/3 |
| C-B | -6227 [-23035, 9773] 6/6/0 | -0.44 [-1.15, 0.15] 7/5/0 | 0.08 [-0.33, 0.50] 3/4/5 |

Against the pre-registered refutation rule: neither B nor C has an input-token CI wholly below zero
(B-A [-22049, 16011], C-A [-20933, 3627]),
so "batching saves tokens" is **not shown at n=12**, and by the rule as written it counts as not supported (the same holds for cost). Correctness did not differ (57/57 in every arm).
What is shown: batching cuts steps (B-A and C-A CIs exclude zero) and tool calls per run (7.5 to 3.0-3.3), because arm A's native parallel calls
already fetch several things per step but need more steps.

## Extension: reps 3-7 added, 32 task-reps pooled, 96 runs, $0.434 total

This extension was decided after seeing the n=12 null, so it is exploratory, not the pre-registered test.

| Arm | input tokens mean / median | cost (USD) mean / median | steps | tool calls | batch calls (inner) | refusals | facts right |
|---|---|---|---|---|---|---|---|
| A | 106067 / 98483 | 0.0046 / 0.0044 | 4.72 / 4.50 | 7.75 / 8.00 | 0 (0) | 0 | 152/152 |
| B | 104736 / 93889 | 0.0047 / 0.0046 | 4.16 / 4.00 | 3.50 / 3.00 | 78 (257) | 0 | 151/152 |
| C | 91488 / 91075 | 0.0043 / 0.0043 | 3.97 / 4.00 | 3.22 / 3.00 | 82 (271) | 0 | 151/152 |

| Delta (paired) | input tokens: mean [95% CI], neg/pos/zero | cost (USD x1e-3) | steps |
|---|---|---|---|
| B-A | -1331 [-20314, 16052] 12/20/0 | 0.16 [-0.42, 0.76] 14/18/0 | -0.56 [-1.00, -0.19] 14/4/14 |
| C-A | -14580 [-28824, -2167] 21/11/0 | -0.30 [-0.74, 0.15] 21/11/0 | -0.75 [-1.16, -0.34] 18/4/10 |
| C-B | -13249 [-25276, -1772] 18/14/0 | -0.47 [-0.89, -0.06] 20/12/0 | -0.19 [-0.53, 0.16] 10/7/15 |

## Reading

- Steps: batching reduces steps in both arms (pooled B-A -0.56 [-1.00, -0.19] 14/4/14, C-A -0.75 [-1.16, -0.34] 18/4/10); CIs exclude zero in both batches.
- Tokens and cost: B is indistinguishable from A (pooled B-A input tokens -1331 [-20314, 16052] 12/20/0).
  C is lower than A pooled (C-A CI excludes zero for input tokens, not for cost), but not at n=12.
- **C-B is not evidence for the refusal rule.** The rule never fired in any run: 0 refusals, and no serial-lookup note appears in the saved streams of B or C
  (the models batched or made two or three calls, so the third-lone-lookup threshold was never reached). B and C therefore received the same treatment in practice, yet the pooled
  C-B input-token CI also excludes zero (-13249 [-25276, -1772] 18/14/0). That is the noise floor made visible: the bootstrap on a handful of heavy-tailed paired deltas under-covers, and a CI excluding zero here can come from
  run-to-run search-path variation alone. The C-A pooled result is subject to the same caveat and should not be read as an effect of the refusal rule.
- Arm A has the heaviest tail (single runs up to 245k input tokens when Haiku searched more widely); a mean that includes one such run moves by about 5k tokens.
- Correctness: 152/152 facts in A, 151/152 in B and C. The one miss (compaction rep 6, B and C) is an ambiguous item: the model named `editKeepToolUses` = 3 in
  llm/anthropic/adapter.go, a different, similarly purposed constant, instead of serverKeepToolUses in agent/compaction.go. Grading was left as pre-registered.

## Limits

- One model (Haiku 5.5), one repository at one commit, four task shapes of constant lookups, n=12 pre-registered and n=32 exploratory; the 4 tasks repeat, so the task-reps are not independent samples of "tasks".
- No run in this benchmark exercised the refusal path, so nothing here measures its cost or benefit.
- The first 15 runs were produced before a parser fix (the final answer was read as the last stream chunk, so every row graded 0); they were re-parsed and re-graded from their saved NDJSON, not re-run. One run in flight when the first job was stopped for that fix may have spent a few cents not counted in the totals (under $0.01 by the per-run cost scale).
- A `notice` ("results produced under these settings are not strict") appears in every run in every arm; it is a constant of the environment, not a treatment.
