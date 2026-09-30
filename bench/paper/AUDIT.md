# Benchmark harness audit (read-only)

Date: 2026-09-29. Branch `paper/tmlr-route-a`, HEAD `ebf81d8`. The registered v2 grid ran on `82e453a`.
Scope: `bench/` Python (`mh/*.py`, `run.py`, `grid.py`, `compare.py`, `figures.py`, `report.py`, `probe.py`, the suites, `tasks/`), checked against `paper/harness_architecture.md` §3–§4 and Appendices A, C and D, `paper/preregistration.md` and `paper/DEVIATIONS.md`.
No repository file was edited apart from this one. One line was appended to `.devcouncil/codeintel/sessions/gaps.jsonl` (see §6). Every probe ran against scratch copies in the session scratchpad.

Confidence labels:
- **RUN** means I ran something that showed the result.
- **READ** means I read the code and checked it.
- **INFERRED** means I reasoned it from the code without running it.

## 0. Baseline: every bench suite run once at HEAD (macOS, Python 3.14.7, `sandbox-exec`)

| Suite | Exit | Summary line |
|---|---:|---|
| `selftest.py` | **1** | `SUITE INVALID -- 2 problem(s): json_patch: passes without any fix; state_machine_fuzz: passes without any fix` |
| `stress_test.py` | 0 | `189 passed, 0 failed` |
| `test_stats.py` | 0 | `ok 215 stats tests` (the paper says 166) |
| `test_tools.py` | 0 | `3 passed, 0 failed` |
| `test_compute.py` | 0 | `37 passed, 0 failed` |
| `test_pool.py` | 0 | `174 passed, 0 failed` |
| `test_runtime.py` | 0 | `197 passed, 0 failed` |
| `test_cerebras_wire.py` | 0 | `70 passed, 0 failed` |
| `test_gemini_wire.py` | 0 | `ok 19 gemini wire tests` |

The same three suites were also run from a `git archive` of the grid commit `82e453a`, extracted to the scratchpad:

| Suite at `82e453a` | Exit | Summary line |
|---|---:|---|
| `selftest.py` | 0 | `suite valid: 19 tasks start broken, ...` |
| `test_stats.py` | 0 | `ok 160 stats tests` |
| `stress_test.py` | 0 | `189 passed, 0 failed` |

`git diff 82e453a HEAD -- bench/mh/bench.py` is empty, so the verifier is identical at the two commits.

The committed `verify.sh:1303-1305` (`HEAD:verify.sh`) runs all nine suites, and any non-zero exit is a `fail`. It follows that the bench gate of `verify.sh` is red on HEAD, and on `main` too (see C3). This is INFERRED: I did not run `verify.sh`, and I did not check whether CI has been green since `138336f`.
The loop covers every bench `test_*.py`. No bench suite is left out.
The working-tree `verify.sh` adds `paper/test_supplement.py`. That edit is uncommitted and belongs to a concurrent agent, so I did not assess it.

## 1. Findings, most severe first

