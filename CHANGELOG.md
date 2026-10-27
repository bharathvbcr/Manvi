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

- Harness specs. `harness.spec` (operator scope, empty by default) selects a
  named spec: prompt sections after the fixed ones, a skills selection, a
  lessons library name and sub-agent roles, from a process catalogue or
  `.devcouncil/harness/<name>.json`. A spec is validated (every problem listed),
  digested, and resolved once per turn by both faces; `prompt/assembled` gains
  `spec_name` and `spec_digest`. A spec cannot set flags, tiers, posture, the
  verify command, fetch hosts, MCP servers or write exceptions, and a role it
  registers is never wider than the session's own tools.
- Host-supplied harness over `manvi serve` (additive within protocol 1). The
  optional op `harness.resolve` takes a `repo`, `named` or `inline` spec and
  returns its sections, roles, a bounded skills listing, a digest and the
  skills it rejected; the host composes `System` from the sections after its own
  essentials. A spec is decoded strictly and validated by the same rules as a
  repository spec, so a flag, tier, posture, MCP, verify, fetch or
  write-exception field, or an Essential section name, is refused with
  `E_HARNESS_REFUSED` listing every problem (`E_HARNESS_NOT_FOUND` when no such
  spec). `chat.prepare` takes an optional `harness_digest` and notes a change as
  one prefix move (`prefix_moved`, `prefix_note`) without disturbing the
  compaction ledger. `work.runs.managed.prepare` takes an optional
  `harness: {kind: "repo"|"named", name}`: the spec's sections are written as a
  delimited block at the tail of the file the CLI reads (`AGENTS.md` for Codex,
  `CLAUDE.md` for Claude) through the gated, pinned writer, before the process
  starts, and taken out again when the run ends however it ends. A block that
  cannot be written aborts the run (`E_HARNESS_MATERIALISE`). The digest is in
  the prepare result and in the run's `effective_configuration` as
  `harness_digest`. The block's lines carry a per-run 128-bit nonce and only
  Manvi's own recorded bytes are ever removed (a block the agent edited inside
  is refused, not guessed at); a record under `.devcouncil/` lets the next
  prepare on the checkout take out a block a dead run left and report it
  (`harness_recovery`); one harness per checkout (`E_HARNESS_BUSY`), and
  `work.verify.task` and `work.scan.gaps` refuse while a block is in the
  worktree. A host that does not see `harness.resolve` in `hello.ops` runs with
  no harness.

### Changed

- `computer.Run.Send` now waits until the run loop admits the control, at most
  one loop step (for example a 3s pause fence), and returns the run's verdict.
  It returns nil once the control passed the epoch check, `ErrStaleControlEpoch`
  for a control addressed to an old epoch, and `ErrRunEnded` (the same "run has
  ended" text as before, now an exported sentinel) when the run has finished.
  It used to return nil on enqueue, and a stale control was then dropped, which
  left a run waiting on it forever. The signature is unchanged, and it is still
  bounded by the caller's context. An automation pause also no longer publishes
  `Paused` with the epoch from before the fence.
- The DevCouncil pin moves from `1ae3bc15` to `3cfe283c` and gusset from
  `984209b5` to `320c56c2`. DevCouncil's store is now at schema 10 with
  workbench 14: workbench items are the one task source, and the legacy
  `tasks` table is retired when a store is opened. A store or profile written
  by this build cannot be read by a Manvi, DevCouncil or GitPulse that predates
  these schemas. `devcouncil_next_task` and `manvi watch` read ready tasks
  through `ReadyReport(ctx, store.ReadyEnforce)`. The `manvi serve` contract
  adds `work.runs.managed.follow` and `work.runs.managed.wake`.
- The DevCouncil pin moves from `3cfe283c` to `b1a29105` (69 commits), whose
  gusset CI pin is now the same `320c56c2` this repository builds.
  DevCouncil's store moves from schema 10 to 11, which adds the `reviews` table
  and the `review-append` and `reviews` commands that `llm_review` acceptance
  criteria are discharged from. Workbench stays at 14, and the devmap store
  stays at 27, with readers accepted from 25. The linked store client now
  requires a `dcstore` that answers schema 11. `manvi doctor` reports a schema 10
  `dcstore` as store UNAVAILABLE, and a state database that a schema 11
  `dcstore` has opened is refused by a schema 10 one, so install the two
  together. The linked command gate adds the hard rule `command.harness_only`,
  which refuses review recording in agent shells. `contracts/verdict.schema.json`
  now lists that rule, matching DevCouncil at the pin. gusset stays at `320c56c2`.
- `manvi serve`'s verification critic (`work.verify.task`) now runs on the
  advisor tier, read-only, through the same provider, tier and effort setup as
  `manvi run`. It used to inherit the session's model, bypassing the tier
  profile. With the default profile that is `claude-fable-5-1`, about 2.5 times
  the session model's price per verification. A tier profile or model this
  binary cannot run no longer stops anything at startup: serve starts as before,
  and each verification is refused with the tier error `manvi run` gives.
- The commit tool's secret scan reads only what a commit adds. Binary files, by
  git's own verdict with attributes included, are streamed from the object store
  and searched for known credential values, so a binary copy of a secret is
  refused and a large binary is committed rather than refused for size.
  Credential values are collected from the secret-path file's HEAD and index
  versions as well as the working tree. A limit the agent cannot cure (a line
  over 1 MiB, more than 1,048,576 values, 64 MiB in all, or an unreadable
  working-tree copy) is reported as a `scan_notes` entry on the result and in
  its degraded list, with exact copies still refused by blob id. It is never a
  refusal. Staged content beyond the 1 GiB stream budget or the commit's
  deadline is refused, naming the blob.
- The MCP client's `initialize` is single-flight. A call that times out while it
  is outstanding leaves it running, later callers wait for the same handshake,
  and a late reply is adopted once with one `notifications/initialized`. A
  handshake that ends with the session in an unknown state ends the client: the
  request failed locally, the server answered with a protocol revision this
  client does not speak, or `initialized` could not be written. The Manager then
  starts a fresh server. `Client.Alive` is false as soon as the client is
  closed.
- The pre-turn baseline is recorded beside the first model request instead of
  before it, and a turn's first tool call waits for it. `agent` publishes two new
  events, `ToolsDispatching` before a response's tool calls run and `TurnEnded`
  on every exit of a turn whose `TurnStarting` was published. The end-of-turn
  diff starts from the baseline's index instead of re-hashing the worktree.
  Benchmarks: time to the first request with the sensor attached went from about
  50 ms to under 0.1 ms, and the scoped diff went from 63 to 47 ms.
- `artifacts` exports `ErrPublishedUnsynced` and `NewStoreWithDirSync`.
- `render.Line.Wrap` breaks a word wider than the line in time linear in the
  word. The hard break measured everything it had left at every cut, so a
  160 KB base64 blob or minified-JSON line cost the square of its length: the
  benchmark went from 306 ms to 1.0 ms (no-space) and from 1.19 s to 3.9 ms
  (CJK), on a loaded machine, min of 8 interleaved runs. The rows are
  unchanged: a property test holds it to the previous implementation over
  10,000 random lines at widths 1 to 200.
- A streamed reply that is one long paragraph — a base64 blob, minified JSON, a
  wide table row — is laid out from the last few rows each frame instead of
  from its first byte. The open line used to be wrapped whole, and every row
  put through the gutter again, at every frame. A plain paragraph (no code span
  or bold, long enough that no heading, list, diff or fence prefix can still
  apply) now keeps the rows before its last two in their final form. A marker
  that arrives late, a break, or a resize drops the cache and the line is laid
  out whole, as before. So does a blank other than space and tab (a no-break
  or ideographic space, U+2028, a carriage return), which the wrap breaks at
  but the cache does not model; and rows are not kept while the text ends
  partway through a multi-byte character, whose width is not known until its
  last byte arrives (a tag character or variation selector completes to zero). Per frame at 160 KB, min of 8 interleaved runs at load
  40 to 50: prose 2.03 to 0.12 ms, table row 4.76 to 0.13 ms, base64 367 to
  0.12 ms, minified JSON 369 to 0.12 ms, against the 8 ms frame budget. A
  8,000-delta unbroken prose stream went from 749 to 66 ms and 1.46 GB to
  25 MB allocated; the same stream as base64 from 73.8 s to 66 ms and 124 MB to
  16 MB. A reply broken into lines is unchanged (41.0 to 41.8 ms, same
  allocations). The rows are held to a whole render by property tests over
  random growth with late markers, resizes, two alternating widths and every
  rune class above. `mdState.line` now classifies through `mdState.kind`,
  checked against the previous branches over random input.

### Fixed

- `manvi run --approve-fd`: tool children no longer inherit the approval
  descriptor, and a decision is bound to its request. The descriptor was
  wrapped with no close-on-exec, so every shell command, MCP server and `git`
  the harness started held it: with a writable descriptor (`3<>fifo`) the
  model's own command wrote the "human" approval for its own escalation, and
  with a pipe or socketpair it could read or steal the host's decisions. It is
  now marked close-on-exec (Windows: inherit flag cleared) straight after the
  flags are parsed, before anything is spawned, and a descriptor that is not
  open is refused at startup naming the flag. Requests now carry unique ids
  (`approval-<nonce>-<n>` on escalations, which had none; `question-<nonce>-<n>`
  in place of the per-call `QUESTION-000N`), a decision naming another id is
  refused, and a line is read only while a request is pending: one that arrives
  earlier, or for a request the turn abandoned, is discarded with a `notice`
  rather than approving the next request. `approval_id` stays optional; when a
  decision omits it a `notice` says it was bound by timing only. `chosen` is
  checked against the offered choices (and `multi_select`), and a decision line
  is capped at 64 KiB, an over-long one being a refusal skipped to its newline
  without being buffered (it was unbounded, 256 MiB held in the audit). A
  cancelled turn (`--timeout`, SIGINT, SIGTERM) returns at once while an
  approval is pending on a blocking descriptor; it hung before, holding the
  approver's lock, so only SIGKILL ended the run. One reader goroutine owns the
  descriptor and the descriptor's blocking mode is left as the host set it.
  The claim is made as the first act of `manvi run`, before the configuration
  is read or the repository is scaffolded (scaffolding runs `git`, which would
  otherwise have inherited it), and the approver claims the descriptor itself
  as well. A descriptor that is not a pipe, FIFO, socket, device or file, or is
  the runtime's own event poller (a closed `--approve-fd 3` is reissued as one
  once a file has been opened), is refused. A blank line from the host is
  ignored rather than read as a refusal.
  Making `approval_id` mandatory is not done here; it breaks hosts that omit it
  and is left for a minor release.
