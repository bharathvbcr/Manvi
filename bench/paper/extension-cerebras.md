# Extension — a Cerebras-served third and fourth arm

**Status:** protocol, suitability assessment, and results. The
`gpt-oss-120b` arm is **complete**: 9 configs × 20 seeds × 8 tasks = 1,440
episodes, run 2026-08-25/26 under tag `ext-cerebras`, $104.63, 8 h 15 m.
Every cell holds 160 episodes over 20 repeats and 8 tasks with zero unserved,
zero starved, a single protocol, and zero protocol drift between cells. Vendor
figures are quoted from documentation read 2026-08-25 and labelled as such.
The Gemma arm has not been run. §7 carries the results.

**Not a revision of [`preregistration.md`](preregistration.md).** That document
registers two locally served models, and its §12 makes any departure after the
first registered episode a declared deviation. This is a separate experiment
with its own registration, run under its own tag, reported as an extension. The
frozen 720-episode grid and the registered v2 grid are untouched by it.

---

## 1. What is being added

| | `cerebras:gpt-oss-120b` | `cerebras:gemma-4-31b` |
|---|---|---|
| parameters | 120B | 31B dense |
| context (free / Developer) | 65k / 131k | 65k / 131k |
| max output (free / Developer) | 32k / 40k | 32k / 40k |
| throughput | ~3,000 tok/s | ~1,850 tok/s |
| price in / out per 1M tokens | $0.35 / $0.75 | $0.99 / $1.49 |
| reasoning | `reasoning_effort`, default `medium` | off unless `reasoning_effort` is set |
| tool calling | yes | yes, including parallel |
| Developer rate limit | 1K RPM, 1M TPM | 300 RPM, 500K TPM |
| free-trial rate limit | 5 RPM, 30K TPM, 1M TPD | 5 RPM, 30K TPM, 1M TPD |

Source: Cerebras Inference documentation, read 2026-08-25 —
`models/overview`, `models/openai-oss`, `models/gemma-4-31b`,
`api-reference/chat-completions`, `support/rate-limits`.

---

## 2. Is this a suitable choice?

### Yes, on four counts

**It supplies the capability axis the interaction test does not currently have.**
H4 asks whether the harness delta shrinks as capability rises. The registered
design tests it across Qwen 3.8 27B and Ornith-1.5 35B-A3B — two models whose
full-harness means sit close together, and where the *larger* one scored lower
on every configuration. All eight v1 interaction intervals included zero, and
§7 of the manuscript already reports the test as underpowered. A 120B model that
is plausibly stronger than both widens the axis rather than adding a third point
next to the first two. That is the single best reason to do this.

**Both models are open-weight, which the excluded Gemini arm was not.** A reader
can obtain `gpt-oss-120b` and `gemma-4-31b` and attempt a refutation on their own
hardware. Nothing about `gemini-3.7-flash` was recoverable that way, and its
arm is excluded by declaration (§4.3). Replacing an opaque third arm with two
reproducible-in-principle ones is a strict improvement in the paper's evidentiary
position, independent of what the numbers turn out to be.

**Gemma 4 31B is size-matched to the local arms.** At 31B dense against Qwen's
27B and Ornith's 35B-A3B, it separates two things the current design confounds:
model scale, and the serving stack. If a 31B hosted arm behaves like the 27B and
35B local arms on the ablation ladder, that is evidence the ladder is measuring
harness structure rather than a property of one serving setup.

**The cost is not the constraint, and neither is time.** Measured below (§4):
**~$111** for a complete 1,440-episode gpt-oss arm, against days of exclusive
GH200 time for an equivalent local arm. The median episode finishes in 16
seconds. Gemma is roughly 2.7× the price per token and its arm is estimated at
~$295, which is the least certain figure in this document.

### But it costs the paper four things, and they must be declared

**1. Sole tenancy — the one hard rule — does not apply.** The runner enforces
"exactly one model resident" in code, and §5.6 reports a 43.8-hour compute
envelope from 78,027 nvidia-smi samples. Neither exists for a hosted arm. Batch
composition, co-tenancy and routing on the provider's side are invisible and
uncontrolled. The manuscript already refuses wall-clock comparisons across
configurations (§7.6), which absorbs most of the damage; what it cannot absorb is
that these arms have no compute envelope at all, and Table 8 must say so rather
than leave the row blank.