### C1 — CRITICAL — An exit-zero "solution" is graded PASS on every Python task (RUN)
- **Where:** `mh/bench.py:352-354` and `:384` (`return rc == 0, ...`). Every `tasks/*/hidden_test.py` imports the candidate in-process and ends with `sys.exit(1 if bad else 0)`. `bench.py` is identical at `82e453a` (empty diff), so the v2 grid ran under this verifier.
- **Failure scenario:** put `import os; os._exit(0)` (or `raise SystemExit(0)`) at the top of the module under test. The hidden test never runs a check, the process exits 0, and `Task.verify` returns `(True, "exit=0")`. The model is told "The hidden checks pass."
- **Evidence:** `scratchpad/probe_exit_all.py` prints **18/18 Python tasks graded PASS**, the 8 hard tasks included. That is at HEAD **and** against the `82e453a` tree. `probe_verify.py` shows that both `os._exit(0)` and `SystemExit(0)` pass `binsearch`.
- **Why it matters for the paper:** §3.2 describes "three independent mechanisms" (hidden test out of sandbox, SHA-256 protection, no shadowing). None of the three touches the *verdict channel*. The verdict is the exit code of a process that the candidate runs inside. The existing tests only probe start-up hooks: `selftest.py:62-70` (a `sitecustomize.py` holding `os._exit(0)`) and `stress_test.py:501-554` (`.pth`). They never probe the module under test.
- **Exploited in v2?** Unknown. It is a vulnerability, not evidence of exploitation. DEVIATIONS O5 already records that no exploitation audit ran on v2 or ext-cerebras, and the v2 episodes are not in this checkout. Audit query for the run host: search `events[].args` of every episode for `_exit(`, `SystemExit`, `sys.exit`, `_getframe`, `f_back`, `inspect`.
  - Limitation 1: args are truncated to 200 characters (`harness.py:441`).
  - Limitation 2: `.work/` sandboxes are `rmtree`'d on the next `materialise` (`bench.py:252-254`), so the final sandbox contents are probably gone. The query is a lower bound.
- **Fix:** make the verdict come from launcher code that runs after the candidate's last chance to act.
  1. Hidden tests set a result object, for example `__mh_result__ = {"bad": n, "ran": k}`, and do not call `sys.exit`.
  2. The launcher reads it after `exec` and prints a per-run nonce marker. The nonce is passed out-of-band, not through the environment the candidate can read.
  3. The harness requires that marker, with rc 0 treated as necessary but not sufficient.
  4. Longer term, run the candidate out-of-process from the checker.
- **Test that proves it:** a new selftest column `exit0-rejected`. Overlay `import os; os._exit(0)` onto every non-protected `.py` and assert `verify()` is False. It fails today on 18/18.

### C2 — CRITICAL — A gated verification can copy the hidden test's source into the sandbox (RUN)
- **Where:** `mh/bench.py:69-85`. `src` (the hidden test source) stays live in the launcher's globals, and the candidate runs in that interpreter with write access to the sandbox (`contained_argv(..., allow_write=[sandbox])`, `bench.py:362`). `bench.py:62-68` and §3.3 claim the exfiltration channel is closed by `model_facing`. `model_facing` only redacts the verifier's *output*.
- **Failure scenario:** under `verifygate` (on in `full`), the model writes a module that walks `sys._getframe().f_back...` to `src` and writes it to `leak.txt`, then calls `finish`. The gate runs the verifier and the payload writes the file. On the next turn `read_file("leak.txt")` returns the hidden test, including its expected values.
- **Evidence:** `probe_verify.py [exfil]` gives `leak_bytes=814`, which is the whole `binsearch/hidden_test.py`. Also: `compile(src,...)`'s code object stays reachable through frames (`co_consts`) even if `src` is deleted.
- **Existing test gap:** `stress_test.py` C4 (`:626-650`) checks only `sys.argv[0]` and `__main__.__file__`. It encodes the specific attacks that were fixed, not the invariant.
- **Fix, in order of strength:**
  1. `del src` after compile. This is weak on its own.
  2. Snapshot `_file_set(sandbox)` plus mtimes before and after each *gated* verify. Delete files the verification created, and flag them in the record. Flag-and-delete rather than fail, because `envbuild` legitimately builds during verify.
  3. Run the candidate in a process that never holds the checker source.
- **Test that proves it:** the `EXFIL` payload from `probe_verify.py`. `leak.txt` must not exist, or must not be readable by the model, after `Harness.run_verifier()`.

### C3 — CRITICAL — HEAD ships solved `setup/` trees for two of the eight hard tasks (RUN)
- **Where:** commit `138336f` ("feat(bench): implement solutions for json_patch and state_machine_fuzz tasks", 2026-09-24) rewrote `tasks/json_patch/setup/patch.py` (+130) and `tasks/state_machine_fuzz/setup/proto.py` (+113). It is **not** an ancestor of `82e453a`, so the v2 grid is unaffected. It is on `main` and on this branch.
- **Failure scenario:** anyone who reproduces from HEAD or `main` gets two hard tasks that pass with zero work. Every cell's pass rate is inflated, deltas compress toward 0 on 2/8 of the suite, and the paper's "19/19 tasks valid" (§4.2) is false for the repository as published.
- **Evidence:**
  - `selftest.py` exits 1 and names both tasks.
  - `probe_exit_all.py` shows `setup_passes=True` for exactly these two.
  - `git merge-base --is-ancestor 138336f 82e453a` returns false.
