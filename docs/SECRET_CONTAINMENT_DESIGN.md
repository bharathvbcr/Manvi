# Shell command containment of secret paths: findings and design

Status: design only, nothing implemented. Written 2026-11-20 on lane/secrets.

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
