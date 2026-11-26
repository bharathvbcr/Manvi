# Feature: 002 - Policy Enforcement, Gusset/Hardening, and Computer-Use Privacy

**Status**: Active Specification | **Canonical Owner**: Manvi (`github.com/bharathvbcr/Manvi`) | **Date**: 2026-11-26 | **Implementation State**: Shipped enforcement runtime (`manvi/devcouncil`, `manvi/serve`, `manvi/credentials`, `manvi/computer`), with cross-repo matrix alignment tracked in `tasks.md`.

## 1. Problem Statement & Motivation

In the DevCouncil stack, policy logic is intentionally split across three planes: DevCouncil defines the rule taxonomy, 6-rung evaluation ladder, 8-state verdict contract, and `grants` ledger; Manvi enforces those decisions at runtime across file writes, shell commands, git commits, post-exec ref updates, locked credential memory, OS sandboxing, and desktop computer-use runs; and Waylode presents verdicts, manages approval UI, and triggers git hooks.

Without a binding specification for Manvi's enforcement plane, subtle drifts break security guarantees—for instance, a preview check (`devcouncil_policy_check_write`) disagreeing with actual writes (`devcouncil_write_file`), symlink indirection bypassing Seatbelt rules, or a missing external check (`dcverify` or commit ref scan) failing open as `passed`. `specs/002` locks down Manvi's enforcement responsibilities, fail-closed invariants, `b27`–`b32` adversarial hardening, and computer-use privacy boundary with Jarvis.

## 2. Canonical Ownership & Non-Overlap Boundary

### Ownership Matrix

| Layer / Capability | Canonical Owner | Manvi Role | Explicit Non-Goals / Delegations |
| --- | --- | --- | --- |
| **Policy Decision Engine, Rule Taxonomy & `verdict.schema.json`** | **DevCouncil** (`DevCouncil/backend/go_orchestrator/{policy,gate,grants}`, `DevCouncil/rust/dc-{glob,redact}`, `DevCouncil/backend/contracts/verdict.schema.json`) | Imports `policy`/`gate`/`grants`, enforces `contracts/verdict.schema.json` byte parity via `manvi/internal/contract/contract_test.go`, and projects `decisionPayload` (`manvi/devcouncil/policy_tools.go`). | Never re-defines rule severities in Go maps outside DevCouncil or alters wire schema fields independently of DevCouncil & Waylode (ratified in `DevCouncil/specs/003-stack-boundaries-and-non-overlap/contracts/ownership-matrix.json`). |
| **Runtime Write, Command & Git Enforcement** | **Manvi** (`manvi/devcouncil/{file_tools,exec_tools,policy_tools,git_tools,commitscan,refguard,readauth,containment}.go`, `manvi/serve/policy.go`) | Enforces 6-rung gate before writes/commands (`authorisingTask`), checks `command.secret_read` operands (`63ca82ea`), runs commit-time blob/value scanning (`commitscan.go`) and post-exec ref rollback (`refguard.go`), and applies macOS Seatbelt secret containment (`containment.go`). | Delegates rule matching patterns (`policy.SecretPathPatterns`) and `dc-glob` matcher (`policy.GoMatcher` / Gusset) to DevCouncil/Gusset. |
| **CLI Pre-Flight & Gusset Diagnostics** | **Manvi** (`manvi/cmd/manvi/main.go`) | Owns `manvi check` exit taxonomy (`0` allowed, `6` soft/grantable, `7` hard, `1` failure), `manvi allow`, `manvi doctor`, and `manvi gusset-check`. | Does not implement `dev` CLI commands directly (`manvi/devcouncil/devbridge.go` delegates to DevCouncil). |
| **Locked-Memory Credentials & Benchverify Hardening (`b27`–`b32`)** | **Manvi** (`manvi/credentials/lockedmem.go`, `bench/stress_test.py`) | Owns `memguard` bounded `mlock` pre-probe (`10s` bound), `ui.Sanitize` / credential scrubbing, `define_role` / `refguard` / non-UTF-8 listing adversarial hardening (`b27`–`b31`), and `b32-benchverify` fail-closed checker/worker RPC. | Does not store persistent OS keychain secrets for Waylode GUI sessions. |
| **Desktop / Computer-Use Privacy & Masking Seam** | **Manvi + Jarvis** (`manvi/computer/{runner,privacy,mask}.go`) | Owns `Desktop` / `Focuser` / `Control` runtime interfaces, host-supplied `PrivacyPolicy`, `EffectivePolicy`, semantic control masking (`sensitiveInputTargets`), and folded secret redaction (`foldSensitive`). | Delegates OS accessibility/compositor capture to injected `Desktop` backends (e.g., `Jarvis` / `manvi-desktop`). |
| **Verdict UI & Operator Hooks** | **Waylode** (`waylode-hook`, `src-tauri/src/harness/mod.rs`) | Consumes `manvi serve` `policy.check.*` decisions (`guard_command` -> `manvi serve`) and renders the 8 verdict classifications (`unchecked`, `denied`, `granted`, `demoted`, `widened`, `degraded`, `warned`, `clean`), and in Waylode Labs tiers optionally layers `policy_bundles` (`Enterprise`), remote command approval over `gitpulse-relay` (`Geek` / `Geek Squad`), and centralized `audit_export`. | Manvi stays 100% free, zero-sign-in, and zero-billing: it enforces local policy, Gusset, and local JSON audit export identically under Waylode Primary and Waylode Labs without verifying subscriptions or holding relay keys; Waylode owns GUI badges, approval dialogs, relay auth, and hooks. |