- **Fix:** restore both files from `82e453a`, since solutions belong in `reference/`. The existing selftest is the regression test and it caught this, which counts in the suite's favour. The fault is that it was committed while red.

### H1 — HIGH — The preregistered §7 infrastructure-failure dual report is not produced anywhere, but DEVIATIONS says it was honoured (READ)
- **Where:** `preregistration.md:112-124` requires a primary rate *and* a sensitivity rate with non-timeout serving errors removed, plus a per-cell infrastructure-failure rate, "for every cell". `DEVIATIONS.md` ("What has *not* been deviated from") lists "§7 infrastructure-failure dual report."
- **Evidence:** there is no `sensitivity`, `infra` or `ModelError` handling in `compare.py`, `report.py`, `figures.py` or `mh/*.py` (rg). The paper (§4.3 line 207) says only that serving errors stay in the denominator. At the time of this audit, no other file under `bench/paper/` held such a computation either. `build_supplement.py` is being written concurrently.
- **Why it matters:** the prereg itself says that in v1 removing these flipped the sign of Ornith's full-vs-baseline delta, and Ornith's v2 `full` cell has 20 `ModelError` rows (§4.3).
- **Fix:** add a `sensitivity` section to `stats_report` beside `cells`. For each cell, give the rate and CI with `stop_reason` starting `error:` (non-timeout) removed, plus `infra_failure_rate`. Carry it into the paper, or record it as a deviation.
- **Test:** a fixture cell holding one `error:ModelError` row must yield a primary rate and a sensitivity rate that differ, and `infra_failure_rate == 1/n`.

### H2 — HIGH — Paper §3.2 item 3 and §3.3 describe the pre-hardening verifier, not the one that produced v2 (READ)
- **§3.2(3), paper text:** "copied into a fresh temporary directory ... with the sandbox supplied as `PYTHONPATH`".
- **§3.2(3), actual behaviour:** the code pipes the test on stdin to `python -I -B` and inserts cwd itself (`bench.py:8-21, 352-355`). The mechanism the paper describes is the one `bench.py:10-13` says was exploited on 18/19 tasks.
- **§3.3 and App. A, paper text:** the gate "returns the failing output (capped at 4,000 bytes)".
- **§3.3 and App. A, actual behaviour:** it returns only redacted labels (`harness.py:470-478`, `bench.py:143-177`).
- Both are true of `82e453a`. The paper's description of its own instrument is wrong in the section a reviewer will check first.
- **Fix:** rewrite §3.2(3) and the §3.3 gate paragraph. Once C1/C2 are fixed, state the verdict channel explicitly.

### M1 — MEDIUM — Preregistered confirmatory family size: prereg, code/paper and DEVIATIONS disagree three ways (READ)
- `preregistration.md:89-91`: H1 is 2 tests and H2 is 1, giving 3 tests and 98.3%.
- `compare.py:276-277` tests H2 on *every* model, giving k=4 and 98.7% (`stats-v2.json` `n_tests: 4`). Paper §4.5 matches the code.
- `DEVIATIONS.md` says "§5 multiplicity — confirmatory claims at 98.3% intervals" was not deviated from.
- `compare.py:252-253, 264-266` comments still say "H2 is one test ... ~98.3% (three tests)".
- Direction: k=4 is *more* conservative. Both H1 remain supported and both H2 remain unsupported (`stats-v2.json`), so no verdict flips. The record is inconsistent, not the result.
- Also: `stats-all3.json` runs the family at k=6 (99.1%) because the third arm is pooled in.
- **Fix:** record the deviation and fix the comments. **Test:** assert `n_tests` equals the registered count for the registered model pair.

