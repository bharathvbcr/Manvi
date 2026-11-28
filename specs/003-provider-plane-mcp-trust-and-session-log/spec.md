# Feature: 003 - Model Provider Plane, Spend Ledger, MCP Trust, and Session Log

**Status**: Active Specification | **Canonical Owner**: Manvi (`github.com/bharathvbcr/Manvi`) | **Date**: 2026-11-28 | **Implementation State**: Shipped (`manvi/llm/`, `manvi/mcp/`, `manvi/session/`); no open work item is recorded for these packages (see `research.md`, "Open work").

## 1. Problem Statement & Motivation

`specs/001` covers the turn loop, tool registry, TUI and `manvi serve` plane; `specs/002` covers policy enforcement and computer-use privacy. Neither covers the three packages those layers stand on:

- `manvi/llm/` is the provider seam. Five adapters (`anthropic`, `gemini`, `openaicompat`, `local`, `xai`) plus a deterministic `replay` provider sit behind one `llm.Provider` interface, share one tool-call settlement rule, and can be wrapped by a spend ledger.
- `manvi/mcp/` spawns external MCP servers. A server declared in a checked-out tree runs a command from that tree, so spawning needs an authorization the tree cannot grant itself.
- `manvi/session/` is the append-only event log that the model's context is derived from.

Without a binding specification these three drift in the ways their own doc comments describe: three adapters each carrying their own "is this tool call complete" rule (`manvi/llm/settle.go` records that this had already happened), a spend cap that refunds a reservation because a context deadline elapsed, an MCP server started from a cloned repository with harness credentials in its environment, or a prompt section reaching the model with no log event behind it.

## 2. Canonical Ownership & Non-Overlap Boundary

| Layer / Capability | Canonical Owner | Manvi Role | Explicit Non-Goals / Delegations |
| --- | --- | --- | --- |
| **Provider seam, capability catalogue, tool-call settlement** | **Manvi** (`manvi/llm/provider.go`, `manvi/llm/settle.go`) | Owns `Provider`, `Registry.Resolve`, `Capability.Validate`, `Catalogue`, `SettleCall`. | Tier resolution and fallback chains (`llm.tier.*`) stay with `specs/001` (`manvi/cmd/manvi/tiers.go`). |
| **Provider adapters and wire decoding** | **Manvi** (`manvi/llm/{anthropic,gemini,openaicompat,local,xai}/`, `manvi/llm/transport/`) | Decodes each wire to the shared `llm.Chunk`/`llm.Response` types; a stream with no terminal event is a retryable `TruncatedStream`, not a response. | No billing or account logic: Manvi stays free with zero sign-in (`docs/PHILOSOPHY.md`, principle 5). |
| **Local server discovery** | **Manvi** (`manvi/llm/local/{discover,endpoints}.go`) | Probes the loopback default listen addresses of Ollama (`11434`), LM Studio (`1234`) and `8080` servers; an ambiguous result is reported, never guessed. | Model weights and runtimes belong to the operator. |
| **Spend ledger** | **Manvi** (`manvi/llm/budget/`) | Reserves a conservative bound before every HTTP attempt, settles from authoritative provider usage, and keeps the reservation when the outcome is unprovable. | Ledger governs its own tally, not the provider's account balance; spend made outside it is invisible. Team rollups of `CostNanoUSD` belong to the host (`specs/001` T-010). |
| **MCP server trust and spawn** | **Manvi** (`manvi/mcp/manager.go`, `manvi/mcp/client.go`, `manvi/mcp/plugin.go`) | Fingerprints each declaration and refuses to spawn a workspace-origin server that is not authorized in a file outside the repository. | Does not pin the contents of the program a declaration names (`Fingerprint` doc comment). |
| **Session event log and store** | **Manvi** (`manvi/session/{log,store,invariant,privacy}.go`) | Owns the append-only `Log`, the projection derived from it, the checksummed generation files, and `AssertModelVisible`. | The evidence chain for DevCouncil tasks is a projection of this log; DevCouncil owns its own stores (`specs/001`, Article II). |