### Verified Overlap & Drift Ledger

1. **Gusset `dc-glob` Archive vs `CGO_ENABLED=0` Process Boundary**:
   - Policy glob matching runs in-process when Gusset links `dc-glob` (`manvi gusset-check`, `manvi/serve/gusset_policy_test.go`), whereas `dcverify` rigor checks (`manvi/devcouncil/rigor.go`) intentionally remain behind a bounded JSON subprocess boundary so static `CGO_ENABLED=0` binaries work cleanly across platforms without mandatory `cgo` linking.
2. **Contract Schema Mirrors (`contracts/verdict.schema.json`)**:
   - `DevCouncil` is canonical owner of `contracts/verdict.schema.json`, `contracts/event.schema.json`, and `contracts/lease.schema.md`. Manvi carries verified copies checked by `manvi/internal/contract/contract_test.go` and `contracts/CHECKSUMS` (`T-08` enforces parity checks during cross-repo sync).
3. **`manvi check` vs `dev` Tool Exit Taxonomy**:
   - `manvi/cmd/manvi/main.go` intentionally separates `manvi check` (`0`/`6`/`7`/`1`) from `manvi tool` (`0`/`1` only), documented explicitly beside `dev tool call` (`0`/`1`/`2`/`3`) so CI callers never conflate policy refusals with tool transport failures.

## 3. User Stories & Acceptance Scenarios

### US1: Unified Write, Command & Git Commit Enforcement (Priority: P1)

As a repository owner, every file write, shell command, and git commit executed by an agent passes the exact same DevCouncil policy gate (`authorisingTask`), commit blob/value scanner, and post-exec ref guard so credentials and protected paths never enter repository history.

- **Scenario 1: Preview-write agreement (`devcouncil_policy_check_write` vs `devcouncil_write_file`)**
  - **Given** a session with or without an active task lease under a configured posture (`manvi/devcouncil/policy_tools.go`)
  - **When** an agent calls `devcouncil_policy_check_write` followed by `devcouncil_write_file` on the same path
  - **Then** both invoke `authorisingTask` identically and report identical `rule`, `severity`, `subject`, and `agent_grantable` payloads.
- **Scenario 2: Secret-read operand check and macOS Seatbelt containment**
  - **Given** an agent running `devcouncil_exec_command` with `command.secret_read` rules (`63ca82ea`) and `harness.exec_containment` active on macOS (`manvi/devcouncil/containment.go`)
  - **When** a command searches a literal regex pattern vs opening a secret file path (including via symlink or dangling symlink)
  - **Then** regex/search pattern operands are not misclassified as path reads, while secret path patterns and resolved symlink targets are denied both by command policy and Seatbelt (`/usr/bin/sandbox-exec`).
- **Scenario 3: Post-exec ref rollback on leaked secret**
  - **Given** a shell command inside a git repository tracked by `refguard.go`
  - **When** the command commits a blob matching a secret path, secret value, or `Deps.KnownSecret` (or fails/exceeds `maxGuardCommits`)
  - **Then** `refguard.go` rolls back only the leaking ref via `git update-ref <ref> <old> <current>` (never clobbering concurrent external updates) or blocks fail-closed with `ruleRefUnscanned`.

### US2: Verdict Schema Parity & `manvi check` Exit Fidelity (Priority: P1)

As an automated pre-flight or host consumer (Waylode), I receive byte-faithful `verdict.schema.json` payloads and unambiguous `manvi check` exit statuses distinguishing allowed writes, grantable soft blocks, and hard refusals.

- **Scenario 1: Hard vs soft exit status in `manvi check`**
  - **Given** a path blocked by a soft rule (`scope.unplanned`) vs a hard rule (`path.secret`, `path.outside_root`)
  - **When** `manvi check PATH` runs (`manvi/cmd/manvi/main.go`)
  - **Then** soft blocks exit `6` with the exact `manvi allow` recovery command, hard blocks exit `7` with no grantable path, and clean allows exit `0`.
- **Scenario 2: Un-overridable fail-closed fallback on unknown rule**
  - **Given** a rule absent from `severityByRule` in `contracts/verdict.schema.json`
  - **When** `decisionPayload` (`manvi/devcouncil/policy_tools.go`) or a Waylode consumer classifies the verdict
  - **Then** severity defaults to `hard` and `clean` never evaluates true when `grant_id`, `demoted`, `widened`, or `degraded` is present.