- Bench: an unreadable or unparseable git pointer no longer unguards the
  repository's other checkouts. `git_guard_roots` answered one unreadable
  `.git` file, `commondir` or sibling `worktrees/<n>/gitdir` with the start
  itself, which the final filter dropped as the bench's own checkout, so the
  main checkout and every sibling worktree (hidden tests, reference solutions)
  became readable to the model. It now raises `GitGuardError` naming the file,
  and `run.py` refuses to start on it, before the containment probe. Pointers
  are read as git reads them, and a worktree admin directory with no `gitdir`,
  an own `.git` or `commondir` naming nothing, and a byte-order mark are
  refused too, each of which used to drop the other checkouts silently.
- An artifact whose directory flush failed after it was published is reported
  as saved with its durability unconfirmed (`ErrPublishedUnsynced` beside the
  artifact; the tools answer `durability: "unconfirmed"`). It used to be
  reported as not written, and a retry then failed with `ErrExists`.
- Directory flushes have one owner, `internal/syncdir`, which does nothing on
  Windows, where a directory handle cannot be flushed. Session saves, artifact
  writes and pinned file writes called it unconditionally there. The Windows
  failure is inferred from the ledger's own note, not observed.
- The budget ledger reclaims a reservation whose process exited with code 259
  on Windows. That code is also STILL_ACTIVE, so the process used to be taken
  as alive forever. Liveness is now confirmed with a zero-wait on the process.
- The seeded pre-turn baseline index kept the time it was copied, not the
  index's own, which defeated git's racy-timestamp check. A same-size edit made
  in the same second as the index write could be recorded as unchanged.
- The commit tool no longer locks the agent out of committing.
  - A 20 MiB binary, a large binary deletion or rename, a large file at a
    secret path, and a tracked secret file missing from the working tree used
    to refuse with a remedy the agent could not perform.
  - A secret copied before its tracked file was replaced by a symlink,
    directory or FIFO is now caught.
- The MCP client could send a second `initialize` while the first was still
  outstanding: after a timeout, or after a malformed frame failed every pending
  call.
- `policy.check.command` now marks a hard refusal for an unreadable construct
  (`cd`, `git -C`, a heredoc, a substitution) with `policy.unreadable_only` once
  the line's redirections have been judged and passed, as the harness gate
  already did. It used to return before the redirect rung, so the marker never
  reached a host and GitPulse, which softens on it alone, refused every `cd`
  line. A line whose redirect is refused (`cd x && echo > .env`), one refused
  for what it does (`cd x && git push --force`), and one whose redirect could
  not be judged for want of a root keep their refusal unmarked.
- A managed Claude run whose stream carries a tool result that does not decode
  now fails with `invalid Claude user message`, as an undecodable assistant
  message already did. It used to skip the frame and end the run as completed
  with that result lost. A user frame with no message, or with plain-string
  content, is still skipped.
- The project instructions (`AGENTS.md`, `CLAUDE.md`, `GEMINI.md`) are read
  through the repository's gated, pinned reader, rooted at the project root.
  They were a bare `os.ReadFile` of the working directory: a symbolic link named
  `AGENTS.md` carried a credential from outside the repository into the system
  prompt and to the provider, and a FIFO of that name hung prompt assembly.
  - A link (even one inside the repository), a file with a second name, a
    FIFO or device, and a file the read gate withholds are refused. The turn
    runs without that file, a prompt-fault notice names it and says why (once,
    not every turn), and the next candidate is still tried, so a repository that
    links `AGENTS.md` to `CLAUDE.md` gets the real file. Absent or blank files
    are not a fault.
  - Every provider has a 256 KiB bound, refused with the file's size rather
    than truncated. It used to bind only where a token budget existed, which is
    local models alone.
  - Read once per turn by the per-turn resolution and carried in
    `PromptOptions.Instructions`; the reserved-name checks and the spec
    collision check no longer read the file (it was read several times a turn).
  - The file is still the working directory's: a session started in `sub/`
    carries `sub/AGENTS.md`, a root file it never carried does not join or
    replace it, and `sub/`'s file wins over the root's when both exist. The gate
    is rooted at the project root and the file is read as `sub/AGENTS.md`
    beneath it. A working directory outside any project root is gated at the
    directory itself.
