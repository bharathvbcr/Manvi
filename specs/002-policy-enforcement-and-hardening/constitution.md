# Constitution: 002 - Policy Enforcement, Gusset/Hardening, and Computer-Use Privacy

Every enforcement component under `manvi/devcouncil/`, `manvi/serve/`, `manvi/credentials/`, `manvi/computer/`, and `manvi/cmd/manvi/` obeys these invariants:

## Article I: Fail-Closed Security & Explicit Degradation
Missing pre-exec snapshots, over-cap scans (`refguard.go`, `commitscan.go`, `containment.go`), malformed checker/worker RPC messages (`b32-benchverify`), or unreadable secrets MUST fail closed (`ruleRefUnscanned`, hard block, or named `degraded` note), never as clean passes.

## Article II: Single Source of Policy & Schema Truth
`policy.Decision`, `ruleId`, `severityByRule`, and `contracts/verdict.schema.json` remain canonically owned by DevCouncil. Manvi MUST NOT weaken `hard` severities, diverge from `verdict.schema.json`, or alter schema wire fields without cross-repo synchronization across DevCouncil, Manvi, and Waylode.

## Article III: Preview-to-Execution Parity
`devcouncil_policy_check_write`, `manvi check`, and actual tool writes MUST evaluate against the identical `authorisingTask` and gate rules so pre-flight previews never mislead operators or models.

## Article IV: Host-Supplied Privacy Boundaries
`PrivacyPolicy` in `manvi/computer/` is supplied strictly by the host and enriched by workflow metadata (`EffectivePolicy`). Model tool arguments MUST NEVER inspect, override, or narrow watched secrets or masked controls.

## Article V: No Secret Echoes in Evidence
Credential scanners (`commitscan.go`, `refguard.go`, `rigor.go`, `ui.Sanitize`) report paths, rule IDs, shapes, and lengths—never raw secret values—into logs, evidence trails, or tool payloads.