### M2 — MEDIUM — The paper cites the unpaired interaction; the prereg specifies seed-paired (READ)
- `preregistration.md:80-82`: "Interaction and paired deltas use one index vector per resample applied to both arms."
- §5.6 / Figure 5: "This is the block the manuscript cites" (unpaired). Table 8 matches `stats-v2.json["interaction"]` exactly, and the paired columns match `interaction_paired`.
- `DEVIATIONS.md` lists §4 as not deviated from.
- Both blocks are shown, so nothing is hidden, but which block is *cited* is an undeclared departure. H4 is exploratory and every interval spans 0 under both schemes, so no conclusion changes.
- **Fix:** cite the paired block, or record the deviation.

### M3 — MEDIUM — The coverage audit uses one modal repeat count for a ladder that mixes 5- and 20-repeat intervals (RUN)
- **Where:** `compare.py:380-383` (`n_repeats` = modal over cells) and `interval_reliability` (`:220-249`), which applies that single coverage to *all* ladder intervals.
- **Failure scenario:** adding the 20-rep Cerebras arm flips the modal count from 5 to 20. `stats-all3.json` then reports `measured 0.941` and `fwer_at_measured_coverage 0.768` for 24 intervals, 16 of which are 5-repeat intervals at roughly 82% coverage. The mixed-ladder value is `1-0.823^16*0.941^8 = 0.973` (`probe_stats.py`).
- The paper quotes the v2 figure (96%), not 77% (rg), so this is a report-artifact defect, not a published number.
- **Fix:** audit coverage per distinct `(n_repeats, n_tasks)` shape and compute FWER as `1 - prod(c_i)`. **Test:** a report built from 12 five-rep and 12 twenty-rep cells must not report a single coverage.

### M4 — MEDIUM — The Linux (bwrap) containment is never self-checked at run time (READ + DevMap)
- **Where:** `tools.py:391-403`. The start-up probe runs only `if backend == "sandbox-exec"`. `containment_proves_itself` (`tools.py:198`) has one caller, `stress_test.py`. DevMap `devmap_neighbors` and an `rg` over `run.py`/`grid.py`/`mh` agree on that.
- **CI coverage:** the Linux CI leg installs bwrap and lifts the AppArmor userns restriction, so the stress suite exercises it there (`.github/workflows/verify.yml:119-131`). The gap is on the *run host*, not in CI.
- **Failure scenario:** on a run host where bwrap cannot set up namespaces, or a mask silently fails, nothing refuses the grid. With a failing wrapper every `run_shell` returns `exit=1` plus a bwrap error, and episodes are scored as model failures. v2 ran on bwrap. Every containment probe in this audit was on macOS, so the Linux path is inferred from source.
- **Fix:** call `containment_proves_itself(sandbox)` once per runner start (`run.py` before the loop), for every backend, and refuse on `ok=False`. **Test:** a monkeypatched backend whose probe prints `READ_OK` must abort `run.main`.

### M5 — MEDIUM — Resume and retry re-sample only failures (INFERRED)
- `runtime.keep_existing_episode` (`runtime.py:70-76`) re-runs *any* first-turn failure on resume, including non-timeout `error:*`. The prereg (§7) and `is_starved_episode`'s docstring (`runtime.py:761-767`) call these results that "must be scored".
- `run.py:378-398` also retries such an episode once, within the same invocation.
- Only failures are ever re-drawn, so each resume or retry can only move a cell upward. v2 reports no `--force`, but whether any cell was resumed is not recorded in this checkout.
- **Fix:** limit the resume/retry re-run to `is_starved_episode` (the registered exclusion), or record the count of replaced rows in `outcomes`. **Test:** a cell holding one first-turn `error:ModelError` row, resumed, must keep that row.

