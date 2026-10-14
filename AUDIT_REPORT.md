# Manvi audit 2026-10-13

No source, config, or dependency was edited by this audit. The only writes are GitPulse board updates and this file. Nothing was moved to `done`. No card was deleted or merged. No `in_progress` card existed at filing time (`gitpulse_list_tasks` status `in_progress` returned 0).

`[MET — verified in source]` means the cited lines were read. Tests were not executed. A review log is evidence of an earlier session, not a re-run.

## Map

GitPulse Insights at report time (branch `main`, one worktree):

- Agents: `ok`, 0 Claude sessions, 0 Codex sessions.
- Collisions: `ok`, 0 overlapping files, 1 worktree scanned.
- Ledger: recording.
- Code graph: store present, generation 644, `source_freshness` true, `analyzer_freshness` false, `is_fresh` false. Reason: stored extraction payload is obsolete. The map-phase DevMap read was generation 640, schema 27, `query_ready`, about 10,433 nodes. Dead and unwired answers are a lower bound (`walk_incomplete`, confidence at most 0.58). Empty graph answers are unknown. The index was not rebuilt.

The worktree is dirty with 8 unstaged files this audit did not write (+254 / −17):

- `docs/DEVCOUNCIL_PORT_ROADMAP.md`
- `docs/KNOWN_GAPS.md`
- `manvi/devcouncil/verify.go`
- `manvi/devcouncil/verify_paths.go`
- `manvi/devcouncil/verify_paths_test.go`
- `manvi/serve/managed_stop_test.go`
- `manvi/serve/verify_checks.go`
- `manvi/serve/verify_test.go`

The verify diff adjusts how an absent coverage file is reported (`coverageUnmeasuredNote` in `verify.go`, applied after `settle` in `verify_paths.go`). `manvi/devcouncil/verify.go` is on `ft-d366f0d0`'s planned files. Wave 1 should not start until that uncommitted edit is settled. The serve files are not the planned files of the new serve-critic criterion (`verifycritic.go`).

## Severity × dimension

Counts are verified findings, after downgrades. One concern can be several findings and still one card. Strict typing and performance have no new finding. Existing performance cards were left as they are (`ft-6e014d9e`, `ft-66398542`, `ft-9bff2fd4`, `ft-feb8e07d`). No benchmark-baseline card was filed.

| Dimension | Critical | High | Medium | Low |
| --- | ---: | ---: | ---: | ---: |
| Security | 0 | 6 | 3 | 1 |
| Defensive validation | 0 | 1 | 0 | 0 |
| Async error boundaries | 0 | 1 | 2 | 0 |
| Correctness (Win / macOS / Linux) | 0 | 1 | 6 | 1 |
| Modular architecture | 0 | 0 | 1 | 0 |
| Deterministic testability | 0 | 0 | 1 | 0 |
| Strict typing | 0 | 0 | 0 | 0 |
| Performance and benchmarks | 0 | 0 | 0 | 0 |

High security: unfiltered `git diff` / `git show` stdout; outside-root credential reads; commit scan skips an unreadable, oversized, or binary secret; quick-action git inherits forge tokens; model shell can fetch while stamped contained. High validation: a repaired truncated tool-call object is dispatched. High async: the desktop broker keeps going after a cancel that arrives once the write has been accepted. High correctness: the Linux worker does not apply the fork and signal ban the seatbelt path applies.

## Top 10 risks

1. Outside-root credential reads are classified before the secret patterns, so a path outside the root can be softened to "command not allowed" at host posture. `ft-b08f884e`.
2. Truncated tool arguments are repaired into a runnable object and dispatched. `{"path":"/tmp/a"},"content":"PARTIAL` becomes `{"path":"/tmp/a"}`. `ft-074e1445`.
3. Leaseless `git diff` and allowlisted bare `git show` return stdout with no secret withhold. `ft-d366f0d0`.
4. Quick-action git runs with the parent environment, so forge tokens and hook scripts see them. `ft-530b2386`.
5. A desktop-broker cancel after the write is accepted deletes the pending call and returns without stopping the broker. `ft-871861f4`.
6. The model shell's seatbelt and bwrap argv allow network, and `containment_event` still records a named backend as contained. `ft-4dab6997`.
7. The Linux worker bwrap argv never receives the fork and signal ban. `ft-6eb61e48`.
8. Commit scan continues when a secret-path file cannot be hashed, is oversized, or cannot be read, and a binary patch with no `+` lines is missed. `ft-b08f884e`.
9. An MCP `Call` that times out during `initialize` can send a second `initialize` while the first is outstanding. `ft-039385c0`.
10. A session `Save` that links the new file and then fails the directory sync records the write as unseen, so the next save reports a conflict. `ft-f505b844`.