**2. A repeat is reproducible only as far as the provider chooses.** `seed` is
documented as best-effort, not as a determinism guarantee, and a fixed model id
can be re-served on a changed stack mid-run. This matters because the entire
analysis is *paired by repeat index*: on a local arm repeat *r* pins seed *r* in
both arms of the contrast, so the pairing is real; if the seed did nothing here,
pairing by index would pair two independent samples, which does not bias Δ but
removes the variance reduction the paired bootstrap assumes and can make the
interval **narrower than it should be**.

**Measured 2026-08-25, and it passes.** On `gpt-oss-120b`, an open-ended prompt
at `temperature=0.6` returned byte-identical completions across two calls at
seed 0, byte-identical across two calls at seed 1, and *different* text between
seed 0 and seed 1. Both halves are needed and both hold: the seed controls
sampling, so repeats on this arm are genuine seeded replicates and the paired
bootstrap is valid. What is still not guaranteed is that the same seed returns
the same text *weeks later*, across a serving-stack change — see §5.5.

**3. The served object is not fully specified.** Cerebras states that public
models are "unpruned" and that it "uses selective weight-only quantization only
during storage", with sensitive layers kept at full precision and dequantized on
the fly. That is a good deal more disclosure than most providers give, and it is
still not a precision the paper can print the way it prints `Q4_K_M` or `Q8_0`
for the local arms. "gpt-oss-120b on Cerebras" and "gpt-oss-120b at bf16 on a
GH200" are not established to be the same measurable object.

**4. Two protocol deviations from the local arms.** `top_k` is absent from the
documented parameter set and is not sent (the local arms run at 20), and Gemma
runs at the vendor's recommended `temperature=1.0` against the suite's 0.6. Both
are stamped into every episode's protocol block. Neither affects a within-model
paired Δ — both arms of every contrast share them — but any cross-model statement
has to carry them.

### The known way this fails

The Gemini arm was not disqualified by its pass rate. It was disqualified because
**32.5% of its episodes were never served, and the refusals were correlated with
the task**: `json_patch` was refused 5/5 while Qwen and Ornith both passed it 5/5.
A serving failure that is missing-at-random is noise; one that tracks difficulty
is a censored measurement wearing a pass rate's clothes.

That is the specific failure this extension has to rule out, not a general worry
about APIs. §5.3 makes it a gate. Two things make it more tractable here than it
was for Gemini: the refusals there were capacity 500s on a demand-constrained
endpoint, whereas a 429 on a documented rate limit is a different mechanism with
a published ceiling; and the client now records `finish_reason` and the 429 count
on every reply, so the audit is a query rather than an investigation.

### Recommendation

**Run it, as a separately registered extension, gpt-oss first.** gpt-oss-120b is
where the scientific value is — it is the arm that widens the capability axis —
and it is the cheaper of the two per episode. Gemma 4 31B is worth adding second,
as the size-matched control, once gpt-oss has cleared the gates below.

Do **not** fold either arm into the registered v2 grid, and do not add them to
`grid.py`'s default `MODELS`. Both would silently change a registered design.

---

## 3. Protocol

Identical to the registered grid except where the endpoint forces a difference.

- **Configs (9):** unchanged — `full`, `baseline`, seven one-flag-off cells.
- **Tasks (8):** unchanged — the hard suite.
- **Seeds:** 20 per cell, seed *r* pinned to repeat index *r*, subject to §5.1.
- **Protocol:** `--max-steps 0 --max-wall 1800 --think`, one runner revision
  throughout, with `num_ctx=65536`, `num_predict` 16,384 (gpt-oss) / 8,192
  (Gemma), `temperature` 0.6 (gpt-oss) / 1.0 (Gemma), `top_p=0.95`, no `top_k`,
  `reasoning_effort=medium`. Every value is stamped on every episode.
- **Tag:** its own (`--tag ext-cerebras`), never `hard`. The Gemini arm's
  provenance note records what happens when an excluded arm shares a tag with
  the reported grid: reproducing the paper needs `--exclude`, and a reader who
  omits it gets different tables.