### M6 — MEDIUM — Paired deltas can compare different task subsets within a repeat (RUN)
- **Where:** `pool.contrast_conflicts` (`pool.py:307-329`) compares task sets per *cell*, not per repeat. A repeat missing tasks in one arm raises only a `ragged_reps` warning.
- **Scenario from `probe_stats.py`:** `full` rep 1 scored {a,b} and the ablation scored {a,b,c,d}. The delta is reported as 0.5, while on the common tasks it is 0.0.
- v2 is unaffected: all cells are complete, 160/40 rows.
- **Fix:** check `(rep, task)` sets per repeat, or restrict each paired delta to the shared tasks of that repeat. **Test:** the probe fixture must raise `CellMergeError`.

### L1 — LOW — The wall clock is not a hard fail line (READ; 0/1,327 local rows affected)
- `harness.py:298-307` checks elapsed time only before each model call. Tool dispatch (up to N×120 s per turn) and a same-turn `finish` plus gate are never checked against it, and neither is the final verifier. A turn whose tools cross 1800 s and then `finish` scores `finished`/PASS with `wall_s > 1800`.
- API clients retry inside one call (`model.py:377` Gemini 16×, `:714` Cerebras 6×). The observed overruns of up to 3,047 s in `gemini-3.7-flash__*__hard-brokenwire` are consistent with that, but I did not trace which call produced them (INFERRED). Those rows are scored `wall_timeout`.
- Local rows over the wall that passed: 0 of 1,327. v2 rows are not local.
- **Fix:** before the final verdict, `if wall and elapsed >= wall: stop_reason = "wall_timeout"`, and pass the remaining time into tool timeouts. **Test:** a FakeClient turn holding a slow tool plus `finish` past the wall must score fail.

### L2 — LOW — The H2 decision uses `lo >= 0`; the prereg says "excludes zero" (RUN)
- `compare.py:296`. Twenty identical-rate repeats give a degenerate [0,0], which is declared **supported** (`probe_stats.py`). `ci_degeneracy` flags it, but the verdict ignores the flag. It is not triggered in v2 (H2 lo < 0).
- **Fix:** use `lo > 0`, and never support on a degenerate interval.

### L3 — LOW — Robustness edges in stats and assembly (RUN)
- `bootstrap_ci([.5, nan, ...])` returns `(nan, nan, 0.44)`, because sorting with NaN gives garbage, and `ci_degeneracy([nan]*5)` returns None. Rates are generated internally, so this is not reachable today.
- `compare.grouped` raises `TypeError` instead of `CellMergeError` when a cell mixes `rep` `"1"` and `1`. `seed_conflicts` sorts before the malformed-rep refusal can surface (`pool.py:374`).
- `passed: "false"` counts as a pass (`stats.py:149`).
- **Fix:** validate types at `compare.load`.
- **Test:** a `compare.load` fixture holding `rep: "1"` must raise `CellMergeError`, and a row with `passed: "false"` must be refused.

### L4 — LOW — `run_shell` captures unbounded output before capping (INFERRED)
- `tools.py:337-343` (`communicate()`), capped only afterwards (`:425`). A 120 s `yes` can grow harness RSS by GBs before the 30,000-byte cap applies.
- **Fix:** read in chunks and keep head and tail ring buffers.
- **Test:** `run_shell("yes | head -c 200000000")` must not raise harness RSS by more than the cap plus a constant.

### L5 — LOW — `cap_output` semantics (RUN)
- Output for oversize input is 30,080–30,084 bytes, because the banner is not counted.
- `limit=1` returns the *whole* input as the "tail" (`raw[-0:]`, `tools.py:72`). Unreachable at 30,000 or 4,000.
- **Fix:** `tail = raw[len(raw)-keep:]`.

### L6 — LOW — Dead or misleading code (READ)
- `bench.py:296-299`: the `allow_new` loop returns True on both paths, so it is a no-op.
- `compare.py:605`: arms are ranked by the unweighted `mean()` while cell means are weighted (`:494`). They are identical on complete cells.
- **Tests:** an `allow_new` glob must be able to change a verdict (it cannot today). Arm ranking on a ragged fixture must equal the ranking by weighted cell means.

