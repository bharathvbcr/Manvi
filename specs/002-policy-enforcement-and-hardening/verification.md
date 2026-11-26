# Verification Plan: 002 - Policy Enforcement, Gusset/Hardening, and Computer-Use Privacy

## 1. Automated Verification Commands

1. **Specification Validation (Strict)**:
   ```bash
   devcouncil spec validate 002 --strict --project-root /Users/bharath/Code/devtools/Manvi
   ```
2. **Contract, Schema Parity & Residue Suite**:
   ```bash
   GOMAXPROCS=4 go -C manvi test ./internal/contract/...
   ```
3. **Policy Enforcement, Hardening & Privacy Suites**:
   ```bash
   GOMAXPROCS=4 go -C manvi test ./devcouncil/... ./serve/... ./credentials/... ./computer/...
   devmap doc-drift --json -b 10000 README.md docs specs
   ```

## 2. Acceptance Proof Matrix

| Story | Criterion | Proof Artifact / Test |
| --- | --- | --- |
| US1 | `authorisingTask`, containment, `commitscan`, `refguard` | `manvi/devcouncil/containment_test.go`, `manvi/devcouncil/commitscan_listing_test.go`, `manvi/devcouncil/zsec_refguard_test.go`, `manvi/devcouncil/zz_adv_b28_refguard_test.go` |
| US2 | `SC-001`, `SC-004`, `verdict.schema.json` | `manvi/internal/contract/contract_test.go`, `manvi/serve/gusset_policy_test.go`, `manvi/devcouncil/verdict_test.go` |
| US3 | `SC-002`, `SC-003`, `b27`–`b32` | `manvi/credentials/lockedmem.go` tests, `manvi/devcouncil/zz_advb31_test.go`, `manvi/devcouncil/advb29_define_adv_test.go`, `bench/stress_test.py` |
| US4 | `SC-003`, folded masking & `maskMemory` | `manvi/computer/` tests |