- **Tier:** Developer. The free trial's 30K TPM and 1M TPD are below one
  episode's appetite and cannot run this.

```bash
export CEREBRAS_API_KEY=csk-...
python3 grid.py --models cerebras:gpt-oss-120b --hard --repeats 20 \
                --seed 0 --tag ext-cerebras
```

The exclusion rule (§6 of the registration), the infrastructure-failure dual
report (§7), and the stopping rule (§8) carry over unchanged. The
infrastructure-failure report matters more here than it did locally: a 429 that
exhausts the retry budget surfaces as `error:ModelError` and is scored a failure
in the primary analysis, with the sensitivity rate reported beside it.

---

## 4. Budget — measured

**Measured** on a complete 160-episode `full` cell of `gpt-oss-120b`
(8 hard tasks × 20 seeds, `max_steps=0`, `max_wall=1800`), 2026-08-25. This
supersedes the earlier derived estimate, which was low by about half.

| per episode | median | mean | max |
|---|---|---|---|
| input tokens (summed over turns) | 66,822 | 174,979 | 2,778,790 |
| output tokens | 11,066 | 21,242 | 277,308 |
| steps | 17 | — | — |
| wall clock | 16 s | — | — |

At the mean, and at published prices:

| | measured $/episode | 72-episode pilot | 1,440-episode arm |
|---|---|---|---|
| `gpt-oss-120b` | **$0.077** | ~$6 | **~$111** |
| `gemma-4-31b` (same token profile, Gemma prices) | ~$0.21 | ~$15 | ~$295 |

Three things about this table matter more than the totals.

**The mean is 2.6× the median.** A minority of episodes dominate the bill: the
most expensive single episode consumed 2.78M input and 277K output tokens, some
forty times the median. Budgeting from a median would understate the arm by
more than half. The Gemma row assumes gpt-oss's token profile and is therefore
the least reliable line here; Gemma's own reasoning is verbose (measured: 261 of
265 output tokens on a one-line arithmetic prompt), so treat ~$295 as a floor
and measure the pilot before committing.

**Wall clock is not the constraint, and that is a change of kind.** A median
episode takes 16 s against the local arms' minutes. A 1,440-episode arm is hours,
not days. Throughput limits are never approached: a serial loop against 1M TPM
(gpt-oss) or 500K TPM (Gemma) issues one request at a time.

**Prompt caching did not help.** `cached_prompt_tokens` was 0 on every probe
reply measured. The append-only history ought to be close to the best case for
it, so this is worth re-checking on a long run; the field is recorded per reply,
so the pilot answers it rather than assuming it.

### Budget exhaustion is a run-stopping event, not a data point

**This already happened.** On 2026-08-25 a key hit its quota partway through a
grid. Every subsequent request returned `HTTP 402: Payment required`. The runner
scored each as an ordinary serving failure and continued for another two and a
half hours: **314 episodes** were written as zero-token failures across
`no-envboot` (160/160) and `no-verifygate` (154/154), plus 70 of `baseline`'s
160. Under §7 those rows are scored as failures, so `no-envboot` stood at
**0.0% over 160 episodes** — a number indistinguishable, in the data and in the
transcript, from an ablation that destroys the model.

The instrument now refuses to produce this. 401, 402 and 403 raise
`AccountRefused`, which is not retried, is not scored, and does not become an
episode: the cell aborts with exit code 3 and `grid.py` stops the whole grid
rather than stepping to the next cell to fail the same way. Rows already on disk
from the incident are recognised by `mh.runtime.is_unserved_episode`, reported
per cell by the runner as a refusal to publish, and — being first-turn failures
— re-run automatically without `--force`.

They are **not** rescued statistically. §6's exclusion rule is timeouts only and
adding a second exclusion would move a preregistered denominator. An episode the
provider never served is re-run, not reweighted.

---

## 5. Pilot gates

72 episodes per model — 9 cells × 1 seed × 8 tasks, under `--tag pilot-cerebras`.
**Its data is discarded and not pooled**, as with the registered pilot. Proceed only if all five hold.