### L7 — LOW — Background processes and TOCTOU (INFERRED; one vector RUN and refused)
- On macOS, a `nohup … &` from `run_shell` survives the command, and the episode, because `run_bounded` kills the group only on timeout. It could race the file tools: `resolve()` then `open()` in the *uncontained* harness process, `tools.py:363-374, 441-512`.
- A hard link of a protected file into the sandbox is refused by `sandbox-exec` (`probe_hardlink.py`).
- bwrap `--unshare-pid` kills descendants when the command exits (`tools.py:185-191`), inferred.
- **Fix:** open with `O_NOFOLLOW` or `openat` from a sandbox dirfd, and kill the sandbox's process session at episode end.
- **Test:** a `run_shell("nohup sleep 300 >/dev/null 2>&1 &")` must not survive `Harness.run()` returning.

## 2. Paper claims vs code

| # | Claim (location) | Verdict | Evidence |
|---|---|---|---|
| 1 | Five tools `run_shell, read_file, write_file, edit_file, finish` (§3.1) | VERIFIED | `tools.py:548-560, 563-603` |
| 2 | realpath check refuses symlink, `..` and absolute paths outside the sandbox (§3.1) | VERIFIED (file tools only; see L7) | `tools.py:363-374` |
| 3 | Shell under `/bin/bash -lc`, 120 s timeout returns a recoverable error (§3.1, App. A) | VERIFIED; the OS containment wrapper is not mentioned in §3.1 | `tools.py:27, 406, 413-419` |
| 4 | Head+tail cap of 30,000 bytes with an elision notice (§3.1) | VERIFIED, but output can reach 30,084 B (L5) | `tools.py:26, 52-75` |
| 5 | `outcap` off raises the cap to 10^9 (§3.1) | VERIFIED | `harness.py:51` |
| 6 | Hidden test never in the sandbox (§3.2-1) | VERIFIED; reachable in memory, see C2 | `bench.py:250-256, 343-358` |
| 7 | SHA-256 of protected files, failing before the hidden test runs (§3.2-2) | VERIFIED | `bench.py:234-235, 258-276, 330-334` |
| 8 | Checker copied to a temp dir, sandbox as `PYTHONPATH` (§3.2-3) | **MISMATCH** (H2) | `bench.py:8-21, 352-355` |
| 9 | "Three independent mechanisms" make the grader unreachable (§3.2) | **MISMATCH** (C1, C2) | probes |
| 10 | Final verifier always runs; `wall_timeout` scores fail (§3.2) | VERIFIED; the wall is not hard (L1) | `harness.py:519-525` |
| 11 | Baseline = every component off except native tools (§3, §4.4, Table 1) | VERIFIED | `harness.py:58-62` |
| 12 | envboot 15 s timeout, 4,000 B cap, 20-entry listing, `envboot_empty` (§3.3, App. A) | VERIFIED | `harness.py:22, 71-116, 238-244` |
| 13 | groundfs single sentence (§3.3) | VERIFIED | `harness.py:246-248` |
| 14 | nativetools off gives shell+finish, refusal message, alternate prompt (§3.3) | VERIFIED | `tools.py:607-616`, `harness.py:235, 256-259` |
| 15 | Checklist on the first `finish`, at most once (§3.3) | VERIFIED | `harness.py:453-460` |
| 16 | Gate returns failing output capped at 4,000 B (§3.3, App. A) | **MISMATCH**: redacted labels only (H2) | `harness.py:470-478` |
| 17 | Loopbreak: a signature seen twice in an 8-window blocks the third call (§3.3) | **MISMATCH**: it also requires the last two *outputs* to be byte-identical | `harness.py:26, 403-404` |
| 18 | Truncated turn asks for a small call and does not consume the nudge (§3.4) | VERIFIED; the paper omits the stop after 3 (`MAX_TRUNCATED_NUDGES=2`) | `harness.py:27, 359-376` |
| 19 | Two consecutive no-tool-call turns give `no_tool_call` (§3.5) | VERIFIED | `harness.py:377-384` |
| 20 | Context stop at >90% of num_ctx; 29,491 at 32,768 (§3.5, App. A) | VERIFIED | `harness.py:219-224, 334-344`; per-step `prompt_tok` is monotone in 4 local MLX episodes (same Ollama API, not the GH200 stack), i.e. the full prompt is counted |
| 21 | `max_steps=0` means no ceiling (§3.5) | VERIFIED | `harness.py:295-298`, `run.py:73` |
| 22 | 1800 s wall (§3.5) | VERIFIED as default; not hard (L1) | `harness.py:23`, `run.py:76` |
| 23 | Per-request timeout = remaining wall, first turn 600 s (§3.5, App. A) | VERIFIED | `harness.py:25, 308-313` |
| 24 | "not retried" (§3.5, App. A) | **MISMATCH**: timeouts are not retried; URLError/OSError and 429/5xx are retried 2× (Ollama), 6× (Cerebras), 16× (Gemini) | `model.py:947, 983-1011, 377, 714` |
| 25 | A 0-token first-turn timeout is re-run (App. A) | VERIFIED; the retry also re-runs non-timeout first-turn failures (M5) | `run.py:378-398`, `runtime.py:79-81, 822-833` |
| 26 | Seed = repeat index (§4.4, prereg) | VERIFIED (no `--seed`, or `--seed 0`) | `run.py:52-65, 288-290` |
| 27 | Hidden-test timeout 60 s on all 8 hard tasks (Table 2, App. A) | VERIFIED | `task.json` of all 8 |
| 28 | 7 of 8 hard tasks ship `SPEC.md` in `protect` (§4.1) | VERIFIED (`concurrency_race` has none) | `task.json` |
| 29 | 19 tasks in total, 11 easier (§4.1) | VERIFIED | `tasks/` |
| 30 | `selftest.py` 19/19 valid (§4.2) | VERIFIED at `82e453a` (run); **MISMATCH at HEAD**: 17/19 (C3) | run |
| 31 | `stress_test.py` 189 assertions (§4.2) | VERIFIED at both commits | run |
| 32 | `test_stats.py` 166 assertions "on the same commit that produced the grid" (§4.2) | **MISMATCH**: 160 at `82e453a`, 215 at HEAD | run |
| 33 | "about 3,500 lines of Python" (§3) | **MISMATCH**: `mh/` alone is 4,846; the instrument without tests is about 7,500 | `wc -l` |
| 34 | Stdlib only (§3) | VERIFIED | import scan |
| 35 | Cell mean weighted by scored episodes; 10,000 resamples, seed 0 (§4.5) | VERIFIED | `compare.py:494`, `stats.py:67` |
| 36 | Paired Δ vs the first five reps of `full` (§4.4) | VERIFIED (intersection of rep keys) | `stats.py:182-191` |
| 37 | 4-member confirmatory family, α=0.0127, 98.7% (§4.5) | VERIFIED in code; **MISMATCH with prereg and DEVIATIONS** (M1) | `compare.py:276-282`, `stats-v2.json` |
| 38 | Coverage 82.3% (5-rep), 94.1% (20-rep); 16 intervals, 96% FWER; Šidák 0.0032 (§4.5) | VERIFIED against `stats-v2.json` / `stats-all3.json` and the arithmetic; not re-run | JSON, `stats.py:381-409` |
| 39 | `n_starved=0`, `n_unserved=0` on all 18 v2 cells (§4.3, §4.7) | VERIFIED in `stats-v2.json`; raw episodes not local | JSON |
| 40 | App. A `protocol` object contains `share_gpu_demoted`, `starved_abort` | **MISMATCH**: those keys are in `outcomes`; the protocol also carries `top_p`, `reasoning_effort`, `concurrency`, `env_*` | `runtime.py:295-308`, `run.py:447-459` |
| 41 | App. A: Ornith one-flag cells under `--share-gpu` | **MISMATCH** with §4.3/§4.7 ("share_gpu false on all 1,440"); App. A describes v1 frozen cells under a "Tables 3–8" heading | text |
| 42 | App. A `--seed` = rep for `--repeat 5` | STALE (v2 used 20 and 5) | prereg §3 |
| 43 | Paper cites the unpaired interaction (§5.6) | VERIFIED as stated; **MISMATCH with prereg §4** (M2) | `stats-v2.json` |
| 44 | Containment backend bwrap 0.6.1 on v2 (§4.7) | UNVERIFIABLE here (no v2 episodes locally); O4 documents the false note | DEVIATIONS O4 |
| 45 | GH200 envelope, 78,027 samples (App. C) | UNVERIFIABLE (`results/compute.jsonl` not checked against the table) | — |
| 46 | App. D Fisher counts on `navigate` | UNVERIFIABLE in this pass (not recomputed) | — |

