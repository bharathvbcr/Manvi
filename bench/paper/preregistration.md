# Preregistration — harness ablation, grid v2

**Status:** written before any v2 episode is run. Nothing in this document may be
revised after the first non-pilot episode; see *Deviations* at the end.

**Registered:** ______ (fill before the run)   **Instrument commit:** ______

---

## 1. Why this is a new experiment, not a replication

Grid v1 (720 episodes, `results/*__hard`) was collected with an instrument that
a subsequent audit found defective in ways that bear directly on the outcome:

| defect | measured effect on v1 |
|---|---|
| the verify gate returned hidden-test output, including expected values, on a failed `finish` | 77 of 760 episodes (10.1%) received it — **only in gate-on configs**, so `full` vs `baseline` was 4-vs-0 and 5-vs-0 asymmetric |
| a `sitecustomize.py` in the sandbox executed inside the verifier | passed 18 of 19 tasks with no task file edited |
| `run_shell` had no containment | `../../tasks/<t>/hidden_test.py` was readable and writable |
| five hard tasks did not enforce their stated contracts | `ot_transform`, `cache_invalidation_dist`, `concurrency_race`, `nfa_match`, `globmatch` each accepted a demonstrably wrong solution |
| `summary.json` was rebuilt from one invocation's task subset | root cause of the published revision-2 correction |

All are fixed. Because the tasks are now stricter and the gate no longer leaks,
**v2 pass rates are not comparable with v1 and the two must never be pooled.**
v1 is retained as evidence about instrument failure, not as a measurement.

An audit of all 760 v1 episodes and 3,879 file-writing tool calls found **zero**
episodes that exploited any of the above. v1's numbers are not fabricated; they
are measured on an instrument that could not rule the exploits out.

## 2. Hypotheses

Directions are stated in advance. Δ is always `full − ablation` on per-repeat
pass rate, paired by seed.

**H1 (primary, confirmatory).** The full harness raises pass rate over
`baseline` for each model: Δ(full − baseline) > 0.
*v1 prior:* Qwen +5.0 [0.0, +10.0], Ornith +7.5 [−5.0, +20.0] — both confounded
by the gate leak, which is why H1 is being re-tested rather than assumed.

**H2 (secondary, confirmatory).** Removing the output cap does not help the
weaker model: Δ(full − no-outcap) ≥ 0.
*v1 prior:* Ornith −10.0 [−17.5, −2.5], i.e. v1 contradicts H2. H2 is stated in
the direction the harness design assumes so that the v1 result, if real, falsifies
it. The v1 pair was roughly leak-balanced (5 vs 7 episodes) and the effect grew
when infrastructure failures were excluded (−10.0 → −13.9).

**H3 (exploratory).** The remaining seven one-flag ablations. Reported as a
family, corrected, and labelled exploratory in the manuscript.

**H4 (exploratory).** Capability × harness interaction,
Δ_weaker − Δ_stronger, per ablation.
*v1 prior:* all eight intervals included zero under both pairing schemes.

## 3. Design

- **Models:** `qwen3.8:27b` (Q4) and `hf.co/ornith-ai/Ornith-1.5-35B-A3B-GGUF:Q8_0`.
  Arms are assigned to "weaker"/"stronger" by **v2** empirical full-harness mean,
  not by parameter count and not by v1.
- **Configs (9):** `full`, `baseline`, and seven one-flag-off cells
  (`no-envboot`, `no-nativetools`, `no-outcap`, `no-checklist`, `no-verifygate`,
  `no-loopbreak`, `no-groundfs`).
- **Tasks (8):** the hard suite — `concurrency_race`, `ast_transformer`,
  `state_machine_fuzz`, `cache_invalidation_dist`, `pratt_parse`, `ot_transform`,
  `nfa_match`, `json_patch`.
- **Seeds:** 20 per cell (`--seed 0 --repeat 20`), seed *r* pinned to repeat
  index *r*, the same seed used for both arms of every paired contrast.
- **Total:** 2 × 9 × 20 × 8 = **2,880 episodes**.
- **Protocol, fixed:** `--max-steps 0 --max-wall 1800 --num-ctx 32768
  --num-predict 4096 --temperature 0.6 --think`, sole tenant, one runner
  revision throughout. Every episode records its own protocol stamp; a cell
  whose episodes disagree publishes no single protocol and is reported as mixed.

## 4. Primary analysis

Per-repeat pass rate = passed / scored episodes in that repeat. Cell estimate =
weighted mean over repeats, weighted by scored-episode count. Intervals are
percentile bootstrap 95% CIs, 10,000 resamples, RNG seed 0.

Interaction and paired deltas use **one index vector per resample applied to
both arms** (seed-paired). The v1 code resampled arms independently, which
published `baseline` exactly 2.00× too wide.

