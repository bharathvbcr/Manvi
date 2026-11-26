# Verification Plan: 001 - Agent Harness, Tool Registry, and `manvi serve` Host Plane

## 1. Automated Verification Commands

1. **Specification Validation (Strict)**:
   ```bash
   devcouncil spec validate 001 --strict --project-root /Users/bharath/Code/devtools/Manvi
   ```
2. **Documentation & Contract Gate**:
   ```bash
   GOMAXPROCS=4 go -C manvi test ./internal/contract/...
   go -C manvi test ./devcouncil -run TestDocumentedToolCountMatchesRegistry -count=1
   devmap doc-drift --json -b 10000 README.md docs specs
   ```
3. **Runtime Harness & Host Plane Suites**:
   ```bash
   GOMAXPROCS=4 go -C manvi test ./agent/... ./devcouncil/... ./serve/... ./ui/...
   ```

## 2. Acceptance Proof Matrix

| Story | Criterion | Proof Artifact / Test |
| --- | --- | --- |
| US1 | `SC-001`, `TurnStopping`, `llm.tier.*` | `manvi/devcouncil/tierplacement_test.go`, `manvi/devcouncil/teamplacement_test.go`, `manvi/agent/` tests |
| US2 | `SC-001`, `SC-004`, `devcouncil_debate` | `manvi/devcouncil/toolcount_docs_test.go`, `manvi/devcouncil/debate_test.go`, `manvi/devcouncil/verify_paths_test.go` |
| US3 | `SC-003`, `VerifyRunner`, `Module` | `manvi/serve/protocol_cap_contract_test.go`, `manvi/serve/module_test.go`, `manvi/serve/verify_test.go`, `manvi/serve/contract_test.go` |
| US4 | `SC-002`, TUI sanitization | `manvi/ui/` tests, `manvi/internal/contract/docs_test.go` (`TestMermaidDiagramsAreWellFormed`) |
