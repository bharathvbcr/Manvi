# Changelog

What changed, per release, for someone deciding whether to upgrade.

This is not the [hardening ledger](docs/HARDENING_LEDGER.md). That document is
organised by *defect* — the pattern, its root cause, and the regression test
that now holds it — and it is the better read for understanding why a rule
exists. This one is organised by *release*, and answers a different question:
what is different in the build I am about to run.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions are `MAJOR.MINOR.PATCH`; while the leading zero stands, the surface —
CLI flags, exit statuses, the settings catalogue, the `manvi serve` protocol,
and the native tool schemas — may change in any release, and a change to any of
them is listed here.

Entries are written with the change, not reconstructed afterwards. Add yours
under **Unreleased** in the same commit as the code.

---

## [Unreleased]

### Added

- `work.enhancements.refine`: a host-only operation that proposes acceptance
  criteria, `split_into` parts and `duplicate_of` links for one task, grounded
  in DevMap answers for the symbols the task names and in the open board. It
  takes `id`, `request_id` and `expected_revision`, runs one model call on the
  host's selected provider and model (not a managed coding agent, and not
  callable by an agent), and returns a reviewable proposal. It writes nothing:
  `applied` is always false. The reply must be one JSON object with exactly
  `acceptance_criteria`, `split_into`, `duplicate_of` (and optionally
  `rationale`); anything else is refused whole. A `duplicate_of` must name a
  live, open item in the task's repository and a split must carry every
  protected literal (error codes, paths, quoted text) of the parent, or that
  proposal is dropped and listed in `dropped` with its reason. Each proposal
  cites the DevMap answers it used, with `truncated` and `walk_incomplete`; an
  unbuilt or unavailable index gives `grounding.status: "unavailable"` and no
  citation. A run is capped at 12 proposals, 10 acceptance criteria and 4
  splits (the board growth), and is refused, not truncated, over a cap. The
  same `request_id` replays its result without a second model call, and a
  second refine of the same task while one runs is refused as busy.
- `work.scan.gaps`, `work.scan.get` and `work.scan.cancel` on the `manvi serve`
  workbench plane: a deterministic audit (no model call) of an explicitly named
  repository root, which must be an absolute, symlink-free git work-tree top. It
  claims the scan, answers at once and runs on its own context under a deadline,
  so it never holds the 60 s lane request. It proposes inbox tasks for dead
  symbols (DevMap high-confidence `dead` plus no reference in an ignore-bypassing
  text search, both stated, exempt and registry-looking symbols never proposed),
  changed-since-`since` files no test reaches, open dc-store verification gaps
  and stale paths named in docs. An unbuilt, degraded or stale index is refused
  with a message naming `devmap status`; every category reports `truncated`,
  `walk_incomplete`, `degraded` or `unavailable` rather than an empty "no gaps".
  Proposals carry a stable fingerprint (as a `scan:<fp>` label and a
  `manvi-scan:` line in the description); a finding an open board item already
  carries is reported `attached`, a repeat of an earlier scan `known`, and the
  scan never writes the board. A per-scan proposal cap and concurrent scans of
  one root sharing one job bound the work.
- `work.verify.task` and `work.verify.get` (host plane, enabled with
  `--workbench-db`): an evidence-based completion verdict for a board task,
  `passed`, `failed` or `degraded`, never on the agent's word. The changed
  paths come from git (untracked files included); the rigor gates and the
  operator's `MANVI_VERIFY_COMMAND` run over them; DevMap lists the tests that
  reach the change, recording `truncated` and `walk_incomplete`; and a read-only
  `critic` scores each acceptance criterion read from the stored item, one
  `MANVI-VERDICT[n]:` line per criterion. A stale or unbuilt index, a missing
  `dcverify`, an empty or capped change set, a critic with no or conflicting
  verdict lines, and any check that could not run all degrade the verdict, and a
  criterion the critic did not address is unmet. The op claims durably, answers
  at once and runs on its own job context; a retry with the same `request_id`
  returns the same record without re-running the critic. The record is stored as
  dc-store evidence (type `work.verify.task`) with a verification-run row. The
  request carries no command and no path list.
- The verdict contract now lives in `manvi/devcouncil` (`ParseVerdict`,
  `VerdictInstruction`, and the per-criterion `ParseCriterionVerdicts`) instead
  of the `manvi` command package, so the serve host and the turn sensor read one
  contract.

- `contracts/serve.ops.schema.json`: the `manvi serve` ops and the shapes of
  hello, file and command checks, host scope and managed-run results, generated
  from the Go types. A Go test fails when the file and the types disagree
  (`MANVI_UPDATE_CONTRACT=1` regenerates it), so a host that mirrors the file
  can fail its own build when it sends a field Manvi does not read.

- An account-wide Anthropic spend cap. Set `MANVI_ANTHROPIC_BUDGET_USD=200`
  and every Anthropic request, from any project or any concurrent session,
  draws from one ledger. By default that ledger is
  `<user config dir>/manvi/budget/anthropic.json`; `MANVI_ANTHROPIC_BUDGET_LEDGER`
  overrides it. A request whose worst-case cost would pass the limit is
  refused before it is sent. `manvi budget`, and `budget` in the TUI, show
  spent, retained, in-flight and remaining. A cap that is set but unusable (a
  bad amount, a relative path, a corrupt ledger) stops the provider from
  starting rather than letting it run unmetered.
  - While a cap is set, managed Claude agents started by `manvi serve` do not
    inherit `ANTHROPIC_API_KEY`. They are separate processes that call the API
    themselves, so with the key they could spend from the account without the
    ledger recording it. They fall back to their own login instead.
  - Rejections Anthropic documents as refusals of the request itself (400,
    401, 402, 403, 404, 413, 429, 529) release their reservation. 500, 504 and
    lost connections keep it.
  - A turn cut short after `message_start` is retained at its reported prompt
    cost plus the full output allowance. It used to keep its whole
    byte-sized reservation.
