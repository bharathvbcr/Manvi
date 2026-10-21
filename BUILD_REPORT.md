# Build report: dynamic harness

Dates: 2026-10-20 to 2026-10-21.

The run worked the GitPulse **Dynamic harness 0/6–6/6** cards (the user asked to focus on these). The base was main `9bea4d9`, and integration happened on the non-default branch `dh-stack`. The owner authorised the merge to main on 2026-10-21 ("if done commit and merge to main"). Nothing was pushed.

Every card stays in **review** and none were requested **done**. Manvi's evidence gate has not passed: `./verify.sh` does not reach a verdict on main either (see Verification).

## Process
- **Orientation and roles.** Each wave started with GitPulse insights or collision risk, then DevMap status, impact and affected-tests.
  - GitPulse facets often deferred under git-spawn load. Those were treated as unknown, not clean.
  - DevMap answers were usually `walk_incomplete` or truncated, so the full suite was used as the gate.
- **Per card.** Each card went through the same steps:
  - an Opus brief;
  - a Sonnet implementer in its own worktree;
  - a separate Sonnet adversarial pass;
  - an Opus review, which sent work back each time something broke;
  - an independent re-run in a pinned layout (`tmp/env/`: DevCouncil at pin `1ae3bc1` and gusset at pin `984209b` beside a Manvi worktree, so that both the Go `replace` lines and the `crates/` symlinks resolve to the pins).
- **Fable** ruled on sequencing and judgment calls. Owner decisions were asked once and recorded on the cards.

## Merged to main (via `dh-stack`): in review

| Card | Branch → head | What landed |
|---|---|---|
| ft-aa63dcc6 dh-0 | dh-0-observability `4dc1efd` | Per-step usage on every `step/end`, as `StepEndData` including `NoResponse`. A sha256 digest of the exact System text on `system/prompt_assembled`. New wire-only NDJSON kinds `step.usage`, `verify.report` and `diagnostic`: terminal faces and TUI status skip them, children's `step.usage` is dropped, all text is scrubbed, and diagnostics are capped at 16 KiB. |
| ft-35d054f0 dh-2 | dh-2-skills `9f4cebd` + dh-int `a6c2752` | `manvi/harness` skill loader for `.agents/skills`, plus the `devcouncil_load_skill` tool. The loader goes through the one read gate and has a strict frontmatter parser. A budgeted always-on listing appears in both TUI and headless prompts, with cut skills named. Notices are emitted once per change. |
| ft-42089a55 dh-1 | dh-1-spec `9760289` | `HarnessSpec` with strict decode (forbidden and case-fold-duplicate keys refused) and `Validate` that lists every problem. Digest, Catalogue and repo specs. Roles can never be wider than the session. `spec_name`/`spec_digest` are logged. Operator-scope `harness.spec` flag. `workflow.DecodeStrict` was fixed at the owner for every caller. |
| ft-396e4995 dh-3 | dh-3-serve `2a0e8da` | `harness.resolve` under `ops.optional`. Sections are returned and the host composes System (owner decision). `chat.prepare` gains `harness_digest` and `prefix_moved`. Managed runs materialise the spec into CLAUDE.md or AGENTS.md (owner decision): nonce'd, byte-exact, restored on exit or stop, crash-recoverable, one per checkout, and verify refuses while a block is live. The contract diff is additive only (CHECKSUMS updated). |
| ft-7e483ef4 dh-5 | dh-5-reflect-v2 `21b2a26` | `manvi reflect`. A read-only reflector. An evidence packet built from structured log records only, with no MANVI-VERDICT text. A fail-closed parser. Lessons stored in `artifacts/lessons/<library>/` with CAS revisions, history and scrubbing. `runenv.go` dedupes run and reflect setup. |
| ft-89c22437 artifacts | artifacts-nofollow-read `a1c99b9` | Get, List and Update read no-follow: links, hard links, FIFOs and case aliases are refused, and there is a 4 MiB cap. List returns partial results with skipped entries named. |
| ft-13799305 lint | lint-failover-limitreader `c91cb64` | Main's enforced-lint failure (unbounded ReadAll in `failover_test.go:39`, from 62a22c1). |
| (integration) | dh-stack `576041b`, `301129c`, `f458573`, `46e73aa` | The Windows test build: a constant moved out of a unix-only file. A `devcouncil_load_skill` probe in the content-reader security invariant, mutation-checked to fail when the read path is bypassed. gofmt. No new nilaway findings: `Store.Get` error flow, a `lessons.Parse` guard, and tests that check errors. |