## New cards

Label `audit-2026-10-13` is on each. Status was left as filed. None are `done`.

| Item | Title | Priority | Severity | Status |
| --- | --- | --- | --- | --- |
| `ft-530b23867a667596d974711fdf923733` | Quick-action git inherits the parent environment | 1 | high | backlog |
| `ft-871861f409c0d48fa1205f8965caf8c8` | Desktop broker continues after a post-write cancel | 1 | high | ready |
| `ft-074e1445d6a6de1fff7373fa9c32e0f4` | Repaired truncated tool arguments are dispatched | 1 | high | ready |
| `ft-6eb61e482a0142db909f88d8c177cec4` | Linux worker bwrap drops the fork and signal ban | 1 | high | ready |
| `ft-447797ec3b950dbd07db0e3c4d6126cc` | Non-empty insufficient plan leaves the overflow flag clear | 2 | medium | ready |
| `ft-5bf532700481c97fc4dda3710b43f24c` | Serve checks redirects after allowlist demotion | 2 | medium | ready |
| `ft-f505b844726ef552ba44f7feb9e1136d` | Session sync error poisons the next save | 2 | medium | ready |
| `ft-0b338d23500b424ac153026f23692b0f` | Local discovery advertises images and caches a cancelled probe | 2 | medium | ready |
| `ft-ff7dfd1adb1c6cc1b6f19ae6c543509e` | Session trim charges a comma the join does not write | 2 | medium | ready |
| `ft-e58f537b3a80568d2bd589af5f9c99a6` | Fetch prefix block list misses documented ranges | 2 | medium | ready |
| `ft-de8d7a3292b567d8f477f6a1dc5b8c57` | Child env filter is case-sensitive | 3 | low | backlog |
| `ft-89c224376bf199fab8d5f87ee7eb8045` | Artifact names follow a symlink and List is case-sensitive | 3 | low | backlog |

`ft-5bf53270` is the redirect-order finding only. It does not change GitPulse's default `enforce_allowlist`. Current GitPulse still denies `cd … > .env` when `policy.unreadable_only` is absent, so the write does not land today. The bug is the order in `manvi/serve/policy.go`: `checkCommand` demotes, then returns on `Blocked()`, and only then would call `EvaluateRedirects`.

## Folded cards

Each stayed `review`. Priority was not lowered. The only severity change is `ft-9adb5aff`, from unset to medium.

| Item | What was added | Revision |
| --- | --- | --- |
| `ft-d366f0d0d8fa62f12840446948d17334` | Leaseless shell `git diff` / bare `git show` must withhold secret paths. Criteria 1, 3, 4 marked met in source. Criterion 2 stays open. | 3 |
| `ft-b08f884e641b525f6e83a6c26695defa` | Commit scan fails closed on unreadable, oversized, and binary secrets. An outside-root credential path is a hard secret-read. GitPulse `enforce_allowlist` is not changed. Criteria 1–4 were not marked met. | 3 |
| `ft-4712431ba5726bfb3d7566ba9ac95e4a` | A header timer that fires in the same window as a successful `Do` must not discard that response. All four prior criteria marked met in source. | 3 |
| `ft-039385c0d4f9298cc2f40920abdc3213` | Initialize timeout or cancel must not start a second handshake, and a late result is adopted once or discarded. Criteria 1–5 were not marked met. | 3 |
| `ft-abefbeed09b4ee55cb984736bbf4849c` | Directory sync after `Link` / `Rename` must roll back. Criteria 1, 2, 3, 5 marked met in source. Criterion 4 stays open: `writeContained` returns `d.Sync()` after the name is published. | 3 |
| `ft-4dab6997ff3ce430f5823ac76c21d69f` | Model shell denies network the way the worker does. An unreadable `.git` pointer still guards the object store. Criterion 3 marked met (`rpc.py:957` `(deny network*)`, `rpc.py:1011` `--unshare-net`). Criteria 1, 2, 4 stay open. Remaining edit is `bench/mh/tools.py` only. | 3 |
| `ft-730dc5e52b3c63fbdb25b0a10ba70a60` | `serveCritic.Judge` must set the critic tier through `agents.Route` and must not inherit the session model. Criterion 4 (checkpoint sensor) marked met in source (`sensor.go:380-387`, registry floor `subagent.go:448`). Criteria 1–3 and 5–7 stay unmarked. | 4 |
| `ft-95e4e915e8eb01d7220cad8cad3bc7e8` | Win32 exit code 259 (`STILL_ACTIVE`) must not keep a dead process in flight (`lock_windows.go:73-74`). Criteria 2, 3, 4, 6, 7, 8 marked met in source. Criteria 1 and 5 stay open. `Prices.Reserve` still uses `max(Input, CacheWrite)` (`prices.go:112-114`). | 4 |
| `ft-9adb5aff81e72650229b40ee35ec08b4` | `cargo audit` must cover `native/Cargo.lock`. Severity raised to medium. Criteria 1 and 2 marked met in source (`verify.sh:553-563`); the commands were not re-run. Criteria 3 and 4 stay open. | 3 |