- Claude Opus 5.5, Sonnet 5.5, Haiku 5.5 and Fable 5.1 in the Anthropic
  catalogue. `manvi probe anthropic` now uses Haiku 5.5.
- `manvi budget reconcile` retains the reservations of attempts whose process
  exited before resolving them. Every reservation does the same first, so a
  crashed fan-out no longer holds in-flight slots until every later request
  fails at the 256-attempt bound. Reclaimed attempts stay counted against the
  limit.

### Changed

- Pins move to DevCouncil `acfaa229` (v0.2.4 line) and gusset `23e925b`. DevCouncil's
  read-only command allowlist now judges each command's arguments, so
  `git diff --output=`, `git diff --no-index` outside the root and pytest's
  `--junitxml`/`--basetemp` no longer pass as read-only. `policy.ReadRefused`
  is gone upstream; `grep`'s secret-path filter asks `policy.ClassifyRead` and
  withholds any path it does not class as readable, as before.

- The end-of-turn check no longer reports a clean pass for a turn that deleted
  files: a path that is gone is recorded as omitted and a would-be `passed`
  becomes `degraded` (`devcouncil.VerifyPresent`, shared with `work.verify.task`).
- Anthropic requests use automatic prompt caching (top-level
  `cache_control: ephemeral`). An agent loop now reads the history it already
  sent instead of paying full input price for it on every turn.
- An Anthropic stream that ends before `message_stop` is an error, not a
  response. A connection closed mid-message used to produce a truncated
  response that looked complete, with no output tokens in its usage.
- Haiku 4.5 requests no longer send `thinking: {type: "adaptive"}`, which
  that model rejects with a 400.
- Anthropic usage reports `InputTokens` as the whole prompt, including cached
  tokens, as the OpenAI-compatible and Gemini adapters already did. It was the
  uncached remainder alone.
- `llm/budget` is now an account ledger rather than a single-run campaign.
  - Prices are per model and cache-aware.
  - Reservations are sized from the request.
  - Settled spend folds into totals, so the file no longer grows per request
    (it used to stop at 4096 attempts).
  - A per-operation `flock` replaces the exclusive lock, so many sessions can
    share one limit.
  - The 60-second deadline the wrapper put on every stream is gone; it would
    have cut off long reasoning turns.

- Policy decisions use Go's fnmatch in every build, not the Gusset engine.
  This covers `manvi` CLI gates and `serve`'s `policy.check.file` and
  `policy.check.command`. The answers are the same, since the engine was held
  equal to fnmatch, and fnmatch gives them about twice as fast (DevCouncil's
  `docs/gusset-candidates.md` has the A/B). `serve` no longer refuses a policy
  check with `E_INTERNAL` when the engine fails its check, and no decision
  comes back as `path.engine_unavailable` or `command.engine_unavailable`.
  Those refusals guarded an engine that no longer decides anything. Release
  binaries still link the engine, and `manvi gusset-check` still proves it.
  `gussetcheck.Matcher` and `gussetcheck.Ready` are removed.
- The DevCouncil pin moves to `ad5c941c` (DevCouncil v0.2.4 line, by way of
  `a29a11b`), which carries `dc-redact`, the substitution ruling the `serve`
  policy tests pin, and dc-store workbench schema 11, so a release build no
  longer refuses a profile a current DevCouncil or GitPulse has upgraded. The
  `dc-*` crates built here now report DevCouncil's version, 0.2.4.
- **`contracts/` is checked against DevCouncil at the pin.**
  `scripts/check-release.mjs` compares every contract file with DevCouncil's
  `backend/contracts` at the commit `scripts/module-pins.txt` pins, and
  `contracts/CHECKSUMS` with this copy's own files; it used to check only that
  the copy agreed with itself. The mirror is refreshed to `ad5c941c`:
  `verdict_json` is described as the `PolicyVerdict` projection the ledger
  stores, and the owner paths name DevCouncil's sources. The unused
  `contracts/tools/checksums.py` is removed, as it was upstream.
- A managed attempt's one turn now opens with what the launcher alone knows,
  ahead of the store's brief: the turn is unattended; an `inspect` attempt is
  told not to modify files or change the task's status, and every other mode
  is told when to mark the task done with `gitpulse_complete_task`; nothing is
  published on the user's behalf. A Claude session is also told to read the
  repository's AGENTS.md and CLAUDE.md itself. It runs with
  `--setting-sources user`, which leaves the project CLAUDE.md out of its
  context (measured on Claude Code 2.1.289). How to work comes from the brief's
  own `## Agent guidance` section (DevCouncil dc-store), which the turn carries
  unchanged; a brief without that section is not described as having it.
- A managed Codex run that ends on a server request the adapter does not answer
  now records `unsupported Codex callback "<method>"; no response was granted`
  as its reason. MCP elicitation, dynamic tool calls, account token refresh,
  attestation and the legacy `applyPatchApproval` / `execCommandApproval`
  requests carry no `itemId` or no `threadId` (codex-cli 0.153.4 schema), so
  they used to fail the turn-identity check first and the run read only
  "Codex callback has no matching active turn". The callback is still refused,
  and nothing reaches the decision queue.
- Preparation bounds the composed turn at 128 KiB, not the brief alone. A brief
  just under the limit used to be claimed and its provider started, then
  refused by the adapter when the turn was sent.
- The opt-in that lets cross-boundary tests skip when a toolchain is missing is
  renamed `MANVI_TEST_ALLOW_SKIP` → `DEVCOUNCIL_TEST_ALLOW_SKIP`, since the
  helper is DevCouncil's `testsupport` (GAP-19). The old name no longer opts
  anything in: a test that reaches a missing prerequisite with it set fails and
  names the new variable, and `verify.sh` refuses either name. The build lock
  and stable binary copies move to `.devcouncil-testbin.lock` and
  `target/devcouncil-testbin/`; an existing `target/manvi-testbin/` is no longer
  used and goes with the next `cargo clean`.

