# Constitution: 001 - Agent Harness, Tool Registry, and `manvi serve` Host Plane

Every change under `manvi/agent/`, `manvi/cmd/manvi/`, `manvi/devcouncil/`, `manvi/serve/`, and `manvi/ui/` obeys these non-negotiable articles:

## Article I: Truthful Verification & Honest Degradation
A check that timed out, exceeded capture caps, or lacked `dcverify` / `devmap` MUST report `degraded` or error (`PathReport.Verdict = "degraded"`, `VerifyRunner` `VerifyDegraded`, or `okDegraded`). Empty findings never masquerade as a clean pass when inputs were incomplete.

## Article II: Single Source of Engine Truth (Anti-Overlap)
Manvi MUST NOT duplicate `dcstore` persistence, `dcverify` stub/secret/coverage parsing, `devmap` graph queries, or DevCouncil policy rule tables in Go. Manvi owns orchestration, transport, and runtime enforcement around those engines.

## Article III: Bounded Resources & Correlated Wire Errors
All subprocess captures (`rigorClient`, `gitDiff`, `listChanges`), token-window inputs (`maxTokenCount = 1 << 24`), active background verification jobs (`verifyMaxActive = 4`), and `devcouncil_debate` invocations (`2`–`4` debaters, max `3` rounds) are strictly bounded. `manvi serve` emits errors as `ok: false` JSON responses rather than terminating shared host connections.

## Article IV: Explicit Security Boundaries
Host-only `work.*` operations and `WorkBinding`-scoped `work_*` agent tools remain separate from managed external subprocess CLIs (Codex/Claude), which receive no in-process tool channel back to `manvi/devcouncil/work_tools.go`.

## Article V: Per-Server Extensibility & Fail-Closed Boot
`serve.Router` is per-`Server` and frozen prior to serving; `configureModule` traps module panics before protocol input is read, and duplicate operation registrations fail unless `Router.Replace` is invoked explicitly.