Adversarial passes broke the following, all fixed before merge:
- dh-0: 4
- dh-2: 8
- the dh-2 wiring: 3
- dh-1: 4
- dh-3: 6 (materialisation)
- dh-5: 8 tests
- artifacts: 4
- dh-4: aliasing and command rungs

## Prepared, not merged
- **ft-d4be683f dh-4 (security).** Branch `dh-4-prompt-inputs` `dab94d9` holds Manvi tests only. They are red until the DevCouncil pin moves, so the branch is not merged.
  - The DevCouncil patch (11 files) and HANDOVER.md are in `.claude/worktrees/agent-addf660a74529f65e/tmp/dh4/`.
  - It adds a posture-independent, write-only `PromptInputPatterns` rung (`RuleRestrictedPath`). It covers AGENTS.md, CLAUDE.md, GEMINI.md, mcp.json and .mcp.json at any depth, skill roots, and plugin declarations. It checks lexical and identity aliases.
  - Fable accepted documented residuals: interpreters, archives, and git reset/merge/am under every posture.
- **ft-e112a743 dh-6.** Deferred by the owner. Its open questions are recorded on the card.

## Verification (pinned layout, full `./verify.sh` on `dh-stack`)
- **Passed:**
  - Release leg.
  - gofmt, and `go vet` on windows, linux and darwin.
  - **Full Go suite: 4,958 tests**, with the same 3 skips as main (4,146 on main).
  - Statement coverage 84.1% (main 83.7%).
  - `go mod tidy`, dependency surface with cgo off and on.
  - Enforced lint 0 with cgo off and on.
- **Debt ratchet.** Main itself breaches `.golangci-debt.counts`: errcheck 196/179, errorlint 3/1, staticcheck 37/33. `dh-stack` has identical counts on those linters and fewer findings overall (612 against 617; gosec 255→254 and 247 cgo, nilerr 10→7). Recorded on ft-43c56026.
- **govulncheck.** It fails on main and on the stack with go1.27.2, on stdlib GO-2026-6809 (crypto/x509, fixed in go1.27.3). With `GOTOOLCHAIN=go1.27.3` it is clean on the stack. This is the local toolchain, not the code.
- **nilaway.** Main reports 73 against a ceiling of 71. `dh-stack` reports 73, identical file for file, after the stack's 12 new findings were fixed.
- **Not run on any tree, main included:**
  - the legs after nilaway (Rust fmt, clippy and test; parity fixtures; the cgo gusset-engine leg; store and Python interop; bench);
  - `-race` on `cmd/manvi` and `serve`, which needs a libgusset build.
- **Blocked by cargo slots.** Store-backed tests time out whenever this machine's cargo-slot-gate (two slots) is held by other sessions' long jobs (ft-5845a89b). Three full runs passed when slots were free; the last run, after a toolchain switch forced rebuilds, did not.
- **Benchmarks.** dh-4's write-evaluation rung costs +10.9% (235.6 → 261.2 µs, +23 allocs). That is justified as a security rung at about 26 µs per evaluation, on a loaded machine. No other touched hot path has a benchmark (`endStep` and `Log.Append` were unchanged).
- **Live runs.**
  - Local (ollama `qwen3.8:27b-mlx`): the skills listing was in the prompt and the model loaded a skill on demand, including a cut one. `manvi reflect` on a real session gave `NO_ENTRY`.
  - Cloud providers: not verifiable, because no anthropic, gemini or xai credential is configured.