- The docs tooling (the mermaid grammar gate, jsdom) is provisioned with **bun**
  instead of npm: `bun.lock` replaces `package-lock.json`, `package.json` pins
  `packageManager: bun@1.4.2`, and `verify.sh` installs with
  `bun install --frozen-lockfile`. The parser still runs on node. Dependabot
  moves to the `bun` ecosystem and CI sets up bun in place of the npm cache.
  Nothing here reaches the `manvi` binary.
- `verify.sh` gains a `Docs tooling — supply chain` step running `bun audit`
  against `bun.lock`. GitHub's dependency graph does not read `bun.lock`, so
  Dependabot raises no alerts for it; this step is what catches a vulnerable
  package. Without bun the step reports `NOT COVERED` instead of passing.
- Docs: `ARCHITECTURE.md`, `README.md` and the docs index no longer claim a
  `CGO_ENABLED=0` / zero-dependency shipped binary, matching the v0.0.6 engine
  link. The visual architecture guide is rewritten with fuller descriptions of
  each plane, ladder rung, posture and lifecycle stage.

- **Long turns and long sessions no longer slow down with their own length.**
  Every agent step copied the whole conversation two or three times through a
  JSON round trip, so a turn's cost grew with the square of its steps; the
  copy is now structural (about 57× faster on a 300-message history) and the
  per-step invariant check no longer copies at all. What is sent to a
  provider is byte-for-byte unchanged, so prompt caching is unaffected. Other
  costs taken off the hot paths, none of which changes what is written,
  sent or shown:
  - a session save encodes each event once instead of three times;
  - a large tool result is encoded and scrubbed before the session log's
    lock is taken, not while every reader waits;
  - streamed lines are no longer copied an extra time, and MCP replies (up to
    16 MiB) are decoded once instead of twice;
  - clients of local model servers share one connection pool, so a
    capability probe reuses a warm connection instead of opening new ones and
    leaving the old to idle out;
  - `mcp_list_tools` starts its servers concurrently;
  - `grep` checks whether a file is a credential file once per file, not once
    per matching line;
  - a new session reuses the code graph already loaded unless the file changed;
  - the TUI lays out only the entries that changed, rather than the whole
    transcript, at most once per width per frame;
  - hosted providers keep up to 64 idle connections per host instead of
    `net/http`'s default of two, so a fan-out over HTTP/1.1 stops paying a
    fresh TLS handshake per sub-agent per step (a replaced
    `http.DefaultTransport` is still used as it stands);
  - a stream's 1 MiB read buffer is recycled once the stream ends rather than
    allocated per model call;
  - the Gemini adapter decodes each streamed event once instead of twice;
  - `get_diff` and verification render untracked files eight at a time
    instead of one after another (300 new files: ~840 ms → ~390 ms), with the
    same bytes, order, budget cuts and notes as before;
  - `find_files` skips a base-name match that cannot succeed;
  - the TUI renders a streaming reply incrementally (a long reply: ~5 s of
    layout and 2.4 GB of allocation → ~1 s and 51 MB), caps reasoning like
    tool results, and keeps each entry's lowercased text for search.
- **`manvi serve`: `capability.probe` reuses a successful answer** for a repeat
  probe with identical parameters for up to the local adapter's discovery TTL
  (60 s — the freshness the harness already accepts on its own request path).
  A changed base URL or any other changed parameter probes afresh; failures and
  declared-window fallbacks are never reused, and a "not served" answer from a
  reused probe is re-asked of the server before it is returned, so a retry
  after starting a server or pulling a model still sees the change. A repeated
  probe went from two to four HTTP round trips on the sidecar's dispatch loop
  to none.
- **The TUI draws at most 2,000 lines or 64 KiB of a tool result, a model's
  reasoning, an error or a harness message**, followed by one line saying how
  much was not drawn. A multi-megabyte `read_file` or
  command output used to become tens of thousands of rows, rebuilt on every
  resize, fold and theme switch. The entry keeps the whole result: search
  still finds text past the cut.
### Fixed

- `manvi serve`: stopping a managed run now ends it for every session, not only
  process-backed ones. `drive` ran on its own `Background` context, so a
  session that obeys only the context it is handed ignored `Stop` until the
  45-minute bound; it now derives from the job's context (the bound is kept).
  A run stopped while its provider was still starting is recorded
  `interrupted` rather than `unresolved`. `ManagedRunner.Stop` has tests for
  preparing, awaiting activation and running, double stop, stop after done and
  stop racing activation.
- `local.scan` no longer fans out without bound: more than 64 `endpoints` is
  refused `E_BAD_REQUEST` naming the cap, and endpoint probes and per-model
  capability questions run on worker pools of 8 each instead of one goroutine
  apiece (cancelling the scan skips the unstarted ones). Endpoints are still
  not restricted to loopback, because this plane is host-owned and a model
  server on the LAN is a legitimate target; the doc comment says so.
- `policy.check.command` honours the `allowed_commands` a host's `scope`
  declares, and names that scope's task on the decision. `serve.HostScope` had
  no field for them, so GitPulse's bound-task commands were dropped in decoding
  and came back `command.not_allowed`: denied with the allowlist enforced,
  demoted without it. Hard rules still run first; a scope allowlist admits
  nothing they refuse.
- `manvi serve` no longer answers one request at a time. Cheap operations
  (`hello`, the policy checks, `chat.*`, `work.runs.managed.stop`) are
  answered on the read loop; anything that can block runs on a lane per
  operation family (local probes, devmap, managed launch, workbench), in
  arrival order within the family, under its own deadline. A stop sent while a
  managed prepare waited on a provider's handshake used to wait behind it, and
  a host that timed out tore the sidecar down with every session it hosted. A
  lane holds at most 32 waiting requests; past that a request is answered
  `E_BUSY` (retryable) at once. Responses on different lanes can arrive in
  either order; as before, hosts correlate by id.