**5.1 The seed controls sampling — PASSED 2026-08-25 on `gpt-oss-120b`.**
Two conditions, both required. Same seed twice must give identical completions,
*and* a different seed must give a different one. The second is the one that is
easy to omit and fatal to omit: a prompt the model answers near-greedily returns
the same text whatever the seed, which reads as perfect determinism while the
seed is inert. `probe.py` runs both against an open-ended prompt. If either
fails, the arm is reported as **unseeded**: repeats are independent samples, the
paired bootstrap is replaced by the unpaired estimator, and the manuscript says
so. Decided here, before any registered episode, and not after seeing a delta.

**5.2 The model actually calls `finish`.** Measured on the 160-episode `full`
cell: **88 of 160 episodes (55%) ended in `no_tool_call`, not `finished`**, and
the verify gate fired in only 94 of 160. The pattern is legible in the
transcripts — gpt-oss completes the work and then writes a prose summary
("Implemented full async control-flow desugaring: …") for three turns running
instead of calling the tool. It is a property of the model, not of the wire:
those episodes make 30–40 successful tool calls first, 59 of the 88 still pass
verification, 71 episodes do stop on `finished`, and only 4 assistant turns in
3,790 were truncated. That distinguishes it cleanly from the Gemini client
defect, which produced *zero* `finished` stops and repeated identical calls.

It still matters, and it is a gate rather than a footnote, because the verify
gate is the component §5.7 of the manuscript makes its strongest claim about: a
gate that fires in 59% of episodes is being measured at 59% of its strength, and
`full` versus `no-verifygate` on this arm is correspondingly weaker than the same
contrast on a local arm. Record the `finished` rate per cell and report it beside
the ablation delta. **Do not "fix" this by nudging harder** — the nudge policy is
part of the harness under test, and changing it would break comparability with
every frozen local cell.

**5.3 Refusals are not task-correlated.** Tabulate every non-2xx and every
`error:ModelError` by task. The Gemini arm's disqualifying pattern was a task
refused 5/5 while both local models passed it 5/5. If any task's serving-failure
rate differs materially from the rest, the arm is reported as censored and is not
used for H1 or H2 — the same disposition, on the same grounds, as §4.3.

**5.4 The suite is neither saturated nor floored.** No task passed in every cell;
full-harness rate ≥ 20%. Same thresholds as the registered pilot. A 120B model
saturating the hard suite is a live possibility and would mean the task set,
not the arm, needs revising — that is what happened when Qwen 3.8 27B scored
11/11 on the original suite.

**5.5 The serving stack held still — use a canary, NOT `system_fingerprint`.**

An earlier draft of this document proposed gating on `system_fingerprint`, on the
assumption that it identifies the serving build the way OpenAI's field is
documented to. **Measured 2026-08-25, it does not.** Across four requests of
identical shape it took two distinct values, and those values tracked the
*request* — the two seed-0 calls shared one fingerprint and the two seed-1 calls
shared another; two calls at the same seed with different prompts also differed.
It is a function of request content, not of the server build. Gating on it would
have fired on every run, for no reason.

The workable check measures the thing directly. Fix a canary — one prompt, one
seed, one set of sampling parameters — and re-issue it at the start of the run
and after each cell. If its completion changes mid-run, the serving stack changed
underneath the experiment, and the affected episodes are reported as mixed rather
than pooled, the same treatment a cell whose episodes disagree on protocol
already gets. This works precisely *because* §5.1 passed: a pinned (prompt, seed)
is reproducible within a stack, so a change in its output is a signal rather than
noise. `system_fingerprint` is still recorded per reply — it costs nothing and
may yet turn out to carry information — but nothing is gated on it.

---

## 6. What the harness records for these arms

Per episode, in addition to everything the local arms record:

- `protocol.env_api_provider`, `protocol.env_api_endpoint` — the serving side.
  `env_gpu`, `env_node`, `env_ollama_version` are **null**, because this box did
  not serve the episode. `env_client_node` records the box that ran the loop and
  is deliberately not a pooling key, so two identical remote cells launched from
  different machines still pool.
- `protocol.top_p`, `protocol.reasoning_effort` — settings that change what the
  model does and that `think` alone cannot express. A cell at effort `low` and a
  cell at effort `high` both record `think=true`; without these keys they would
  pool as one experiment.
