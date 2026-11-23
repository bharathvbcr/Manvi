# Batching-policy benchmark: pre-registration

Written and committed before any measured run. Target: Manvi at commit
f29f2d9 (clean detached checkout, `manvi map build` run once), binary built from
that commit with CGO_ENABLED=0. Model: claude-haiku-5-5 via the default
anthropic-account account. Driver: bench/batching/run_batching.py.

Arms (all with MANVI_LLM_PROVIDER_DEFAULT=anthropic MANVI_LLM_TIER_MAIN=claude-haiku-5-5):

- A: MANVI_HARNESS_BATCH_TOOL=off (no batch tool; native parallel tool calls remain possible).
- B: batch tool on, MANVI_HARNESS_SERIAL_LOOKUPS=off (offered, not forced).
- C: batch tool on, serial lookups at the default (refuse).

Design: 4 read-only multi-lookup tasks (tasks.json, 4-5 facts each, answer key
written from the code and checked against the source), 3 reps each = 12
task-reps, each run in all three arms; arm order rotates per task-rep through
all six permutations. Unit of analysis: the task-rep (paired). `--max-steps 60
--timeout 10m`.

Primary comparison: paired per-task-rep delta of total input tokens
(turn.usage input_tokens, which Manvi already defines as including cache
reads and writes) and of cost (cost_nano_usd), for B-A and C-A, with a
percentile bootstrap 95% CI on the mean of the 12 deltas (mh.stats.bootstrap_ci,
10000 resamples). Secondary: steps, tool calls, output tokens, correctness
(facts right out of facts asked, graded automatically), C-B.
Runs with a non-zero exit, no usage event or no step are invalid; a task-rep
missing any valid arm is dropped from all arms and reported, never zero-filled.

Refutation of "batching saves tokens". The claim is refuted for an arm X in
{B, C} if either: (1) the paired CI of input_tokens delta X-A does not lie
wholly below zero (zero inside the CI, or CI above zero), or (2) X's mean
correctness (fraction of facts right) is lower than A's by more than 5 points
or X has more incorrect facts than A in the pooled count. A mean below A with a
CI containing zero is "not shown", not "supported". The same test is applied to
cost. A result where B and C both fail the test is a null/refuting result and
will be reported as such.

Known limits stated in advance: n=12 pairs on one model and one repository;
the interval is a percentile bootstrap on 12 points and under-covers; four
tasks are a small sample of lookup shapes; the tasks are constant lookups, so
they favour batching if it favours anything; token totals depend on
Haiku's search path, which the earlier n=3 runs showed varies by 191k-320k.