- A skill's body is refused on the same runes as its description and a spec's
  text, by one enumerated policy (`hiddenRuneProblem`, over `hiddenRanges`) and
  not by Unicode category: C0 and C1 controls except tab, newline and carriage
  return, the bidi embedding, override and isolate controls (U+202A-202E,
  U+2066-2069) and the tag block (U+E0000-E007F). `devcouncil_load_skill` hands
  the body to the model whole and it checked UTF-8 alone, so an escape sequence
  or a bidi override could sit in steps a reviewer read as plain text. A
  refusal names the skill, the rune (code point and name), and its line and
  column, and never strips. `${...}` and `{{...}}` in a body are still allowed.
  - Descriptions and a spec's text are loosened to the same set. They used to
    refuse every control, format and private-use rune, which refused real text:
    the zero-width joiner in a family or technologist emoji, the zero-width
    non-joiner in Persian, the soft hyphen, a zero-width space, a word joiner, a
    byte-order mark mid-text, the left-to-right, right-to-left and Arabic letter
    marks, variation selectors and private-use glyphs. These now load.
- Listing the skills directory is bounded. It read every entry
  (`ReadDir(-1)`) on every turn and every `devcouncil_load_skill` call; it now
  reads at most `harness.MaxSkillDirEntries` (10,000) in chunks, checking for
  cancellation between them, and reports a cut (`SkillSet.ListingCut`, a prompt
  notice, a `harness.resolve` note) instead of silence. A lookup of a name not
  among the entries read says the listing was cut rather than that the skill is
  absent. Measured once each on a 100,000-directory skills directory: one
  lookup went from 377 ms to 40 ms, and `LoadSkills` from 3.07 s to 1.88 s, the
  rest being the read gate's cost for the 200 skills it examines.
- A lesson can no longer write its own History or verdict counts. The store
  re-reads an entry's Markdown by its headings (`historyOf` takes every
  `- revision` row under the first `## History`; `supersededRow` switches
  section on any `## ` line and counts `- <verdict>:` lines under
  `## Attribution`), and a lesson or evidence line that spelled a heading, such
  as `## History` followed by a `- revision 99, ...` row, was stored verbatim
  and adopted by the next revision.
  - One predicate, `readsAsStructure` (a column-0 `## ` line), is what the
    store switches section on and what `lessons.Parse` refuses in the lesson and
    the evidence, first line included, wherever it sits. Nothing else is
    refused: `# python comment`, `### Testing`, `#123`, `#!/bin/sh`, a lone `#`
    and an indented `## x` are prose and stay legal. A continuation line is now
    stored with its indentation, because trimming it turned an indented `## x`
    into a column-0 one. `- helped:` and `- revision ...` bullets stay legal;
    without a heading to put them under the store never counts them, which a
    test checks through a real revision. `historyOf` now takes only the rows
    under a `## History` heading, through the same predicate.
  - `lessons.Save` makes the same check on a `Result` that did not come from
    `Parse`, and refuses a cue, change or note that spans lines. Nothing is
    written when it refuses.
  - The reflector's prompt states the rule, since a refused answer costs a
    rerun.
  - Entries already stored with a forged heading are not rewritten.
- A refused read says it is a read and what to do about it. The reader's
  refusal of a symbolic link said `refusing to write through ...` because every
  pinned operation pinned through the write path's text; it now says
  `refusing to read "AGENTS.md": it is a symbolic link ... Replace the link with
  the file itself, or with a copy of it`, and a removal says it is one. The read
  gate's refusals (`path.aliased_read`, `path.outside_root`, `path.secret_read`)
  carry a remedy each (replace the hard link or the link out of the repository
  with a copy; rename a file that holds no credentials). They reach the prompt
  notice for a refused instruction file, a skill's rejection and a spec's.
- The one-harness-per-checkout guard keys a checkout by the directory, not by
  its path string. `.../MyRepo` and `.../myrepo` are one directory on a
  case-insensitive volume (APFS, NTFS default) and used to be two checkouts, so
  two managed runs proceeded and the second overwrote the first's pending
  record. Checkouts are compared with `os.SameFile` (device and inode; volume
  serial number and file index on Windows), which is also right on a
  case-sensitive volume, where a case-folded string would not be.
- A managed run's harness block write that fails after the file was replaced
  (the rename is done, the directory sync after it failed;
  `WriteMayHaveChanged`) is no longer taken for a write that did nothing. The
  materialisation used to delete the record of the block and carry on as if the
  file were untouched, leaving the block in the user's instruction file with
  nothing to take it out. It now reads the file back: the block is there and the
  run proceeds, restoring it when it ends; it is not and the attempt fails with
  no record; the file cannot be read to tell and the record is kept and the run
  is aborted with it, so the next attempt on the checkout takes the block out.
- A managed run's harness block can no longer be written where it could not be
  taken out again, and a failed restore no longer deletes its record. The
  block's size was bounded apart from the file's, so a CLAUDE.md a few hundred
  bytes under the 4 MiB read limit was accepted, grew past it, could not be read
  by the restore, and the next prepare then cleared the record: Manvi's bytes
  stayed in the user's file and `HarnessBusy` stopped reporting them. There is
  now one bound, on the file with the block in it, checked before anything is
  written (`E_HARNESS_MATERIALISE` naming it), and shared by the read before the
  write and the read that restores. A restore that fails for any reason other
  than the agent having edited inside or removed the block keeps its record:
  every `work.runs.managed.prepare` on that checkout is refused, and
  `HarnessBusy` refuses a verification of it naming why, until the cause is put
  right and a later recovery takes the block out. An unreadable record, or a
  block the agent edited inside, is still refused once and cleared. A checkout
  whose record an earlier build wrote past the new bound stays refused with the
  restore's error until the file is put right by hand.
- A managed run that ends with its agent process not confirmed gone no longer
  gives its checkout away. `Close` answering "not reaped" is recorded
  `unresolved`, but `finish` still restored the harness block (a read and then a
  write over a file the agent may be writing, which overwrote its edit in
  between) and closed the job, so the next run was admitted on a checkout the
  first agent might still be in, with two agents and one block. The contract now
  is: **hold until reap or restart.** The block and its record stay and the
  checkout is held. Every attempt on it, with a harness or without, is refused
  `E_HARNESS_BUSY` at once, and `HarnessBusy` refuses a verification; both name
  the held run, since when, the block's nonce and file, and say to restart
  `manvi serve` to recover. A background reaper, one per held run, keeps asking
  the provider whether the process has exited (`Close` is idempotent and
  bounded; about ten minutes of asks, tied to the runner's lifetime); when it
  confirms, it takes the block out and releases the checkout, with nobody
  asking. The hold is in memory, so a restart of Manvi ends it and the record is
  applied as any dead run's is. The ask does not run inside a prepare, so a held
  checkout delays no other checkout's attempts (it used to cost every prepare on
  the one managed lane up to five seconds per retry). Not covered: a factory that
  fails after possibly starting a process returns no session to ask, so that end
  still restores as before.
- The hold's release and the pending record are safer. The reaper acts on the
  record only when it is the held run's own (the nonce): with the checkout
  directory deleted and recreated, a second run is admitted as a new checkout, and
  the old hold's reaper used to strip that live run's block and record. A block
  the agent edited inside, found when the hold is released, is now reported as
  when a run ends normally (the record is kept, `HarnessBusy` refuses naming it
  and the run, the next attempt reports it once) and no longer cleared and
  released unseen. Every mutation of a pending record is under one in-process
  lock, so removing a record is atomic with another run's write of it (it was
  read, compared, then unlinked, and another run's record written between was
  deleted); two Manvi processes on one checkout can still interleave. A
  restore-failure message no longer builds a shell command from names an agent
  controls: the remedy is in words, a symbolic link's target is shown only when it
  is inside the checkout (quoted) and as "points outside the checkout" otherwise.
  When the reaper has used its attempts, an attempt on the checkout (still
  refused at once) restarts the bounded reaper off the lane, so an agent that
  exits later releases the checkout without a restart.
