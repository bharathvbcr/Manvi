# Architectural Trade-offs

Two costs are paid deliberately in MANVI's architecture. Both are documented here at their real size, because a trade-off described as worse than it is gets worked around, and one described as smaller than it is gets discovered by an operator instead of read by one.

---

## 1. Strict Posture Buys Write Discipline with Shell Breadth — Not with Visibility

Declaring planned files in a task constrains **writing**, never **looking**.
- `devcouncil_read_file`, `devcouncil_list_dir`, `devcouncil_grep`, and `devcouncil_find_files` reach the filesystem through root containment alone and never enter the policy gate. Exploratory bug hunting under `strict` costs exactly what it costs under `dev`.
- A write outside the plan is also not an immediate stop: the neighbour rung admits a path in the same subsystem as a planned file, or in a declared neighbour of one, and only past that does `scope.unplanned` fire—a soft rule that an agent may clear for itself, bounded by a 15-minute TTL and a required justification reason. That is one extra tool call recorded as `granted`, with no human in the loop.

### The Command Allowlist Asymmetry

The friction that is real is the **command allowlist**, and it is the deliberate half of the trade.
- `command.no_lease` and `command.not_allowed` are soft rules, so a grant can clear them, but they are the two soft rules an agent may **not** grant itself.
- Under `strict`, an unlisted command (e.g. `go test ./...`) stops until a human grants it, the task lists it, or `grants.agent.allow_commands` is explicitly enabled—a flag rather than a default, so the choice appears in the run report.

**The Rationale for the Asymmetry**:
An unplanned file write is bounded by every rung above it and re-evaluated by the `dc-verify` engine afterwards. In contrast, arbitrary shell command execution is the mechanism through which gates themselves could be subverted, with no second gate behind it. 

Thus, `dev` is not an escape hatch for exploration (which never needed one). It is an escape hatch for writing unattended—and a `dev` run that denied nothing is not evidence that nothing would have been denied, which is why `Report.Strict()` refuses to call a demoted run clean.

---

## 2. Two Toolchains, Because Two Invariants Have One Owner Each

A full source build requires:
- **Go 1.26** (`manvi/go.mod`)
- **Rust 1.85+** (`crates/Cargo.toml` declaring edition 2024 with resolver 3)
- **A working C compiler** (`cc`): `dc-store` depends on `rusqlite` with the `bundled` feature, compiling SQLite from source rather than linking against whatever dynamic `libsqlite3` the host happens to ship.

### The Zero-Cgo Guarantee

**"No cgo" is a property of the shipped Go binary, not of the build.** 
- `CGO_ENABLED=0` is what keeps the Go execution plane a single static artifact with trivial cross-compilation.
- The process boundary is what preserves this property: Rust pays the C compilation cost once, at build time, on its side of the process boundary.

### Runtime Asymmetry

The requirement is not symmetric at runtime:

| Missing Binary | Consequence |
|---|---|
| `dcverify` | Secret scanning, stub detection, and diff coverage report as *did not run*, named in the decision's `degraded` list rather than counted as passing. |
| `dcstore` | Every store-backed tool fails hard (no leases, no tasks, no mutual exclusion). Under `dev`, a Go-only build still drives turns; writes land as `allow [demoted]` because "no task authorises this" is a soft rule. |

### Binary Discovery & Process Boundary

Discovery locates binaries automatically:
1. `MANVI_*_BINARY` environment variables
2. System `PATH`
3. Sibling build artifacts in `crates/target/{release,debug}`

Loosening the two-toolchain requirement would mean reimplementing the lease store in Go. However, the invariant it protects is a partial unique index in the SQLite schema (`WHERE status = 'active'`). Having two implementations of that would create two competing owners of a single guarantee—the exact architectural defect the dual-plane split was created to prevent.

The boundary stays a clean process boundary: stdio, one JSON object per call, zero cgo.