- Wrapping text no longer hangs when a character is wider than the line
  (a double-width character at width 1); it is placed on a line of its own.
- **The TUI saves every turn of a session, not only the first.** Each save
  built a new session store, and a store refuses to replace a saved
  generation it has no record of writing — so from the second turn on every
  save was refused as another run's work, and the session silently stopped
  being saved. Sessions from affected builds kept their first turn.
- After a theme switch, a transcript long enough to scroll kept drawing the
  old theme's colours until the next event arrived.
- `GOOS=windows go build ./...` compiles again; it had failed on
  `syscall.Flock` since the spend cap landed. The ledger takes LockFileEx on
  Windows and skips the directory fsync Windows cannot do. The Windows path
  compiles and vets but has not been run on Windows.
- A metered response that finished while its caller was being cancelled is
  settled at its reported usage. It used to keep its whole worst-case
  reservation.
- **Read-only commands no longer write to the repository, and the managed
  `.gitignore` rules no longer reach past the harness's own state.** Every
  command except `help` and `version` created `.devcouncil/` and appended
  about fifty ignore rules to `.gitignore`. That included `logo`, `flags`,
  `providers`, `tools`, an unknown command and a usage error, and outside a
  git repository it created a new `.gitignore`. The rules included
  repository-wide `*.sqlite`, `*.db`, `.claude*`, `.cursor/` and `logs/`, which
  hid tracked fixtures and checked-in agent settings. Now:
  - Only commands that keep state prepare the repository: the face, `tui`,
    `run`, `serve`, `watch`, `doctor`, `tool`, `allow`, `lease
    list|acquire|release` and `map build|rebuild`. `manvi tools` also stopped
    creating `.devcouncil/artifacts/`, which it did even with
    `MANVI_HARNESS_INIT_ENABLED=false`, because the artifact store now creates
    its directory on its first write.
  - The managed rules are `.devcouncil/*`, `!/.devcouncil/` and
    `!.devcouncil/config.yaml`, plus the anchored path of anything relocated
    inside the repository with `MANVI_STATE_DIR`, `MANVI_STORE_DB` or
    `MANVI_GRAPH`.
  - A rule that would ignore a tracked file is not written, and stderr names
    the file. Git decides which files match.
  - No `.gitignore` is written outside a git working tree.

  A repository that already received the old rules keeps them; delete any you
  do not want.
- **Manvi no longer re-includes nested `.devcouncil/` caches in a repository's
  `.gitignore`.** Its managed rules carried an unanchored `!.devcouncil/`.
  Rules are matched by text and appended last, so in a repository that
  already re-includes only its own state directory with `!/.devcouncil/`
  (DevCouncil does), any command — `manvi gusset-check` included — appended
  the unanchored form, and, being last, it re-included every nested
  `.devcouncil/` index cache the file had been written to keep out. The rule
  is now `!/.devcouncil/`. A repository that already received the old line
  keeps it; replace it with `!/.devcouncil/` by hand.

## [0.0.6] — 2026-09-30

### Changed

- Pins move to gusset 92c2056 (DevCouncil 3b58dd7): handles dropped to the
  GC backstop close at most four at a time, so a burst of them no longer
  leaves one OS thread per handle in the process.
- A policy question the engine does not answer within 250 ms is answered by
  fnmatch (DevCouncil 63aa5e5) instead of denied as `engine_unavailable`: a
  busy engine is not a broken one. `manvi` names a failed engine check on
  stderr at startup. macOS release binaries are linked for macOS 12.0 and
  refused if they record a newer minimum.
- **Release binaries link the Gusset engine.** They were `CGO_ENABLED=0`, so
  the engine that now makes policy decisions was absent from everything
  shipped. Each target builds on a native runner (`macos-15` for both darwin
  architectures, `ubuntu-latest`, `ubuntu-24.04-arm`); Linux binaries are fully
  static against musl through DevCouncil's `cgo-env.sh --target`, and every
  binary must pass its own `gusset-check` before upload. The asset names are
  unchanged. `MANVI_RELEASE_ENGINE=0 scripts/release-build.sh` still builds
  the cgo-off binary for a local dry run. Docs amended: the "`CGO_ENABLED=0`
  for every shipped build" rule is now "one linked archive, the Gusset
  umbrella".
- **Policy decisions are made by the Gusset engine in a cgo build.** Every
  pattern question the write, read and command ladders ask — secret paths,
  restricted and protected paths, planned-file globs, command allowlists —
  now crosses into DevCouncil's dc-glob engine in `manvi serve`, in the
  attended session's gate, and in `manvi check`. Before, the engine was a
  health check and Go's fnmatch decided. Answers are held equal to fnmatch's
  by the CPython fixture, a fuzz differential through the boundary, and
  DevCouncil's policy suite re-run with the engine as every gate's default.
  A matcher that cannot answer mid-decision is a hard denial under the new
  `path.engine_unavailable` / `command.engine_unavailable` rules (added to
  `contracts/verdict.schema.json`), never a guessed bool. A `CGO_ENABLED=0`
  build has no engine and keeps fnmatch.
- `devcouncil_git_stage` and `devcouncil_git_commit` ask the gate's matcher
  for the secret-path check, so they read the list exactly as the write
  ladder does; a matcher error refuses the stage or commit and says the check
  did not run.
- `manvi gusset-check` now runs `gussetcheck.SelfTest`: CPython fnmatch
  parity through the boundary, the batched match-any path, and a deliberate
  Rust panic inside the linked archive on a throwaway handle, which must come
  back as `ErrPanic` then `ErrPoisoned` while the shared handle keeps matching.
  Rust prints the induced panic to stderr; that line is the proof running. The
  policy plane's `Ready` still runs the panic-free `Run`.