## 3. Test quality

- **Tests encode fixed attacks, not invariants.**
  - `selftest.py` hooks only `sitecustomize.py`.
  - `stress_test.py` C4 checks only `argv[0]` and `__file__`.
  - Neither exercises the verdict channel (C1) or frame introspection (C2). Both are one-line additions, and both fail today.
- **A red suite was committed.** `selftest.py` was correct and caught C3. `138336f` landed with `verify.sh` failing.
- **Mocked into tautology:** none found in the files read. `stress_test.py` F3 (`:253-258`) checks both that loopbreak fires and that the flag caused it. The stats tests assert on numbers.
- **Count parsing in `verify.sh:1306-1307`** takes the first integer on the last line and requires it to be above 0. Exit codes govern pass or fail, so this is sound.

## 4. Duplicated logic (DevMap `devmap_clones`, 106 groups, 52 shown, truncated)

- `check()` is an exact clone in 8 bench test files. It is harmless.
- `_under_untrusted` / `_Blocked` are exact clones in `concurrency_race`, `globmatch` and `nfa_match/hidden_test.py`. Hardening applied to one hidden test drifts from the others: only 3 of 19 hidden tests carry the stdlib-shadow and import guards.
- A hidden-test helper is structurally cloned in the visible test (`state_machine_fuzz` `frame`/`crc16`). That is expected.
- DevMap did not surface a clone between `mh.pool.PROTOCOL_KEYS` (allowlist) and `mh.runtime.PROVENANCE_KEYS` (denylist). Both files document that the two must agree. There is no test tying them together.

