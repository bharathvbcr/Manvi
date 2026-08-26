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
mh/model.py     ollama / Gemini / Cerebras clients, per-model serving specs;
                tolerant parsing of reasoning + tool-call shapes
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
stress_test.py  adversarial tests of the harness itself, no GPU needed
test_stats.py   bootstrap / Δ / seed unit tests, no GPU needed
test_pool.py    cell-assembly guards and pooling refusals, no GPU needed
test_runtime.py resume, extension and starvation semantics, no GPU needed
test_compute.py nvidia-smi parse + tok/s, no GPU needed
test_gemini_wire.py    Gemini request shape, no network or credential
test_cerebras_wire.py  Cerebras request/response shape, no network or credential
```

## Use

```bash
python3 selftest.py                 # every task starts broken, accepts its reference, rejects tampering
python3 stress_test.py              # harness defences, mock model, seconds
python3 test_stats.py               # bootstrap CIs, paired Δ, seed pinning
python3 test_pool.py                # cell assembly, pooling refusals, CLI end-to-end
python3 test_runtime.py             # resume, extensions, starvation classification
python3 test_compute.py             # nvidia-smi parse, decode tok/s
python3 test_gemini_wire.py         # Gemini wire shape, no network
python3 test_cerebras_wire.py       # Cerebras wire shape, no network
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

### API-served arms (Cerebras)

Two hosted models are addressable, in addition to local ollama:

```bash
export CEREBRAS_API_KEY=csk-...        # or a CEREBRAS_API_KEY= line in .env.local
python3 probe.py cerebras:gpt-oss-120b # test the key before spending a grid on it
python3 run.py --model cerebras:gpt-oss-120b --config full --repeat 5 --seed 0 --tag ext
python3 run.py --model cerebras:gemma-4-31b  --config full --repeat 5 --seed 0 --tag ext
```

`probe.py` reports where it looked for the credential, whether the endpoint
accepts it, whether a real tool call round-trips, the usage / reasoning-token /
rate-limit counters, and whether the same seed twice produces the same output
(pilot gate §5.1). It never prints the key. Exit 0 clean, 1 a live failure,
2 no credential found.

`.env.local` is read from this checkout's root **and**, when run from a linked
git worktree, from the main checkout's root — a worktree does not carry the main
checkout's untracked files, so a key at the main root used to be invisible to
every provider.

The `cerebras:` prefix is required and is what routes the request. `gpt-oss-120b`
and `gemma-4-31b` are also local ollama tags, and a bare name that means the
hosted model on one host and a local quantised build on another is exactly what
the protocol stamp exists to prevent. The key is read from the environment or
from `.env.local`; it is never written into an episode.

Each model carries its own serving specification (`mh/model.py`, `MODEL_SPECS`),
so `--num-ctx`, `--num-predict`, `--temperature` and `--top-p` default to the
vendor's documented values rather than to the local suite's. Every local model
still resolves to the suite's historical `32768 / 4096 / 0.6 / 0.95 / top_k 20`,
so no frozen cell changes because this table exists. Anything given on the
command line wins over both, and the resolved values are what the episode's
protocol block records.