- `manvi serve` releases the engine handle on exit through
  `gussetcheck.Shutdown(2s)`: jobs are cancelled and the join is bounded, so a
  stuck engine is reported as a shutdown error and left to process exit rather
  than hanging it.
- `verify.sh` gains an always-on `Go — gusset engine (cgo)` step. The default
  gate is cgo-off, so the only leg that linked the engine was the opt-in race
  run. The DevCouncil host and this step build through DevCouncil's
  `rust/gusset-engine/cgo-env.sh`, which rebuilds the archive every time and
  keys Go's caches on its hash; before, a rebuilt archive could be skipped by
  the Go build cache and a cached test result replayed against the old one.
- The DevCouncil and gusset pins in `scripts/module-pins.txt` move to the
  revisions this was verified against.
- `gussetcheck.Ready` no longer caches a transient failure for the life of
  the process. It was a `sync.Once`: a first check that ran out of its 5 s
  budget, or met an OS resource error opening the handle, refused every later
  policy check until restart. A pass is kept; a verdict (not linked, poisoned
  or panicking engine, parity mismatch) is latched; a transient failure is
  retried after 30 s.
- A policy answer refused by the engine gate now carries a fixed message; the
  engine's own error goes to stderr, as other internal failures already did.
- `manvi serve` and `manvi gusset-check` drain the engine's Rust log ring to
  stderr; nothing read it before, so worker respawns and caught panics were
  evicted unseen.
- `verify.sh`: five `NOT COVERED` lines (skipped tests, an unreadable or stale
  map, starved fuzz targets) printed mid-log but never reached the verdict
  list; they now go through `notcovered` like the rest.
- `verify.sh` runs `bench/paper/test_supplement.py` with the other bench
  tests, so the supplementary-archive check is part of the gate.

---

## [0.0.5] — 2026-09-14

Re-cut from the 2026-09-11 build. The entries below landed after that stamp and
ship under the same version; everything from the original 0.0.5 follows them
unchanged.

### Removed

- **`verify.rigor.enabled` and `verify.diff_coverage.enforce` are gone.**
  DevCouncil retired both as switches nothing read, and `manvi/devcouncil` was
  the last thing still reading them — so the package stopped compiling against
  a current sibling checkout. The gates they named run in `dcverify`, reached
  by finding the binary; neither key ever decided whether a gate ran, so
  setting `enforce: true` bought a belief the run did not honour. Their
  defaults are now the whole behaviour: `stub_detection` findings are always
  kept, and an unmeasured or uncovered diff is always reported and never
  blocks. **A config file or `MANVI_*` variable still setting either key is
  refused by name at startup** — remove them. DevCouncil's own `verification.*`
  spellings in `.devcouncil/config.yaml` are unaffected.

### Changed

- **The macOS input guard is partitioned by what the input can commit.** It
  demanded exact equality across all fifteen `HIDSystemState` counters,
  `MouseMoved` included, so anyone touching the mouse aborted an otherwise
  valid run — a 40-run campaign suspended 17 times while all 40 independent
  saved-state checks passed. Buttons, keys, modifiers, scroll and drags can
  change application state and stay a hard refusal; `MouseMoved` and
  `TabletPointer` cannot press, type, scroll or drag, so element-addressed work
  (`observe`, `AXPress`, `AXValue`) reports them instead of aborting, and still
  revalidates the window, the scoped accessibility fingerprint and the target
  before dispatch. Coordinate-addressed input — `TypeText`, `Click`, `Scroll`,
  every visual click — still refuses motion, because there the pointer position
  is part of the action. *Security impact: this narrows the guard, it does not
  remove it.* Nothing that can change application state is newly admitted, no
  pre-dispatch revalidation is bypassed, and tolerated motion is recorded as
  `pointer_motion` on the observation and receipt so evidence still shows a
  person was present. The field is omitted when nothing was observed, so quiet
  runs serialize exactly as before.
- Document DevCouncil as independently selectable components and modules, Manvi
  as the wrap around them, and GitPulse as a host that takes both for their
  respective jobs.

### Added

- **Claude Code managed adapter.** Added managed session adapter for Claude Code (`codingagent.StartClaude`) driven over stdio `stream-json` transport with `--permission-prompt-tool stdio` approval proxying.
- **Runtime managed provider publishing.** `manvi serve` now dynamically advertises supported `ManagedProviders` in its `HelloResult` handshake payload, enabling clients to verify agent capabilities before claiming runs.
- **Gusset diagnostic check.** Added `manvi gusset-check` CLI subcommand and `gussetcheck` package verifying DevCouncil's linked Gusset diagnostic engine and pool sizing before execution.
- **An external-interaction refusal names the input category.** A refusal said
  only that hardware input changed, so an operator could not tell a person
  using the machine from a misfiring guard — a 40-run campaign suspended 22
  times with no way to attribute one of them. Refusals now name the coarse
  category that moved (`keyboard`, `scroll`, `drag`, `pointer_button`),
  deduplicated and ordered. Deliberately coarse: no counts, keycodes, buttons
  or pointer coordinates reach the message, and a test asserts it.
- **`replay.Decode` takes fixture bytes and a source label**, so a consumer can
  ship a fixture inside its binary. `replay.Load` reads the file and delegates;
  no behaviour change for existing callers. Jarvis's `--provider fixture`
  rehearsal derived its transcript path from `runtime.Caller`, which under
  `-trimpath` resolves to a module-relative name that is not on disk — it
  worked under `go test` and failed in the shipped binary.
- **`TransientObservation` names the re-observable capture failures once** —
  `observation_inconsistent`, `window_changed`, `state_changed`,
  `window_ambiguous`, `window_occluded` — so the execution loop and callers
  outside it agree on what a bounded re-observation may resolve. Jarvis's drift
  diagnostic did not retry at all and failed where replay succeeds.