## 3. User Stories & Acceptance Scenarios

### US1: One Provider Seam With Assembly-Time Validation (Priority: P1)

As a harness author, I resolve a provider and model once and get a refusal that names exactly what is wrong, so an impossible request fails before it is sent instead of as a 400 halfway through a turn.

- **Scenario 1: Unknown or incapable model is refused at assembly**
  - **Given** a `llm.Registry` with registered providers (`manvi/llm/provider.go`)
  - **When** `Registry.Resolve` is called with an unregistered provider, a model the adapter does not serve, or a request the model's `Capability` cannot satisfy (tools on a non-tool model, an effort level outside `EffortLevels`, `MaxTokens` above `MaxOutputTokens`, an image on a non-image model)
  - **Then** it returns an error naming the provider, model and violated capability, and an adapter never answers an unknown model with a permissive default.
- **Scenario 2: Duplicate provider names are rejected**
  - **Given** a registry that already holds a provider named `anthropic`
  - **When** a second provider with the same `Name()` is registered
  - **Then** `Register` returns an error rather than replacing the first, because provenance on replayed history depends on the name being unambiguous.

### US2: A Tool Call Is Dispatched Only When It Is Complete (Priority: P1)

As an operator, a half-received tool call never runs, whichever adapter decoded it.

- **Scenario 1: Single settlement rule across adapters**
  - **Given** a decoded call with a name, an id, accumulated argument bytes and the wire's framing (`CallFraming`: required, optional or none)
  - **When** the stream ends and the adapter calls `SettleCall` (`manvi/llm/settle.go`)
  - **Then** the verdict is `CallComplete` only when the call can be routed and answered, its arguments are a whole JSON object, and the wire's framing says it is over; otherwise it is `CallCutByLimit` (output limit ended it) or `CallUnusable`, and neither dispatches.
- **Scenario 2: A stream without its terminal event is not a response**
  - **Given** a connection that ends cleanly but without the provider wire's terminal event
  - **When** a decoder reaches end of stream (`manvi/llm/transport/streamguard.go`)
  - **Then** it reports `TruncatedStream`, a retryable failure, and records no usage and runs no tool call from it.

### US3: Spend Is Capped Across Models and Concurrent Sessions (Priority: P1)

As an account owner, a provider's spend cannot exceed the configured limit even when several sessions share one ledger file, and an uncertain charge counts against the limit.

- **Scenario 1: Reserve before send, refund only on proof**
  - **Given** a `budget.Provider` wrapping an inner provider with a `budget.Ledger` (`manvi/llm/budget/provider.go`)
  - **When** an HTTP attempt is admitted
  - **Then** a conservative reservation is recorded first, is reduced only by proof that nothing was sent or by authoritative provider usage, and is kept for a lost stream, a rejected or unclassifiable attempt, or a crash; a refused admission returns `ErrExhausted`.
- **Scenario 2: Headroom counts what is in flight**
  - **Given** a reservation held by another session on the same ledger file
  - **When** `Ledger.Headroom` is read
  - **Then** it equals the limit less everything committed (spent, retained and in flight), and `Provider.Estimate` equals the number the gate later reserves.
- **Scenario 3: A dead owner's reservations are reclaimed**
  - **Given** in-flight attempts whose owning process no longer exists
  - **When** `Ledger.Reconcile` runs
  - **Then** attempts are judged by the random `Owner` token and `OwnerStart` rather than the pid alone, so a reused pid does not keep another process's record alive.

### US4: Local Servers Are Discovered, Not Guessed (Priority: P2)

As an operator running Ollama, LM Studio, vLLM, llama.cpp or an MLX server, `manvi local` finds the one server that answers, and reports every candidate when more than one does.

- **Scenario 1: Unambiguous resolves, ambiguous is reported**
  - **Given** one or several loopback servers answering on the documented default ports (`manvi/llm/local/endpoints.go`)
  - **When** discovery runs (`manvi/llm/local/discover.go`)
  - **Then** a single answering server is resolved with its runtime identified, and multiple answering servers are reported with the setting (`MANVI_LLM_LOCAL_BASE_URL`) that selects one.