## 5. What I could not check

- The v2 and ext-cerebras episodes: only the two `qwen3.8_27b-mlx__*__v2` dirs are local. The exploitation audit of v2 (O5, C1, C2) therefore remains open.
- The `envbuild` shell task was not probed for an exit-zero or sourced-`exit 0` bypass.
- The bwrap path (M4, L7) is inferred from source. Every containment probe ran under `sandbox-exec`.
- The 94.1% / 82.3% coverage figures were read from JSON, not recomputed. Recomputing at 20 reps is about 1e9 resampling ops.
- App. C and App. D numbers were not recomputed.
- `figures.py`, `report.py`, `probe.py` and `grid.py` were only skimmed where they touch the findings.

## 6. Tool trail

- DevMap generation 302, `repository.root` = this repo, `is_fresh: true`. Calls made: `devmap_status`, `devmap_neighbors` (5 targets), `devmap_clones`.
- Gap recorded: callees were unavailable for `Task._expected_new` and `seed_for_repeat` ("no indexed traversal start"). Python resolution is 236‰, so verifier-internal edges were read from source. Appended as `GAP-MANVI-BENCH-AUDIT-CALLEES`.
- GitPulse insights: 1 worktree, no other agent sessions, 4 untracked files. It reported source freshness false because of the untracked paper files.
- Probes, all writing only under the scratchpad:
  - `probe_verify.py`
  - `probe_exit_all.py`
  - `probe_stats.py`
  - `probe_hardlink.py`