**Decision rule for H1 and H2:** the hypothesis is supported if the 95% interval
excludes zero in the stated direction. No other rule is admissible.

## 5. Multiplicity

- H1 is two tests (one per model). H2 is one test. **α = 0.05 split Šidák-wise
  across these three preregistered tests**: per-test α = 1 − (0.95)^(1/3) = 0.0170,
  i.e. 98.3% intervals for the confirmatory claims.
- H3's seven ablations × two models = 14 tests, reported at 95% **and** with the
  family-wise probability stated. With 16 v1-style intervals, P(≥1 excludes zero
  under the global null) was 56% at nominal coverage and 89% at the measured
  coverage — the manuscript will state this number rather than the phrase
  "the only interval excluding zero."
- H4 is exploratory; no correction, labelled as such.

## 6. Exclusion rule (fixed in advance)

**Excluded from every denominator:** an episode that produced zero output tokens,
made at most one step, and stopped on a timeout (`wall_timeout`, or an error
whose text names a timeout). This is the instrument failing to take a
measurement. It is the only exclusion.

**Not excluded:** every other outcome, including `context_exhausted`, a non-timeout
`error:*`, and a wall timeout that produced output. These are results.

Cells are reported with their excluded count. A cell losing more than **10%** of
its episodes to exclusion is reported as degraded and is not used for H1 or H2.

## 7. Infrastructure-failure rule (fixed in advance)

Serving errors that are not timeouts (`error:ModelError` from an HTTP 500, a
malformed response, and similar) are **scored as failures** in the primary
analysis, and **both** of the following are reported for every cell:

1. the primary rate, and
2. a sensitivity rate with those episodes removed, alongside the per-cell
   infrastructure-failure rate.

Neither is chosen after seeing the data. In v1 this rate ran 0–2.5% for Qwen and
10–25% for Ornith, and removing them flipped the sign of Ornith's
full-vs-baseline delta — which is why it is a preregistered dual report rather
than a post-hoc sensitivity analysis.

## 8. Stopping rule

All 2,880 episodes are run. **No interim analysis, no peeking, no early stop.**
If the run is interrupted, it resumes into the same cells; the analysis is
performed only when every cell holds 20 complete repeats. A cell that cannot be
completed is reported as incomplete and excluded from H1 and H2.

## 9. Power

v1's resampled power, computed on v1 data with v1 tasks, gave for n=20:
Qwen full-vs-baseline 98%, Ornith full-vs-baseline 60%, Ornith `no-outcap` 100%.
**These are priors, not predictions**: the tasks changed, so the per-repeat
variance will change. Ornith full-vs-baseline was the weakest at 60% and may
remain underpowered at n=20; that will be stated rather than discovered.

Power is not recomputed after the run to justify an outcome.

## 10. What would falsify each hypothesis

- **H1 falsified** if the 98.3% interval for Δ(full − baseline) lies at or below
  zero for a model. A null (interval containing zero) is reported as
  "not detected at this n", not as support.
- **H2 falsified** if Δ(full − no-outcap) < 0 with the interval excluding zero —
  i.e. if v1's finding replicates on a clean instrument.
- **H4** cannot be falsified at this n; it is reported as underpowered unless an
  interval excludes zero.

## 11. Pilot gate (runs before the registered experiment)

144 episodes — 18 cells × 1 seed × 8 tasks. Its purpose is feasibility, not
inference; **its data is discarded and not pooled.**

Proceed only if all four hold:

1. **Containment is active.** `containment_proves_itself()` returns ok on the run
   host. If not, the experiment does not start.
2. **The suite is not saturated.** No task is passed by the stronger model in
   every cell. If ≥3 of 8 are, the task set is revised *before* registration and
   this document is re-registered.
3. **The suite is not floored.** The weaker model's full-harness rate is ≥ 20%.
   If below, the task set is revised and this document is re-registered.
4. **Throughput is within 1.5× of the projection** (181 s mean episode × the
   host's measured scaling). Otherwise the budget is re-derived before committing.

## 12. Deviations

Any departure from this document after the first registered episode is recorded
in a `DEVIATIONS` section of the manuscript with its date, its reason, and
whether it was made before or after the deviating data were seen. An analysis
choice made after seeing data is reported as exploratory regardless of how it
was arrived at.

---

## Appendix — what v1 is used for

v1 is not re-analysed for effect sizes. It appears in the manuscript as the
instrument-failure record: the defects in §1, the finding that no episode
exploited them, the protocol drift across its cells, and the two measurement
failures already documented (a mid-grid turn-ceiling change that inflated the
same contrast to +22.5, and difficulty-correlated censoring in an API arm).
