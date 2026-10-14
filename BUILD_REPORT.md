# Build report

Date: 2026-10-13. Repository: Manvi, main at `01c6241`. No merge, no push, no card marked done.

`dev gate status` from the earlier session reported `verification_mode: off` (origin default). Done is evidence-gated and was not requested. `dev verify` was not run. The full suite was not run as one gate. GitPulse does not see the Grok worktrees (`REPOSITORY_TRUST_REQUIRED` on those checkouts); collision scans only the main checkout.

## In review

Opus approved these. They stay in review because verification did not pass a Manvi gate.

| Item | Revision | Branch | Head | What landed |
|---|---|---|---|---|
| ft-530b2386 | 3 | hx/wave2-quick-git-env | b90dda9 | `/pull` and `/push` use `childenv.Git()`. `gh` issues stay on `Inherit()`. No `core.hooksPath`. HOME and `SSH_AUTH_SOCK` stay. |
| ft-871861f4 | 3 | hx/wave1-broker-cancel | e38af08 | After a write starts, cancel calls `stop`. The pre-write slot wait still returns `ctx.Err()` and leaves the client usable. |
| ft-d366f0d0 | 5 | hx/wave1-git-stdout | 472abe9 | `git diff` and `git show` withhold model-visible stdout on success, non-zero exit, and timeout. The verifier scan stays unfiltered. |
| ft-074e1445 | 3 | hx/wave1-truncated-args | c076630 | A truncated tool argument is not dispatched. Withhold matches the prefix `the arguments were cut off mid-value`. |
| ft-5bf532700481c97fc4dda3710b43f24c | 3 | hx/wave2-redirect-order | 1b63242 | `checkCommand` matches the gate. Precise hard returns first; redirects are judged for an unreadable hard denial; the marker is stamped only when those redirects pass. |
| ft-6eb61e482a0142db909f88d8c177cec4 | 3 | hx/wave2-bwrap-fork | cd847be | Worker cBPF denies fork and non-thread clone with EPERM, clone3 returns ENOSYS, kill of another pid returns EPERM. |
| ft-4712431ba5726bfb3d7566ba9ac95e4a | 5 | hx/wave2-header-timeout | 81c11d6 | A late header timer does not discard a 200. A caller that is already canceled does not receive one either. |
| ft-cf1ec230de71a27c30592bbcbebe6f93 | 5 | hx/wave1-git-stdout | cc21e94 | A substitution that would execute a git diff is refused before `runShell`. Git is not run a second time. `echo $(git diff)` still runs once and withholds. |
| ft-f505b844726ef552ba44f7feb9e1136d | 3 | hx/wave3-session-sync | 82d498d | A directory flush error after `os.Link` is still returned, and `seen` records that generation so the next `Save` is not a conflict. |
| ft-0b338d23500b424ac153026f23692b0f | 3 | hx/wave3-local-discovery | 8f1d89e | `SupportsImages` stays false. A cancelled model or dimension probe is not cached for `DiscoveryTTL`. A real HTTP error still is. |

ft-074e1445 tests, this session: `go test -C manvi ./llm/openaicompat/ -count=1 -timeout 180s` ok in 2.489s. Agent tests `TestAdversarialSiblingDispatch`, `TestAdversarialOutputCapWithholdsEveryCall`, `TestACutOffResponseDoesNotRunACompleteLookingCall`, and `TestACancelledRun` ok. The re-attack commit `c076630` is parented on the fix `7e345e8` and its message is test-only. The three earlier misses (a `}` inside a string, the cut-off phrase inside a permanently invalid payload, an unclosed array dispatched as its inner object) passed on that tree.

## Wave 2

Redirect, bwrap, the header timer, and the exec follow-up are in review above. Nothing in this wave is still open.

