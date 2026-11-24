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
repository root, is `(allow default)` plus five deny forms, each verified live
with `sandbox-exec` on darwin (`TestContainmentStopsRenameAndLinkLive`):

| deny form | on | stops |
|---|---|---|
| `file-read*` | each secret file (literal), each secret directory (subpath) | `cat`, `grep -r`, `cp`, a program that opens it |
| `file-write*` | same | writing, appending, `chmod`, `rm`, and renaming the file itself |
| `file-link` | same | `ln .env y` (a hard link under another name) |
| `file-clone` | same, and every directory above a secret path | `clonefile("cfg", "cfgc")`, `clonefile(repo, repo_c)`: a clone copies the tree by reference to a name no rule matches (`TestContainmentStopsCloneLive`, which calls clonefile from inside the sandbox) |
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
- Deleting or moving a directory tree that contains a secret-named file
  (`rm -rf node_modules` with a `*.pem` inside, `mv` of a parent) fails with
  EPERM while containment is on: the unlink deny covers every ancestor, and the
  secret file itself cannot be removed.
- Writing, linking, cloning or deleting a secret file is denied too, so a contained
  command cannot edit `.env`; legitimate tooling that does fails with EPERM.
- `sandbox-exec` is deprecated by Apple and still shipped; each macOS release
  must be re-verified. Where it is missing the command is refused, not run.
- The verify command is not contained (see "Gaps that are not platform-specific").
- Linux and Windows have no enforcement; what the code does there is in "Per-platform status".

## Per-platform status

Written 2026-11-22. Labels: **verified** (read in this tree or run on this host),
**doc** (stated by the cited page, as fetched and summarised that day),
**inferred** (my reasoning, not stated by a source and not run).

### darwin: implemented

- Enforcement is `/usr/bin/sandbox-exec -p <profile> sh -c <command>`, a fixed
  absolute path never looked up on PATH (`containment.go:29`, `:34-46`,
  `:85`). The profile and its five deny forms are in the table above
  (verified: `containment.go:212` `seatbeltProfile`, live tests
  `TestContainmentStopsRenameAndLinkLive`, `TestContainmentStopsCloneLive`).
- Deprecation status (verified on this host, Darwin 27.2.0): `man sandbox-exec`
  is headed "sandbox-exec - execute within a sandbox (DEPRECATED)" and says
  "The sandbox-exec command is DEPRECATED. Developers who wish to sandbox an app
  should instead adopt the App Sandbox feature"; the page is dated March 9, 2017.
  The binary is still present and the live tests ran against it. The code calls
  the CLI only (`containment.go:85`), not `sandbox_init(3)`; the man page's SEE
  ALSO lists `sandbox_init(3)`, `sandbox(7)`, `sandboxd(8)`. `/usr/include/sandbox.h` is absent on this machine, but the Xcode SDK
  (`xcrun --show-sdk-path`, `usr/include/sandbox.h`) has it: line 7 says "This
  header is deprecated and may be removed in a future release", and
  `sandbox_init` (line 49) carries `API_DEPRECATED("No longer supported",
  macos(10.5, 10.8), ...)` (line 46), so the C entry point is deprecated since
  macOS 10.8 as well (verified).
- Watch: the failure mode of Apple removing the binary is closed, not open:
  `containmentLookup` stats the path and refuses when it is missing or not
  executable (`containment.go:38-44`), and `exec_command` then returns an error
  without running the command (`containment.go:297-300`, `exec_tools.go:88-91`,
  pinned by `TestContainmentFailsClosedWhenUnavailable`,
  `containment_test.go:376-400`). What is not detected is a release that keeps
  the binary but changes profile semantics; that needs the live tests re-run on
  each macOS release (existing practice, no automation found: **unverified**).
  There is no Apple-documented replacement that takes an arbitrary command and a
  deny list; App Sandbox is entitlement-based and applies to an app bundle
  (**inferred** from the man page text above).

### Current behaviour on every other platform (code, not the card)

The card says "refused there". That is true of the command, not of the setting:

- The setting is accepted on any OS. `harness.exec_containment` is an enum of
  `off|secrets` with no platform check (`cmd/manvi/promptspec.go:58-59`);
  `execContainmentMode` just returns the value (`promptspec.go:104-110`). An
  operator on Linux or Windows can set `secrets` and nothing complains until a
  command runs. The description text names macOS but is not a gate.
- `containment.go` has no build tag (`containment.go:1`), so the profile
  builder compiles everywhere; only `containmentLookup` is platform-aware.
- At the first `devcouncil_exec_command`, `containmentLookup` returns
  `sandbox-exec exists only on macOS and this host is <GOOS>`
  (`containment.go:35-36`); `containedShellArgv` turns that into
  `secret-file containment is on (harness.exec_containment=secrets) but
  unavailable: ...; the command was not run` (`containment.go:297-300`) and
  `exec_command` returns it as a tool error before the command is spawned (the gate and its ref snapshot run first, `exec_tools.go:78`, `refguard.go:1244`)
  (`exec_tools.go:88-91`). So with `secrets` on, every shell command is refused
  on Linux and Windows: fail closed, matching the card, but late and per command.
  With `off` (the default, `promptspec.go:58-59`) nothing changes.