`ft-b08f884e` is split across waves on purpose. Wave 1 owns the DevCouncil policy files for the outside-root rule. Wave 4 owns `commitscan.go`. It does not edit `manvi/serve/policy.go` (that file is `ft-5bf53270`).

## Verified in source, board text not fully annotated

These were read or carried from the verification pass. They were not moved to `done`. Where this filing did not rewrite the card, the board text is unchanged.

Marked on the card during a fold (see the table above): portions of `ft-d366f0d0`, `ft-4712431b`, `ft-abefbeed`, `ft-4dab6997`, `ft-730dc5e5`, `ft-95e4e915`, `ft-9adb5aff`.

Read as met in source and not rewritten on the board:

- `ft-14c6dbb5` managed Prepare unlock.
- `ft-b6b049` string-aware commas. xAI `TextToolFallback` stays false. This is not the repaired-tool-args card.
- `ft-7d4c987` EOF without `finish_reason` is `TruncatedStream`.
- `ft-23cb1db3` replace-by-rename. Left open in the log: `safefile.WriteAtomic` does not fsync.
- `ft-dff034cd`.
- `ft-986b39fc` `baselineFor` in `sensor.go`.
- `ft-32d63674` double default suite.
- `ft-66398542` cache half landed; `Save` still rewrites the session. Left open. Not a home for the session-sync or session-trim findings.

Partial, left open, not rewritten:

- `ft-7fb7af56` Ubuntu and macOS run `verify.sh`. `windows-latest` is `CGO_ENABLED=0` go build/test, not `verify.sh`. The cgo gap remains `ft-fed65e48`. The budget card's Windows cross-build stays here.
- `ft-f2fb1b30` `PROVENANCE.json` is not a signature.
- `ft-01bbeea30` pins exist. `release.yml` still has `curl | sh` for rustup.
- `ft-0fcd6c4c`, `ft-fbfb77ab`, `ft-43efef72`, `ft-38893ac5` were re-read. Their logs claim landings. Criteria that are still unmet stay unmet. None were closed.
- `ft-e72ff569` gate criteria were not blanket-marked. The log says gates landed on `hx/gates` `9f94988`. They were not re-executed.

Auditor-only, not verifier-marked, board text unchanged: `ft-25a21990`, `ft-2eb75386`, `ft-434ccbfd`, `ft-536e31fd`, `ft-632d334d`. The env-docs log claims a landing on `hx/docs` `d4a9ece`. The budget env vars are present in `docs/CLI_AND_CONFIGURATION.md:106-107`, which is why budget criterion 8 was marked on `ft-95e4e915`. The env-docs card itself was not rewritten.

Not retrieved by the CLI auditor: `ft-6e014d9e`, `ft-43c56026`.

## Waves

At most four cards in a wave. Files inside a wave do not overlap. A later wave may edit a file an earlier wave already finished.

1. Outside-root half of `ft-b08f884e` (DevCouncil policy files, not `manvi/serve/policy.go`). Desktop cancel `ft-871861f4` (`manvi/computer/client.go`, `runner.go`). Repaired tool args `ft-074e1445` (`jsonrepair.go`, `loop.go`). Secret stdout `ft-d366f0d0` (`exec_tools.go`, `readauth.go`). Hold `verify.go` until the uncommitted coverage edit is settled.
2. Quick git `ft-530b2386` (`quick_actions.go`, `tui.go`). Linux fork ban `ft-6eb61e48` (`bench/mh/rpc.py`, `bench/stress_test.py`). Header-timeout race `ft-4712431b` (`transport.go`). MCP initialize `ft-039385c0` (`manvi/mcp/client.go`). Redirect order `ft-5bf53270` (`manvi/serve/policy.go`).
3. Shell network and git-pointer criteria on `ft-4dab6997` (`bench/mh/tools.py` only; do not edit `rpc.py`). Overflow flag `ft-447797ec` (`loop.go`, after wave 1). Session sync `ft-f505b844` (`manvi/session/store.go`). Local discovery `ft-0b338d23` (`manvi/llm/local/`).
4. Session trim `ft-ff7dfd1a` (`store.go`, after wave 3). Fetch prefixes `ft-e58f537b` (`manvi/fetch/fetch.go`). Serve critic `ft-730dc5e5` (`verifycritic.go`). Commit-scan half of `ft-b08f884e` (`commitscan.go`).
5. Windows exit 259 `ft-95e4e915` (`lock_windows.go`, `budget.go`; do not edit `verify.sh`). Native `cargo audit` `ft-9adb5aff` (`verify.sh`). Artifact durability `ft-abefbeed` (`artifacts.go`). Env case `ft-de8d7a32` (`manvi/childenv/childenv.go`).
6. Artifact symlink `ft-89c22437` (`artifacts.go`, after `ft-abefbeed`).

