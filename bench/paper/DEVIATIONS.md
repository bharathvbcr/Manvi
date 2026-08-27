# Deviations — registered v2 grid, tag `v2`

Companion to [`preregistration.md`](preregistration.md), which requires (§12) that
any departure be recorded with its date, its reason, and **whether it was made
before or after the deviating data were seen**. That last clause is the whole
point of this file, so each entry states exactly what had been observed at the
moment the choice was made.

**Instrument commit:** `82e453a` — the §Registered header leaves this blank; it
is filled here. Verified byte-identical on the run host across `mh/runtime.py`,
`mh/tools.py`, `mh/harness.py`, `mh/model.py`, `mh/pool.py`, `mh/stats.py`,
`grid.py` and `run.py`.

**Run host:** Lambda 1×GH200 480GB (97,871 MiB, 900 W cap), aarch64
Neoverse-V2 ×64, Ubuntu 22.04.5, Python 3.10.12, ollama 0.33.0, containment
backend `bwrap` 0.6.1. Stamped on every episode as `env_*`.

**Tag:** `v2`. The frozen v1 grid lives under `*__hard` and must never be pooled
with this one (§1); a separate tag is what enforces that, because
`compare.py --tag hard` pools every directory ending in that tag.

---

## Timeline

The order matters more than the clock, so it is given as a sequence:

1. Harness defects fixed and committed (`855acc1`, `82e453a`). Full suite green
   on both macOS and the run host.
2. Pilot cell run under tag `v2-pilot`: 8 episodes, qwen `full`, 1 seed.
3. **D1 taken** — registered grid launched without the full §11 gate.
4. Grid running; qwen `full` incomplete (~111 episodes).
5. **D2a taken** — Ornith reduced from 20 seeds to 10. *No cell complete; no
   pass rate known for any cell.*
6. qwen `full` completed: 152/160 = 95.0%.
7. **D2b taken** — seeds reallocated by hypothesis. *qwen's `full` marginal rate
   and per-task profile were known. No ablation cell existed, so no Δ, no
   contrast, and no hypothesis test had been evaluated for either arm.*
8. **D2c taken** — `no-outcap` promoted back to 20. Same information state as 7.

Nothing in steps 5, 7 or 8 was decided with a delta in hand. What *was* known at
7 and 8 is stated plainly in D2 below, because "before the deviating data" is a
claim this document should not make loosely.

---

## D1 — §11 pilot gate not run in full

**Registered:** 144 episodes (18 cells × 1 seed × 8 tasks), discarded, clearing
four conditions before the registered experiment starts.

**Actual:** 8 episodes — one cell, qwen `full`, 1 seed, tag `v2-pilot`.
Discarded and not pooled, as registered.

**Reason:** the pilot's purpose is feasibility, and three of its four conditions
were answerable from one cell. The fourth was not, and that is the cost.

| §11 condition | status |
|---|---|
| 1. Containment is active | **cleared** — `containment_proves_itself()` returns ok on the run host, and `bwrap` is recorded as the backend in every one of the 8 episodes |
| 2. Suite is not saturated | **NOT cleared** — needs several cells; see O1 |
| 3. Suite is not floored (weaker arm ≥ 20%) | **cleared** — 7/8 = 87.5% |
| 4. Throughput within 1.5× of projection | **cleared** — 147 s/episode measured against the 181 s projection |

**Decided:** before the first registered episode.

**Consequence:** condition 2 went untested before launch, and a saturation
signal has since appeared (O1). Had the full gate run, that signal would have
been available *before* committing GPU time, which is what §11 exists for.

---

## D2 — §3 seed allocation weighted by hypothesis rather than uniform

**Registered:** 20 seeds for every one of 18 cells. 2 × 9 × 20 × 8 = 2,880
episodes.

**Actual:** 1,440 episodes —

| cells | seeds | hypothesis |
|---|---|---|
| `full`, `baseline` (both arms) | 20 | H1, confirmatory |
| `no-outcap` (both arms) | 20 | H2, confirmatory |
| the other six ablations (both arms) | **5** | H3, exploratory |

**Reason:** the registered design spent 20 seeds uniformly, which left the
*weakest* contrast in the experiment underfunded relative to the exploratory
family. §9 records Ornith's `full`-vs-`baseline` at 60% power **even at n=20** —
the lowest in the design — while §5 already labels the seven one-flag ablations
exploratory. Reallocating moves samples from tests that were never going to be
confirmatory into the two that are.

