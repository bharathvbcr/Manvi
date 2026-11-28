# Verification Plan: 003 - Model Provider Plane, Spend Ledger, MCP Trust, and Session Log

## 1. Automated Verification Commands

1. **Specification validation (strict)**:
   ```bash
   devcouncil spec validate 003-provider-plane-mcp-trust-and-session-log --strict --json --project-root /Users/bharath/Code/devtools/Manvi
   devcouncil spec validate --all --strict
   ```
2. **Provider plane, MCP and session suites**:
   ```bash
   GOMAXPROCS=4 go -C manvi test -count=1 ./llm/...
   GOMAXPROCS=4 go -C manvi test -count=1 -p 2 ./mcp/... ./session/...
   ```
3. **Documentation and contract gate** (baseline `ok` before this spec was added):
   ```bash
   GOMAXPROCS=4 go -C manvi test -count=1 ./internal/contract/...
   devmap doc-drift --json -b 10000 README.md docs specs
   ```

## 2. Acceptance Proof Matrix

| Story | Criterion | Proof Artifact / Test |
| --- | --- | --- |
| US1 | `SC-003` | `manvi/llm/catalogue_test.go` (`TestACatalogueRefusesAnUnknownModelNamingTheAdapterAndWhatItServes`), `manvi/llm/effortorder_test.go` |
| US2 | `SC-003`, `SC-004` | `manvi/llm/settle_test.go` (`TestSettleCallIsOneRuleForWhatACompleteCallIs`), `manvi/llm/adaptertest/settlement_conformance_test.go`, `manvi/llm/adaptertest/truncation_conformance_test.go` |
| US3 | `SC-001` | `manvi/llm/budget/budget_test.go`, `manvi/llm/budget/admission_test.go`, `manvi/llm/budget/reclaim_test.go`, `manvi/llm/budget/adv_b31_settlement_test.go` |
| US4 | `SC-001` | `manvi/llm/local/discover_test.go`, `manvi/llm/local/endpoints_test.go` |
| US5 | `SC-002` | `manvi/mcp/zfix_mcp_test.go` (`TestAWorkspaceDeclaredServerIsNotSpawnedWithoutAuthorization`), `manvi/mcp/pathinside_test.go` |
| US6 | `SC-002` | `manvi/session/invariant_test.go`, `manvi/session/store_test.go`, `manvi/session/crash_test.go` |

## 3. Result Log

- 2026-11-28: `./llm/...`, `./mcp/...`, `./session/...` and `./internal/contract/...` all `ok` on the dirty `main` working tree.
- Not run: live-provider calls, Linux/Windows ledger lock and `procstart_*` paths.
