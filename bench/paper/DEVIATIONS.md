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

**Driver script:** [`run_v2.sh`](run_v2.sh), committed here verbatim, md5
`20f4e46b31a8dd4d463122cf75181e46` — byte-identical to the copy executing on the
run host. It was not previously under version control, which left the six-phase
chaining and the per-phase seed counts undocumented outside this file. Its header
comment is *not* authoritative about blindness; see the note at the end of D2.

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

**A conflicting label in the driver script.** `run_v2.sh`'s header comment reads
"Decided blind: qwen's baseline cell had not finished when this was set, so no
delta had been seen for either arm." Both of those facts are true and are
restated above. The label "decided blind" is not: qwen's `full` cell *had*
completed when D2b and D2c were taken. The comment is left unedited because the
script is the run record and rewriting it after the fact would be worse than the
overstatement; **this entry is authoritative where the two disagree.**

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
could not be known until the ablations completed.

**Resolved — the gate is cleared.** With all nine qwen cells complete, the
registered check was evaluated across the full arm:

| task | worst cell | passed in every cell? |
|---|---|---|
| `json_patch` | 1/5 (`no-verifygate`) | no |
| `concurrency_race` | 9/20 (`baseline`) | no |
| `ast_transformer` | 3/5 (`no-verifygate`) | no |
| `state_machine_fuzz` | 3/5 (`no-verifygate`) | no |
| `nfa_match` | 15/20 (`baseline`) | no |
| `cache_invalidation_dist` | 4/5 (`no-loopbreak`) | no |
| `pratt_parse` | 4/5 (`no-groundfs`) | no |
| `ot_transform` | 18/20 (`baseline`) | no |

**Zero of eight.** §11's condition 2 is satisfied on the stronger arm; the task
set required no revision and none was made. The three tasks saturated under
`full` all fail somewhere else in the arm — `json_patch` most dramatically,
falling to 1/5 without the verify gate. Saturation under a single cell was not
saturation of the suite.

**What still stands, and it is narrower than it first looked.** The gate is
about the *task set*; the ceiling is about the *arm's mean*, and that is
arithmetic no gate can clear.

**Implication for interpretation:** with `full` at 0.950 against a hard ceiling
of 1.0, a component that *hurts* qwen can show a Δ of at most −0.05. The
`ext-cerebras` arm found exactly such a result (`no-checklist`, Δ = −0.075,
95% CI [−0.144, −0.006]); qwen structurally cannot reproduce an effect of that
size. Large positive Δ (H1) remains fully detectable. This asymmetry should be
stated wherever qwen's H3 nulls are reported, so that "not detected" is not read
as "not present".

---

## O2 — A floored task on the Ornith arm, and an asymmetry in §11

Recorded for the same reason as O1: it is evidence the §11 gate would have
weighed, arriving after launch because D1 ran the gate on one cell.