Both confirmatory hypotheses therefore run at the depth §9's power calculation
justified: H1 at n=20 on both arms, and H2 at n=20 on both arms (§9: "Ornith
no-outcap 100%"). H2 in particular must clear a **98.3% interval** under §5's
Šidák split, which is materially wider than 95% and so is more sensitive to n
than any other test here.

**Decided:** in two steps, at points 5 and 7–8 of the timeline.

- The Ornith reduction (D2a) was decided with **no cell complete and no pass
  rate known**.
- The reallocation and the `no-outcap` promotion (D2b, D2c) were decided **after
  qwen's `full` cell completed at 152/160 (95.0%)**. That cell's marginal rate
  and its per-task profile were known. No ablation cell existed at that moment,
  so no Δ, no contrast and no hypothesis test had been evaluated for either arm.

**This is not a fully blind deviation and is not claimed as one.** qwen's
`full` rate informed the reasoning: at 0.950 against a ceiling of 1.0, an
ablation can exceed `full` by at most 0.05, so H3 effects on that arm are
necessarily small and n=5 versus n=20 changes little about what is resolvable
there. A reader is entitled to weigh that. What did *not* inform it is any
comparison, which is what H1, H2, H3 and H4 are all made of.

**Consequence:** the six H3 ablations run at v1's sample size, and inherit v1's
statistical weakness. Two specifics belong in the manuscript rather than being
left to a reader inferring from an `n` column:

1. **Interval coverage degrades.** Measured on the completed `ext-cerebras`
   grid, the percentile bootstrap delivered 94.1% against 95% nominal at n=20.
   At n=5 it is materially worse — this is the defect §5 was written to address,
   and these six cells reintroduce it.
2. **H4 on those six is not inferential.** The capability × harness interaction
   over the six reduced ablations will be very wide and should be reported as
   descriptive. H4 on the *primary* contrast is unaffected: both arms hold
   `full` and `baseline` at n=20.

The six reduced cells should be presented as descriptive, not as tests.

---

## O1 — Saturation signal (an observation, not a deviation)

Recorded here because §11's condition 2 was never cleared (D1) and this is the
evidence that would have decided it.

qwen `full`, complete at 160 episodes over 20 repeats: **152/160 = 95.0%**,
95% CI [0.900, 0.988].

- Per-repeat rates take four distinct values (0.625, 0.75, 0.875, 1.0), so the
  estimator has spread and `ci_degeneracy()` reports none. The bootstrap is not
  degenerate.
- **14 of 20 repeats sit at exactly 1.000.**
- Three of eight tasks passed in every repeat of this cell: `json_patch`,
  `ot_transform`, `pratt_parse`.

§11.2 revises the task set when ≥3 of 8 tasks are passed **in every cell**.
Three are saturated in *this* cell; whether they are saturated in every cell
cannot be known until the ablations complete, and a task saturated under `full`
may well fail under `baseline`. So this is not a breach of the gate — it is the
early warning the gate was designed to produce, arriving after launch instead of
before it.

**Implication for interpretation:** with `full` at 0.950 against a hard ceiling
of 1.0, a component that *hurts* qwen can show a Δ of at most −0.05. The
`ext-cerebras` arm found exactly such a result (`no-checklist`, Δ = −0.075,
95% CI [−0.144, −0.006]); qwen structurally cannot reproduce an effect of that
size. Large positive Δ (H1) remains fully detectable. This asymmetry should be
stated wherever qwen's H3 nulls are reported, so that "not detected" is not read
as "not present".

---

## What has *not* been deviated from

- **§4 primary analysis** — per-repeat pass rate, weighted mean over repeats,
  percentile bootstrap, 10,000 resamples, RNG seed 0, one index vector applied
  to both arms.
- **§5 multiplicity** — confirmatory claims at 98.3% intervals; H3 at 95% with
  the family-wise probability stated.
- **§6 exclusion rule** — zero output tokens, at most one step, stopped on a
  timeout. Still the only exclusion. Notably, the account-refusal rows that the
  `ext-cerebras` run produced are *not* excluded by any rule; they are re-run.
- **§7 infrastructure-failure dual report.**
- **§8 stopping rule** — all cells run to completion, no interim analysis, no
  early stop. The observations in this document are marginal rates and
  operational health, not contrasts; no hypothesis has been evaluated.
- **Protocol** — `--max-steps 0 --max-wall 1800 --num-ctx 32768
  --num-predict 4096 --temperature 0.6 --think`, sole tenant, `concurrency 1`,
  one runner revision (`82e453a`) throughout. Stamped on every episode.
- **Tasks** — the same eight hard tasks, unrevised.
