# Shell command containment of secret paths: findings and design

Status: the darwin seatbelt seam is implemented (`manvi/devcouncil/containment.go`,
`harness.exec_containment`); the text below is the original design and finding. See
"Implemented profile and its remaining gaps" at the end for what is contained now.
Written 2026-11-20 on lane/secrets.

## Finding (verified)

`devcouncil_exec_command` refuses a command only when the command line *names*
a path under DevCouncil's `policy.SecretPathPatterns` (rule
`command.secret_read`, hard, not grantable). It does not constrain what the
child process opens. Reproduced in-process (fixture repo, task leased,
`harness.posture=dev`, fake token `FAKE-SUPERSECRET-0000` in `secrets/vault.go`):

| command | result |
|---|---|
| `grep -r SUPERSECRET .` | exit 0, printed `./secrets/vault.go:const Token = "FAKE-SUPERSECRET-0000"` |
| `rg SUPERSECRET` | exit 0, printed `secrets/vault.go:const Token = ...` |
| `go run read.go` (program that `os.ReadFile("secrets/vault.go")`) | exit 0, printed the whole file |
| `cat secrets/vault.go` | refused, `command.secret_read` |

Under `strict` the first three are refused only by `command.not_allowed` (the
command allowlist); a task whose `allowed_commands` permits `grep`, `rg` or
`go run` leaks the same way, so strict posture narrows the exposure but is not
a containment of it.

## What exists in Manvi today (verified by search)

- One exec path: `manvi/devcouncil/exec_tools.go` `execCommand` -> `runShell`
  -> `runProc` (`subprocess.go`), argv `sh -c <command>`, env from
  `sanitizedEnv` (allowlist), process group, 1 MiB output cap.
- No sandbox seam: no seatbelt/`sandbox-exec`, landlock, bwrap or namespace use
  anywhere in the Go tree (`rg -i 'sandbox-exec|seatbelt|landlock|bwrap'` hits only
  verify scripts and the Python bench). Env scrubbing is the only containment.
- No output post-processing: `commandResult` returns the captured text as is.
  (Session-level scrubbers redact the operator's own credential values from
  transcripts; they do not know the repository's secret files.)

## Redaction: no callable contract (verified)

`dc-redact` (`DevCouncil/rust/dc-redact`) is a dependency-free Rust library
exposing `find_secret`, `redact_secrets`, `contains_secret`. It has no binary,
no C ABI and no gusset-engine export; `gusset-engine` and the Go `gussetfn`
package carry only glob matching. The only compiled consumers are
`dc-verify` (evidence strings) and `devmap-resolve`. A Go reimplementation of
its table is forbidden by the task and would drift. Redacting command output
therefore needs a DevCouncil change first: a `dcredact` stdio binary (stdin
text -> redacted text, same JSON-on-stdio process contract as `dcgrep`) or a
gusset function. Manvi would then wrap it like `dcgrep` (pinned binary,
fail closed when absent: an unredacted result must say it was not redacted,
never read as clean). Redaction is a backstop only: it covers credential
*shapes*, not arbitrary file contents.

## Containment: where the seam goes and what it costs

Seam: `runShell` / `runProc`, the single child-process owner. Wrap argv on
darwin in `/usr/bin/sandbox-exec -p <profile> sh -c ...` (Linux: Landlock or
bwrap via a helper). Profile: `(allow default)` followed by
`(deny file-read* (subpath|regex ...))` built, at call time, from the
repository's secret files: enumerate the tracked and untracked files matching
`policy.SecretPathPatterns` (read through the pinned DevCouncil Go module with
`fnmatch.MatchAnyFold`, never copied) under the root and deny each literal
path or directory subtree. Resolve symlinks first, since seatbelt matches real
paths.

Costs and risks, none of which is cheap:

1. `sandbox-exec` is deprecated by Apple (still shipped); behaviour on new
   macOS releases must be re-verified each release. There is no equivalent on
   Windows.
2. Patterns such as `.env`, `*.key`, `*.pem` match files that build and test
   tooling legitimately reads (`docker compose`, `dotenv` loaders, TLS test
   fixtures); those commands would fail with EPERM. An operator opt-out
   (operator-scope setting, refused in repository `config.yaml` like
   `harness.serial_lookups`) and a per-path allow list are required.
3. A secret file created after the profile is built (a command that writes
   `.env` then reads it) needs a pattern-based rule, not a literal list;
   seatbelt regexes cannot express every fnmatch pattern exactly, so profile
   generation needs a parity test against `MatchAnyFold` over the
   `policy/testdata` fixtures.
4. Child `git` and compilers read `.git` and caches; the deny list must not
   touch them. Hard links to a secret file escape a path-based deny.
5. Each command pays a profile compile (~ms); needs a benchmark before
   shipping.

Recommendation: do the DevCouncil `dcredact` contract first (cheap, portable,
fail-closed), then the darwin seatbelt seam behind an operator setting
defaulting on in `strict`, off in `dev`, after the owner decides items 1-2.
Neither was built in this lane because the redaction half is blocked on
DevCouncil and the containment half is a new subsystem with an opt-out policy
the owner should choose.

## Implemented profile and its remaining gaps

The profile, rendered per command from the secret files found under the real
repository root, is `(allow default)` plus four deny forms, each verified live
with `sandbox-exec` on darwin (`TestContainmentStopsRenameAndLinkLive`):

| deny form | on | stops |
|---|---|---|
| `file-read*` | each secret file (literal), each secret directory (subpath) | `cat`, `grep -r`, `cp`, a program that opens it |
| `file-write*` | same | writing, appending, `chmod`, `rm`, and renaming the file itself |
| `file-link` | same | `ln .env y` (a hard link under another name) |
| `file-write-unlink` | every directory above a secret path, up to `/` exclusive | `mv cfg cfg2`, `mv repo repo2`: a rename needs unlink on the source name |

An earlier version denied only `file-read*`; `mv .env y && cat y` and
`mv cfg cfg2 && cat cfg2/.env` printed the token because the moved file no
longer matched a denied path. The unlink deny is on the ancestor directories
only, not a write deny on them, so creating, editing and renaming other files
in the repository still works.

Remaining gaps (not contained):

- A secret-named file created *outside a secret directory* while the command
  runs (`echo $T > .env && cat .env`): the profile is built before the command,
  so the new name is not in it. Secret directories are denied as subtrees and
  do cover files created inside them.
- Writing, linking or deleting a secret file is denied too, so a contained
  command cannot edit `.env`; legitimate tooling that does fails with EPERM.
- `sandbox-exec` is deprecated by Apple and still shipped; each macOS release
  must be re-verified. Where it is missing the command is refused, not run.
- The verify command is not contained.
- Linux and Windows have no seam.
