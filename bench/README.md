# `bench/` — a lightweight harness for small local models

A stdlib-only Python harness and benchmark rig, built to test one claim
from **Meta-Harness** (arXiv:2603.28052): that harness structure matters far more
for weak models than for strong ones. It adds no dependencies to this repo.

It is deliberately *not* MANVI. MANVI is the production harness — policy ladder,
grants, leases, Rust verifier. This is a measurement instrument: small enough to
read in one sitting, with every component switchable so its contribution can be
measured rather than assumed.

## Layout

```
mh/model.py     ollama client; tolerant parsing of reasoning + tool-call shapes
mh/tools.py     five tools, sandboxed by resolved path
mh/harness.py   the agent loop and the switchable components
mh/bench.py     task loading, tamper detection, verification
mh/stats.py     bootstrap CIs, paired ablation Δ, weak-vs-strong interaction
mh/compute.py   GH200 snapshots: decode tok/s, power, HBM, mem controller (not GPU-Util)
tasks/<name>/   TASK.md, setup/, reference/, hidden_test.*, task.json
run.py          the runner; one resident model; --seed pinned per repeat
grid.py         resumable 3×7×5 CUDA matrix
compare.py      result tables plus bootstrap CIs on pass rates and Δ
selftest.py     proves the suite is valid before any GPU time is spent
stress_test.py  106 adversarial tests of the harness itself, no GPU needed
test_stats.py   bootstrap / Δ / seed unit tests, no GPU needed
test_compute.py nvidia-smi parse + tok/s, no GPU needed
```

## Use

```bash
python3 selftest.py                 # every task starts broken, accepts its reference, rejects tampering
python3 stress_test.py              # harness defences, mock model, seconds
python3 test_stats.py               # bootstrap CIs, paired Δ, seed pinning
python3 test_compute.py             # nvidia-smi parse, decode tok/s
python3 run.py --model qwen3.8:27b --config full --repeat 5 --seed 0 --tag grid
python3 compare.py --tag grid --json-out results/stats.json
python3 grid.py --smoke             # CUDA host: one cell, prove the box
python3 grid.py --tag hard --models mid --configs full,baseline --hard
```

`--config` selects a harness variant: `full`, `baseline`, or a single-component
ablation (`no-envboot`, `no-verifygate`, `no-checklist`, `no-loopbreak`,
`no-outcap`, `no-groundfs`, `no-nativetools`). Native file tools stay on
except in `no-nativetools`.

`--seed N --repeat 5` pins seeds `N .. N+4`, one per repeat index. That is
what makes a cell reproducible; omitting `--seed` on a multi-repeat run still
pins to `0 .. repeat-1`.

### Linux / CUDA (Lambda GH200)

`qwen3.8:27b-mlx` is Apple-only and will not run on NVIDIA. The grid uses the
CUDA/Ollama tags:

| role | tag |
|---|---|
| mid | `qwen3.8:27b` |
| mid (larger, not stronger on this suite) | `hf.co/ornith-ai/Ornith-1.5-35B-A3B-GGUF:Q8_0` |

Capability for the interaction test is the empirical full-harness mean, not
this table. `expand_hard.sh` runs the one-flag ladder on those two models
only, sole tenant, `max_steps=0`.

## The one hard rule

The runner evicts every other model before it starts and refuses to run if one is
still resident. On a 64 GB Mac that is a memory fact (a 38 GB Q8 MoE and an 18 GB
dense model cannot coexist). On GH200 it is experimental isolation: every cell
sees the same sole-tenant GPU. Either way it is enforced in code.

This is a **1×GH200** serial agent loop, not a multi-node cluster. GPU-Util
near 100% only means the SM is occupied; it is not saturation. Episode rows
record decode tok/s (ollama `eval_count`/`eval_duration`), power vs 900 W
cap, HBM occupancy, and memory-controller %. A sidecar
`python3 mh/compute.py --out results/compute.jsonl` logs the same envelope
without interrupting a running cell. Do not restart Ollama mid-cell.

## Tasks

Fifteen tasks became nineteen after Qwen 3.8 27B scored 11/11 on the original
suite. The last eight are the high-difficulty tier (`grid.py --hard`).

| task | what makes it hard |
|---|---|
| `binsearch` | off-by-one that makes the second bound search hang, not fail |
| `classfix`  | passing the three visible cases is easy; the contract is the test |
| `ttlcache`  | two interacting bugs: recency never refreshed, expired entries hold capacity |
| `perf`      | needs an algorithmic change, not a micro-optimisation |
| `cryptic`   | the `KeyError` names a symbol in a file that is not the buggy one |
| `multifile` | the fix spans two modules and requires the stale-write insight |
| `envbuild`  | the build names a compiler this machine does not have, plus a C loop bug |
| `parser`    | implement to a written spec, including error cases and escaping |
| `globmatch` | implement fnmatch semantics with no `re`/`fnmatch`, checked on ~2,300 pairs |
| `intervals` | half-open interval algebra, checked against a point-set oracle |
| `navigate`  | wrong totals in a 6-module package; the cause is two files from the symptom |
| `concurrency_race` | deadlock / lost-wakeup in a bounded queue; FIFO and `close()` under contention |
| `ast_transformer` | desugar `async for` / `async with` including break-aclose and aexit suppression |
| `state_machine_fuzz` | incremental binary protocol: CRC, seq, fragments, resync; stateful fuzzer |
| `cache_invalidation_dist` | split-brain cache; heal must not resurrect a deleted key from a stale replica |
| `pratt_parse` | unary vs `^` vs postfix `!`, right-assoc power, integer division toward zero |
| `ot_transform` | concurrent ins/del operational transform; site-id tie-break; convergence |
| `nfa_match` | regex `| * + ? () .` fullmatch with no `re`, checked on hundreds of pairs |
| `json_patch` | RFC 6901/6902 pointer escaping, array `-`, move-into-self, no mutation |

Verification is separate from the visible tests on purpose: `classfix` and `parser`
both pass their visible tests with implementations that are wrong in general.

## Non-cheating invariant

Inherited from this repo's own rules and enforced in `mh/bench.py`:

- protected files are hashed at load and re-checked at verify time; editing or
  deleting a test fails the task and says so,
- the hidden test runs from a fresh temp directory, so a same-named file inside
  the sandbox cannot shadow it,
- a verifier that could not run returns *failed*, never *passed*,
- the verifier runs at the end of every episode regardless of what the model
  claimed, and its verdict — not the model's `finish` call — is the score.
