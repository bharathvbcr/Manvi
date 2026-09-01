# Architectural Trade-offs

Three costs are paid deliberately in MANVI's architecture. All are documented here at their real size, because a trade-off described as worse than it is gets worked around, and one described as smaller than it is gets discovered by an operator instead of read by one.

---

## 1. Strict Posture Buys Write Discipline with Shell Breadth — Not with Visibility

Declaring planned files in a task constrains **writing**, never **looking**.
- `devcouncil_read_file`, `devcouncil_list_dir`, `devcouncil_grep`, and `devcouncil_find_files` reach the filesystem through root containment alone and never enter the policy gate. Exploratory bug hunting under `strict` costs exactly what it costs under `dev`. `devcouncil_grep` narrows what it *returns* by the repository's ignore rules, which is a readability decision rather than a policy one: `include_ignored` lifts it with no gate and no grant, and every reply states which mode it ran in.
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
| `dcgrep` | `devcouncil_grep` refuses, naming the build command and `MANVI_GREP_BINARY`. It does not fall back to a Go walker: a second search implementation would answer the same question differently, and the failure this refusal prevents — a missing searcher reporting `{"count":0}` for a repository that does contain the symbol — is worse than the tool being unavailable. `manvi doctor` reports the searcher beside the store and the verifier. |

### Binary Discovery & Process Boundary

Discovery locates binaries automatically:
1. `MANVI_*_BINARY` environment variables
2. System `PATH`
3. Sibling build artifacts in `crates/target/{release,debug}`

Loosening the two-toolchain requirement would mean reimplementing the lease store in Go. However, the invariant it protects is a partial unique index in the SQLite schema (`WHERE status = 'active'`). Having two implementations of that would create two competing owners of a single guarantee—the exact architectural defect the dual-plane split was created to prevent.

The boundary stays a clean process boundary: stdio, one JSON object per call, zero cgo.

---

## 3. Four Provider Adapters, and a Hosted OpenAI-Compatible Endpoint Is Not a Config Change

The provider seam serves four adapters: `anthropic`, `gemini`, `xai`, and
`local`. There is no OpenAI adapter, no DeepSeek adapter, and no Groq, Together,
or Cerebras adapter. That is a decision, not a backlog item, and this is the
size of it.

### The wire is already here; the credential rule is what stops it

`manvi/llm/openaicompat` implements the OpenAI wire in full — streaming,
tool-call assembly, and recovery from the truncations local servers produce —
and the `local` adapter speaks it. Every provider named above serves an
OpenAI-compatible API, so the obvious move is to point `llm.local.base_url` at
one of them and call the question answered.

It does not work, and the reason it does not is deliberate. The `local` adapter
accepts `OPENAI_API_KEY` as a convenience, and `llm.local.base_url` does not
have to name this machine. Those two facts together once meant that an operator
who pointed this provider at a remote inference host shipped their real key to
that host as a bearer token, silently. The adapter now refuses to attach a
credential to any destination that is not this machine, before the request
reaches the network — `TestABorrowedCredentialIsNotSentOffThisMachine` in
`manvi/llm/local` is that rule, and the refusal names the variable it declined
to send without quoting its value.

So `local` is the adapter for a server on this machine. Relaxing the destination
check to reach a hosted endpoint would trade a class of silent credential
exfiltration for the convenience of not writing an adapter, which is the wrong
side of that trade at any price.

### What a hosted provider therefore costs

A first-class adapter, with its own credential requirement registered in
`manvi/credentials`, its own `Capability` table (context window, output cap,
tool support, and the *ordered* reasoning tiers the loop escalates through), and
its own entry in `buildProvider`. On the OpenAI wire most of the body is
`openaicompat` already, so the work is small — but it is not zero, and it is not
the part that costs.

The part that costs is keeping it honest. `verify.sh` already ends every run by
reporting that `anthropic, gemini and xai are verified against scripted servers
only` — a gate that did not run, named as such. Each additional hosted provider
is one more wire whose real behaviour nothing in the gate observes, and the
`bench` rig's two wire suites (`test_gemini_wire.py`, `test_cerebras_wire.py`)
exist because a Gemini serialization defect produced a 315-episode arm with zero
`finished` stops before anything noticed. A provider list that grows faster than
that scrutiny is a list of adapters nobody is checking.

### What exists and what it is

- **`bench/` has a Cerebras client.** It is a benchmark instrument, not a
  harness provider. Promoting it means the paragraph above, in full.
- **`reference/deepseek-harness` is study material**, kept locally and
  `.gitignore`d. It is not a partially-landed adapter.

### If you want one of these models today

Serve it locally — through Ollama, vLLM, llama.cpp, LM Studio, or Jan — and the
`local` adapter drives it with no credential leaving the machine, which is the
case this harness is built for. A hosted endpoint needs the adapter, and the
adapter needs the scrutiny; a pull request that brings both is welcome, and one
that relaxes the destination check instead is not.