### US3: Locked-Memory Credentials & `b27`–`b32` Adversarial Hardening (Priority: P1)

As a security reviewer, credential buffers avoid swap exposure via bounded `memguard` locking, and adversarial inputs across RPC, role definition, non-UTF-8 paths, and ref updates fail closed rather than hanging or passing silently.

- **Scenario 1: Pre-`mlock` bounded probe (`manvi/credentials/lockedmem.go`)**
  - **Given** a process with constrained `RLIMIT_MEMLOCK` initializing `memguard`
  - **When** `lockedmem.go` runs its `guarded` probe within the `10s` bound (`ErrLockedMemory`)
  - **Then** failures return `ErrLockedMemory` cleanly without hanging the harness process or leaking plaintext credentials on retry.
- **Scenario 2: Fail-closed `b32-benchverify` checker/worker RPC & `b27`–`b31` guards**
  - **Given** malformed RPC frames in `bench/stress_test.py` (`b32-benchverify`) or adversarial symlinks/non-UTF-8 entries (`b28`–`b31`)
  - **When** the worker/checker or harness inspects them
  - **Then** malformed RPC payloads fail closed cleanly (and `scripts/verify-expected-gaps.json` records platform-specific OS skips such as `TestAdvB31StreamedListingNonUTF8Name` on APFS).

### US4: Computer-Use Privacy & Folded Secret Masking (Priority: P2)

As an operator running desktop/computer-use workflows (`manvi/computer/`), sensitive input values and the UI controls holding them are masked across reformatting, zero-width controls, and adjacent sibling nodes before model inspection.

- **Scenario 1: Folded and spatial control masking**
  - **Given** a host-supplied `PrivacyPolicy` (`manvi/computer/privacy.go`) plus sensitive workflow parameters merged via `EffectivePolicy`
  - **When** an application reformats a watched credential (spaces, full-width ASCII, dashes, zero-width joiners) or recreates the control in-place
  - **Then** `foldSensitive` (`minFoldedSecretLen = 6`), sibling window scanning (`maxSplitSiblings = 8`, `maxSplitBytes = 4096`), and `maskMemory` (`maxRememberedMasks = 256`, `rememberedOverlap = 0.5`) redact both text and rendered screen regions.

## 4. Success Metrics & Testability Criteria

- **SC-001**: `GOMAXPROCS=4 go -C manvi test ./internal/contract/...` passes all `contracts/verdict.schema.json` and checksum parity tests.
- **SC-002**: `GOMAXPROCS=4 go -C manvi test ./devcouncil -run 'Test.*Refguard|Test.*Containment|Test.*Commit|Test.*Adv'` passes refguard, containment, commitscan, and `b27`–`b31` adversarial tests (with only `scripts/verify-expected-gaps.json` documented APFS skips on macOS).
- **SC-003**: `GOMAXPROCS=4 go -C manvi test ./credentials/... ./computer/...` passes `memguard` locked-memory and folded computer-use masking suites.
- **SC-004**: `manvi check` exits `0` / `6` / `7` / `1` matching `docs/CLI_AND_CONFIGURATION.md` and `manvi/cmd/manvi/main.go`.
- **SC-005**: `devcouncil spec validate 002 --strict --project-root /Users/bharath/Code/devtools/Manvi` passes with zero blocking or advisory findings.

## 5. Edge Cases, Exclusions & Open Questions

- **Exclusions**:
  - `manvi/devcouncil/commitscan.go` documents explicit physical residuals: files exceeding the `1 GiB` stream budget fail closed (requiring operator commit outside the harness), and PEM/multi-line secrets split across `+` lines are matched by exact blob ID rather than cross-line reconstruction.
  - `manvi/devcouncil/refguard.go` guards the repository at `Registry` root (and linked worktrees sharing refs), not separate nested `git init` clones created during a command.
  - `manvi/devcouncil/containment.go` skips `.git/objects` during working-tree secret walks (`maxContainmentWalk = 2_000_000`) and bounds generated Seatbelt profiles at `256 KiB`.
  - Manvi contains zero billing, account, or subscription code: Waylode Primary runs all local policy gates and JSON audit export free with no sign-in, while Waylode Labs org-signed `policy_bundles` (`Enterprise`) and remote command approvals (`Geek` / `Geek Squad` via `gitpulse-relay`) enter through host-mediated `manvi serve` policy seams.
- **Edge Cases**:
  - Untracked unreadable directories reported via `git ls-files` warnings are recorded in `degraded` (`scanNote`) rather than ignored as clean listings (`commitscan.go`).
  - Values shorter than `minFoldedSecretLen` (`6`) in `manvi/computer/mask.go` stay literal-matched only to avoid redacting ordinary 4-digit numbers.
- **Open Questions**: None (all ambiguities and boundaries are resolved; cross-repo matrix alignment is tracked in `tasks.md`).