- Test coverage of that branch: **unverified on a real non-darwin host**. The
  fail-closed test stubs the lookup with a fake "plan9" error
  (`containment_test.go:378-383`); the `runtime.GOOS != "darwin"` check runs on darwin, but its TRUE
  branch (the one a Linux or Windows host takes) is untested.
- Follow-up this implies (not done here, changes behaviour): reject
  `secrets` at `manvi doctor` / config-set time on a host with no launcher, so
  the refusal is not discovered mid-task.

### Linux: design (not implemented)

Two candidate facilities. Both are **designs only**; nothing below runs.

**Landlock** (https://docs.kernel.org/userspace-api/landlock.html)

- Rights by ABI (doc): ABI 1 `EXECUTE, WRITE_FILE, READ_FILE, READ_DIR,
  REMOVE_DIR, REMOVE_FILE, MAKE_CHAR, MAKE_DIR, MAKE_REG, MAKE_SOCK, MAKE_FIFO,
  MAKE_BLOCK, MAKE_SYM`; ABI 2 `REFER` (link or rename across directories,
  always denied by default); ABI 3 `TRUNCATE`; ABI 4 TCP bind/connect; ABI 5
  `IOCTL_DEV`; ABI 6 abstract-unix-socket and signal scopes; ABI 9
  `RESOLVE_UNIX`. First kernel is 5.13 for ABI 1; the page gave no kernel
  version for later ABIs, so a host check must read the ABI at run time with
  `landlock_create_ruleset(NULL, 0, LANDLOCK_CREATE_RULESET_VERSION)`.
- It is allow-list only (doc: access is denied by default for handled rights and
  a rule grants it; no deny rule is described). It therefore **cannot express**
  "deny read of these files, allow everything else" directly. A rule is
  `LANDLOCK_RULE_PATH_BENEATH` on a directory or file fd, and the page
  recommends putting rights on the leaves of hierarchies. **Inferred**
  consequence: to deny `repo/cfg/.env` the profile must grant read on the
  complement, not subtract from a grant:
  1. grant read and execute on every top-level entry of `/` except the ancestors
     of the repository root, then on every entry of each ancestor except the
     next one down, until the root; inside the repository, on every file and
     directory that is not (and does not contain) a secret-named path, and on
     each directory that does contain one, grant only the non-read rights
     (MAKE_*, REMOVE_*, WRITE_FILE as policy allows) and read on the individual
     non-secret entries;
  2. the number of rules grows with the size of the tree walked, so the
     `maxContainmentWalk` (`containment.go:54`) and `maxProfileBytes` (`:55`)
     style refusals would become a rule-count bound;
  3. a file created in a directory that holds a secret is writable (MAKE_REG)
     but not readable back, because the new name has no read rule. That breaks
     tools that write then read in the same directory; it is the Linux form of
     the existing gap "created mid-command outside secret dirs", but fails
     closed (EACCES) instead of open. A secret directory handled as one subtree
     is simply not granted.
  So Landlock gives a fail-closed complement profile, at the cost of a walk per
  command and a larger breakage surface than darwin's deny-list.
- Stacking and inheritance (doc): each `landlock_restrict_self` adds a layer,
  max 16 (`E2BIG`), children inherit, restrictions cannot be removed. It needs
  `no_new_privs` unless the caller has `CAP_SYS_ADMIN`; ABI 11 adds a flag that
  sets it only when enforcement succeeds. Unavailable kernels return `ENOSYS` or
  `EOPNOTSUPP`, which maps onto the existing fail-closed refusal.
- Doc limitations that matter here: only listed operations are controlled
  (`chdir`, `stat`, `chmod`, `chown`, `setxattr`, `utime`, `access` are not
  yet restrictable, so metadata of a secret file stays visible); a file opened
  before enforcement is not covered; rules follow bind mounts but OverlayFS
  layers are treated separately; `/proc/<pid>/fd/*` pipes and sockets cannot be
  restricted directly; `mount` and `pivot_root` are denied to the sandboxed
  thread. Hard links: cross-directory link needs `REFER`, which is denied unless
  granted, so not granting it covers the `ln` gap (doc for REFER; the mapping to
  our hard-link case is **inferred**).
- Integration (**inferred**): the restriction applies to the calling thread and
  its children, so Manvi would run a helper (re-exec of the manvi binary or a
  small Rust/Go stub) that builds the ruleset, calls `prctl(PR_SET_NO_NEW_PRIVS)`
  and `landlock_restrict_self`, then `exec`s `sh -c`. It slots in beside
  `seatbeltProfile` at `containArgv` (`containment.go:72-86`), reusing
  `findSecretDenials` unchanged. The raw syscalls need constants from
  `golang.org/x/sys/unix`, which is already a direct requirement
  (`manvi/go.mod:11`, `golang.org/x/sys v0.50.0`, no `// indirect`), so no new
  dependency is needed. In that version's source, `unix/zerrors_linux.go` and
  `unix/zsysnum_linux_amd64.go` match Landlock names (`rg -c` on
  `LANDLOCK_CREATE_RULESET_VERSION|SYS_LANDLOCK_RESTRICT_SELF|LANDLOCK_ACCESS_FS_REFER`:
  2 and 1 lines); I did not check which of the three each file defines, nor
  the other architectures, so the full constant set is unverified.

**bubblewrap** (https://github.com/containers/bubblewrap,
https://manpages.debian.org/bookworm/bubblewrap/bwrap.1.en.html)

- Doc: unprivileged via user namespaces (setuid mode removed; the man page says
  the user namespace "is required if bwrap is not installed setuid root"); builds
  a new mount namespace whose root is an empty tmpfs; `--ro-bind SRC DEST`,
  `--bind SRC DEST`, `--tmpfs DEST`, `--remount-ro DEST`; uses
  `PR_SET_NO_NEW_PRIVS`; `--new-session` is needed against TIOCSTI when seccomp
  does not filter it; and "is not a complete, ready-made sandbox with a specific
  security policy", so the caller owns the policy.
- It expresses a deny-list naturally (**inferred**; the pages I read do not
  mention binding over a single file or `/dev/null`): `--ro-bind / /` or the
  needed system roots, `--bind` the repository read-write, then mount a
  `--tmpfs` over each secret directory and a read-only empty file over each
  secret file. Reads see empty content; `unlink` and `rename` of a mount point
  fail, which resembles darwin's unlink deny. No complement walk, no rule-count
  growth beyond one mount per secret path.
- Costs (**inferred**): requires unprivileged user namespaces, which some
  distributions restrict (not read from a source here); a missing `bwrap` binary
  must refuse like a missing `sandbox-exec`; the launcher must be a fixed
  absolute path, not PATH-resolved, for the reason at `containment.go:25-28`; a
  mount namespace changes what the command sees (`/proc`, `/dev`, `/tmp`) and
  needs an argument set per toolchain; a secret created mid-command is no more
  covered than on darwin.

**Recommendation (inferred, owner decision):** bubblewrap first, because its
deny-list shape matches `secretDenials` and the existing "refuse when the
launcher is missing" contract; Landlock as the fallback or the no-userns path,
accepting the complement-walk cost. Neither is implemented; until one is, Linux
stays on the refusal documented above. A Linux implementation needs a live test
on a Linux host with the same cases as the darwin live tests (read, write, link,
clone-equivalent, rename of an ancestor), none of which can run on this machine.

### Windows: explicit refusal

No seam is planned. Current behaviour is the non-darwin refusal above: with
`secrets` on every shell command is refused (`containment.go:35-36`,
`:297-300`); with `off` nothing is contained. The nearest facility is
AppContainer (https://learn.microsoft.com/en-us/windows/win32/secauthz/appcontainer-isolation):
doc says file access is controlled with read-write grants to specific files and
that read-only access is "less restricted", i.e. it is an allow-list model for an
application identity, not a way to deny named files to an otherwise ordinary
child. The page does not address launching an arbitrary existing console program
or `sh -c` inside a container; whether that works for build tools, and how
`sh` is even provided, is **unverified**. Treat Windows as unsupported and
refused until an owner asks for it.

### Gaps that are not platform-specific

- **Verify command not contained.** `runVerifyCommand` calls
  `runShell(cmdCtx, r.deps.Root, command)` (`verify_paths.go:496`), and
  `runShell` builds `sh -c` itself (`exec_tools.go:150-152`). Only
  `exec_command` goes through `containedShellArgv` (`exec_tools.go:88`; the only
  non-test caller, `rg containedShellArgv`). The verify command is the operator's
  own (`verify_paths.go:485`), so this is a trust decision rather than an
  oversight, but a verify script that prints a secret file reaches the
  transcript uncontained, and containing it would need the allow list (verify
  commonly reads `.env` and test keys). Do not wire it in before the allow list
  exists.
- **Secret-named file created mid-command outside a secret directory** (`echo $T
  > .env && cat .env`): the profile is built before the command
  (`containment.go:77-85`). Not coverable by a literal deny list on any
  platform. darwin could add a regex deny but needs the parity test against
  `MatchAnyFold` already called for in cost 3 above; Landlock and bwrap are
  literal by construction.
- **Seam note.** The "Seam: `runShell` / `runProc`" sentence in the original
  design above predates the implementation. The actual seam is
  `containedShellArgv` in front of `runArgv` (`exec_tools.go:88-95`).
- **Open owner decisions, not touched here:** default under strict posture, and
  the per-path allow list (it widens access; acceptance criteria 1 and 2 of
  ft-aeb570dc).