## New and updated cards
- **ft-5845a89b:** store-backed Go tests starve behind the cargo-slot-gate (DevCouncil testsupport).
- **ft-13799305:** main's enforced-lint failure (fixed and merged).
- **ft-f045f425:** DevCouncil `onDiskSpelling` re-lists directories on every call.
- **ft-4e03a738 (updated):** sibling DevCouncil drift now breaks the build: `ReadyReport(ctx, store.ReadyMode)`.
- **ft-43c56026 (updated):** main breaches the re-baselined debt counts.

## Decisions
**Fable:**
- wave order;
- new NDJSON kinds rather than reusing `notice`/`turn.usage`;
- dh-4 posture-independent, write-only;
- any-depth prompt-input names;
- reuse `RuleRestrictedPath` (no verdict-contract change);
- unmodelled programs as a documented residual.

**Owner:**
- dh-3 returns sections and the host composes System;
- dh-3 materialises into managed checkouts;
- dh-6 deferred;
- the pin bump needs a fresh ask with deltas;
- merge to main.

**Opus (review):**
- `harness.spec` is operator-only (a repo naming one needs a DevCouncil catalogue change);
- the reflector role is fully read-only;
- inline specs are refused on the managed plane;
- one read gate only (a second, lighter gate was rejected).

## Owner decisions still pending
1. **Apply the dh-4 DevCouncil patch** from a DevCouncil-rooted session, then **bump `scripts/module-pins.txt`**. The bump also pulls in the existing drift (sibling `aacffb8` against pin `1ae3bc1`), and Manvi must adopt `ReadyReport(ctx, store.ReadyMode)` in the same change (ft-4e03a738).
2. **Should yolo still allow writes to secrets and agent-config trees** (`.env`, `.claude/settings.json` hooks)? This would overturn `TestYoloPostureAlsoDisablesHardRules`. Fable recommends making secret writes posture-independent but keeping agent-config trees writable under yolo.
3. **Cross-repo consumers of the serve contract.** GitPulse re-vendoring and Jarvis are next; see `.claude/worktrees/agent-ad4ccf9496e65d7b3/tmp/dh3-HANDOVER.md`. Jarvis is still blocked by ft-8e5a8922.
4. **dh-6:** Python oracle in bench, cost and wall-clock budget, probe isolation.
5. ~~**Local Go toolchain** to ≥ go1.27.3 (GO-2026-6809). Also, `go.mod` pins `toolchain go1.26.9`.~~ Done on 2026-10-21. Homebrew Go is now go1.27.3, and `go.mod` uses `toolchain go1.26.10` (merge 502ef14). See below.

## Later waves (2026-10-21)

The owner asked Fable to make the pending decisions. Fable's rulings are recorded on each card:
- **dh-4:** pin to `1ae3bc15` + patch (commit X), not to the `aacffb8` drift.
- **Yolo:** secret writes become posture-independent (ft-f4dbb1f8); agent-config trees stay writable.
- **Serve contract:** GitPulse re-vendors now; Jarvis waits for ft-8e5a8922.
- **dh-6:** Python oracle allowed; $1 and 10 minutes per tuned task; host-provided worktrees for isolation; no live cloud probes without the owner.
- **Debt:** fix it down, don't re-baseline.
- **Push:** don't push.

The owner then authorised cloud spend through Claude and a Go upgrade.