Ornith `full`, complete at 160 episodes: **97/160 = 60.6%**. That clears §11's
condition 3 (weaker arm's full-harness rate ≥ 20%) with room to spare, and the
arm carries no ceiling — a harmful component can show a Δ down to −0.394 here,
against qwen's −0.05. **Ornith is the arm that can resolve what qwen
structurally cannot**, which is the O1 warning's counterpart.

Per-task in that cell:

| task | passed |
|---|---|
| `ast_transformer` | **0/20** |
| `nfa_match` | 6/20 |
| `state_machine_fuzz` | 9/20 |
| `concurrency_race` | 13/20 |
| `pratt_parse` | 13/20 |
| `cache_invalidation_dist` | 18/20 |
| `json_patch` | 19/20 |
| `ot_transform` | 19/20 |

**`ast_transformer` is floored at 0/20.** This is not a gate breach. §11 is
asymmetric: condition 2 states a *task-level* saturation rule ("no task is
passed by the stronger model in every cell", revise if ≥3 of 8), while
condition 3 states only a *suite-level* floor ("the weaker model's
full-harness rate ≥ 20%"). A task floored in every cell is not covered by any
registered rule. Nothing is being retired on the strength of this — the task
set is unrevised (§11.2's remedy requires re-registration, which did not
happen) — but the gap is named here rather than left for a reader to notice.

**Consequence, and it is the same consequence O1 has.** A task pinned at an
extreme contributes no signal to any within-arm delta on that arm, because both
cells score it identically; it only dilutes the mean over eight tasks toward
zero. The two arms lose signal in opposite directions:

- qwen: **all eight tasks vary across cells** (O1's resolution), so none is dead
  weight. Two move only slightly — `ot_transform` spans 0.90–1.00 and
  `pratt_parse` 0.80–1.00 across the nine cells — while `json_patch` spans
  0.20–1.00. Signal is unevenly distributed, not absent.
- Ornith: `ast_transformer` is floored at 0/20 in `full`, and `json_patch` and
  `ot_transform` sit at 19/20.

**Resolved — the floor does not hold across the arm.** With all nine Ornith
cells complete, `ast_transformer` scores 0/20 in `full`, `baseline` and
`no-outcap`, but **1/5 in `no-checklist` and 3/5 in `no-nativetools`**. It is
not floored in every cell, and no other task is either. Running the same check
in the saturation direction: no Ornith task is passed in every cell (`json_patch`
comes closest and drops to 3/5 under `no-verifygate`).

So **both arms clear §11.2's condition in both directions** — zero tasks
saturated, zero floored, on either arm. The task set required no revision and
none was made.

This is the second time a degeneracy that looked absolute in one cell dissolved
when the whole arm was examined; O1 was the first. The lesson is recorded rather
than quietly dropped: **a task pinned at 0 or 20 in the `full` cell says nothing
about whether it is informative in the experiment**, because the ablations are
precisely the cells where it moves. Both O1's and O2's original framings
generalised from `full` alone, and both were wrong to.

The two arms still weight their deltas differently — `ast_transformer` carries
real signal on qwen and almost none on Ornith, where it moves only under two of
nine configurations — so any cross-arm interaction (H4) compares two means whose
informative mass sits in different places. But that is a statement about
*weighting*, not about dead tasks, and it is much weaker than what this entry
originally claimed. **The per-task profile must be
reported alongside H4**, not just the aggregate; the aggregate can agree while
the profiles disagree, which is already visible between qwen and `ext-cerebras`.

## O3 — The two local arms share a protocol exactly

Verified on the completed `full` cells: qwen and Ornith agree on **all 17
protocol keys**, with no differences. This was intended (§Protocol keeps
`num_ctx 32768 / num_predict 4096 / temperature 0.6` for local arms) but is
worth stating as measured rather than assumed.

It matters for H4. `extension-cerebras.md` §7 correctly records that the hosted
arm differs from the local arms on `num_ctx`, `num_predict` and
`reasoning_effort`, so the hosted arm's *pass rate* is not comparable to a
local arm's and only its within-arm deltas are. **That restriction does not
apply between qwen and Ornith.** Their pass rates are directly comparable, so
the qwen-vs-Ornith interaction is a cleaner capability × harness test than
either against `ext-cerebras`, and should be presented as the primary H4
contrast, with the hosted arm as the third point.

## O4 — Every v2 episode carries a false containment note

Found while asking whether the v1 exploitation audit had ever been repeated on
v2. It had not, and the first v2 episode inspected opened with this:

```
{"t": "containment", "backend": "bwrap",
 "note": "shell commands are not OS-contained in this episode"}
```

Both halves cannot be true. `bwrap` **is** OS containment: `contained_argv()`
routes Linux through `_bwrap_argv`, and the live process tree on the run host
shows `bwrap --ro-bind / / --dev /dev --proc /proc --tmpfs <bench root>`
wrapping every shell command.

**Cause.** `harness.py` emitted the note on `backend != "sandbox-exec"` — the
same macOS-only assumption that had already been found and fixed in the C5
containment probe, surviving in a second place. On Linux the condition is always
true, so **every episode of the entire GPU arm is stamped as uncontained while
being contained.**

**Scope.** Annotation only. The note lives in `events`, never in `row`,
`protocol` or `summary`, so it touches no pass rate, no delta and no pooling
key. `res.containment` separately records the correct backend. No result
changes.

**Why it still matters.** This is the paper's own subject: a provenance record
that misstates a safety property. v1's defects overstated the instrument;
this one understates it. Both leave an auditor unable to tell a contained run
from a leaky one by reading the record — which is exactly the failure the v1
audit existed to rule out.

**Fixed, and deliberately not deployed.** The decision now lives in one place,
`tools.containment_event()`, with a regression test in `test_runtime.py` that
fails against the old condition. The run host is **not** being patched: the
registered grid must finish on the single instrument revision `82e453a` that
DEVIATIONS pins, and swapping harness code mid-grid would be a far worse
deviation than a wrong annotation. So:

- every v2 episode already on disk, and every one still to be written, carries
  the incorrect note;
- **the `ext-cerebras` arm is unaffected.** It ran from macOS, where the backend
  *is* `sandbox-exec`, so the old condition was false and no note was emitted.
  Its episodes read `{"t": "containment", "backend": "sandbox-exec"}`, correctly;
- readers and any audit script must treat `events[].note` on the v2 grid as
  known-wrong and take **`events[].backend`** as the record — it correctly says
  `bwrap` on every episode;
- the fix applies to the next grid, not this one.

**Correction to an earlier draft of this entry**, which named
`protocol.env_containment` and `res.containment` as the authoritative fields.
Neither is persisted. A real episode's protocol block holds seventeen keys and
**none of them is containment**; `Result.containment` is assigned in the harness
and never written to disk. The containment event is the *only* per-episode
record, which is what makes a wrong note in it worth correcting rather than
shrugging at.

**A second finding falls out of that.** Containment is not in `PROTOCOL_KEYS`,
so it is **not a pooling key**: an episode run with `MH_UNCONTAINED=1` would
pool silently with a contained one, and only the event would show it. No
contamination occurred here — every v2 episode records `bwrap` and every
`ext-cerebras` episode records `sandbox-exec` — but for a study whose central
claim is that containment was active, the property should be stamped and
pooled on, not merely logged. Owed for the next grid alongside the O4 fix.

## O5 — The exploitation audit has not been repeated on v2

v1's headline credibility claim is an audit of all 760 episodes and 3,879
file-writing tool calls that found **zero** exploiting the five defects. **No
equivalent audit has been run on v2, or on `ext-cerebras`.** There is no audit
script in the repository; the v1 pass was one-off analysis.

The gap is not that exploitation is suspected — the defects v1 had are fixed and
the tasks are stricter. It is that "the fixes are correct, therefore no episode
cheated" is inference, and the same inference was available for v1 before anyone
checked. O4 is evidence for taking that distinction seriously: the containment
record on this grid is demonstrably wrong in one direction, found only because
someone looked.

Two specifics make v2 *more* in need of an audit than v1, not less:

1. **The Linux containment path is new code.** It had never been exercised
   before this grid, and ungating it revealed hidden tests reachable through
   `TMPDIR` bind ordering, orphaned children surviving their parent, and a probe
   asserting the wrong invariant. All were fixed before the grid started, but
   the whole GPU arm is the first real workload that path has carried.
2. **The episode record supports the query.** Every episode stores `events` and
   `verify_output`, so the audit is a query over data already on disk, not a
   re-run.

**Owed before publication:** an audit script, run over all v2 and `ext-cerebras`
episodes, checking that no episode read or wrote a hidden test, that no pass was
recorded without the verifier running, and that the containment backend in each
episode matches the one its host could provide. Its result belongs in the
manuscript next to v1's, whatever it says.

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