- `row.seed` is `null` on any arm whose endpoint cannot accept a seed, so a seed
  that never left the runner is never indistinguishable, on disk, from one the
  model actually sampled under. (This applies to the Gemini arm today: its
  endpoint rejects every `generation_config` key except `thinking_level`, so no
  seed ever reached it, and episodes previously recorded one regardless.)
- `decode_tok_s` / `prompt_tok_s` from the provider's `time_info` block. These
  are the provider's measurement, as ollama's `eval_count`/`eval_duration` are
  ollama's. Neither is ours.
- `raw.finish_reason`, `raw.rate_limited`, `raw.reasoning_tokens`,
  `raw.cached_prompt_tokens`, `raw.system_fingerprint`. The last is recorded but
  not interpreted: see §5.5 for what it was measured to be.

The API key appears in none of it.

---

## 7. Results — `gpt-oss-120b`, complete

`compare.py --tag ext-cerebras`. Per-repeat pass rate, weighted by scored
episodes, percentile bootstrap, 10,000 resamples, RNG seed 0.

| config | pass rate | 95% CI | finish rate |
|---|---|---|---|
| `no-checklist` | 88.8% | [83.8, 93.8] | 64% |
| `no-outcap` | 84.4% | [78.8, 89.4] | 46% |
| `no-loopbreak` | 82.5% | [76.9, 87.5] | 43% |
| **`full`** | **81.2%** | [76.2, 86.2] | 45% |
| `no-envboot` | 80.6% | [75.6, 86.2] | 42% |
| `no-groundfs` | 80.6% | [75.0, 86.2] | 41% |
| `no-verifygate` | 71.2% | [66.2, 75.6] | 54% |
| `no-nativetools` | 66.9% | [61.9, 72.5] | 29% |
| `baseline` | 66.2% | [55.6, 75.6] | 91% |

### Confirmatory (§5 of the registration: Šidák across the preregistered tests)

Two confirmatory tests on this arm, so α = 0.0253 and the decision is made on
**97.5%** intervals — not on the 95% ones in the ladder. `compare.py` now
computes and prints these rather than leaving the correction as an exercise.

| | contrast | Δ | 97.5% CI | verdict |
|---|---|---|---|---|
| **H1** | full − baseline | **+0.150** | [+0.019, +0.300] | **supported** |
| **H2** | full − no-outcap | −0.031 | [−0.100, +0.037] | not detected at this n |

H1 holds at the corrected level, and at the registered three-test level
(98.3%: [+0.006, +0.306]) as well. It does not survive a Šidák correction
across all eight ladder tests (99.36%: [−0.013, +0.331]), which is not the
registered rule but is worth knowing about a delta this marginal.

H2 is not falsified: v1's Ornith `no-outcap` result (−10.0, interval excluding
zero) does **not** replicate here.

### Exploratory (H3) — read at 95%, with the family-wise number

| ablation | Δ | 95% CI |
|---|---|---|
| `no-nativetools` | +0.144 | [+0.075, +0.212] |
| `no-verifygate` | +0.100 | [+0.037, +0.169] |
| `no-checklist` | **−0.075** | [−0.144, −0.006] |
| `no-outcap` | −0.031 | [−0.087, +0.031] |
| `no-loopbreak` | −0.013 | [−0.087, +0.075] |
| `no-envboot` | +0.006 | [−0.062, +0.069] |
| `no-groundfs` | +0.006 | [−0.075, +0.094] |

Four of eight intervals exclude zero where a global null predicts 0.4.
P(≥1 excludes zero | global null) = 34% at nominal coverage, 39% at measured.
Measured bootstrap coverage is **94.1% against a nominal 95%** — n=20 is close
to honest, unlike v1's n=5.

`no-nativetools` and `no-verifygate` survive even a Šidák correction across all
eight tests (99.36%: [+0.044, +0.244] and [+0.013, +0.200]). **`no-checklist`
does not survive any correction** — it stops excluding zero at 97.47% — and is
reportable only as exploratory, with the family-wise number attached.

### The finish rate moves with the ablation, and that bounds §5.2

The verify gate can only fire on a `finish` call, so a cell's finish rate is a
ceiling on how much of `verifygate` was ever exercised in it. That rate is not
a constant of the model:

    baseline        91%          full            45%
    no-checklist    64%          no-nativetools  29%

