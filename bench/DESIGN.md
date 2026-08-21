# Lightweight harness for small local models

Grounded in **Meta-Harness** (arXiv:2603.28052, Lee et al., Stanford/MIT/KRAFTON).
Paper facts this design is built on (all read from the PDF, not recalled):

- A harness is "a stateful program that wraps a language model and determines what
  context the model sees at each step" (§3).
- Harness choice matters *more* for weak models. On TerminalBench-2, Haiku 4.5 spans
  13.9% (OpenHands) -> 37.6% (Meta-Harness), a 2.7x spread; Opus 4.6 spans
  58.0% -> 81.8%, only 1.4x (Table 7). Small models are the high-leverage case.
- The single component Meta-Harness *discovered* for agentic coding (§B.3) is an
  **environment bootstrap**: one compound shell command run before the first LLM turn,
  injected as an `[Environment Snapshot]` block. It collects cwd, dir listing (truncated
  to 20 entries), language versions, package managers, memory. Guarded by a 15s timeout,
  fails silently. ~80 LOC. It removes "the 2-4 exploratory turns that agents typically
  spend discovering what tools and files are available".
- It inherits from Terminus-KIRA: **native tool calling** (not ICL JSON parsing), a
  **30KB output cap**, and a **multi-perspective completion checklist**.
- Practical tips (§D): baseline + a search set *hard for it*; log everything queryable
  as JSON, hierarchically; cheap validation before expensive benchmarks.

## Ablatable components (each on/off, so effect is measured not assumed)

| flag | component | failure it targets |
|---|---|---|
| `envboot`   | environment snapshot before turn 1        | F1 wasted exploration / hallucinated paths |
| `nativetools`| native tool_calls vs ICL JSON            | F2 malformed tool invocation |
| `outcap`    | head+tail output cap                      | F5 truncation, F6 context blowup |
| `checklist` | completion checklist before finish        | F4 premature done |
| `verifygate`| harness runs the verifier, model can't self-declare | F9 claimed-but-unrun success |
| `loopbreak` | duplicate-call detector + intervention    | F3 repetition loops |
| `groundfs`  | read-before-write + path existence echo   | F1 hallucinated files |

## Hypothesised failure points for 27B/35B-class local models

F1 hallucinated file paths and APIs -> env bootstrap, real listings, read-before-write
F2 hallucinated tool names/args     -> native tools, schema validation, corrective replies
F3 repetition / infinite loops      -> duplicate-call detection; never raise token cap to escape
F4 premature completion             -> checklist + harness-run verifier
F5 oversized tool output            -> head/tail cap, never silent middle-drop
F6 context blowup, slow prefill     -> append-only history (KV prefix reuse), capped outputs
F7 reasoning channel mishandling    -> read `thinking`/`reasoning_content`/`reasoning`; never assume prefill
F8 malformed JSON arguments         -> accept dict or string, tolerant coercion
F9 success claimed without running  -> non-cheating invariant: unrun != passed
F10 GPU contention between models   -> strictly one model resident; runner enforces

## Non-cheating invariant (inherited from this repo's MANVI rules)
A check that did not run must never report the same result as a check that ran and
passed. Verifiers live outside the sandbox and are never writable by the agent.

## Measured results (2026-08-19)

Suite: 11 tasks, 127 agent episodes, 8.4 GPU-hours. One model resident at a time,
resident at a time, enforced by `mh/runtime.py`.

Context headroom check: the worst episode observed (Ornith, `globmatch`, 40 steps)
peaked at 19,247 prompt tokens against `num_ctx=32768` — 13.5k of headroom, so no
silent context truncation occurred in any run reported here. That is luck rather
than a guarantee; see the overflow guard added after this measurement.

### Retracted: the wall-clock speedup

An early 8-task run showed the full harness finishing in 589s against the
baseline's 1082s, a 1.84x speedup. **That result did not survive a rerun.** On the
same eight tasks, a second pair of identical runs gave 641s vs 628s -- 0.98x. The
baseline's own total moved 1082s -> 628s (0.58x) between two runs that differed in
nothing but sampling.

Per-task, identical reruns ranged from 0.23x to 2.36x. Wall clock on this setup is
too noisy at n=1 to support any speed claim, in either direction, and none is made.

What the harness costs is real and visible: an env-bootstrap shell command, an
extra checklist round trip, and verifier runs. On easy tasks that overhead makes it
*slower* -- median per-task speedup is 0.56x (Qwen) and 0.80x (Ornith). It pays for
itself only where the baseline would otherwise fail or run away.

### Known limitation: single-sample variance

Every (model, config, task) cell is n=1. Two identical Qwen `full` runs on
`binsearch` produced 5 steps/47s and 9 steps/111s — a 2.4x spread on the same
model, config and task at temperature 0.6. Per-task numbers therefore carry little
weight individually. The claims that survive this are the aggregate ones, where 11
paired comparisons point the same way, not any single task's delta.

### Established: the verify gate

`navigate`, 26 episodes differing in nothing but `verifygate`:

| config | passed |
|---|---|
| `full` (gate on) | 13/13 |
| `no-verifygate` (gate off) | 7/13 |

Fisher exact two-tailed **p = 0.015**. This is the single-variable comparison; a
pooled version that also folds in the all-off baseline gives p = 0.006 but
confounds six components, so the conservative figure is the one that stands.

The failure it prevents is always the same: the model deletes the currency
conversion from `loader.load()` -- breaking that module's documented contract --
instead of removing the duplicate call in `pipeline.build_report()`. Totals then
look right, so the visible test passes and the model calls `finish`. Only a check
the model does not control catches it.

Getting here took three rounds of repeats. At 4/4 vs 2/5 the same effect sat at
p = 0.167. Stopping at the first favourable-looking number would have reported a
significant result on the strength of nine episodes.

### Established: step reduction on the hardest task

`globmatch`, four runs per arm: full `[14, 16, 17, 28]` vs baseline
`[35, 37, 40, 40]`. Disjoint ranges, Mann-Whitney exact **p = 0.029**. The
baseline's median is 40, which is the step ceiling itself.

### The verify gate is model-dependent, not universal

Repeating the single-variable experiment on Ornith did **not** reproduce the Qwen
result:

| model | gate on | gate off | Fisher p |
|---|---|---|---|
| Qwen 3.8 27B | 13/13 | 7/14 | **0.006** |
| Ornith-1.5-35B-A3B | 7/7 | 6/7 | 1.000 |

The base rates explain it. Without the gate, Qwen commits the contract-breaking
fix on **50%** of episodes; Ornith on **14%**. The gate is insurance, and its value
tracks how often the model actually makes the error — not model size. Ornith is the
larger model (35B vs 27B) and is the one that needs the gate less.

Pooling the two gives 20/20 vs 13/21, p = 0.003. That figure is not the finding:
pooling presumes a common effect, and the effect is precisely what differs here.

**Consequence for the harness:** components should be justified per model, not
switched on globally because they helped somewhere. The measurement rig matters
more than any particular component, because which components pay depends on the
model you are pointing it at.
