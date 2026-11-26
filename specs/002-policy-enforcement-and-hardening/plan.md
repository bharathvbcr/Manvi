# Implementation Plan: 002 - Policy Enforcement, Gusset/Hardening, and Computer-Use Privacy

**Branch**: `main` | **Date**: 2026-11-26 | **Spec**: `specs/002-policy-enforcement-and-hardening/spec.md`

## 1. Summary & Architectural Topology

Manvi enforces DevCouncil's policy governance at every side-effect boundary—tool calls, shell subprocesses, git staging/commits, ref mutations, credential buffers, and desktop automation—while keeping rule definitions in DevCouncil and operator presentation in Waylode. Because Manvi ships zero billing or sign-in code (`MIT OR Apache-2.0`), Waylode Primary (Free) uses the full local enforcement and audit-export plane directly, while Waylode Labs (`gitpulse-relay`) optionally delivers host-brokered remote command approvals and org-signed `policy_bundles` across `manvi serve` without changing Manvi's fail-closed invariants.

```text
  DevCouncil Policy Definitions (backend/go_orchestrator/{policy,gate,grants})
       |  verdict.schema.json + SecretPathPatterns + GoMatcher/Gusset
       v
  Manvi Enforcement Plane
   +-- Tool & CLI Gate       (manvi/devcouncil/{policy_tools,file_tools,exec_tools}.go, cmd/manvi/main.go)
   +-- OS & Git Guards       (manvi/devcouncil/{containment,commitscan,refguard,readauth}.go)
   +-- Host Wire Gate        (manvi/serve/policy.go -> policy.check.file, policy.check.command)
   +-- Credential Hardening  (manvi/credentials/lockedmem.go, manvi/ui/sanitize)
   +-- Computer Privacy      (manvi/computer/{runner,privacy,mask}.go -> Jarvis Desktop seam)
       |
       v  verdict.schema.json (8-state consumer projection)
  Waylode Desktop UI & Hooks
```

## 2. Verified File Map

- **Contracts & Parity**: `contracts/verdict.schema.json`, `contracts/CHECKSUMS`, `manvi/internal/contract/contract_test.go`
- **Runtime Gate & CLI**: `manvi/cmd/manvi/main.go`, `manvi/devcouncil/policy_tools.go`, `manvi/devcouncil/readauth.go`, `manvi/serve/policy.go`
- **Containment, Commit Scan & Ref Guard**: `manvi/devcouncil/containment.go`, `manvi/devcouncil/commitscan.go`, `manvi/devcouncil/refguard.go`
- **Credentials & Computer Privacy**: `manvi/credentials/lockedmem.go`, `manvi/computer/runner.go`, `manvi/computer/privacy.go`, `manvi/computer/mask.go`
- **Adversarial & Expected-Gap Evidence**: `manvi/devcouncil/{zz_adv_b28_refguard_test,zz_advb31_test,advb29_define_adv_test}.go`, `scripts/verify-expected-gaps.json`, `bench/stress_test.py`

## 3. Constitution Alignment Check

- **Principle I (Fail Closed on Unscanned/Missing Checks)**: `refguard.go` treats over-cap/timed-out scans as `ruleRefUnscanned` hard blocks; `commitscan.go` refuses unhashable/unlisted staged blobs; `rigor.go` never turns missing `dcverify` into a clean pass. Status: **PASS**.
- **Principle II (Single Source of Policy Truth)**: `policy.SecretPathPatterns`, `policy.GoMatcher`, `Decision`, and `contracts/verdict.schema.json` originate in DevCouncil and are never forked in Manvi. Status: **PASS**.
- **Principle III (Bounded Security Mechanisms)**: `containment.go` caps directory walks at `2,000,000` entries and Seatbelt profiles at `256 KiB`; `lockedmem.go` bounds `mlock` probes at `10s`; `mask.go` bounds sibling spans (`8` nodes / `4096` bytes) and remembered rectangles (`256`). Status: **PASS**.
- **Principle IV (Non-Clobbering Recovery)**: `refguard.go` rolls back via `git update-ref <ref> <old> <current>` CAS checks and leaves staged/working-tree edits intact; `PrivacyPolicy` is strictly host-supplied, never model-supplied. Status: **PASS**.

## 4. Complexity Tracking

| Mechanism / Seam | Why Needed | Simpler Alternative Rejected | Guardrail / Invariant |
| --- | --- | --- | --- |
| Post-exec `refguard.go` snapshot & rollback alongside pre-exec Command Gate | Shell expansions (`${IFS}`, scripts, hooks, interpreters) can run `git commit` without appearing literally in pre-exec argv | Pre-exec argv parsing alone can be bypassed by dynamic shell expansion | Pre/post ref OID comparison + CAS `update-ref` rollback + `ruleRefUnscanned` fail-closed |
| Commit-time blob & `+`-line secret scanner (`commitscan.go`) | `cp .env notes.txt` bypasses filename-only `SecretPathPatterns` checks | Filename-only filtering leaks renamed or edited credential values | Checks blob OID identity, `KEY=VALUE` lines, and `Deps.KnownSecret` within bounded budgets |
| Folded Unicode & sibling-window secret matcher (`manvi/computer/mask.go`) | GUI apps reformat secrets (spaces, full-width digits, zero-width joiners, split sibling accessibility nodes) | Literal substring redaction fails immediately on formatted UI echoes | `foldSensitive` (`>= 6` runes) + `maxSplitSiblings = 8` + `maskMemory` overlap (`0.5`) |
| Subprocess `dcverify` vs optional Gusset `dc-glob` linking | Preserves static `CGO_ENABLED=0` binaries while supporting Gusset fast-path when built with it | Mandatory `cgo` linking breaks cross-platform static builds | `manvi gusset-check` verifies linked Rust `dc-glob` explicitly when enabled |