After the wave, GitPulse insights on main: branch `main`, 11 dirty files, 0 live Claude or Codex sessions, collisions ok, overlapping files 0, scanned worktrees 1. Grok worktrees stay invisible to that scan. DevMap status generation 656, `repository.root` matched, `is_fresh` true. GitPulse's codeintel facet on the same generation said `is_fresh` false (`stored extraction payload is obsolete`). That disagreement is not a clean index. `devmap_impact` for `redactGitStdout` returned no indexed start: the symbol is not on main. Gap `exec-stdout-redact-unmerged` is recorded. The git-stdout worktree has no `devmap.sqlite`.


## Wave 3 started

Session sync `ft-f505b844` is in review. Fix `c1c6186`, attack tests `82d498d`. Package `ok` in 3.209s on the attacker's run. The parent did not re-run it. Impact of `manvi/session/store.go` was truncated (30 of 302, depth cap) and `devmap_affected_tests` fail-closed on an obsolete extraction payload. Gap `session-store-blast-truncated` is recorded. The edit stayed in `manvi/session`. No benchmark exists for `Save`. None was added.

Local discovery `ft-0b338d23` is in review. Fix `97a35f1`, attack tests `8f1d89e`. Package `ok` in 13.301s on the attacker's run. The parent did not re-run it. `SupportsImages` stays false. A cancelled probe is not cached. An HTTP 400 still is. No discovery benchmark exists. None was added.

`ft-4dab6997` and `ft-447797ec` stay deferred. Their files are held by unmerged branches.

## Deferred or blocked

- ft-b08f884e, review revision 4. Outside-root half needs DevCouncil `policy/file.go`, which is dirty in the sibling checkout, along with `command_operands.go`. Criterion 7 stays open. Commit-scan stays on `commitscan.go` for a later wave. `manvi/serve/policy.go` is not this card.
- ft-039385c0d4f9298cc2f40920abdc3213 (MCP initialize, review). Not reopened. ft-80a33c14 (review) also plans `manvi/mcp/client.go`.
- ft-447797ec3b950dbd07db0e3c4d6126cc (context-overflow flag, ready). Not started. hx/wave1-truncated-args still holds `manvi/agent/loop.go`.
- ft-9bff2fd4 plans `loop.go` and stays untouched while that file is held.
- Inbox spikes ft-2550b25d and ft-10d901e9 are not implementation work.
- ft-4dab6997ff3ce430f5823ac76c21d69f stays in review (revision 3). Its open criteria need `bench/stress_test.py`, which the unmerged bwrap branch also edits, and `bench/mh/rpc.py` is on its planned-file list. This loop does not merge, so that card is not reopened in parallel.

## Benchmarks

No benchmark exists for the computer client, exec stdout, or quick-git spawn. Those are process launches.

`BenchmarkRepairJSONLiterals` was not edited by ft-074e1445's last fix. `-benchtime=50ms -count=3`, 3 allocs/op:

| size | ns/op |
|---|---|
| 4096 | 22664, 22565, 22141 |
| 8192 | 44043, 45349, 46884 |
| 16384 | 91004, 93297, 92311 |

Prior steady band was about 22348–22597, 43836–44542, and 89504–89980. One 8192-byte sample is about 5.3% over that ceiling. The other two are within 2%. A 50ms sample on an unmodified function. Not counted as a regression of the truncation change.

No benchmark exists for the serve policy check, the bwrap argv builder, the header-timer claim, or exec stdout. None was added. The bwrap change is process setup. The header change is one atomic claim on a path that already had no benchmark. Exec stdout is a process launch.

## New and updated tasks

ft-cf1ec230de71a27c30592bbcbebe6f93 (`gp-audit-2026-10-13-exec-diff-bypass`) was filed for the leaks ft-d366f0d0 did not cover, then overwritten at revision 2 to add `env -- git diff`, `command -p git diff`, `$'git' diff`, and a backslash-newline between `git` and `diff`. It is in review at revision 5 (sequence 4170, `verification` null). It stays on the git-stdout branch and is not folded back into ft-d366f0d0. No new task was filed from the second attack: the residuals did not print the secret.

## Fable's decisions

