# Governance

How decisions are made in MANVI, who makes them, and how that can change. It is
short because the project is small.

## Roles

**Maintainer.** [@bharathvbcr](https://github.com/bharathvbcr) is the sole
maintainer and the owner of the repository. The maintainer merges changes, cuts
releases, owns the security process and enforces the
[Code of Conduct](CODE_OF_CONDUCT.md). [`.github/CODEOWNERS`](.github/CODEOWNERS)
names the maintainer for every path.

**Contributor.** Anyone who opens an issue, a discussion or a pull request.
Contributors do not need permission to propose a change; see
[CONTRIBUTING.md](CONTRIBUTING.md).

There are no other standing roles.

## How decisions are made

- **Most changes** are decided in the pull request. If `./verify.sh` is green and
  the change does what its description says, the maintainer merges it.
- **Larger or contested changes** — a new command, a change to the policy ladder
  or to what a hard rule protects, a new dependency, a move of responsibility
  between MANVI and DevCouncil — should start as a
  [discussion](https://github.com/bharathvbcr/Manvi/discussions) or an issue, so
  the shape is agreed before the code is written.
- **The maintainer decides.** Discussion is how the decision is informed, not how
  it is counted. Where a decision is a judgement call, the reasoning goes in the
  pull request, the [changelog](CHANGELOG.md) or the
  [hardening ledger](docs/HARDENING_LEDGER.md), so that it can be read later.
- **Safety claims are held to the gate.** The harness makes load-bearing promises
  (see [SECURITY.md](SECURITY.md)). A change that weakens a check, deletes a test
  to make a change pass, or lets a gate report success without running will not
  be merged, whoever proposes it.

## Where a change belongs

MANVI wraps [DevCouncil](https://github.com/bharathvbcr/DevCouncil)'s components
(`devmap`, `dcstore`, `dcverify`, `dcgrep` and the crates behind them). DevCouncil
owns those components and is upstream for them. MANVI links none of them; it
resolves each as a binary and speaks JSON over stdio. The DevCouncil and gusset
commits MANVI builds against are pinned in
[`scripts/module-pins.txt`](scripts/module-pins.txt), and `./verify.sh` fails
when a sibling checkout is at another commit. A change to a component's behaviour
is made in DevCouncil and reaches MANVI by a pin bump. The table in
[CONTRIBUTING.md](CONTRIBUTING.md#5-which-repository-does-this-change-belong-in)
says which side a given change belongs on, and
[`docs/COMPONENTS_AND_HARNESS.md`](docs/COMPONENTS_AND_HARNESS.md) explains the
boundary.

## Becoming a maintainer

There is one maintainer today and the project would be better with more. The path
is practical rather than formal:

1. Contribute sustained, reviewed work: changes that passed `./verify.sh`, each
   with a test that fails against the code it fixes.
2. Review other people's pull requests and answer questions in Discussions.
3. Show that you read the gate's refusals as information rather than obstacles —
   the hardening ledger is the place to see what that looks like.

When the maintainer thinks that describes you, they will invite you. You can also
ask, in a discussion or by contacting the maintainer through their GitHub
profile. A new maintainer is added to `CODEOWNERS` and to the repository's
access list in the same change that records it here. A maintainer who has been
inactive for a long period, or who asks to step down, is moved to emeritus in the
same way.

## Branches and merging

- **`main` is the only long-lived branch**, and it is protected. Contributors
  change it only by pull request. A pull request needs one approving review from
  a code owner, all conversations resolved, and the required status checks
  passing. Force-pushes to `main` and deletion of `main`
  are not allowed.
- **Release tags (`v*`) are protected**: they cannot be moved or deleted once
  published, short of the documented rollback below.
- **Branches** are named by intent: `feat/`, `fix/`, `docs/`, `chore/`.
- **Every commit is signed off** (`git commit -s`), certifying the
  [Developer Certificate of Origin](https://developercertificate.org/).
- **Commit messages carry the reasoning.** The history keeps it in commit bodies,
  so a merge keeps the commits' messages rather than replacing them with a summary.
- **Nothing is merged with `./verify.sh` red.** The maintainer is one person, so
  the review rule is a second pair of eyes only when there is one; the checks are
  not optional either way.

## Releases

A release is a `v<major>.<minor>.<patch>` tag on a commit that carries its notes
in `docs/releases/<tag>.md`. Pushing the tag runs
[`release.yml`](.github/workflows/release.yml), which builds the binaries from
the pinned DevCouncil and gusset commits, signs and notarizes the macOS ones,
and publishes SBOMs, `SHA256SUMS` and `PROVENANCE.json`. A run that cannot sign
does not publish. The procedure, including what each file in a release means, is
in [`docs/RELEASING.md`](docs/RELEASING.md).

A bad release is withdrawn with
[`rollback-release.yml`](.github/workflows/rollback-release.yml), a manual
workflow that marks the release as a pre-release or draft, optionally deletes its
assets, and restores the previous release as latest. It never touches the git tag
unless `delete_tag` is set, and it does nothing unless the tag is typed twice. A
fixed release is published under a new tag.

Versions are pre-1.0. Fixes land on `main` and ship in the next tag; older tags
are not patched (see [SECURITY.md](SECURITY.md)).

## Security and conduct

- Vulnerabilities go through
  [private vulnerability reporting](https://github.com/bharathvbcr/Manvi/security/advisories/new),
  as described in [SECURITY.md](SECURITY.md).
- Conduct problems are reported privately to the maintainer, as described in
  [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md). Because the maintainer is also the
  enforcer, a report about the maintainer's own conduct cannot be handled inside
  the project; GitHub's own abuse reporting is the remaining route.

## Changing this document

This file changes by pull request like any other. Changes to the roles, the
decision process or the branch rules are discussed in a
[discussion](https://github.com/bharathvbcr/Manvi/discussions) first.