| | `cerebras:gpt-oss-120b` | `cerebras:gemma-4-31b` | local default |
|---|---|---|---|
| `num_ctx` | 65,536 | 65,536 | 32,768 |
| `num_predict` | 16,384 | 8,192 | 4,096 |
| `temperature` | 0.6 | **1.0** (vendor's recommendation) | 0.6 |
| `top_p` | 0.95 | 0.95 | 0.95 |
| `top_k` | *not sent* | *not sent* | 20 |
| reasoning | `--think` → effort `medium`, `--no-think` → `low` | `--think` → `medium`, `--no-think` → off (its default) | `think` boolean |
| price | $0.35 / $1M in, $0.75 / $1M out | $0.99 / $1M in, $1.49 / $1M out | GPU-hours |

`num_ctx` is a harness-side guard, not a wire parameter: the endpoint's own
window is 65k on the free trial and 131k on Developer, and 65,536 is the floor
across both. `num_predict` becomes `max_completion_tokens`, which on
`gpt-oss-120b` covers reasoning tokens as well as the answer — that is why its
ceiling is four times the local arms' and not the same number.

**Four declared deviations from the local protocol.** None of them is absorbed
silently; each is either stamped into every episode or announced by the runner.

1. `top_k` is absent from the documented parameter set and is not sent. The
   local arms run at `top_k=20`.
2. Gemma 4 31B runs at `temperature=1.0`, the vendor's recommended starting
   value, against the suite's 0.6. Within-model ablation deltas are unaffected
   (both arms of every paired contrast run at the same temperature); a
   cross-model comparison has to say so.
3. `seed` is documented as best-effort, not as a determinism guarantee, and the
   serving stack behind a fixed model id can change under a run. Measured
   2026-08-25 on `gpt-oss-120b`, the seed does control sampling — same seed
   twice is byte-identical, a different seed differs — so a repeat is a genuine
   seeded replicate *within* a serving stack. What is not guaranteed is that it
   stays so across one. `system_fingerprint` does **not** identify the stack
   (measured: it varies with request content, not with the server build); use
   the fixed-canary check in `paper/extension-cerebras.md` §5.4 instead.
4. Sole tenancy, the GH200 compute envelope, and decode tok/s from ollama do not
   exist for these arms. `decode_tok_s` is instead derived from the provider's
   own `time_info` block, which is its measurement and not ours.

`--reasoning-effort low|medium|high` overrides the mapping above and is recorded
in the protocol stamp; a local model refuses it rather than accepting it and
ignoring it.

**The free trial will not run an experiment.** Its 30k tokens/minute and 1M
tokens/day ceilings are below one episode's appetite; a 429 whose `Retry-After`
exceeds 60 s is refused outright rather than slept through, because that sleep
would come out of `--max-wall` and turn a rate limit into an episode the model
appears to have failed. Use the Developer tier.

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

## Parallelism, and the one shape that is safe

Renting a GPU by the hour makes parallelism tempting. Only one shape of it is
supported, and the others are refused in code rather than discouraged in prose.

```bash
python3 grid.py --models qwen3.8:27b --hard --repeats 20 --seed 0 \
                --tag v2 --parallel 4
```

**One resident model, N cells in flight.** The weights load once; N episodes
decode against them. Every parallel cell carries `--keep-resident` so none of
them evicts the others, and each runner announces the model it holds in
`results/.runners/<pid>.json`.

What is refused, and why:

| | |
|---|---|
| `--parallel` with two models | Each cell calls `unload_all(keep=its own model)` and would evict the other's weights mid-episode. The victim's next `/api/chat` returns 0 tokens, times out, and is scored a failure with nothing in the row naming the cause. Refused by the grid, and again by the runner's lease if you launch by hand. |
| `--parallel` with `--share-gpu` | Two models *and* N episodes contending at once. v1 already recorded what one axis of that did: concurrent Qwen+Ornith starved Qwen's first turn. Refused. |

**`--parallel N` is stamped on every episode as `concurrency`, and it is a
pooling key.** A contended GPU moves wall clock, `--max-wall` scores a fail,
and a cell run four-up is therefore not the same experiment as one run alone
even when every other setting matches. Cells at different `N` will not pool,
and `compare.py` will say so rather than average them.

That last point is the one that costs money if ignored: pick an `N`, measure
that it does not push episodes into `wall_timeout`, and keep it for the whole
grid. Changing `N` partway through splits the grid into two experiments.

A lease whose process is gone is treated as stale and deleted, so a crashed
runner does not lock the box against every later one.

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
- the hidden test *file* is copied to a fresh temp directory and run from there,
  so a same-named file inside the sandbox cannot shadow the test itself. This does
  **not** extend to the modules the test imports: the sandbox is put on
  `PYTHONPATH` (it has to be — that is how the test reaches the model's
  solution), which places it ahead of the standard library on `sys.path`. A
  `json.py` or `re.py` written into the sandbox is therefore imported by the
  hidden test in preference to the stdlib module of that name. Nothing currently
  detects that: `tampered()` hashes only the paths a task lists in `protect` (the
  visible test, and `SPEC.md` where there is one), and a file the model newly
  creates is not one of them. This applies to the eighteen `python`-kind tasks;
  `envbuild` is `shell`-kind and runs with an unmodified environment,
- a verifier that could not run returns *failed*, never *passed*,
- the verifier runs at the end of every episode regardless of what the model
  claimed, and its verdict — not the model's `finish` call — is the score.
