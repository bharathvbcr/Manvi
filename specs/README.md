# Manvi Specifications

This directory holds Manvi's canonical 7-layer specifications (`spec.md`, `plan.md`, `research.md`, `data-model.md`, `contracts/`, `tasks.md`, `verification.md`, plus `constitution.md`), validated by `devcouncil spec validate --strict`.

## Index

| ID | Feature | Scope & Canonical Boundary | Status |
| --- | --- | --- | --- |
| `001` | [Agent Harness, Tool Registry, and `manvi serve` Host Plane](001-agent-harness-and-serve-plane/spec.md) | Turn loop, `feat/team-harness`, 55-tool native `Registry`, `devcouncil_debate`, TUI/event stream, `manvi serve` NDJSON host plane & `VerifyRunner`; audits `manvi/devcouncil/verify.go` vs DevCouncil `verify` and shared `devcouncil_*` tools. | Active (core shipped; overlap closures tracked) |
| `002` | [Policy Enforcement, Gusset/Hardening, and Computer-Use Privacy](002-policy-enforcement-and-hardening/spec.md) | Runtime enforcement of DevCouncil policy + Gusset engine, 6-rung gate & 8-state verdict contract, `command.secret_read`, `containment.go`, `commitscan.go`, `refguard.go`, `memguard`, `b27`–`b32` hardening, and Jarvis computer-use privacy masking. | Active (controls shipped; matrix sync tracked) |
| `003` | [Model Provider Plane, Spend Ledger, MCP Trust, and Session Log](003-provider-plane-mcp-trust-and-session-log/spec.md) | `manvi/llm/` (provider seam, five adapters, `SettleCall`, local discovery, `budget` ledger), `manvi/mcp/` (fingerprint authorization of workspace-declared servers), `manvi/session/` (append-only log, checksummed store, `AssertModelVisible`). | Active (shipped; no open work recorded) |

## Validation

Run from the repository root:

```bash
bun run check:specs   # devcouncil spec validate --all --strict
```

The command fails loudly when the `devcouncil` binary is missing. It is deliberately not part of `verify.sh` or the GitHub Actions workflows, because CI runners do not carry the binary. Packages not covered by any spec are listed in `003-provider-plane-mcp-trust-and-session-log/research.md`.

## Cross-Repository Boundary Reference

- **Design Philosophy**: [`docs/PHILOSOPHY.md`](../docs/PHILOSOPHY.md) (*An embeddable, local-model-first agent harness under explicit policy*).
- **Stack-Wide Ownership Matrix & 4-Lane Work Split**: `DevCouncil/specs/003-stack-boundaries-and-non-overlap/contracts/ownership-matrix.json` (12 capability domains and 4 exclusive `work_lanes` across DevCouncil, Manvi `lane-b-manvi`, Waylode Primary, and Waylode Labs; tracks `OV-VERIFY-01` and `OV-TOOLS-01` closures in `001/tasks.md`).
- **Consolidated Audit & Executable Boundary Gate**: [WORK_SPLIT_AND_DRIFT_AUDIT.md](../../DevCouncil/specs/003-stack-boundaries-and-non-overlap/WORK_SPLIT_AND_DRIFT_AUDIT.md) and [check-stack-boundaries.mjs](../../DevCouncil/scripts/check-stack-boundaries.mjs) (`--lanes`, `--route`).
- **Waylode Primary (Free) & Labs Boundary**: `GitPulse/specs/003-primary-and-labs-editions/contracts/edition-boundary.schema.json` — defines Waylode Primary (100% free & open-source local/BYOK, zero sign-in) vs. Waylode Labs (`pro`/`team`/`enterprise` over `gitpulse-relay`); Manvi remains 100% free (`MIT OR Apache-2.0`) with zero billing logic, exposing typed `manvi serve` and `CostNanoUSD` seams.
- **Wire Contracts**: [`contracts/README.md`](../contracts/README.md) documents `verdict.schema.json`, `event.schema.json`, and `lease.schema.md` parity rules with DevCouncil and Waylode.
