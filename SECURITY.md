# Security Policy

MANVI executes model-authored file writes and shell commands against a real
repository. The policy ladder is the thing standing between those two facts, so
a way around it is the vulnerability class that matters here.

## Reporting

Use GitHub's private vulnerability reporting: **Security → Report a
vulnerability** on this repository. It is enabled. Please do not open a public
issue for something you believe is exploitable.

Include the posture the run was under (`manvi doctor` prints it), the decision
payload from the run report, and the session log if you have one — every tool
call is recorded there with the rule that judged it, which is usually the
shortest path to the answer.

There is no bounty. This is a personal project, and a fix will be as fast as one
person can make it.

## Supported versions

Pre-1.0: fixes land on `main` and go out in the next tag. Older tags are not
patched.

## What counts

The harness makes a small number of load-bearing promises. A way to break one of
these is a vulnerability, and worth reporting:

- **A write that reaches outside the repository root**, or one that reaches a
  path a hard rule protects — `.git`, `.devcouncil`, agent configs, anything the
  secret patterns match. Hard rules are documented as ungrantable by any
  authority, including the operator.
- **A hard rule cleared by an override.** Soft rules are grantable by design and
  every grant is recorded; hard rules are not.
- **A command that reaches the shell without passing the command gate.** The MCP
  path was exactly this before `mcp_call_tool` was gated as a command: a server
  advertising `run_shell` was a complete route around the gate, the write gate,
  and the approval prompt at once.
- **A credential leaving the machine it was meant for.** The `local` provider
  refuses to attach a credential to a non-local destination. A path that sends
  one anyway — or one that gets a key into a log, a session file, the terminal,
  or a model's context — is a vulnerability even if nothing else breaks.
- **`devcouncil_fetch_url` reaching an address it should refuse**: plaintext,
  a host outside `MANVI_FETCH_HOSTS`, a private or link-local range, a
  non-default port, or any of those on a redirect hop rather than the first
  request.
- **A check that reports success without running.** This one is unusual to see
  in a security policy and it belongs here: a gate that returns "passed" when it
  could not execute is how an unexamined change comes to look approved. If you
  find one, it is a defect of the same class as a bypass.
- **A symlink or hard link that carries a write into a file the ladder did not
  judge.** The artifact store opens with `O_NOFOLLOW` and the write path pins
  inode identity and checks `st_nlink` for exactly this.

## What does not count

- **A `dev` posture allowing an unplanned write.** That is what `dev` is, it is
  documented, the run banner says so, and every demoted allow is marked in the
  report. `Report.Strict()` refuses to call such a run clean. Report a case
  where the demotion is *not* marked — that would be the bug.
- **A command an operator put on the allowlist doing what it says.** The gate
  arbitrates; it does not second-guess an explicit grant.
- **The model doing something unhelpful, wrong, or expensive.** That is a
  quality issue. Open a normal issue.
- **Credential-shaped strings in test files.** The secret scanner needs a corpus
  to detect, so the suites contain deliberately fake keys. See
  [`.github/secret_scanning.yml`](.github/secret_scanning.yml) for the scoped
  exclusions; production paths stay scanned.

## Where the fixes are recorded

[`docs/HARDENING_LEDGER.md`](docs/HARDENING_LEDGER.md) carries the defect
patterns already found, each with its root cause and the regression test that
now holds it. It is the fastest way to see which classes have been walked
before, and to tell a new bypass from a known one.