The all-off baseline closes through the tool nine times in ten; the full harness
does so in fewer than half its episodes. **The harness itself is what suppresses
the `finish` call**, which is the opposite of the direction assumed when §5.2
was written. Two consequences: `+0.100` for `no-verifygate` is a floor rather
than an estimate, since the gate was idle in 46% of `full` episodes; and any
cross-model reading of the ladder has to carry each cell's finish rate, because
the ablations move it by a factor of three. `compare.py` now reports it per
cell and in the JSON (`cells[...].finish_rate`).

### H4 — not answered by this arm alone

The interaction needs a second arm. It is computable across tags and across
protocols: `--tag hard,ext-cerebras` pools the arms, and the statistic is a
difference of within-arm deltas, each seed-paired inside one arm under one
protocol, so a protocol difference *between* arms does not invalidate it.
`compare.py` now prints exactly which protocol keys the arms differ on and
states that their pass rates are not comparable.

## 8. What the run cost the instrument

Four defects surfaced only when 1,440 real episodes ran through the harness,
and two of them had already produced wrong published numbers:

| defect | what it did | fix |
|---|---|---|
| account refusal scored as a model failure | 314 episodes of HTTP 402 written as 0-token failures; `no-envboot` published **0.0% over 160 episodes** | `AccountRefused` is not retried, not scored, writes no episode; cell exits 3 and the grid stops |
| `grid.complete()` blind to unserved rows | the grid printed "skip complete" over both poisoned cells, so a re-run would not have replaced them | shared `unusable()` predicate; episode-level and cell-level checks now agree |
| `resume_conflicts` decided with `!=`, explained with `protocol_diff` | refused 90 keepable episodes and printed `differs on ` with nothing after it; a cell could not be resumed from a second machine | decided by `protocol_diff` |
| `_merge_protocols` grouped by the whole stamp | a cell resumed from a second machine published `protocol_variants` over two groups on the hostname alone | grouped by `protocol_identity`; provenance reattached, or None when it varies |

All four are the same defect: more than one answer to "are these two episodes
the same experiment?". There were four, they agreed until a key existed that one
of them should ignore, and then a single cell was simultaneously poolable,
unresumable, and possessed of two protocols. `mh.runtime.PROVENANCE_KEYS` and
`protocol_identity` are now the one definition, and `test_runtime.py` runs the
separating case through all four call sites and asserts they agree.

---

## 9. Running the local arms in parallel (H4)

The interaction needs `qwen3.8:27b` and `Ornith-1.5-35B-A3B` under a second tag.
Three things decided in advance, because each of them is cheap now and
expensive after 2,880 episodes.

**Tag.** Not `hard`. §1 of the registration says v1 lives in `results/*__hard`
and that v1 and v2 must never be pooled — but `compare.py --tag hard` pools
every directory ending `__hard`, so a v2 run under that tag merges silently
with the defective v1 grid. Use `--tag v2`, and read the interaction with
`compare.py --tag v2,ext-cerebras`.

**Protocol.** The local arms keep the suite's `num_ctx 32768 / num_predict 4096
/ temperature 0.6`, unchanged from the frozen grid. They therefore differ from
the hosted arm on those keys, which is fine and now declared: the interaction
is a difference of within-arm deltas, each seed-paired inside one arm under one
protocol, and `compare.py` prints the differing keys and states that the arms'
pass rates are not comparable.

**Concurrency.** `grid.py --parallel N` runs N cells against one resident
model. N is stamped as `concurrency` and is a pooling key, so it must be chosen
once and held for the whole grid. The risk it carries is specific: contention
inflates episode wall clock, `--max-wall 1800` scores a fail, and an episode
pushed over that line is a contention artefact that looks exactly like a model
failure. Before committing the grid, run one cell at the intended N and confirm
no episode reports `wall_timeout`; the frozen grid's median episode was 181 s
against an 1800 s ceiling, so there is headroom, but the tail is what fails and
the tail has not been measured under contention.

Two models in parallel is not available: each cell would evict the other's
weights mid-episode. That is refused by `grid.py`, and again by the runner's
lease if cells are launched by hand.
