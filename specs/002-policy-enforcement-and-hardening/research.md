# Research & Security Audit: 002 - Policy Enforcement, Gusset/Hardening, and Computer-Use Privacy

**Date**: 2026-11-26 | **Audit Scope**: DevCouncil-Manvi-Waylode enforcement boundary, `b27`–`b32` hardening, Gusset seam, and `manvi/computer` privacy boundary.

## 1. Policy Decision vs Enforcement vs UI Presentation

- **DevCouncil (Decision & Schema Authority)**: Defines `policy.Decision`, `ruleId` (`29` rules + `""`), `severityByRule`, 6-rung evaluation order, `grants.Ledger`, and canonical `contracts/verdict.schema.json`.
- **Manvi (Enforcement & Projection)**:
  - Enforces write/command gates synchronously before every `devcouncil_{write,patch,delete}_file`, `devcouncil_exec_command`, and git mutation.
  - Projects `policy.Decision` into JSON with explicit `subject` (`policy.SubjectOf(d.Rule)`) and `agent_grantable` recovery metadata in `manvi/devcouncil/policy_tools.go`, eliminating prior preview/write mismatch by routing `devcouncil_policy_check_write` through `authorisingTask`.
  - Exposes `policy.check.file` and `policy.check.command` over `manvi serve` (`manvi/serve/policy.go`) with `Posture` (`devcouncil` vs `host`) and `HostScope` validation.
- **Waylode (UI & Hooks)**: Consumes verdicts over `manvi serve` and renders the total 8-state classification (`unchecked`, `denied`, `granted`, `demoted`, `widened`, `degraded`, `warned`, `clean`) per `contracts/verdict.schema.json`.

## 2. Gusset vs Process-Isolated Subprocess Seam

- **Alternative A (Mandatory `cgo` static/dynamic link for all Rust crates)**:
  - *Cons*: Couples standard Go CLI/test builds to `cgo` cross-compilation toolchains and propagates native panics into host processes.
  - *Verdict*: **Rejected** for `dcstore` and `dcverify` (`manvi/devcouncil/rigor.go`: *"joining the two languages with cgo would cost the static binary the execution plane was chosen for"*).
- **Alternative B (Process boundary for `dcstore`/`dcverify`/`devmap`; optional Gusset in-process link for `dc-glob` verified by `manvi gusset-check`)**:
  - *Pros*: Keeps `CGO_ENABLED=0` static portability for CLI/host sidecars while letting Gusset builds prove native `dc-glob` linkage (`manvi/cmd/manvi/main.go`, `manvi/serve/gusset_policy_test.go`).
  - *Verdict*: **Selected**.

## 3. Recent Hardening & Computer-Use Boundary Evidence

- **`command.secret_read` (`63ca82ea`)**: Excludes regex/search text pattern operands from secret-path refusal so searching for patterns does not false-positive as reading a credential file, while keeping path operands strictly gated.
- **macOS Seatbelt Containment (`manvi/devcouncil/containment.go`)**: Resolves real paths (`filepath.EvalSymlinks`) so symlinks cannot escape deny rules, denies dangling links by link name (`deny(path, false)`), skips `.git/objects`, and caps walks at `2_000_000` entries and profiles at `256 KiB`.
- **`memguard` Pre-Probe (`manvi/credentials/lockedmem.go`)**: Runs a `10s`-bounded `guarded` pre-`mlock` probe returning `ErrLockedMemory` cleanly when OS limits disallow locked pages.
- **Adversarial & Benchverify Hardening (`b27`–`b32`)**: Hardened `refguard.go` stdin/rev-list parsing (`zz_adv_b28_refguard_test.go`), runtime `define_role` admission (`advb29_define_adv_test.go`), non-UTF-8 streamed listings (`zz_advb31_test.go` + `scripts/verify-expected-gaps.json` macOS APFS skip), and `b32-benchverify` checker/worker RPC fail-closed error handling (`bench/stress_test.py`).
- **Jarvis Computer-Use Seam (`manvi/computer/`)**: Separates OS capture/input (`Desktop`, `Focuser`, `Control`) from privacy enforcement (`PrivacyPolicy`, `EffectivePolicy`, `foldSensitive`, `maskMemory`), ensuring model tool arguments can never relax watched secrets or target masks.

## 4. Wave A Pin & Waylode Labs Readiness Findings (`DRIFT-PIN-01`, `GAP-LABS-BUNDLE-01`)

- **`DRIFT-PIN-01`**: `scripts/module-pins.txt:8-9` pins `DevCouncil` at `63ca82ea` (259 commits behind `DevCouncil` `HEAD` `5beb1e54`) and `gusset` at `19383ad1` (19 commits behind `0b2db23`), while `manvi/go.mod:19-21` points local builds at sibling checkouts and `crates/dc-*` symlinks.
- **Clean `CGO_ENABLED=0` / Gusset & Fail-Closed Parity**: `verify.sh:256,744` gates both `CGO_ENABLED=0` and `manvi gusset-check`; `manvi/serve/gusset_policy_test.go:19-42` proves `serve` falls back to `fnmatch` when the Gusset engine is closed; `manvi/` contains zero Stripe/billing/sign-in code.
- **`GAP-LABS-BUNDLE-01`**: `manvi/serve/protocol.go:155-186` and `manvi/serve/policy.go:160-260` evaluate per-call `HostScope` and `AllowedCommands` with no `policy_bundles` verification/load operation yet for Waylode Labs `Enterprise` (`T-08`).