- Wave 1 started as desktop cancel, truncated tool arguments, and git stdout. Quick git took the fourth slot because its files did not overlap.
- ft-b08f884e stays deferred. Do not edit DevCouncil from this loop.
- The four extra git-stdout leaks are not a send-back of ft-d366f0d0. Policy's `argumentCommandWord` returns -1 when `env` or `command` is followed by an option, on purpose. They are the follow-up card, and that card stops if it needs a DevCouncil edit.
- Truncation was sent back twice: `strings.Contains` withheld every invalid payload, then a closer inside a string and an unclosed array were misread. The third implementation was re-attacked and approved.
- Broker cancel does not stop the client while it is waiting for the pre-write slot. `KillGroup` is not exercised when `cmd.Process` is nil. That limit is recorded, not a send-back.
- Quick git does not case-fold `GIT` on Windows. That stays ft-de8d7a32.
- An apostrophe in prefix prose (`don't {"a":1}`) is plain invalid under the same string scan as `repairJSONLiterals`. It does not dispatch a shorter call and does not withhold a sibling. Accepted.
- Wave 2 is the four cards above. MCP init and the overflow flag wait. Linux bwrap cannot be executed here. The C6 claim is not dropped.
- Redirect order is approved. A whitespace redirect ahead of `.env` is reported as `path.outside_root`; the command is still denied.
- bwrap `kill(0)` stays allowed. That is the process group, wider than seatbelt's target-self. `--new-session` and `--unshare-pid` keep it off the checker, and `kill(getppid())` stays EPERM. Not a send-back. `tgkill` is filtered on the thread-group id only; the kernel check was not executed.
- ft-4dab6997 is not reopened while its planned `stress_test.py` and `rpc.py` sit on the unmerged bwrap branch.
- Header timeout was sent back once. The accept path must return `ctx.Err()` when the parent is already canceled, and must not relabel that as `ErrHeaderTimeout`. A RoundTripper that returns both a response and an error is out of scope. If the timer claims first and RoundTrip returns that cause, `ErrHeaderTimeout` stands. If the timer has already claimed and the parent then cancels and RoundTrip still returns `(200, nil)`, `send` closes the body and returns `context.Canceled`. That case is accepted.
- Exec follow-up 0000b8c was sent back because `recoverExecutedPatch` re-ran git. The replacement refuses `echo $( $(git diff) )` before `runShell`. `git diff | cat` still runs once and withholds. `git diff | cat | sed` stays refused. `git diff | cat --` is refused too; that is fail-closed. A pipe inside `$( )` is stopped by the command gate before this filter. `sh -lc` replaces `PATH`, so the git-count wrapper does not see that one shape. Approved at review revision 5. Not done.
- Git hooks stay enabled. HOME and `SSH_AUTH_SOCK` stay on `childenv.Git`.
- Local `SupportsImages` stays false. The owner question on ft-0b338d23 was declined. The encoder is not taught to send images. A cancelled discovery is still not cached.

## Owner decisions still pending

- ft-530b2386: whether quick git should disable hooks. Not built. The card shipped with hooks left on.
- ft-cf1ec230: if the six leftover shapes need a DevCouncil parser change, stop. Do not edit DevCouncil until that is accepted.
- ft-b08f884e: outside-root credential reads stay a hard rule. The DevCouncil files are dirty, so the card is blocked.
- ft-6eb61e48: the ban is implemented, not dropped from the suite. A Linux C6 run is still unexecuted on this machine.
- Windows env-case (ft-de8d7a32) stays low and was not folded into quick git.

## Unverified

Full suite, flake check, `dev verify`, Linux, and Windows. The parent did not re-run the worktree suites: `go test -C` with an absolute worktree path is blocked by the harness. Agent results are cited above. DevMap file-level impact was truncated on the earlier wave-2 targets, and `redactGitStdout` has no indexed start on main. Main's dirty files were not touched except untracked `BUILD_REPORT.md`: the docs, `verify.go` and its path tests, `managed_stop_test.go`, `verify_checks.go`, `verify_test.go`, and untracked `AUDIT_REPORT.md`.
