# Research & Evidence Audit: 003 - Model Provider Plane, Spend Ledger, MCP Trust, and Session Log

**Date**: 2026-11-28 | **Audit Scope**: `manvi/llm/`, `manvi/mcp/`, `manvi/session/` as read in the working tree of `main` (HEAD `785d9883`, 106 uncommitted files from other sessions).

## 1. Evidence Observed

1. **Tests run in this session** (verified): `GOMAXPROCS=4 go -C manvi test -count=1 ./llm/...` all `ok`; `go -C manvi test -count=1 -p 2 ./mcp/... ./session/...` both `ok`; `go -C manvi test -count=1 ./internal/contract/...` `ok` (baseline before this spec was added).
2. **Settlement was once triplicated** (verified from the doc comment on `SettleCall`, `manvi/llm/settle.go`): three adapters each carried their own rule and had drifted (a bare JSON string accepted as arguments, a call with no id accepted, a named call with no argument bytes dispatched as `{}`). The single `SettleCall` plus `manvi/llm/adaptertest/settlement_conformance_test.go` is the closure.
3. **Ledger semantics** (verified by reading `manvi/llm/budget/budget.go`): schema version `2`, `maxInFlight = 256`, `maxLedgerBytes = 1 << 20`, `resolveTimeout = 30s`, states `reserved` and `accepted`, `ErrExhausted`. `Snapshot` fields are in `data-model.md`.
4. **MCP trust** (verified by reading `manvi/mcp/manager.go`): `TrustFileEnv = MANVI_MCP_TRUST_FILE`, `TrustListEnv = MANVI_MCP_TRUST`, default file `mcp-trust.json` in the user config dir, `OriginProgram` vs `OriginWorkspace`. The test `TestAWorkspaceDeclaredServerIsNotSpawnedWithoutAuthorization` is in `manvi/mcp/zfix_mcp_test.go`.
5. **Session store layout** (verified from the doc comment in `manvi/session/store.go`): `<dir>/<id>.<generation>.json`, header `{"manvi_session":1,...,"checksum":"sha256:..."}`, written to a temporary file, synced, then linked with `os.Link`.

## 2. Open work

No open work item is recorded for these packages: `docs/KNOWN_GAPS.md` has no entry for `llm`, `mcp` or `session`, and `BRANCH_REPORT*.md` / `BUILD_REPORT.md` name none. Every task in `tasks.md` is therefore a shipped capability. The only uncommitted activity that touches cited files is another session's edit of `manvi/llm/local/{adapter,capability,endpoints}.go` and `manvi/mcp/plugin.go` (also present in the `fix/tui-slash-audit` worktree for the `local` files); this spec describes them as read, and they were not edited.

## 3. Packages still uncovered by any spec

`manvi/workflow/` (compiler and engine, used by Jarvis), `manvi/lessons/`, `manvi/agents/` (roster, placement, tiers), `manvi/codingagent/` (managed Claude/Codex/agy seats), `manvi/fetch/`, `manvi/artifacts/`, `manvi/bootstrap/`, `manvi/harness/`, `manvi/gussetcheck/`, `manvi/gorules/`, `native/` and `crates/` (Rust). `specs/001` mentions tier resolution through `manvi/cmd/manvi/tiers.go` but not `manvi/agents/`. A future spec could take `manvi/workflow/` plus `manvi/codingagent/`.

## 4. Unverified

- No live provider was called: every adapter result above comes from fixtures and local `httptest` servers in the tests.
- Windows and Linux behaviour of the ledger lock and `procstart_*` files was not run (this host is macOS).
- `devmap` was not used to compute callers for the cited symbols; the Manvi index was fresh (generation 861) but `waylode_insights` reports it only as available. File and symbol citations were checked with `test -e` and by reading source.
- The adapters' per-model price tables were not audited for current accuracy.
