# Batching-policy study 2: pre-registration

Written and committed before the first measured run. It supersedes nothing in
[../batching/PREREG.md](../batching/PREREG.md): the first study (n=12 pairs, Haiku) found
that batching cut steps (CI excluded zero) but no significant input-token saving, and its
serial-lookup refusal (harness.serial_lookups=refuse, on the 5th lone lookup in a row)
never fired in any run. It also ran before `devcouncil_script` (the Starlark chaining
tool) existed. This study asks the questions that one could not.

Target: Manvi, binary built with `CGO_ENABLED=0` from the lane checkout, run from a clean
detached clone of the commit that holds the last revision of this file, `manvi map build`
run once. In that clone `bench/batching/` and everything in `bench/batching2/` except
`fixtures/` is deleted before the map is built, so the model cannot read an answer key; the
rest of the tree is byte-identical to the commit. Model: claude-haiku-5-5 via
`MANVI_LLM_PROVIDER_DEFAULT=anthropic MANVI_LLM_TIER_MAIN=claude-haiku-5-5` and
`anthropic-account exec` (account "main", the default). `manvi run --json --max-steps 60
--timeout 10m`. Driver: [run_batching2.py](run_batching2.py), which reuses
[../batching/run_batching.py](../batching/run_batching.py) for the parser, grader, runner and
paired analysis.

## Arms

Arm ids are one letter so a run order is a string. D is the identical-config repeat of C that
this document calls C2.

| id | name | environment (on top of the base above) | prompt |
|---|---|---|---|
| A | batch tool off | MANVI_HARNESS_BATCH_TOOL=off | task prompt |
| C | default | MANVI_HARNESS_BATCH_TOOL=on; MANVI_HARNESS_SERIAL_LOOKUPS removed from the environment, so the default (refuse) applies | task prompt |
| D | C2, noise floor | identical to C | task prompt |
| S | script nudge | identical to C | task prompt plus one paragraph naming devcouncil_script (tasks.json "nudge") |
| R | refusal off | MANVI_HARNESS_BATCH_TOOL=on; MANVI_HARNESS_SERIAL_LOOKUPS=off | task prompt |

With the batch tool off, `devcouncil_script` is withheld with it (devcouncil/tools.go,
`Deps.WithholdBatch`), so A has neither; native parallel tool calls remain possible in every
arm.

## Tasks

Six tasks (tasks.json), 20 repetitions each = 120 units; every unit runs in all five arms
(600 runs). Every answer key is a fact the grader checks (identifier near its value, with
the file name present) and every fact carries `verify` patterns that the unit tests match
against the source it describes; the two trails' keys are re-derived by walking the fixtures.

- `chain-batch-switch` (5 items) and `chain-serial-rule` (6 items), kind `dependent`: each
  item names something the previous answer found (key constant, then the function that reads
  it, then the struct field the result is stored in, and so on). The names are guessable by a
  model that already knows the code, so these are the mild dependent shape.
- `trail-a` and `trail-b` (8 items each), kind `trail`: a pointer chase over 480
  fixture records (bench/batching2/fixtures/ledger/, 12 directories, file names that do not
  say which record they hold). Item 1 names a record; each later item is the record the
  previous record's `refers-to:` line names. A record can be found only by searching for its
  id, so the next lookup cannot be written before the last answer is read. This is the
  shape built so that lone serial lookups happen.
- `limits-fetch` and `session-calibration`, kind `independent`: two of the first study's
  shapes (4 and 5 constants), kept as the case batching favours.

Strata: `chained` = the four dependent and trail tasks (80 units); `trail` = the two trails
(40 units); `independent` = the other two (40 units).

Order: each arm holds each position equally often. The 10 orders are the 5 cyclic rotations of
ACDSR and their reversals; consecutive blocks of 10 units get a shuffle of them from
`random.Random(20261121)`. Up to 4 units run concurrently (the arms of one unit run in
sequence); the order randomisation is what keeps that from favouring an arm.

## Pilot, disclosed

Before this file was committed, 16 runs ($0.1296 by Manvi's own pricing, counted against the
cap, not in the data) checked the keys and the harness:

- C on the three code tasks of an earlier draft. The third, `chain-tier-model` (5 items), was
  dropped for a lighter design: Haiku finished it in 4 steps with one batch, so it adds no
  lone-lookup shape the two kept chains do not.
- C and S on the two trails at two fixture sizes. At 120 records Haiku dumped the whole link
  graph with one search and finished in 3 steps, so the fixture was enlarged to 480 records
  and the trails lengthened from 6 to 8 hops. At 480: C used 369k tokens (10 steps) and 771k
  (20 steps, the refusal fired 3 times, 4 notes); S used 52k and 126k (4 steps, one
  `devcouncil_script` call each). The trail runs are far above the 33.6k scale the power
  calculation below uses, so the spread of the trail deltas is probably larger than planned
  and the stated power is optimistic for P1 to P3.
- A and R on the two trails: A made no batch or script call, R recorded no note and no
  refusal (it used `devcouncil_script` once in one run). `chain-serial-rule` item 3 was
  reworded and its key changed after the first pilot run of that task, and was then run again
  in C and graded 6 of 6.