| Merge | What landed |
|---|---|
| 502ef14 | `toolchain go1.26.10`. It fixes GO-2026-6809 and stays on the 1.26 line that CI and release read from go.mod. That is a patch move, smaller than the go1.27.3 Fable suggested. Verified on go1.26.10: build, vet, tidy, govulncheck 0, core test packages. |
| f39f5e0 | docs/WORKBENCH.md records the **first live managed Claude run**: Claude Code 2.1.306 on the subscription login, through `manvi serve`, with a repo harness spec. |
| bee056f | **Main's own gate breaches fixed down** (ft-43c56026): errcheck 196→179, errorlint 3→1, staticcheck 37→33, nilaway 73→70. They traced to a38dc70 (`cmd/anthropic-account`) and 62a22c1. Along the way, `anthropic-account` no longer exits 0 on a closed stdout. **verify.sh: `PASS with gates that did not run`** — 3 skipped tests, no code_graph.json in a fresh worktree, hosted providers scripted only. This was the first real verdict of the run. |
| 979dcb2 | **wave2:**<br>• ft-447797ec: the overflow flag now has one owner.<br>• ft-ff7dfd1a: the session trim comma off-by-one is fixed, and Save refuses a file Load would refuse.<br>• ft-e58f537b: fetch SSRF ranges, zone and mixed-answer bypasses, end-to-end timeout.<br>• ft-fe6c326d: shell wrappers around `claude` are refused before activation; npm/node launchers are accepted, a correction made in review.<br>• Ratchet lowered: nilerr 7, nilaway 70.<br>• Gate on that tree: every leg passed through the Rust tests and the DevCouncil host build. It stopped at the cgo gusset-engine leg on a cargo-slot wait (`testsupport.DCVerify`). |
| (this merge) | **wave3:**<br>• ft-de8d7a32: Windows env names compare case-insensitively at one owner, `childenv.Key`, which also serves codingagent's `agentEnv` and `mcp.buildEnv` (class fix).<br>• ft-11cebc9d: the killed-save test is deterministic (write-step seam, mutation-proved, 50/50 under load, portable self-kill).<br>• ft-c9cf8150: `devcouncil_load_skill` is registered only when the repo has skills (−122 schema tokens). The skills listing refuses a non-directory and is bounded by ctx, which closes a FIFO hang that also affected main's per-turn prompt assembly.<br>• ft-d9525675: managed Codex has no process-identity check (code-cited), so the change is docs only. |

**Live cloud** (Claude, key injected per process from the Keychain by `anthropic-account`, never printed):
- dh-2's cloud criterion passed on `claude-opus-5-5` for $0.10.
- `manvi probe anthropic` was OK twice; the default probe model is the current `claude-haiku-5-5`.
- The managed Claude run passed: the harness block was materialised, the agent quoted it, and the file was byte-identical afterwards.

**New cards:**
- ft-f4dbb1f8: yolo secret writes.
- ft-fe6c326d: the Claude wrapper (fixed).
- ft-11cebc9d: the flaky killed-save test (fixed).
- ft-c9cf8150: conditional skill tool (fixed).
- ft-d9525675: the Codex launch shape (answered).

**Environment findings, recorded and not bypassed:**
- **Cargo slots:** this machine's two-slot cargo gate starves store-backed tests (ft-5845a89b).
- **Lint lock:** golangci-lint's lock is machine-wide, and another project's run can stop the gate.
- **Load-sensitive Rust test:** `dc-redact`'s scaling test fails at load 33 on 18 cores and passes below 20.

**Still pending:**
1. **dh-4:** apply the patch in DevCouncil (`tmp/handover/dh4/`), push commit X (owner), then bump the pin.
2. **The aacffb8 drift bump** (ft-4e03a738), with both `ReadyReport` call sites.
3. **Other repos:** the GitPulse re-vendor (`tmp/handover/dh3-HANDOVER.md`) and the Jarvis build fix plus its consumer test.
4. **DevCouncil-owned:** ft-f4dbb1f8, ft-5845a89b and ft-f045f425, and repo naming of `harness.spec`.
5. **Owner:**
   - credentials for Gemini and xAI;
   - Codex/OpenAI spend for ft-2550b25d's managed Codex qualification;
   - dh-6, still blocked on the bench containment cards;
   - push main.
6. **gosec:** it stays at 255 until it is measured on linux with cgo. Darwin reports 247 on both legs.
