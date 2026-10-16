# Branch consolidation report

Date: 2026-10-15. Repository: `/Users/bharath/Code/devtools/Manvi`. Scope: local branches. Nothing was pushed.

`claude/happy-elbakyan-b8bad8` is on `main` as merge commit `6c33d78f9bf3e29684092427d7d9bc6544c6c43a`. There were no conflicts. A later request to delete stale branches did not remove it: Claude pid 22242 still has that worktree checked out, and the worktree is clean. `main` was not pushed. It is 184 commits ahead of `origin/main`.

## Merge

| Item | Value |
| --- | --- |
| Commit | `6c33d78f9bf3e29684092427d7d9bc6544c6c43a` |
| Parents | `89aeedb34c84077c2672308d00b96947233b8e09` (main), `89a3537ba5313d550596e9b0c794e00f5935700e` (branch) |
| Command | `git merge --no-ff -F tmp/merge-msg.txt claude/happy-elbakyan-b8bad8` |
| Strategy | ort. 5 files, +29/−8. No conflicts. |
| Files | `contracts/CHECKSUMS`, `contracts/verdict.schema.json`, `docs/POLICY_AND_SAFETY.md`, `manvi/serve/zfix_servepolicy_test.go`, `scripts/module-pins.txt` |

`CONTRIBUTING.md` does not require a rebase. The merge was `--no-ff`, which is what the consolidation steps specified. The branch was one commit ahead of `main` and zero behind, so a fast-forward was also possible. `--no-ff` kept an explicit merge commit.

The commit pins DevCouncil `9add99c2` to `e79c2610` and records hard rule `command.git_read_argument` in the verdict schema, its checksum, the policy doc, and the serve policy test.

You said to merge after the classification had held both branches because live sessions had them checked out. Both sessions were still alive and idle at merge time (pid 42082 on this checkout, pid 22242 on the agent worktree). No task was `in_progress`, and `task_id` was empty on both worktrees. The merge moved `main` under pid 42082. That process is still sleeping, and its cwd is still this checkout.

## Conflicts

None. No file was resolved with ours or theirs.

## Worktrees and live sessions

Before the merge, GitPulse (`/Users/bharath/.cargo/bin/gitpulse-mcp`, protocol `2026-07-28`; the Grok bridge had not registered the server) returned `ok: true` on insights and collision risk. Two worktrees, zero unscanned, zero overlapping dirty files, zero in-progress tasks. `lsof` matched the sessions.

| Worktree | Branch | HEAD now | Dirty | Live session |
| --- | --- | --- | --- | --- |
| `/Users/bharath/Code/devtools/Manvi` | `main` | `6c33d78` | untracked `BRANCH_REPORT.md` only | Claude pid 42082, idle, cwd is this checkout |
| `.../.claude/worktrees/happy-elbakyan-b8bad8` | `claude/happy-elbakyan-b8bad8` | `89a3537` | clean | Claude pid 22242, idle, cwd is that worktree |

After the merge, a fresh `gitpulse_insights` did not scan. `worktrees.ok` is false with: `git worktree deferred under load after 0.000s: background work is shed while the git spawn rate budget is low`. That is a missed scan, not an empty session list. `lsof` after that failure still shows pid 42082 in this checkout and pid 22242 in the agent worktree. `git branch --merged main` lists the pin branch with `+`, which means another worktree still has it checked out.

## Inventory at classification time

This was the state when Fable reviewed, before you said to merge. `git branch --merged main` then listed only `main`. `git cherry -v main claude/happy-elbakyan-b8bad8` printed `+ 89a3537`.

| Branch | Ahead | Behind | Last commit then | Merged then | Worktree | Live session | Task |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `main` | 0 | 0 | 2026-10-13 23:44:26 -0500, `89aeedb`, BHARATH CHANDRA VADDARAM | yes (it is main) | primary | pid 42082 | none |
| `claude/happy-elbakyan-b8bad8` | 1 | 0 | 2026-10-13 23:57:50 -0500, `89a3537`, BHARATH CHANDRA VADDARAM | no | agent worktree | pid 22242 | none |

Fable (`manvi run`, `MANVI_MODEL=claude-fable-5-1`, session `b1f1fb2434079191`) classed both as in use and said to keep them until you ended the sessions or confirmed they were abandoned. You then said to merge. Text: `tmp/fable-review.txt`.

Remote-tracking refs were out of scope. Each still has zero commits that local `main` lacks (`origin/main` 182 behind before this merge, now 184; `origin/claude/app-optimizations-53p4a8` 168 behind; both dependabot branches 217 behind). They were not deleted.

## Backup

| Ref | Object | What it is |
| --- | --- | --- |
| `refs/backup/2026-10-14/main` | `89aeedb34c84077c2672308d00b96947233b8e09` | `main` before this merge |
| `refs/backup/2026-10-14/claude/happy-elbakyan-b8bad8` | `89a3537ba5313d550596e9b0c794e00f5935700e` | The pin commit |
| `refs/backup/2026-10-15/main` | `6c33d78f9bf3e29684092427d7d9bc6544c6c43a` | `main` after this merge |

Bundles, both `git bundle verify` okay, complete history, sha1:

