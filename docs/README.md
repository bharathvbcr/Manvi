# MANVI Documentation Hub

Welcome to the MANVI technical documentation. MANVI (*of Manu*) is a high-performance coding-agent harness built in pure Go and Rust with native tool execution, dual-plane determinism, and zero external runtime dependencies.

---

## Interactive Visual Guides

| Document | Format | Description |
|---|---|---|
| [**Interactive Visual Architecture Guide**](04-visual-architecture-guide.html) | HTML | **Interactive visual guide**, component explorer, state machine explorer, and policy simulator. |
| [**Language & Runtime Partition Decision**](01-language-decision.html) | HTML | Go vs Rust evaluation, memory safety, and dual-plane partition rationale. |
| [**Build Plan & Seam Specifications**](02-build-plan.html) | HTML | The seven architectural seams, plugin tiers, and multi-provider replay state. |
| [**DevCouncil Harness Strategy**](03-devcouncil-harness-strategy.html) | HTML | Original Go+Rust strategy, native integration, and phase milestones. |

---

## Core Architecture & Engine Specifications

- [**Why MANVI is Different (Comparative Analysis)**](COMPARISON.md)  
  Comprehensive feature matrix and deep-dive comparison against SWE-agent, OpenHands, Aider, Claude Code, Pi, Oh My Pi, Kon, and traditional Python agent frameworks.
- [**Technical Architecture Specification**](ARCHITECTURE.md)  
  Dual-plane partition (Go IO/concurrency vs Rust CPU/determinism), stdio JSON IPC protocol, session log invariants, and complete package map.
- [**Policy & Safety Engine Specification**](POLICY_AND_SAFETY.md)  
  6-rung policy ladder, 5 outcome states (`Passed`, `Blocked`, `Granted`, `Demoted`, `Degraded`), Write Gate, Command Gate, and Grants Ledger.
- [**Agent & Turn Lifecycle Specification**](AGENT_AND_TURN_LIFECYCLE.md)  
  Turn execution loop, tool waterfalls (`pre-execute`, `post-execute`), append-only context compaction, SQLite task leases, and clean cancellation.
- [**Architectural Trade-offs**](TRADE_OFFS.md)  
  Explicit rationale for strict posture write discipline vs command allowlists, and two toolchains (Go + Rust) with static `CGO_ENABLED=0` guarantees.

---

## Subsystem & Integration Guides

- [**Running Against Local LLMs**](LOCAL_LLMS.md)  
  Zero-config 30ms discovery, KV prefix-cache preservation (**1.5s warm vs 120s cold prefill**), wire-level parser recovery (Hermes, Qwen3 XML), `<think>` tag sanitization, and recoverable truncation.
- [**Embedded Stdio Host Plane (`manvi serve`)**](SERVE_HOST_PLANE.md)  
  Zero-cgo line-delimited JSON protocol over stdio for embedding MANVI inside IDE extensions (VS Code, JetBrains), editors, and host desktop applications.
- [**Terminal UI & Event Subsystem**](TUI_AND_EVENT_SUBSYSTEM.md)  
  Modern full-screen Elm-loop TUI, multi-session tab strip, dynamic live theme switcher (`/theme`, `Ctrl+Y`), session modal (`Ctrl+S`), syntax highlighting, zero-allocation damage-diff painter, and keybindings.
- [**CLI & Configuration Reference**](CLI_AND_CONFIGURATION.md)  
  Complete CLI subcommand reference, exit codes (`0` through `5`), full flag catalogue, mutability scopes (`human` vs `startup`), `.devcouncil/config.yaml` schema, and posture matrix.
- [**DevCouncil Native Tool Suite Reference**](TOOLS_REFERENCE.md)  
  Category summary of all 44 native tools, with detailed parameter and permission specifications for Task Lifecycle, Guarded Mutation, Multi-Agent, Override Seam, Verification, Code Graph Navigation, Git Integration, and the External CLI Bridge. The dynamically activated groups (tool discovery, sub-agents, artifacts, questions, MCP) are tabulated but not yet specified there.

---

## Quality, Parity & Hardening

- [**Verification & Parity Specification**](VERIFICATION_AND_PARITY.md)  
  Cross-language testing methodology, 1,031 parity fixtures (`fnmatch-parity.tsv`, `command-parity.tsv`), diff-coverage intersection, anti-stub rigor gates, and the master `./verify.sh` gate.
- [**Hardening Ledger & Defect Catalogue**](HARDENING_LEDGER.md)  
  Complete catalogue of 30+ hardened invariants, defect patterns, failure modes, and automated regression tests.
