# Data Model: 002 - Policy Enforcement, Gusset/Hardening, and Computer-Use Privacy

**Date**: 2026-11-26 | **Source Verification**: `contracts/verdict.schema.json`, `manvi/devcouncil/{policy_tools,containment,commitscan,refguard}.go`, `manvi/credentials/lockedmem.go`, `manvi/computer/{privacy,mask,runner}.go`, `manvi/cmd/manvi/main.go`.

## 1. Wire Verdict Classification (`contracts/verdict.schema.json`)

| Property / Type | Values / Rules | Evaluation Invariants |
| --- | --- | --- |
| `action` | `allow`, `warn`, `deny` | Raw gate decision. `allow` alone is insufficient; consumers must check `grant_id`, `demoted`, `widened`, `degraded`. |
| `severity` | `hard`, `soft`, `warn`, `none` | Unknown rules default to `hard` (`severityByRule`). Only `soft` rules may be demoted or granted. |
| `classification` | `unchecked`, `denied`, `granted`, `demoted`, `widened`, `degraded`, `warned`, `clean` | Total precedence order (`0` no verdict -> `unchecked`; `1` `action==deny` -> `denied`; `2` `grant_id!=""` -> `granted`; `3` `demoted!=""` -> `demoted`; `4` `widened!=""` -> `widened`; `5` `degraded` non-empty -> `degraded`; `6` `action==warn` -> `warned`; `7` else -> `clean`). |

## 2. Runtime Safety Bounds & Guards

| Component | Bounds / Exit Codes | Fail-Closed Invariant |
| --- | --- | --- |
| `manvi check` (`cmd/manvi/main.go`) | Exit `0` (not blocked), `6` (soft block clearable via `manvi allow`), `7` (hard block), `1` (failure) | Exit `0` never masks a soft or hard refusal; `7` never prints a grant command. |
| `containment.go` (`/usr/bin/sandbox-exec`) | `maxContainmentWalk = 2,000,000`, `maxProfileBytes = 262,144` (`256 KiB`) | Dangling symlink denies link path; live symlink denies resolved target; exceeding walk/profile bounds refuses command execution rather than running uncontained. |
| `commitscan.go` | `1 GiB` per-staged-file stream budget | Unlistable, unhashable, timed-out, or over-`1 GiB` staged blobs block the commit cleanly; unreadable untracked directories emit `scanNote` in `degraded`. |
| `refguard.go` | Pre/post OID snapshot across refs, `refs/stash`, detached `HEAD` | Missing pre-exec snapshot blocks command; leaking commit rolls back via CAS `git update-ref`; unscannable commit sets block as `ruleRefUnscanned`. |
| `lockedmem.go` | `memguardBound = 10s`, `ErrLockedMemory` | Pre-`mlock` probe prevents process hangs when `RLIMIT_MEMLOCK` blocks allocation. |

## 3. Computer-Use Privacy Structures (`manvi/computer/`)

| Entity | Fields / Constants | Invariants |
| --- | --- | --- |
| `PrivacyPolicy` (`privacy.go`) | `PID`, `WindowID`, `SensitiveTargets` (`[]Selector`), `Watched` (`[]string`), `memory` (`*maskMemory`) | Host-supplied only; `EffectivePolicy` merges controls written by sensitive `set_value` / `type_text` workflow steps (`sensitiveInputTargets`). |
| `secretMatcher` (`mask.go`) | `minFoldedSecretLen = 6`, `maxSplitSiblings = 8`, `maxSplitBytes = 4096`, `maxRememberedMasks = 256`, `rememberedOverlap = 0.5` | `foldSensitive` strips whitespace, separator punctuation (`.,/\·•`, Unicode `Cf`/`Z`/`Pd`/`Pc`), folds full-width ASCII (`0xFF01..0xFF5E`), and lowercases before matching. |