### US5: MCP Servers From a Checked-Out Tree Need Authorization (Priority: P1)

As a developer who clones untrusted repositories, an MCP server declared inside a clone does not spawn until I authorize that exact declaration outside the repository.

- **Scenario 1: Workspace-origin declaration is refused until authorized**
  - **Given** a server declared in a file under the tree (`OriginWorkspace`) and no matching fingerprint in `mcp-trust.json`, `MANVI_MCP_TRUST_FILE` or `MANVI_MCP_TRUST`
  - **When** the `Manager` is asked to start it (`Manager.authorize`)
  - **Then** the spawn is refused with an `authorizationError` describing the invocation, and nothing from the tree can authorize itself.
- **Scenario 2: Changing what runs invalidates the authorization**
  - **Given** an authorized declaration
  - **When** its command, arguments, declared environment or forwarded variables change
  - **Then** `Fingerprint` changes and the declaration must be authorized again; a program-origin declaration (`OriginProgram`, registered through the Go API) is not subject to the file-based check.

### US6: Everything the Model Saw Is Reconstructable From the Log (Priority: P1)

As an auditor, I can show that every message in a model request is backed by a logged event, and a resumed session is exactly the saved one.

- **Scenario 1: Injected content is named**
  - **Given** a `session.Log` and a request message list
  - **When** `AssertModelVisible` compares them (`manvi/session/invariant.go`)
  - **Then** a message present in the request but not the log returns an `InvariantError` naming the message index and carrying the injected message.
- **Scenario 2: A damaged session file never loads as an empty one**
  - **Given** a saved generation file that is truncated or altered in place
  - **When** `Store.Load` reads it (`manvi/session/store.go`)
  - **Then** it returns an error naming the file (checksum covers the exact encoded event bytes) and never an empty log; concurrent saves of one session cannot overwrite each other because a generation is linked into place with `os.Link`.

## 4. Success Metrics & Testability Criteria

- **SC-001**: `GOMAXPROCS=4 go -C manvi test -count=1 ./llm/...` passes for all eleven `llm` packages (run 2026-11-28 on the dirty `main` checkout: every package `ok`, `transporttest` has no test files).
- **SC-002**: `GOMAXPROCS=4 go -C manvi test -count=1 -p 2 ./mcp/... ./session/...` passes (run 2026-11-28: `mcp` ok in 79s, `session` ok in 19s).
- **SC-003**: `go -C manvi test ./llm -run 'SettleCall|Catalogue' -count=1` passes the shared settlement and catalogue tests in `manvi/llm/settle_test.go` and `manvi/llm/catalogue_test.go`.
- **SC-004**: `manvi/llm/adaptertest/settlement_conformance_test.go` and `truncation_conformance_test.go` run the same settlement and truncation cases against each adapter, so a rule cannot diverge per adapter.
- **SC-005**: `devcouncil spec validate 003-provider-plane-mcp-trust-and-session-log --strict --project-root /Users/bharath/Code/devtools/Manvi` passes with zero findings, and `package.json` exposes `check:specs`.

## 5. Edge Cases, Exclusions & Open Questions

- **Exclusions**:
  - `manvi/workflow/`, `manvi/lessons/`, `manvi/agents/`, `manvi/codingagent/`, `manvi/fetch/`, `manvi/artifacts/`, `manvi/bootstrap/` and `manvi/harness/` are not covered by any spec (`research.md` lists them); this spec does not claim them.
  - The ledger is a tally, not the provider's balance: spend from outside this ledger is invisible to it (`manvi/llm/budget/budget.go` package doc).
  - A fingerprint does not pin the contents of the program it names (`manvi/mcp/manager.go`, `Fingerprint`).
- **Edge Cases**:
  - A resolve write that times out leaves the reservation in place (`resolveTimeout = 30s`), never refunds it.
  - The ledger file is bounded: `maxInFlight = 256` unresolved attempts and `maxLedgerBytes = 1 << 20`.
- **Open Questions**: None.
