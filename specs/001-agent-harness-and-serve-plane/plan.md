# Implementation Plan: 001 - Agent Harness, Tool Registry, and `manvi serve` Host Plane

**Branch**: `main` | **Date**: 2026-11-26 | **Spec**: `specs/001-agent-harness-and-serve-plane/spec.md`

## 1. Summary & Architectural Topology

Manvi serves as the single Go execution harness (`CGO_ENABLED=0`-friendly, subprocess-isolated against `dcstore`, `dcverify`, and `devmap`, with optional Gusset linkage for `dc-glob`) bridging DevCouncil's deterministic governance packages and Waylode's desktop UI. Manvi is 100% free and open source (`MIT OR Apache-2.0`) with zero sign-in or paywall logic: it powers Waylode Primary locally and exposes clean telemetry (`CostNanoUSD`, `StallCost`) and host-plane seams (`manvi/serve/`) when a host optionally layers Waylode Labs relay/org workflows (`policy_bundles`, remote command approvals, team spend rollups) outside Manvi.

```text
  Waylode Host (GUI / Electron / SwiftUI)
       |  NDJSON stdio / Unix socket (ProtocolVersion = 1)
       v
  Manvi Host Plane (manvi/serve/)
   +-- server.go / protocol.go    (8 built-in ops + Router/Module seam)
   +-- workbench.go               (work.* methods via dc/store, VerifyRunner, ManagedRunner)
   +-- verify.go / verify_checks.go (devmap QueryAffected batches + devcouncil.VerifyPresent + critic)
       |
       +--> Manvi Harness Core
       |     +-- manvi/agent/loop.go           (PreStep, LLMRequest, TurnStopping, compaction)
       |     +-- manvi/cmd/manvi/tiers.go      (llm.tier.*, roster, fallback chains)
       |     +-- manvi/devcouncil/tools.go     (55-tool Registry, debate.go, work_tools.go, verify.go)
       |     +-- manvi/ui/                     (Elm TUI, Sanitize, headless NDJSON)
       |
       +--> DevCouncil Core (imported Go packages + Rust binaries)
             +-- backend/go_orchestrator/{dc,dc/store,dc/devmap,policy,gate,grants,flags}
             +-- rust/{dc-store,dc-verify,dc-proc,devmap-cli}
```

## 2. Verified File Map

- **Turn Loop & CLI**: `manvi/agent/loop.go`, `manvi/cmd/manvi/main.go`, `manvi/cmd/manvi/tiers.go`
- **Tool Registry & Debate**: `manvi/devcouncil/tools.go`, `manvi/devcouncil/debate.go`, `manvi/devcouncil/work_tools.go`, `manvi/devcouncil/subagent_tools.go`, `manvi/devcouncil/rigor.go`, `manvi/devcouncil/verify.go`, `manvi/devcouncil/verify_paths.go`
- **`manvi serve` Plane**: `manvi/serve/server.go`, `manvi/serve/module.go`, `manvi/serve/protocol.go`, `manvi/serve/workbench.go`, `manvi/serve/verify.go`, `manvi/serve/verify_checks.go`, `manvi/serve/policy.go`, `manvi/serve/managed.go`, `manvi/serve/socket.go`
- **Contract & Doc Verification**: `manvi/internal/contract/{contract_test,docs_test,docs_pins_test,docs_residue_test,readme_truth_test}.go`, `manvi/devcouncil/toolcount_docs_test.go`

## 3. Constitution Alignment Check

- **Principle I (Truthful Observability)**: `PathReport` (`manvi/devcouncil/verify_paths.go`), `VerifyRecord` (`manvi/serve/verify.go`), and `okDegraded` (`manvi/devcouncil/tools.go`) surface every timeout, cap, and missing binary as `degraded` or error—never a clean `passed`. Status: **PASS**.
- **Principle II (Non-Overlapping Ownership)**: Business logic for stores (`dc/store`), rigor gates (`dcverify`), and structural indexing (`devmap`) remains in DevCouncil; Manvi owns runtime orchestration and host-plane transport. Tracked seam cleanups (`T-06`, `T-07`) close remaining DTO/schema parity gaps without dual implementations. Status: **PASS**.
- **Principle III (Bounded Resource & Process Isolation)**: `rigorClient` caps stdout (`16 MiB`), stderr (`64 KiB`), and Lean deadlines; `VerifyRunner` bounds active jobs (`4`), changed paths (`2000`), and `devmap` batch size (`16`); `protocol.go` caps `maxTokenCount` (`1 << 24`). Status: **PASS**.
- **Principle IV (Static Binary Portability)**: Rust `dcverify` / `dcstore` run over bounded subprocess JSON boundaries (`rigor.go`), avoiding mandatory `cgo` coupling in standard Go builds. Status: **PASS**.

## 4. Complexity Tracking

| Deviation / Seam | Why Needed Now | Simpler Alternative Rejected | Closure / Guard |
| --- | --- | --- | --- |
| In-process wrapper registration of 8 `devcouncil_*` tools in `manvi/devcouncil/tools.go` beside `DevCouncil/registry.go` | In-process agent loops share live `authorisingTask`, `WorkBinding`, and zero-IPC gate/override state per file edit | Calling `devcouncil mcp` over stdio on every read/write adds subprocess latency and loses in-process `ui`/`approval` callbacks | Tracked schema-parity test (`T-07`) ensures the 8 shared tool names/schemas stay aligned with DevCouncil |
| `manvi/devcouncil/verify.go` maintains `Gap` / `NextAction` with harness-specific `Tool` + `Arguments` alongside `DevCouncil/.../verify/types.go` | Manvi routes model repair directly to `devcouncil_request_override` with exact rule/target arguments | Dropping `Tool`/`Arguments` would force models to construct override payloads from prose; duplicating gap types risks drift | Tracked audit/alignment (`T-06`) to project shared `DevCouncil/devcouncil/verify` gap taxonomy cleanly |
| `manvi/serve/verify.go` (`VerifyRunner`) splits `devmap` `QueryAffected` into batches of `16` (`maxDevmapAffectedBatch`) | DevMap rejects single queries exceeding 16 targets (`verify_checks.go` L151-155) | Degrading whenever >16 files changed would hide affected tests on routine feature branches | Batched queries stay deterministic and only degrade if total paths exceed `maxAffectedTargets` (`128`) |