- `unpriced_steps` was 0 in every pilot row.

No change to tasks or arms was made in response to a token or cost outcome; the changes
above were made for lone-lookup shape and grading. These runs are the only exposure to
outcomes before this commit.

## Primary comparisons

All on total input tokens (`turn.usage` input_tokens, which already includes cache reads and
writes), per unit, paired, difference x minus y (negative favours x). Each comparison pairs the
two arms it names, so a unit missing a third arm still counts. The interval is the percentile
bootstrap on the mean delta (`mh.stats.bootstrap_ci`, 10000 resamples, seed 0) at
alpha = 0.05/3 = 0.0167 (Bonferroni over the three).

- P1: C minus A, all 120 units. Does the default policy save input tokens against no batch tool?
- P2: S minus C, all 120 units. Does naming devcouncil_script in the prompt save tokens against the default?
- P3: C minus R, the 80 chained units. Does the refusal rule save tokens where it can act?

Noise-floor gate: D minus C on all units, same interval. If its CI excludes zero, the
instrument's run-to-run variation exceeds what an identical-config pair should show, and
every primary reading is reported as not interpretable (numbers still given).

Readings, fixed now. For a primary comparison, if the gate holds:
- refuted if x's mean correct_frac is below y's by more than 5 points (paired);
- else supported if the CI lies wholly below zero; harm shown if wholly above zero;
  otherwise not shown (CI contains zero). Not shown is not support.
- P3 is read only if the refusal fired in at least 10 chained runs of C; otherwise it is
  reported as not exercised, and nothing is concluded about the rule.

## Power

Planning spread: the standard deviation of the n=12 paired input-token deltas, recovered from
bootstrap CIs in [../batching/results/stats_prereg_n12.json](../batching/results/stats_prereg_n12.json)
as sd = sqrt(12) (hi - lo) / (2 x 1.96): B-A 33.6k, C-A 21.7k, C-B 29.0k tokens. The
largest, 33.6k, is used. The saving worth acting on is set at 10,000 tokens (about 10% of that
study's arm A mean of 97,979). With a normal approximation to the mean of the deltas,
two-sided alpha = 0.0167 and 80% power, n = ((z(1-alpha/2) + z(0.8)) sd / d)^2 = 119 pairs.
The plan has 120 units. Minimum detectable effects at 80% power and sd 33.6k: 9.9k tokens
at 120 units (P1, P2), 12.2k at 80 units (P3), 17.2k at the 40 trail units (secondary); at the
smaller sd of 21.7k, 6.4k at 120 units. The sd is from a different task mix and 12 points, so
the real power is not known; the C2 pair measures the actual noise.

## Secondary and exploratory (no multiplicity control, 95% intervals)

Every other arm pair (S-A, R-A, C2-C, and P1 to P3 repeated) overall and in each stratum,
on input tokens, cost, steps, tool calls, output tokens and correct_frac. Per arm and stratum,
over all valid runs: runs with a refusal, runs with a serial-lookup note, runs that called
devcouncil_script, runs that called devcouncil_batch, with the call counts. These are
reported whatever the primary result. They are not used to confirm anything.

## Validity, stopping, spend

Isolation: each of the 4 concurrent jobs has its own clean clone, and the session logs a
run leaves in `.devcouncil/sessions/` of its clone (which hold every answer it read) are
deleted after each run, so no run can read another's answers.

Arm checks, fixed now: any A run with a batch or script call, or any R run with a
serial-lookup note or refusal, means that arm did not receive its treatment; every primary
comparison involving that arm is then reported as not interpretable.

A run with a non-zero exit, no usage event or no step is invalid. It is retried once, at once,
in the same arm (the row records `attempts` and the cost of the discarded attempt); a run
still invalid is stored as invalid and its pair drops from the comparisons that need it, listed,
never zero-filled. Spend is Manvi's own pricing of each step (`cost_nano_usd`), not the
provider's invoice; `unpriced_steps` is summed and, if above 0, spend is stated as a lower
bound. Spend cap: $6.00 in total including the $0.1296 pilot. The runner stops starting runs
at $5.75 of measured spend (up to 4 runs in flight can add a few cents) and the study is then
analysed on the units completed, labelled underpowered; no run is added after seeing results
and the cap is not raised. No interim look at outcomes. Rows hold metrics and the graded
answer only; the checkout path and home directory are replaced by `<repo>` and `~` before a
row is stored, and the rows are searched for the user name and the scratch path before they
are committed.

## Known limits, stated in advance

One model, one repository and one fixture, at one commit. The first four tasks are code
reading and a task repeats 20 times, so the 120 units are not independent samples of tasks.
The trail fixture is synthetic, built to make dependent lookups; how often real work has that
shape is not measured here. The nudge is one wording. A percentile bootstrap on 120
heavy-tailed deltas is still an approximation, which is why the C2 gate exists. Haiku's
search path varies by run (the first study saw 191k to 320k tokens on one task).
