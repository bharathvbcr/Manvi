# Manvi ↔ DevCouncil: native integration plan

Owner's design (2026-11-20, stated by the owner; keep it as the north star):

- **DevCouncil is modular.** Its components (devmap, dc-grep, dc-verify, dc-store, dc-glob, dc-redact, …) can be integrated into any of the owner's apps. They are developed in DevCouncil's own repository.
- **Manvi is a custom harness that uses DevCouncil natively.** It calls those components first-hand, through adapters, and never reimplements them. The adapters are DevCouncil's Go clients in `backend/go_orchestrator/dc/*`, which Manvi links through its `../../DevCouncil` replace and pins in `scripts/module-pins.txt`.
- **Rust ↔ Go interconnect is gusset** for Rust hosted in-process. Separately versioned components keep a JSON-on-stdio process contract. Which transport a component uses is decided per component in DevCouncil's `docs/gusset-candidates.md`.
- **Agents in Manvi are forced into good behaviour by the harness, not by prompt bulk.** That means auditing before acting, navigating by the code graph, batching independent lookups, never assuming, and verifying before claiming. The prompt stays light; enforcement lives in Go.

## Done on branch feat/native-devcouncil-tools (uncommitted as of 2026-11-20)

- `devcouncil_batch` runs up to 16 lookups in one step, 4 at a time, through the same agent-loop guards as single calls. Each call can keep only matching lines (`match`) or a line cap (`max_lines`), and every call and the whole result have a byte cap.
- `devcouncil_code` answers explore, impact, trace and affected questions through DevCouncil `devmap.Client.Advanced`. It is a core tool whenever a code map is attached, and source excerpts in its answers pass the read gate.
- `devcouncil_read_file` takes `offset` and `limit`.
- `devcouncil_write_file` refuses to overwrite an existing file that was not read in full this session. A ranged read or a filtered batch view does not count as reading it.
- The loop enforces batching (`harness.serial_lookups`, operator scope, default `refuse`). It adds a note at the third lone lookup in a row and refuses the fifth once; see the second pass below.
- A 3-line essential "operating contract" prompt section (55 estimated tokens, 68 with the batch line), held under 120 tokens by a test.
- DevCouncil branch feat/devmap-ask-evidence adds an `ask_evidence` kind to `Advanced` and a matching devmap status capability. Its validator was checked against real devmap output.

## 2026-11-20, second pass (Fable's advice, built)