- A managed run's checkout is held only when its agent process is not reaped.
  `Close` answering `Reaped: true` beside an error (os/exec's `ErrWaitDelay`,
  when the agent exited 0 but an orphaned grandchild, a dev server or an MCP
  child, still holds its stdout; the wait error is sticky, so every later
  `Close` returns it) means the agent is gone: the block is taken out and the
  checkout released as after a clean exit, and the error stays on the run as a
  note. Before this, such a normal run held its checkout until Manvi restarted.
  What outlives the agent is the process-group problem, which this does not
  address.
- A managed run's end no longer removes another run's pending record. There is
  one record file per checkout directory, and a directory deleted and recreated
  is a new directory to the one-harness guard, so a second run was admitted
  while the first was live and its record replaced the first's; the first run's
  restore then removed it by path, leaving the second's block in the file with
  nothing to take it out. A record is now removed only when it carries the
  nonce of the block being restored.
- A refused restore says what to do about it. When the pinned read of the
  instruction file is refused (the file replaced by a symbolic link, given a
  second name, grown past the bound, made a directory), the record is kept and
  the `E_HARNESS_MATERIALISE` refusal now says it was a read and nothing was
  written or removed, names the file, the block's nonce and byte offset and the
  lines that bound it, and gives the remedy for that cause (replace the link with
  a regular copy; give the file one name; shrink it) or the manual one (remove
  the block's lines, then delete `.devcouncil/managed-harness-pending.json`). A
  failed write during the restore is told the same way. `HarnessBusy` builds its
  refusal from the record and the file on disk, so after a restart it no longer
  promises that the next prepare takes the block out when that would be refused.
  The run id is named on the run's own record, not in the file: the pending
  record does not carry one. `ReadPinned` also names a read when a link refuses
  it; it said "refusing to write through".
- `work.runs.managed.activate` no longer holds the run's lock across its two
  task-store round trips (`runs.get`, `runs.started`). `managed.follow` is
  answered inline on the serve read loop and takes that lock, so a slow store
  held up `hello`, the policy checks and a queued `managed.stop` behind it for as
  long as the store took (bounded only by the lane's one-minute deadline). A
  separate per-run lock now serialises activations, so a retry still waits for
  the first call and gets its receipt, and follow never waits on the store.

## [0.1.0] — 2026-10-17

### Added

- `manvi version --json` prints the build identity as one object, plus
  `withholds_provider_credentials` (boolean) and `withheld_provider_env` (sorted
  variable names): the provider credentials no child of this build inherits.
  The serve `hello` result carries the same two fields
  (`contracts/serve.ops.schema.json`). A host that wants to start `manvi` with
  a key in its environment checks, before it does, that every name it would
  inject is listed; a build that omits the keys does not withhold them.
  `manvi version` with any other argument is now refused.

- `work.tasks.suggest` (host plane, enabled with `--workbench-db`): ranks the
  open board for what to work on next and returns a short list with a reason
  each. It is deterministic and makes no model call: a fixed point table over
  priority, severity, due date, idle age, the open tasks a task blocks, a
  `ready` status, a repository gap-scan origin and the newest stored
  verification, every point named in the answer. Done, archived, in-progress,
  review, duplicate and blocked tasks are left out and counted. Reads are
  bounded and the answer says when the board or the verification probes were
  capped.

- Every environment variable the binary reads is documented in
  `docs/CLI_AND_CONFIGURATION.md` (credentials, `ANTHROPIC_BASE_URL`, the
  `MANVI_*_BINARY` overrides, root resolution, terminal conventions), and
  `internal/contract` now parses the production source for the names it reads
  and fails when one is missing from that document, reporting the reads whose
  name it could not resolve to a literal. Documentation and tests only; no
  behaviour changes.
- `scripts/check-consumers.sh` builds and vets Jarvis against this checkout, so
  an exported symbol cannot be removed while a consumer still calls it. It exits
  2, not 0, when the consumer checkout is missing.
- `session.RestoreSensitiveLog` is back (cab5166 removed it as unreferenced; its
  consumer is Jarvis, in another repository), with its test. `gemini-3.8-flash`
  is a served model with its stated context and output limits.
- `workflow.State.Cause` (omitted when empty), the `human_takeover` failure
  code, and the `external_interaction` and `human_takeover` intervention reason
  codes.
- `agent.ImageTokens`.

- `manvi serve` binds every repository read to an explicit root. `devmap.status`
  and `devmap.query` require `root`, `work.enhancements.refine` requires
  `repo_root`, and `work.verify.task` validates its `worktree`, all through one
  check (absolute, existing, not a symlink, the top of a git work tree). A
  missing or unusable root is refused with the new `E_BAD_ROOT`; the sidecar's
  working directory is never substituted, and a devmap answer whose envelope
  names another repository is refused. This is a breaking change for hosts that
  called `devmap.*` or refine without a root.
- `hello` reports `op_limits`: each served op's `lane` and `deadline_ms` as
  enforced by the server (`"inline"` and 0 for ops answered inline), so a host
  sizes its client timeouts from the server. Additive; an older host ignores it.

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
- A refinement is now stored, not only returned. `work.enhancements.refine`
  reserves the profile's one generation slot (a `refine` enhancement, dc-store
  schema 12) before it spends the model call, so another generation in flight is
  refused as `busy` up front; the finished proposal is stored with its full
  source snapshot and grounding, and the reply carries `enhancement_id`. A run
  that fails or is cancelled records the failure and releases the slot at once.
  Apply it with the existing `work.enhancements.accept` (`fields` among
  `acceptance_criteria`, `split_into`, `duplicate_of`; `split_ids` names the new
  tasks) and reject it with `work.enhancements.dismiss`; accepting only adds:
  criteria are appended, duplicate links merged, and each split becomes a task
  with a `parent` link. A refinement cannot be revised or undone, and
  `work.enhancements.generate` refuses one. **Schema 12 is a hard fence:** a
  Manvi built against dc-store schema 11 refuses an upgraded profile, so move
  every host that shares a profile together; this build needs DevCouncil with
  schema 12 (the pin in `scripts/module-pins.txt` moves after that lands).
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
- Model tiers: `basic`, `lite`, `heavy`, `main` and `advisor`, cheapest
  first, each with a model and an effort (`llm.tier.<tier>`,
  `llm.tier.<tier>.effort`; defaults Haiku 5.5, Sonnet 5.5, Opus 5.5, Opus
  5.5, Fable 5.1, every effort unset). They are operator-scope settings, read
  from `MANVI_LLM_TIER_*`, `manvi flags set`, or the user config
  `<user config dir>/manvi/config.yaml` (moved with `MANVI_USER_CONFIG`; a path
  inside the repository is refused). A repository `config.yaml` that sets a
  tier key is refused outright, naming the lines, rather than ignored. A
  headless run or a TUI attach refuses to start, naming the tier, when a model
  is not in the provider's catalogue, is unpriced while
  `MANVI_ANTHROPIC_BUDGET_USD` is set, or has an effort it cannot honour.
  `manvi tiers` prints the resolved table with each value's source and exits 1
  on a profile a session would refuse. With the Anthropic provider and
  `MANVI_MODEL` unset, the session runs the main tier's model; `MANVI_MODEL`
  still wins.

- An account-wide Anthropic spend cap. Set `MANVI_ANTHROPIC_BUDGET_USD=200`
  and every Anthropic request, from any project or any concurrent session,
  draws from one ledger. By default that ledger is
  `<user config dir>/manvi/budget/anthropic.json`; `MANVI_ANTHROPIC_BUDGET_LEDGER`
  overrides it. A request whose worst-case cost would pass the limit is
  refused before it is sent. `manvi budget`, and `budget` in the TUI, show
  spent, retained, in-flight and remaining. A cap that is set but unusable (a
  bad amount, a relative path, a corrupt ledger) stops the provider from
  starting rather than letting it run unmetered.
  - Managed Claude agents started by `manvi serve` do not inherit
    `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN`, whether or not a cap is set.
    They are separate processes that call the API themselves, so with the key
    they could spend from the account without the ledger recording it. They
    fall back to their own login instead. Codex agents do not inherit
    `OPENAI_API_KEY`.
  - Rejections Anthropic documents as refusals of the request itself (400,
    401, 402, 403, 404, 413, 429, 529) release their reservation. 500, 504 and
    lost connections keep it.
  - A turn cut short after `message_start` is retained at its reported prompt
    cost plus the full output allowance. It used to keep its whole
    byte-sized reservation.
- Tier profile: advisor (Fable 5.1), main and heavy (Opus 5.5), lite (Sonnet 5.5)
  and basic (Haiku 5.5), each with a configurable model and effort
  (`llm.tier.<tier>`, `llm.tier.<tier>.effort`). Operator scope only: the
  environment (`MANVI_LLM_TIER_*`), `manvi flags set`, or a user config outside
  the repository (`MANVI_USER_CONFIG`); a tier key in the repository's
  `config.yaml` is refused at startup. A session refuses to start, naming the
  tier, when a tier's model is not in the catalogue, is unpriced under a
  spend cap, or cannot honour its effort. `llm.tier.main` supplies the
  Anthropic session model when `MANVI_MODEL` is unset. `manvi tiers` prints the
  resolved table with the source of each value.
- Claude Opus 5.5, Sonnet 5.5, Haiku 5.5 and Fable 5.1 in the Anthropic
  catalogue. `manvi probe anthropic` now uses Haiku 5.5.
- `manvi budget reconcile` retains the reservations of attempts whose process
  exited before resolving them. Every reservation does the same first, so a
  crashed fan-out no longer holds in-flight slots until every later request
  fails at the 256-attempt bound. Reclaimed attempts stay counted against the
  limit.
- Sub-agent fan-outs are admitted against the spend cap, and narrowed to it
  rather than refused for the sum of their worst cases. A request reserves a
  worst case — its whole prompt at the dearest input rate plus the full output
  bound — so eight children reserve many times what they spend. The dispatch is
  refused up front only when a child's first request cannot be reserved even
  alone, with its figure and the headroom in the message. Otherwise children
  start in order while the running children's reservations, plus the next
  child's, fit the headroom read when the fan-out began and the headroom read
  now; a waiting child starts when a running one finishes and its reservation
  settles to what it spent. Once a started child is refused by the cap, or
  nothing is running and the next child still does not fit, no further child is
  started; each one held back is reported with status `not_started` and reason
  `budget` (or `cancelled`, if the dispatch was cancelled while it waited),
  carries no writes, and is counted apart from failures.
- Escalation on evidence. A sub-agent that shows evidence of being out of its
  depth — the step ceiling reached, an empty summary, no verdict where one was
  asked for, or an end-of-turn check still failing after its bounces — is
  re-run once on the next tier up that runs a different model, from a fresh
  conversation (the original prompt and a short account of what the first
  attempt left, never its transcript or thinking). Effort is raised first, by
  the loop's own ladder. The advisor never escalates; a child on the parent's
  model or an operator-pinned role does not either. Each fan-out shares one cap
  of ceil(N/2) escalations. A re-run that shows the same evidence fails the
  child with a typed `ExhaustedError`; a re-run the provider or cap refuses, or
  a cancellation, with a typed `EscalationError`, and neither climbs further.
  The fan-out report lists each escalation (child, from, to, evidence, cold
  cache) under `escalations`.
- The usage line is priced and split by model. Each step is priced from its
  model's own table as it arrives, and the `turn.usage` event carries
  `cost_nano_usd`, `unpriced_steps` and `by_model[]`; the terminal prints the
  cost and a row per model. Steps no price table covers make the cost a lower
  bound ("at least $X") and are never shown as `$0.00`. See
  [Reading the per-model usage line](docs/TUI_AND_EVENT_SUBSYSTEM.md#reading-the-per-model-usage-line).
- `ANTHROPIC_WORKSPACE_ID`: when set, every Anthropic request carries it in the
  `anthropic-workspace-id` header, which an API key not scoped to a workspace
  needs (without it the API answers 400). A key created inside a workspace
  needs neither. The value is trimmed and must be letters, digits, `_` and `-`;
  anything else is refused before a request is built, and `manvi providers`
  reports the provider as misconfigured.

### Changed

- Every child process Manvi starts is given an environment it built, and none
  of them inherits a provider credential: every key variable in the credential
  catalogue plus `ANTHROPIC_WORKSPACE_ID` and `ANTHROPIC_AUTH_TOKEN`
  (`manvi/childenv`). Git children — of `manvi serve`, the bootstrap and the
  agent's git tools — get an allowlist (`gitAllowlist`), so a hook, filter,
  fsmonitor or alias in an untrusted repository no longer runs with the
  operator's keys, and a parent's `GIT_DIR`/`GIT_INDEX_FILE` no longer reaches
  Manvi's own git. Managed Codex and Claude agents, the `claude --version`
  probe, the desktop broker, the verifier, the `devcouncil` CLI and
  `pull`/`push`/`issues` inherit everything else. A contract test fails on any
  production spawn that does not assign an environment.
- Managed Claude agents never inherit `ANTHROPIC_API_KEY`, whether or not
  `MANVI_ANTHROPIC_BUDGET_USD` is set. It used to be withheld only under a cap,
  so without one a managed run silently moved from the CLI's subscription login
  to API billing on the host's key. They also no longer inherit
  `ANTHROPIC_AUTH_TOKEN`; `CLAUDE_CODE_OAUTH_TOKEN` still passes. Codex agents
  no longer inherit `OPENAI_API_KEY`.
- The DevCouncil pin moves to `1ae3bc15` (extract schema 94); gusset stays
  `984209b5`. A read-only git command whose arguments do more than read — an
  expansion, a path outside the root or a secret path, `--output`,
  `--no-index`, `-O`, an unsafe global or a `GIT_*` assignment — is reported
  as `command.git_read_argument` instead of `command.bypass_flag`.
  `command.bypass_flag` now names only the verification-bypass flags. What is
  refused is unchanged; only the label moved. Realigning the verify-check
  constants with gofmt changes no behaviour.
- `scripts/.dev-build.sh` builds a local darwin/arm64 binary into `dist/`
  without a release-tag stamp. `dist/` is gitignored.

- **The DevCouncil pin moves to wave 6 (`9add99c2`) and gusset to `984209b5`,
  together** (`scripts/module-pins.txt`). `crates/Cargo.toml` mirrors the new
  workspace `rust-version`, which `dc-grep` now inherits; without it the whole
  Rust workspace failed to load. `manvi/go.mod` gains `toolchain go1.26.9`:
  `govulncheck` under go1.26.6 reported ten reachable standard-library
  advisories (net/http, net/textproto, crypto/tls, os), none under 1.26.9.
  The `contracts/` mirror takes `lease.schema.md` and `verdict.schema.json` (the
  new hard rule `command.operand.unverifiable`) with their `CHECKSUMS`.
- **`command.operand.unverifiable` is not softened anywhere in Manvi.** A
  program the gate does not model, handed a path outside the root or one the
  restricted list protects (`go build -o /tmp/x`, `sqlite3 .devcouncil/...`), is
  a hard refusal in the agent gate, `manvi check` (status 7) and `manvi serve`
  under every posture. Manvi does not read `policy.unreadable_only` to relax it.
- **Grants are saved on every ledger change, atomically.** `manvi` wrote the
  grant ledger only when a grant was issued, so a revoke, or a single-use grant
  being spent, was undone by the next process. It now saves from `Gate.OnChange`
  through `safefile.WriteAtomic`, so a crash leaves the previous ledger rather
  than half of one.
- **Tier settings are read with their line numbers and block lists.** The user
  config and repository config go through `flags.ParseConfigLayer` /
  `Registry.LoadLayer`, so a block list written under a scalar tier key is
  refused naming the line instead of loading as an unset tier.
- **The MCP client speaks a real revision and checks the answer.** It asked for
  `2025-03-20`, which is no revision, so a server configured modern-first
  refused it. It now asks for `2025-03-26`, accepts only a handshake revision in
  the reply (a per-request revision such as `2026-07-28` is refused and not
  cached), returns a server's JSON-RPC error as a typed `*RPCError` (so `-32022`
  and its `supported` list are readable), and fails an in-flight call with the
  server's own reason when it answers `"id": null`.
- **`devcouncil_next_task` and `manvi watch` name a lease the store could not
  read.** A task held by a lease with an unparseable expiry is withheld by the
  store; both now say so (`unreadable_leases`) instead of reporting no ready
  task, or a complete watch report.
- `docs/KNOWN_GAPS.md` GAP-1 carries DevMap's measured per-language call-edge
  accuracy (fixtures only; no real-corpus labels exist).

- **The DevCouncil pin moves to the workbench schema 13 store** (`40b6bbbf`).
  Schema 13 shortens request receipts (a retry rebuilds its response from the
  event), so it is a hard fence: a Manvi built at the previous pin (schema 12)
  refuses a profile this build has upgraded with `schema_unsupported`. The
  upgrade still runs only on a write, after the `VACUUM INTO` backup, and
  `work.store.upgrade` takes it explicitly. Receipts a schema 12 store wrote in
  full keep replaying. `contracts/lease.schema.md` and `docs/WORKBENCH.md` name
  the new rung.
- **`manvi serve` never upgrades a shared workbench profile on a read.** With
  the DevCouncil store that upgrades only on a write, a `work.*` read of a
  profile one schema behind is refused with the typed error
  `schema_upgrade_required` and the profile is left untouched (an older
  GitPulse, MCP server or sidecar sharing the file is never locked out by a
  reader). A `work.*` write upgrades it after an online backup
  (`<profile>.pre-schema-<N>.bak`), and the new `work.store.upgrade` op does
  the same without writing a task. GitPulse upgrades at app startup. The serve
  contract gains `work.store.upgrade`; the store also gains `items.delete`
  refusing a task with a live run (`task_in_use`, `even_if_running` overrides)
  and an optional `scan_sequence` on task creation (`board_changed`).

- A task scope now fences every command that writes a file, not only the
  redirect and `sed -i`, `tee`, `cp`, `mv`. `policy.check.command` (and the
  harness's own shell tool) judges the files `rm`, `truncate`, `touch`,
  `dd of=`, `perl -i`, `install`, `ln`, `rsync` and `patch FILE` write against
  the same decision `policy.check.file` gives: outside the plan is
  `scope.unplanned`, a credential `path.secret`, outside the root
  `path.outside_root`. Under a declared scope, `git apply`, `patch` with no file
  operand, `xargs`/`find -exec`/`-delete` running a writer, and a recursive `rm`
  of a pattern write files the command text does not name and are refused as
  `command.substitution`; with no scope they are unchanged. DevCouncil pin
  `ec26e906`.
- Subprocess cost per tool call. The turn-start baseline seeds its scratch
  index from a copy of the repository's own index instead of `read-tree HEAD`,
  so `git add -A` reuses git's stat cache and re-hashes only changed files; the
  tree id is unchanged and a seed git refuses falls back to the old route.
  Untracked files in a diff are rendered by one `git diff` over an
  intent-to-add scratch index instead of one process each (258 git processes
  for 256 files became 5), and anything the batch cannot attribute exactly
  falls back to the per-file route. `graph_query`, `code_dead`, `graph_context`
  and the new-file reuse check read `devmap status` once per call instead of
  two or three times, and `graph_context` runs its per-file `devmap deps`
  queries five at a time, in input order, stopping on cancel. Measured numbers
  and the items closed without a change (dcgrep index, task cache, secret
  pattern fold, concurrent tool dispatch) are on the card ft-9bff2fd4.
- `docs/WORKBENCH.md` documents the bounded retention policy `store.compact`
  enforces (DevCouncil `docs/workbench-retention.md`).

- The crate-pin mirror check has one owner, `manvi/devcouncil/crate_pins_test.go`,
  and it compares against DevCouncil at the pinned commit, not the sibling
  working tree. The hand-rolled TOML check in `check-release.mjs` is deleted.
- A TUI repaints on the 100ms tick only while something animates (a running
  turn, a pending approval, the dashboard, a notice, the empty screen's rain,
  the composer's rotating tip); an idle terminal no longer redraws ten times a
  second.
- Session saves encode only the events appended since the previous save, and
  read the previous generation's dates from memory rather than parsing it:
  allocations per save of a 5000-event log fall from about 15,300 to about 230
  and the save is about a fifth faster. The file format, generation numbering,
  conflict rules and crash safety are unchanged.

- The gate certifies the build that ships. The dependency allowlist, both lint
  sets, `govulncheck` and `nilaway` also run under DevCouncil's
  `gusset-engine/cgo-env.sh` exports, and the cgo allowlist names the gusset
  engine. `go mod tidy -diff` is gated, `native/` (the desktop broker) gets
  format, clippy and tests, `GOOS=windows go vet ./...` guards the tests'
  portability, CI gains a `windows-latest` job that builds and tests the cgo-off
  Go plane, and CI installs golangci-lint, govulncheck, nilaway, cargo-audit,
  Rust and node at the versions in `scripts/tool-versions.env`.
- The local provider's credential seam returns `(Secret, present, error)`
  instead of a `samber/mo` Option, and `samber/mo` is removed from `go.mod`.
- `manvi providers` is a table: a ready count, ready providers first, a status
  mark and the state in their own columns, the credential's source and length
  (and the Anthropic workspace, when one is set), and for a missing key the
  variables to set with where to get one on the line beneath. It is coloured
  with the session's theme on a terminal and plain in a pipe or in `/providers`
  inside the full-screen face. `manvi doctor` output is unchanged.
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

- `ANTHROPIC_BASE_URL` is read as the host root, as the vendor SDK and Claude
  Code read it. Manvi used to treat it as including `/v1`, so the value those
  tools set (`https://api.anthropic.com`) requested `/messages` and every turn,
  including `manvi probe anthropic`, was refused with a 404 before any content.
  A value ending in `/v1` (with or without a trailing slash) keeps working: the
  segment is dropped rather than doubled.
- The end-of-turn check and `work.verify.task` no longer demote an otherwise
  passed verdict because no coverage file was supplied. The note stays on the
  report (`diff_coverage: no coverage file was supplied, so every changed file
  is reported unmeasured; pass one with --coverage or MANVI_COVERAGE to get a
  real answer`). A missing binary, an empty diff, or an omitted path still
  demotes. `work.verify.task` asks DevMap for affected tests in groups of 16
  targets, which is what the producer accepts. A change of up to 128 paths is
  split across those groups, and the split itself does not degrade the
  verdict. A failed batch, a truncated answer, or a walk that stopped short
  still does. Past 128 paths, only the first 128 are queried and the verdict
  is degraded for the cap, as before.
- An MCP request write that fails because the child closed stdin waits up to
  2 seconds, or until the call's context ends, for the server's read loop to
  drain stderr. The error then includes what the exiting server reported
  (`— the server reported: …`) when it wrote diagnostics. It used to return
  at once, so the same failure carried that explanation or not depending on
  whether stderr had been drained yet.
- `devcouncil_git_commit`'s staged-content scan forces `a/` and `b/` prefixes,
  passes `--no-ext-diff` and `--no-textconv`, unquotes a C-quoted name, and
  counts hunk lines from the hunk header. With `diff.noprefix`,
  `diff.mnemonicPrefix`, or a quoted name, the scan used to skip the file's
  content, and an added line beginning `++ ` used to end it. An added line
  it cannot attribute to a path, or a hunk header it cannot read, fails the
  commit closed.
- An artifact name is refused when any path component is a metadata sidecar,
  ignoring case, or a store temporary (a leading dot, the base name, 16 hex
  digits and `.tmp`). On a case-insensitive filesystem `x.META.JSON` is the
  sidecar of `x`, and a directory named `x.meta.json` occupies that name. A
  leftover write temporary is not listed as an artifact.
- `work.tasks.suggest` spends its link-read budget best-first, and only on
  inbox, backlog and ready cards: cards the request's repository and priority
  keep, then by priority, due date and board position. `in_progress` and
  `review` cards are not read. A task whose links were not read is counted
  under `excluded.unread` (required on the suggest result) and is not
  suggested or listed as blocked. `board.complete` is false when any open
  task's links were not read, and `board.unhydrated` counts those tasks in
  any column. A `repository_id` limits every count and the blocked list to
  that repository; a task outside it is not counted as filtered. Whether a
  task is blocked, or a duplicate, is still judged against the whole board.
  Inside a `duplicate_of` cycle the task with the lowest id is the one that
  can be suggested. Blocker ids on a blocked task are unique. An evidence
  list cut off before a verification record counts that candidate as
  unchecked. A suggestion with no ranking signal serializes `signals` as
  `[]`, and a task overdue by less than a day says "overdue".

- A file the agent rewrites is replaced, not truncated: the new content goes to
  a temporary sibling, is fsynced, and is renamed over the original, so a write
  that fails part-way (disk full, quota, I/O error) leaves the original intact.
  A failed fsync or close now fails the write, and a write that fails after the
  file may have changed still names the path so the end-of-turn check looks at
  it. The same applies to targets outside the repository.
- Artifact store: names ending in `.meta.json` are refused (they collided with
  another artifact's metadata), `Create` is exclusive, writes are atomic,
  `Update` no longer creates an artifact or resets its revision history over a
  missing or corrupt metadata file, and a failed write keeps the previous
  revision. `Update` on an artifact that does not exist is now an error.
- An untracked file that cannot be rendered into the verification diff (an
  unreadable file, a deadline that ran out mid-fan-out) is named in the diff
  notes with the reason (and "rendered N of M") instead of being dropped
  silently, and any diff shortfall makes `devcouncil_verify_task` report
  `verdict: "degraded"` with `passed: false`: never a pass, and never a failure
  on that account alone. A blocking gap still gives `failed`.
- The release workflow can publish. The publish job now fetches the pinned
  DevCouncil before `scripts/check-release.mjs` reads it, a tag no longer builds
  or publishes unless `verify.yml` passed at that commit
  (`scripts/release-verify-gate.sh`), every checkout drops its credentials, and
  Rust and Node are pinned (`1.99.0`, `22.22`). Releases carry `PROVENANCE.json`
  beside `SHA256SUMS`; neither is a signature (see the v0.0.6 notes). A
  `workflow_dispatch` run is a dry run that stops at `gh release create`.
- `check-release.mjs` fails by default when a sibling DevCouncil or gusset is
  not exactly at its pin, naming its HEAD, the pin, the commit distance and
  whether it is dirty. `--allow-unpinned` or `MANVI_ALLOW_UNPINNED_MODULES=1`
  turns that into a `NOT COVERED` line on a developer machine; CI, `--tag` and
  `--require-pins` ignore it.
- `scripts/fetch-modules.sh --link-main` lets a worktree nested under the main
  checkout build: it links the main worktree's pinned siblings instead of
  cloning, and refuses to replace anything that exists.
- Bench containment no longer leaks hidden tests and reference solutions through
  git. The shell and the verifier's checker and worker now also cannot read the
  git dir and common dir enclosing the benchmark (or a task), nor any other
  worktree registered in that repository (`git_guard_roots`), so
  `git show HEAD:bench/tasks/<t>/hidden_test.py`, `git cat-file` and
  `git -C .. log` are refused. The verifier's worker on macOS now denies
  network, as bwrap's `--unshare-net` already did on Linux. `containment_proves_itself`
  probes the git dirs. The shell's own network is unchanged on both platforms.
- Bench: the bootstrap draw is inlined (about 2.5x to 3x faster, bit-identical
  RNG stream, pinned by a randrange oracle in `test_stats.py`); the nine copied
  `check()` helpers are one `mh/checks.py`; `verify.sh` lints `bench/` and
  `scripts/*.py` with ruff's F and E9 rules (NOT COVERED when ruff is absent).
- `agent.CountRequestTokens` priced an image at zero tokens, so a computer-use
  session of screenshots never compacted. Images, top level or inside a tool
  result, now cost `ceil(w/28) * ceil(h/28)` visual tokens, capped at 4,784.
- `workflow.ResultFromState` and `computer.StuckReasonCode` guessed the failure
  code from the phase and from substrings of free text: a deadline or action
  limit reported `checkpoint_failed`, an external-interaction pause or human
  takeover reported `ambiguous_target` or `observation_budget`, and native text
  mentioning a "deadline" reported `deadline_near`. Both now read a typed
  `State.Cause`; `human_denied` is produced for a denied approval.
- Catalog publication never synced the directory after linking or renaming the
  published name, so a power loss could lose a revision.
- `Client.Act` and `Client.HumanAct` were one 193-node clone; they share
  `dispatch`.

- `verify.sh` and CI no longer pass when a check failed or never ran. The
  command-gate differential, the documentation contract and the cross-boundary
  counts read their `go test` run's exit status instead of masking it with
  `|| true`; a failed `go list` is not an empty dependency surface; the lint
  debt ratchet refuses a report holding `typecheck` rows or a failed run; the
  `nilaway` ceiling refuses a run in which a package dropped out of the
  analysis; and the nightly `--race` and `--fuzz` jobs keep a log and run the
  same missing-tool and unlisted-gap guards as the main job
  (`scripts/check-verify-log.sh`, with a per-mode `modes` field in
  `scripts/verify-expected-gaps.json`). The suite runs once, as `go test -json`,
  instead of twice.
- Provider streams that end without their terminal event no longer settle as
  success. openaicompat/xAI (no `finish_reason` and no `[DONE]`), anthropic (no
  `message_stop`) and gemini (no `interaction.completed` or `done`) now fail with
  a retryable `transport.TruncatedStream` error instead of running half-received
  tool calls as `{}` and recording zero usage; the budget ledger settles such an
  attempt at the prompt usage the stream reported or at its worst-case
  reservation. A gemini interaction that ends `failed` or `cancelled` is an error
  naming the status, and a tool call that was never closed (`content_block_stop`,
  `step.stop`) and carries no arguments is never surfaced as `{}`. A final
  `data: [DONE]` with no trailing blank line is a clean end (`SSE.Terminated`).
- Every provider client now bounds the wait for response headers
  (`Client.ResponseHeaderTimeout`: 2 minutes hosted, the 15-minute first-token
  budget on loopback; `openaicompat.Options.HeaderTimeout`,
  `SetResponseHeaderTimeout` on anthropic and gemini). It is a time-to-first-byte
  bound and never cuts off a stream already flowing; a timeout is a typed,
  retryable `transport.ErrHeaderTimeout`. The stream preflight that gemini had is
  now `transport.PreflightStream`, bounded by the first-token budget, and
  anthropic and openaicompat use it: an `overloaded_error`/`api_error` (or a
  transient openaicompat `error` chunk) that arrives before any content is
  retried like the 529/5xx it stands for, never after content has been emitted.
- openaicompat no longer rewrites string arguments (`,]` and `,}` inside string
  values were stripped by the JSON repair) and no longer runs prose as tool calls:
  text-channel tool-call recovery is `Options.TextToolFallback`, on for `local`
  and off for hosted xAI, and a fenced-JSON call is recovered only when nothing
  follows it. A call whose arguments were rewritten is reported as
  `Decoding.ArgumentsRepaired`.
- A shell command whose operand or input redirection names a secret path
  (`cat .env`, `cp secrets/x y`, `base64 < .env`) is refused by a new hard rule,
  `command.secret_read`, in every posture except `yolo`. Under the default dev
  posture it was a soft `command.no_lease` the posture demoted, so the shell
  printed credentials `devcouncil_read_file` refuses. `devcouncil_git_commit`
  now also refuses staged content that is, or contains, what a secret-path file
  or a known credential holds, so `cp .env notes.txt` then stage and commit no
  longer puts the credential in history under another name.
- `docs/POLICY_AND_SAFETY.md` named rules that do not exist (`cmd.secrets.leak`,
  `cmd.shell.destructive`, `path.credential`, ...). It now names only catalogue
  rules, says plainly that destructive commands have no dedicated rule, and a
  contract test checks every rule id the page mentions against the catalogue.

- The read gate now covers every tool that can return repository bytes, through
  one owner (`readRefusal`, `withholdDiff`, `withholdHistoryDiff` in
  `devcouncil/readauth.go`). `devcouncil_get_diff` withheld nothing, so an
  untracked or modified `.env` was rendered; `devcouncil_grep` and `get_diff`
  judged the name only, so a hard link to `.env` leaked; `devcouncil_git_show`
  printed a patch that adds a credential from history; `devcouncil_read_file`
  judged the typed name but opened the path the process working directory
  resolved it to; and the code graph's source spans were not judged at all.
  Each result now names what it withheld and the rule (`path.secret_read`, or
  `path.aliased_read` for a file with more than one name). A test enumerates
  the registry's read-only tools and fails if one is not classified.

- Secrets forwarded to an MCP server (declared `env` values and
  `env_passthrough` variables) are now scrubbed from what the server says on
  stderr, which every failed call attaches to its error. The tool, session-log,
  run, TUI, serve and watch scrubbers are all built by one function
  (`newArmedScrubber`) that also watches GitHub's token variables, so the
  quick-action and tool paths cannot drift apart again. Before, only provider
  keys were watched and a server echoing its token put it in the session file
  and the next request.

- `devcouncil_spawn_subagents` no longer runs an unregistered role `type` as a
  write-capable child on the parent's whole tool surface. Both dispatchers
  now resolve roles through one function (`resolveRole`): an unregistered name
  runs read-only and is reported as `unknown_types` on the result and
  `unknown_type` on the child's outcome. Previously only
  `devcouncil_invoke_subagent` failed closed.

- The end-of-turn check no longer skips a turn whose writes carried a policy
  rule. `progress.mutated` read any `Rule` as "refused", but the gate attaches
  one to allowed results too (demotion, grant, protected-file warning), so
  under `--yolo`, advisory mode, a grant, or a leased write to `package.json`
  the turn reported `Mutated=false` and the sensor answered "no change".
  Only `Blocked` now means refused.

- TUI: the loop no longer deadlocks on its own queue (its own notices and turn-start bookkeeping are sent without blocking and drained in the same iteration), every producer outside the loop (event sink, `Send`, session effects, a turn's final report, the approval seam) returns once the runner stops instead of parking forever, and quitting now cancels every running turn and waits (bounded, 15s) for it to finish saving its log before `Run` returns, so session leases are released only after the turns have ended.
- Managed runs: an approval whose decision was delivered no longer fails a Claude run at the callback's expiry (expiry applies only to callbacks still awaiting a person) and its decision row now ends `resolved` ("Decision delivered to the provider") rather than `cancelled`, "delivery may be uncertain"; a Claude `control_cancel_request` withdraws the pending callback (row `cancelled`) and an answer racing it no longer fails the run; a `Prepare` that failed before `runs.claim` no longer locks the run id in the process; the 32 MiB stdout cap is documented as per session and its failure reason says so.
- The end-of-turn check no longer loses the turn's pre-turn baseline after its first read: a bounced turn's re-check now runs against the same captured tree instead of being forced to `degraded` with a false "no pre-turn baseline was recorded".
- `Loop.Run` closes the turn and step it opened on every exit, so a provider failure, a cancel or a refused pre-step no longer leaves a `turn.start` with no `turn.end` (or a dangling `step/start`) in the log and the NDJSON stream; `NewLoop` refuses a nil bus instead of letting `Run` panic.
- `kill_all` no longer reports finished or already-terminating sub-agents as termination failures, a second kill of a `canceling` child is refused instead of reported as success, a child whose completion is refused after a kill ends `errored` rather than `canceling` forever, and the instance manager keeps at most 128 finished instances.
- MCP client: a server-initiated request is now answered instead of dropped (`ping` gets an empty result, every other method, `roots/list` included, gets -32601), `initialize` no longer advertises a roots capability nothing serves, and a `Call` that times out or is cancelled sends `notifications/cancelled` with its request id so a non-idempotent tool is not left running unannounced. Spec: modelcontextprotocol.io basic/utilities/ping, basic/utilities/cancellation, client/roots.
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
- Cache writes were dropped when a turn's usage was summed, for the parent and
  for every sub-agent, so the dearest input tokens a turn bought were reported
  as never having been bought. Each child's usage now also names its model and
  carries its cost.

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

[Unreleased]: https://github.com/bharathvbcr/Manvi/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/bharathvbcr/Manvi/compare/v0.0.6...v0.1.0
[0.0.6]: https://github.com/bharathvbcr/Manvi/compare/v0.0.5...v0.0.6
[0.0.5]: https://github.com/bharathvbcr/Manvi/compare/v0.0.2...v0.0.5
[0.0.2]: https://github.com/bharathvbcr/Manvi/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/bharathvbcr/Manvi/releases/tag/v0.0.1