- `tmp/branch-backup-2026-10-14/manvi-branches.bundle` — 14,149,299 bytes. Heads at the pre-merge tips.
- `tmp/branch-backup-2026-10-15/manvi-branches.bundle` — 14,149,607 bytes. `main` at `6c33d78`, the pin branch at `89a3537`.

A copy outside the repository was refused by the shell gate, rule `path.outside_root/hard`, for `/Users/bharath/Code/devtools/manvi-backups/2026-10-15`. The gate was not bypassed. From a shell that may write there:

```bash
mkdir -p /Users/bharath/Code/devtools/manvi-backups/2026-10-15
cp tmp/branch-backup-2026-10-15/manvi-branches.bundle \
  /Users/bharath/Code/devtools/manvi-backups/2026-10-15/manvi-branches.bundle
git bundle verify /Users/bharath/Code/devtools/manvi-backups/2026-10-15/manvi-branches.bundle
```

## Deleted

None. On the delete pass, GitPulse insights and collision risk both returned `ok: true`: 2 worktrees, 0 unscanned, 0 overlapping files, 2 live Claude sessions (pid 42082 on `main`, pid 22242 on the pin worktree). `gitpulse_active_changes` for the pin worktree returned `ok: true` and `total: 0`.

`claude/happy-elbakyan-b8bad8` is an ancestor of `main` and has backup ref `refs/backup/2026-10-14/claude/happy-elbakyan-b8bad8`. It was not deleted because pid 22242 still has it checked out. `git branch -d` cannot drop a branch that a worktree holds, and `git worktree remove` would take that session's cwd away. There is no other local branch.

Remote-tracking branches were not deleted. `origin/claude/app-optimizations-53p4a8` and both dependabot branches are ancestors of local `main` and are not ancestors of `origin/main`, so their commits are only on the unpushed local history. `origin/main` stays. Main has not been pushed.

## Kept

| Ref | Reason |
| --- | --- |
| `main` | Trunk. Now at the merge commit. Live session pid 42082. 184 commits ahead of `origin/main`, unpushed. |
| `claude/happy-elbakyan-b8bad8` | Merged, and still checked out by pid 22242. Deleting it would disrupt that session. |
| `refs/backup/2026-10-14/*` and `refs/backup/2026-10-15/main` | Restore points. |
| `refs/rescue/*` (9 refs) | Rescued stashes, not branches. |
| `refs/cline/*`, `refs/codex/*` | Agent checkpoints, not branches. |
| Tags `premerge/angry-moore`, `premerge/jarvis-replay-fidelity`, `premerge/main`, `premerge/rust-go-ports`, `v0.0.1`, `v0.0.2`, `v0.0.3`, `v0.0.5`, `v0.0.6`, `v0.1.0` | Release and pre-merge tags. |
| `origin/main` and the three other remote-tracking heads | Already contained in local `main`. You did not ask to delete remotes, and `main` was not pushed. |

## Blocked

The merge itself is on `main`. What is still blocked:

- Deleting `claude/happy-elbakyan-b8bad8` and removing its worktree, until pid 22242 is gone or you confirm that session is abandoned.
- A green `./verify.sh`. See below. The failure is the sibling checkouts, which were already ahead of the pins before this merge. Aborting the merge would not make that check pass.

## Verification

This repo has no `ci:local` script. The gate is `./verify.sh`.

On the merge commit, `./verify.sh` exited 1. Release unit tests passed (110). The sibling check then failed and the script stopped before a verdict, and before gofmt:

```
check-release: DevCouncil is at e7a70d204c5e, pinned e79c2610f270 (47 ahead, 0 behind), with uncommitted changes.
check-release: gusset is at 320c56c27e10, pinned 984209b54288 (97 ahead, 0 behind), clean.
FAIL release checks
INCOMPLETE verify.sh exited (status 1) before reaching a verdict.
```

Log: `tmp/verify-main-20261015-merged.log`.

Before the merge the same check named DevCouncil pin `9add99c2` and a distance of 90. The merge moved the pin to `e79c2610`; the sibling is now 47 ahead of that pin and still has uncommitted changes. Gusset was not part of this commit and is still 97 ahead.

`gofmt -l manvi/serve/verify_checks.go` still prints that path. The merge did not touch that file. `./verify.sh --fix` was not run.

`git fsck --full` before the merge exited 0: 16 dangling commits, 54 dangling trees, 2 dangling blobs, 1 dangling tag, no corrupt objects. Log: `tmp/fsck-2026-10-15.txt`. Those objects were not pruned. Fsck was not re-run after the merge.

## Restore

To put `main` back where it was before this merge, while the backup ref still points there:

```bash
git checkout main
git reset --hard refs/backup/2026-10-14/main
```

`reset --hard` discards the merge commit from `main`. Pid 42082 has this checkout. Do that only if you mean to move that session's HEAD back. The pin commit stays on `claude/happy-elbakyan-b8bad8` and on `refs/backup/2026-10-14/claude/happy-elbakyan-b8bad8`.

To recover the merge commit if `main` is moved off it:

```bash
git checkout main
git reset --hard refs/backup/2026-10-15/main
```

From the post-merge bundle, without checking out a branch:

```bash
git fetch tmp/branch-backup-2026-10-15/manvi-branches.bundle \
  refs/heads/main:refs/backup/restore-main \
  refs/heads/claude/happy-elbakyan-b8bad8:refs/backup/restore-happy-elbakyan
```