- Batching is now enforced. `harness.serial_lookups` defaults to `refuse`: the third lone lookup in a row gets a note, and the fifth is refused once without running. Sent again, it runs. A one-call batch counts as a lone lookup. The refusal stays out of the repeat ledger and costs one step, not a stall. `note` and `off` also exist.
- Batch fixes found by a live Haiku 5.5 run:
  - The description now names full tool names (it said `grep`, and Haiku's batch failed).
  - A short name is answered with the full one.
  - Each call gets a fixed byte budget that the handler's view and the fold share. The fold used to cut later answers that `read_file` had already marked as read, so the write guard trusted a cut read.
  - Line counting now matches the batch view: a 200-line file read whole under a 200-line view had not counted as read.
- Live run (Haiku 5.5, `anthropic-account` default account; no `manvi-cloud` account exists). Same 4-question read-only task, `off` and `refuse` alternating, n=3 each:
  - Every run answered 4/4 correctly and changed no files.
  - Every run batched on its own, with 16-24 calls in 4-7 batches.
  - The rule never fired: 0 notes and 0 refusals.
  - Median input was 303k tokens for `off` and 204k for `refuse`. Spend was about $0.01 per run.
  - The first request was identical (11,411 tokens), so the arms were the same treatment and that gap is run-to-run variance in the search path, not an effect of the rule.
  - The pilot, run before the fixes, used 543k input tokens, 13 steps and 35 unbatched calls. That run differed in code and in what answers existed, so it is not a clean comparison.
- Chaining through Go code works. A script run with `devcouncil_exec_command` can call `./bin/manvi tool …`, and the chained `read_file` of a secret was refused. The same script can also read the file directly, and `grep -r` through the shell printed it in dev posture. Only a command that names the secret path (`cat secrets/x`) is refused. That gap is pre-existing, in DevCouncil's command gate. Advertising scripts as the chaining path waits on the owner.
- dc-verify: Manvi already runs it at the end of every turn that changed files (`cmd/manvi/sensor.go`: dcverify plus an optional `MANVI_VERIFY_COMMAND`), sends failures back and escalates. A read-only turn records `skipped: no change`.
- Pin delta, read-only: DevCouncil main is now 147 commits past 230bbf04. Of the paths Manvi links, about 60 commits changed 155 files (+19,330/−1,999), across verify, dc-redact, dc-store, policy, dc-grep and dcrelease. Neither dc-store nor devmap-store changed its schema version. The off-pin grep test failure most likely comes from 29cf2dde (dc-grep's opt-in hidden/ignored search flags); that is inferred, not bisected.
- Not done:
  - The dc-redact gusset A/B. It needs dc-redact added to DevCouncil's gusset engine, a DevCouncil change that should get its own go-ahead.
  - The scripted token-arithmetic bench, superseded by the live runs.
- `harness.serial_lookups` is operator scope. A `.devcouncil/config.yaml` that sets it is refused at startup with a named rule; set it from the environment or with `manvi flags set`.
- The refusal fired live on a prompt that demanded one read per step. On the default, Haiku got the note at the third read and the refusal at the fifth, resent the read and it ran. With `off`, neither happened. Because the prompt forbade batching, the refusal cost one step (8 steps and 524k input tokens against 7 and 429k, n=1 each). `TestTheFaceHandsTheSerialLookupModeToTheLoop` pins the setting from a face's registry through to the loop. Sub-agents get it through `subAgentConfig`, which is wired but not separately tested.
- No step after `read_file` cuts a plain top-level read: `ReadPinned` refuses a file over 2 MiB and does not truncate it, and the tool pipeline and loop apply no cap on result size.
- There is no evidence yet that a batch beats native parallel tool calls. Haiku made up to four native calls per step unprompted, and no arm ran without the batch tool.
- Known limit of "audit first": `devcouncil_patch_file` and delete need no prior read.

## Open decisions (owner)

1. **Pin bump.** Manvi pins DevCouncil at 230bbf04, which is 147 commits behind main as of 2026-11-20 (about 60 of them touch the paths Manvi links; see the second pass). Bump it so Manvi can expose `ask_evidence`, then carry the bump to GitPulse (re-vendor) and Jarvis. Ask before bumping.
2. **True code-mode chaining.** One call's output feeding the next would need a script interpreter, which is a new dependency.
3. **Online search for agents.** This widens network access, so it needs a security-impact statement. The only existing route is `devcouncil_fetch_url` behind an operator allowlist.
4. **Deeper gusset.** The measured record and a 2026-11-20 survey (Sonnet survey, Fable review) found no Manvi hot path that meets the revisit bar. The one experiment worth running is pattern redaction of tool results through dc-redact, framed as a security feature, behind an isolated interleaved A/B and a parity oracle. Moving dcgrep, devmap, dcverify or dcstore in-process would reverse a measured decision.
5. **What "dc-verify executed to the end" means.** It could mean running Manvi's `verify.sh` to completion, or making agents run DevCouncil's dc-verify before they finish. Not yet confirmed.

## Where things are

- Worktrees: `~/Code/devtools/Manvi/tmp/env9/{Manvi,DevCouncil,gusset→symlink}` holds the branches. `tmp/env10` holds the same Manvi changes with DevCouncil and gusset at their pins, for the full gate.
- The full gate passes only at the pins. Against DevCouncil main, `TestCratePinsMatchDevCouncil` and one grep test fail, before and after this work, because main has drifted from the pin.
- 2026-11-20 gate (`tmp/env10/Manvi`, PATH led by `/opt/homebrew/bin` for Python ≥3.11, `MANVI_CONSUMER_JARVIS` set):
  - The verdict was "PASS with gates that did not run": 39 steps, 8627 Go tests with 8 skips that predate this work, and the local and live provider contracts not run.
  - Jarvis builds against the tree.
  - Live through the built binary, `devcouncil_code` answers all four kinds against real devmap.
  - That live run found the redaction stripping devmap's `evidence.source` provenance tag (Resolver, Stored or Reconstructed). Fixed, with `TestCodeGraphKeepsEvidenceProvenance`.
  - `ask_evidence` works end to end through DevCouncil's Go client against a devmap built from feat/devmap-ask-evidence. The installed devmap 0.2.5 refuses it because the capability is not advertised.
  - Graph read gate, checked against real output: only `definitions.items[].source` and ask's `files[].units[].source_span` carry file text, and both name their own `file_path`. `TestCodeGraphGatesAskEvidenceUnits` pins the real ask_evidence fixture. In a live negative, `secrets/vault.go` came back as `withheld: path.secret_read` (devmap also blanks it upstream).
  - Go-script chaining already works: `devcouncil_exec_command` runs `go run` under the command gate (it needs a lease in enforce). A script that calls devmap or dcgrep directly bypasses Manvi's read gate on their output.
  - `devcouncil_batch` is exercised through the agent loop in tests only. `manvi tool` cannot drive it, and no live model turn was run.