### Fixed

- **The command-gate differential still ran `go test ./gate` in this
  module.** That package moved to DevCouncil with the rest of the ladder, so
  the count was zero. The gate now runs those tests in the sibling checkout.
- **`dc-verify`'s json contract looked for the host binary inside this
  repository.** The crate is reached through a symlink, so the test's
  grandparent is Manvi, and the binary lives in the DevCouncil checkout.
  The gate builds that host and points `DEVCOUNCIL_BIN` at it.
- **Ubuntu verify could not sandbox the bench stress suite.** AppArmor
  blocks unprivileged user namespaces, so `bwrap` failed with
  `setting up uid map: Permission denied` and every contained check
  failed. The Linux job now allows that namespace before the suite runs.
  The suite still refuses an uncontained shell.
- **The verify job linked the wrong `libgusset.a`.** `devcouncil_gusset_init`
  is exported by DevCouncil's `rust/gusset-engine` umbrella, not by the
  gusset repository archive. CI built the latter and the host link failed
  on both runners. The gate now builds the umbrella and passes its
  directory to the linker first.
- **A server's stderr explanation could lose the race with stdout EOF.**
  GitHub's macOS runner reported a bare connection close and empty
  diagnostics for a server that had written its failure. The stdout reader
  now waits for the stderr reader to finish before failing the pending call.
  The pty ioctl `unsafe.Pointer` calls are marked `#nosec G103`, so the
  gosec count is 392 on both darwin and linux.
- **A missing managed-adapter callback dereferenced a nil pending entry.**
  nilaway counted four new panics in the Claude and Codex responders, which
  put the tree over its ceiling of 78. A missing entry now returns the same
  stale-callback error the preflight already uses.
- **The lint-debt counts were from an earlier golangci-lint.** 2.13.2,
  which CI installs as `@latest`, recounts the same tree. The ratchet
  numbers are the new measurement.
- **The recording proxy read a peer body with no limit, and several tests
  used numeric HTTP statuses.** Both fail the current golangci-lint, which
  is what CI installs. The proxy now refuses a request past 32 MiB instead
  of forwarding a truncated one, and the statuses are the `net/http` names.
- **The dependency allowlist still described the tree from before the
  DevCouncil import.** `go list -deps` names
  `github.com/bharathvbcr/DevCouncil/backend/go_orchestrator`, which is the
  component module this harness imports through the `replace` in
  `manvi/go.mod`. The allowlist now names that module. `gusset` stays off
  it: that import is cgo, and this gate certifies `CGO_ENABLED=0`.
- **`gussetcheck` imported a cgo engine into the `CGO_ENABLED=0` build.**
  `verify.sh` and the release matrix both compile that way, so `go vet` and
  the tag build die on `gusset/internal/ffi` before any test runs. The cgo
  call now lives behind a build tag. A cgo-off binary reports that the
  engine is not linked, and policy checks in that build keep their verdict.
  A cgo build still refuses a policy answer when the handle is poisoned.
  Security impact: the shipped binary does not gain a path around the
  ladder. It also does not link an engine it cannot link, so it does not
  turn every policy answer into `E_INTERNAL`.
- **A tag push would have failed the release, and a green one would have
  shipped the wrong notes.** `go build` on the runner resolves
  `manvi/go.mod`'s replace directives to sibling checkouts that are not in
  the repository, so the matrix dies at the first import — the same failure
  the nightly verify job has been reporting as `go vet`. The build and
  verify workflows now fetch the commits in `scripts/module-pins.txt`
  before Go runs, and refuse a checkout that is some other commit. The
  publish step reads `docs/releases/<tag>.md` with `--notes-file` and edits
  an existing release instead of creating it twice. Notes are not placed in
  an environment variable (GitHub caps those at 48 KB). A missing, empty,
  oversized, still-zipped, or partial asset set fails before `gh release
  create`.
- **Offline deterministic replay rejected its own valid journals.** `NewState`
  allocated `RecoveryCounts` as an empty non-nil map while the field is tagged
  `omitempty`, so encoding dropped it and decoding yielded nil, and a recorded
  trace never compared equal to a recomputed canonical admission.
  `ResultFromState` had the same shape on `Result.Outputs`. Allocating only
  when non-empty restores round-trip equality and leaves the wire format
  byte-identical, so existing evidence bundles and their hashes stay valid. A
  reflective guard now fails any map or slice field tagged `omitempty` but
  allocated empty, so the shape cannot return on a future field.
- **The `crates/` workspace failed to load, not just one member.** A symlinked
  member inherits `workspace = true` from `crates/Cargo.toml` rather than from
  DevCouncil's, so upstream's `sha2` (dc-evidence) and `libc` (dc-proc) had to
  be mirrored there, and `dc-verify`'s path dependency on the new `dc-proc`
  needed a sixth symlink.
- **`devcouncil_dev_inspect` routed to an Extended tool from the Core profile.**
  The tool description and unavailable error recommended `devcouncil_get_gaps`,
  which the core profile does not offer — stranding an agent running under
  `llm.local.core_tools_only` with an unknown-tool error. Replaced with
  `devcouncil_verify_task, manvi map`, which are both in Core, resolving
  `TestNoToolNamesAToolTheCoreProfileDoesNotOffer`.

### Added

- **Profile workbench storage and opt-in `work.*` host operations.**
  Persistent workspaces, board items, enhancement proposals, automation
  schedules, run records, and managed Codex activation over
  `manvi serve --workbench-db`. Storage lives in `dc-store` (`work_*`
  tables); the Go host advertises the operations only when that flag is
  set. Documented in [WORKBENCH.md](docs/WORKBENCH.md).