`ft-66398542` still lists `manvi/session/store.go`. Do not build it in the same wave as session sync or session trim.

## Rejected

- T4, a missing test around an already-guarded path. No live leak was shown. Not filed.
- List-skip of legacy `*.meta.json` stays a note on `ft-abefbeed`. It is not its own card. The case-sensitive `List` check is an acceptance criterion of `ft-89c22437`.

Downgraded, then filed at the lower severity:

- Env-case comparison, high to low. The Windows effect is inferred.
- Artifact symlink follow, high to low. The store is the same user.
- Redirect order, high to medium. Latent while GitPulse denies the write.
- Header-timeout race, priority 1 to 2. The window is the timer and a successful `Do` together.
- Session sync, high to medium. `syncDir` does sync on the Unix path. The failure is the error after the name is published, not a platform that never syncs.

## Decisions made without asking

1. The named Sonnet, Opus, and Fable seats were this session's model with those role instructions. Subagents were read-only code readers. The parent grouped findings and set wave order. Severity follows the verification pass. There was no severity disagreement that changed scope.
2. The DevMap index was not rebuilt. No gap was written under `.devcouncil`.
3. No card was deleted, merged, or moved to `done`.
4. GitPulse codeintel `is_fresh: false` is recorded. Empty graph answers stay unknown.
5. S2 folds into `ft-b08f884e` and does not change GitPulse `enforce_allowlist`. S6 is its own card and is the only edit of `manvi/serve/policy.go`.
6. Bench shell-network and the unreadable git pointer fold into `ft-4dab6997`. The Linux fork ban is the new card `ft-6eb61e48`. The fix is to implement the ban. Dropping the C6 claim is not the fix.
7. The serve critic folds into `ft-730dc5e5`. Windows exit 259 folds into `ft-95e4e915`. MCP initialize folds into `ft-039385c0`. The separate MCP-init card was not filed.
8. Session sync does not fold into `ft-66398542`, `ft-38893ac5`, or `ft-23cb1db3`. Session trim does not fold into done card `ft-8ceecd13`. Repaired tool args do not fold into `ft-b6b049`, `ft-7d4c987`, `ft-4712431b`, or `ft-9bff2fd4`.
9. Fetch prefixes do not fold into `ft-43efef72` or the local-discovery card. Env case does not fold into quick-git, the secret cards, the budget card, the router, the subagent-role card, or the env-docs chore. The symlink card does not fold into `ft-abefbeed` or `ft-d366f0d0`. Desktop cancel does not fold into `ft-039385c0`.
10. Redirect order does not fold into `ft-10d901e9`, `ft-9b535b8f`, or `ft-0fcd6c4c`.
11. Budget criterion 8 was marked met after `docs/CLI_AND_CONFIGURATION.md:106-107` was read this pass. The earlier plan had left it unmarked pending that read.
12. Native-gate criteria 1 and 2 were marked met because the `verify.sh` steps exist. `verify.sh` was not re-run, and an unformatted file was not reintroduced.

## Owner decisions already encoded

These are recorded on the cards. They do not need a new question before building.

- Outside-root credential reads are a hard Manvi and DevCouncil rule. GitPulse's default `enforce_allowlist` stays as it is.
- Git hooks stay enabled on quick actions. `HOME` and `SSH_AUTH_SOCK` stay on the allowlist. `core.hooksPath` is unchanged.
- Local `SupportsImages` is either false until an encoder exists, or the encoder is taught. The card does not pick which.
- The Windows env-name comparison is an inferred, low-severity card.
- The Linux fork and signal ban is implemented. The bwrap card does not allow the test to be dropped.

No data-loss, product-security, or public-API stop condition fired. Building waits for an explicit go-ahead.