- **Computer-use foundations, typed multimodal tool results, and a native
  desktop broker.** Tool results carry text or image blocks; Gemini accepts
  image function results; session projection publishes scrubbed events
  without rewriting private image bytes. `computer` drives a Rust
  `manvi-desktop` broker over NDJSON stdio, with optional `llm/budget`
  admission before each HTTP attempt. Documented in
  [COMPUTER_USE_FOUNDATIONS.md](docs/COMPUTER_USE_FOUNDATIONS.md).
- **Workflow capability compiler.** Pure compile/reduce in `manvi/workflow`,
  interpreted by the computer-use runner rather than by the LLM loop.
- **Published module path `github.com/bharathvbcr/Manvi/manvi`.**
  `go install manvi/cmd/manvi@latest` was impossible while the module was the
  bare name `manvi` (`missing dot in first path element`). Remote install is
  now `go install github.com/bharathvbcr/Manvi/manvi/cmd/manvi@latest`;
  contributors keep `go -C manvi install ./cmd/manvi` from a checkout.
- **GitHub Actions release workflow for darwin/linux amd64+arm64.**
  Tag `v*` builds with `CGO_ENABLED=0` and stamps `-X main.stampedVersion`, then
  attaches SHA-256 checksums. No Windows target (see `verify.yml`).
- **Documented `go -C manvi install ./cmd/manvi` as the PATH install one-liner
  for a local checkout.** Puts the CLI in `~/go/bin` (or `GOBIN`) so GUI hosts
  that do not inherit a shell profile can still find it.
- **Retarget analysis-crate symlinks at DevCouncil `rust/`.** `dc-glob`,
  `dc-grep`, `dc-store`, `dc-verify`, and `dc-evidence` follow the flattened
  workspace instead of `rust-port/crates`.
- **Harden enhancement JSON extraction.** Constraint sentences stay verbatim,
  fenced or prefixed model output is still one object, and a stress suite
  covers oversized and adversarial replies.

### Fixed

- **CI would have failed on Linux on its first run.** `.golangci-debt.counts`
  was measured on one platform, and `unconvert` reads 1 finding on darwin
  against 3 on linux — `Stat_t`'s field widths are per-GOOS, so the conversions
  in `devcouncil/safefs.go` are load-bearing on darwin and redundant on linux.
  The ratchet fails on any increase, so a green tree would have gone red on
  `ubuntu-latest` with nothing changed. The conversions now carry the
  portability reason in place, the count is 0 on both platforms, and
  `unconvert` has moved to the enforced set — 21 linters at zero tolerance, 10
  left in the debt file.
- **The Windows CI job would also have failed on its first run**, and its
  premise was wrong. This tree does not build for Windows: `manvi/artifacts`
  fails on `syscall.O_NOFOLLOW` and `manvi/devcouncil` on `syscall.Stat_t`.
  Both are load-bearing — `O_NOFOLLOW` is how the artifact store refuses to
  follow a symlink out of itself, and `Stat_t` carries the inode pin and the
  `st_nlink` hard-link check — so a Windows build needs real equivalents rather
  than stubs. The job is gone and the reason is written down where a reader
  will find it, rather than a green check standing in for a port that does not
  exist.
- **Four documents called the policy ladder "5-tier"; it has six rungs.** The
  guard for this existed and matched only the literal phrase "N-tier policy
  ladder", so a "5-tier ladder", a "5-tier policy gate", a "5-tier evaluation
  ladder" and a "5-tier policy composition" all drifted past it while it
  reported clean. It now matches the number and the noun wherever they appear,
  and reads the HTML guides too — the drift that survived longest was in one of
  them.

### Added

- **`local.scan` on the `manvi serve` protocol**, which lists the model servers
  running on the machine and the models each one advertises. The discovery
  itself already existed — it probes the well-known loopback endpoints
  concurrently and identifies every runtime by asking it, never by assuming
  whichever runtime conventionally holds the port that answered — but no host
  could reach it, because `capability.probe` needs the model name up front and
  that is the answer rather than the question. Every model carries
  `capabilities_known` beside the capability flags, so "does not support tools"
  and "nobody asked" stay two different answers instead of the same `false`.
  Documented in [the host plane reference](docs/SERVE_HOST_PLANE.md), along
  with `chat.forget`, which was on the wire and in the served set but had no
  worked example.
- **Tests for `llm/replay`**, which had none. Its playback half was exercised
  second-hand by eight other packages; its recording half — `Load`,
  `NewRecord`, `Save`, and the recording stream — had been executed by nothing
  at all, in a package whose doc comment offers exactly that as the way a real
  session becomes an offline regression test. It works; it had simply never
  been run. The package goes from 0% self-coverage to 96.8%, with no function
  left unexecuted, and the tree total moves to 82.0%.
- **`SECURITY.md`**, pointing at this repository's private vulnerability
  reporting. It says what counts (a way past a gate, a credential leaving the
  machine, a check that reports success without running) and what does not (the
  `dev` posture doing what `dev` documents, an operator's own allowlist entry).
- **`.github/dependabot.yml`** for all three ecosystems — actions, gomod, and
  cargo. `govulncheck` and `cargo audit` catch a *vulnerable* dependency on
  every run; nothing was watching for a merely old one, and nothing looked at
  the actions at all.

### Changed

- **Every GitHub action is pinned by commit SHA**, with its version in a
  comment beside it. A tag is a mutable reference, and CI is the one place a
  silent substitution would run with a token in scope. Dependabot moves the
  pins; `verify.sh` decides whether the move is safe.
- **`UnusedExports` runs.** The repository's own dead-code detector — written,
  and documented with the defect it exists to catch — was called by nothing. It
  now reports alongside the other contract scans. It stands at 147 findings, of
  which twelve are its own documented blind spot (`internal/testsupport` and
  `llm/adaptertest` exist to be called from tests, and test files are
  deliberately not counted as callers). It reports rather than gates: the
  remaining 135 need reading one at a time, and an allowlist filled in without
  that reading would certify whatever was true the day somebody stopped looking.

### Added

- **Continuous integration.** `.github/workflows/verify.yml` runs `./verify.sh`
  on every pull request and every push to `main`, on Linux and macOS. The
  workflow installs every analysis tool the gate can use and then *fails* if any
  of them turned out to be missing — a gate that could not run must not report
  as one that passed. `--race` and `--fuzz` run nightly and on demand.
- **A statement-coverage gate.** `verify.sh` now measures how much of the tree
  the suite executes (`-coverpkg=./...`, so a package is credited for what any
  test reaches, not only its own) and fails below a floor of 78%. The profile is
  produced by the existing test run rather than a second one. A missing, empty,
  or unparseable profile fails loudly instead of reporting zero.
- **An end-to-end test of the shipped binary.** `manvi/cmd/manvi/zgap_endtoend_test.go`
  runs `manvi run` as a child process against a scripted OpenAI-compatible
  server over real HTTP, and asserts the file the model asked for appears with
  the right bytes, that the result is fed back for a second turn, that a write
  outside the repository root is refused as a hard rule, that the step ceiling
  exits 2, and that an unreachable model server fails non-zero rather than
  rounding to success. Deterministic and offline.
- **Specifications for the last fourteen tools.** `docs/TOOLS_REFERENCE.md` now
  specifies all 44 — tool discovery and activation, sub-agent management,
  artifacts, interactive questions, and MCP — where five categories were
  previously tabulated only. `TestToolsReferenceSpecifiesEveryTool` replaces the
  guard that policed the old disclaimer and now fails the build if a tool ships
  without a specification row.
- **A Windows compile job** in CI. It builds and vets the Go plane and builds
  the Rust plane on `windows-latest`, which is what covers the `_other` half of
  the `procgroup` and `mkfifo` build-tag splits — compiled by nothing else in
  the matrix. It does **not** run the Go suite: `verify.sh` is bash, and whether
  the tests that fork shells and drive the Rust binaries pass on Windows remains
  unestablished. Named as a gap rather than covered by a green check.
- **`CONTRIBUTING.md`** and this changelog.
- **Trade-off 3 in `docs/TRADE_OFFS.md`**: why the provider set is four adapters
  and why a hosted OpenAI-compatible endpoint is not a configuration change.

### Changed

- **Repository search runs on ripgrep's engine.** `dc-grep` joins the analysis
  plane, and `devcouncil_grep`, `devcouncil_find_files`, and `devcouncil_list_dir`
  now enumerate exactly the same file set through one walk and one set of ignore
  rules. A missing `dcgrep` refuses rather than falling back to a second walker
  that would answer the same question differently.
- **The Rust plane takes three dependencies**, declared by the members that need
  them: `rusqlite` (bundled) in `dc-store`, and `grep-regex`, `grep-searcher`,
  and `ignore` in `dc-grep`. `cargo audit` covers all of them on every run.
- **`verify.sh` gates the analysis tools themselves**, runs every fuzz target it
  declares with a per-target budget sized by the rate that target just measured,
  and reports a fuzz finding differently from a fuzz that could not run.

### Fixed

- Every analysis-plane subprocess now gets its own process group, so a killed
  parent cannot leave a child holding the terminal.
- An Anthropic tool call the harness cannot route is refused rather than
  silently dropped.
- The code graph is checked against the commit it was built from; a graph
  describing an older tree reports as unavailable instead of resolving no area
  for every file.
- The command gate's two shell scanners agree; splitting bypasses that one saw
  and the other did not are closed.
- An empty instrumented test run is no longer read as "nothing skipped".
- A `WaitGroup` is added to before it is waited on.

### Documentation

- `devmap` is no longer described as part of the Rust analysis plane. It is an
  external tool this repository does not build, resolved from `PATH` or
  `MANVI_MAP_BINARY`; `crates/` holds four members and `manvi/dc/devmap` is only
  the client. Corrected in `README.md`, `docs/ARCHITECTURE.md`,
  `docs/COMPARISON.md`, and the visual architecture guide.

---

## [0.0.2] — 2026-08-29

### Added

- **End-of-turn check.** The harness runs its own check when a turn ends,
  rather than trusting the turn's own account of itself.
- **Bounded documentation lookup.** `devcouncil_fetch_url` — off by default,
  registered only when an operator sets `MANVI_FETCH_HOSTS`, `https`-only, with
  private and link-local ranges refused on every request and every redirect hop.
- **A Cerebras arm in the benchmark rig**, with a wire suite covering it.

### Fixed

- Bare letters can no longer answer an approval card.
- A cross-arm protocol check could not see the arm it existed to flag.
- Contained Linux episodes were being stamped as uncontained.
- The runner's decode slots are counted, not the ceiling it was offered.

### Changed

- The benchmark paper's headline was replaced with the registered v2 grid, its
  figures regenerated from the v2 report, and the "three inert components" claim
  withdrawn as an upper bound rather than a measurement.

---

## [0.0.1] — 2026-08-29

First tagged release: the MANVI harness, dual-plane in Go and Rust with zero
cgo, the five-rung policy ladder and grants ledger, SQLite task leases across a
process boundary, the native tool suite, the full-screen TUI, multi-provider
support (Anthropic, Gemini, xAI, and local servers), the benchmark rig, and
`verify.sh` as the single gate over all of it.

[Unreleased]: https://github.com/bharathvbcr/Manvi/compare/v0.0.6...HEAD
[0.0.6]: https://github.com/bharathvbcr/Manvi/compare/v0.0.5...v0.0.6
[0.0.5]: https://github.com/bharathvbcr/Manvi/compare/v0.0.2...v0.0.5
[0.0.2]: https://github.com/bharathvbcr/Manvi/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/bharathvbcr/Manvi/releases/tag/v0.0.1
