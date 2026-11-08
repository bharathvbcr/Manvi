# Profile workspaces and task storage

The workbench stores persistent repository groups and Kanban items in one profile
SQLite database. Its schema, validation, queries and transactions live in
[`dc-store`](../crates/dc-store/src/workbench/mod.rs), a DevCouncil component that
Manvi wraps. Repository execution tasks and writer leases remain a separate model. A host must supply a different database
file for its profile; opening that file also creates the store's legacy tables,
but workbench operations only access `work_*` tables.

This implementation provides storage, the local host API, asynchronous enhancement
generation and a durable proposal lifecycle. GitPulse has basic boards, editors,
enhancement review, terminal handoffs, a structured request inspector and a macOS
notification adapter. Managed Codex now uses its stdio App Server protocol, with
native activation and durable request delivery. The real read-only path is verified;
real-account approval modes, managed Claude, code review, sync and installed
notification qualification remain open. Passing storage tests does not
qualify those workflows or establish whole-application performance.

## Enable the local host API

```sh
MANVI_HARNESS_INIT_ENABLED=false manvi serve --workbench-db /absolute/profile.sqlite
```

Create the parent directory first. `MANVI_STORE_BINARY` selects the `dcstore`
binary through the existing tool resolver. The Go client uses the existing lazy
store process pool, with a maximum of eight children and the existing ten-second
call deadline. A serial host uses one child. Closing the server closes its pool.

The flag is optional; only a server configured with it advertises `work.*`
operations in `hello.ops`. The operation set describes the host handlers;
successful storage requests additionally require a compatible `dcstore` binary.
An old or unavailable binary produces an error, never an empty successful board.
A `dcstore` build that reports its profile schema answers `dcstore --version`
with `{"ok":true,"id":"dcstore","component":"dc-store","version":"…","workbench_schema":14}`;
earlier builds print the same object without `workbench_schema`.

The API is for the owning local host. Possession of that stdio connection permits
profile CRUD. These operations are not registered as agent tools, do not grant
execution permissions, and do not acquire or renew repository leases. An
agent-facing API with writes is decided (owner, 2026-11-07) but not built: it needs
the scope and authority checks in
[Owner decisions of 2026-11-07](#owner-decisions-of-2026-11-07) before any write
is exposed. Until that implementation is reviewed and lands, nothing here is
callable by an agent.

## Operations

All names below receive a `work.` prefix over `manvi serve`.

| Resource | Methods |
|---|---|
| Workspaces | `workspaces.list`, `workspaces.get`, `workspaces.put`, `workspaces.delete` |
| Repositories | `repositories.list`, `repositories.get`, `repositories.put`, `repositories.relink` |
| Items | `items.list`, `items.get`, `items.brief.get`, `items.put`, `items.delete`, `items.restore`, `items.history` |
| Changes | `events.list` |
| Enhancements | `enhancements.create`, `enhancements.claim`, `enhancements.complete`, `enhancements.recover`, `enhancements.get`, `enhancements.list`, `enhancements.accept`, `enhancements.revise`, `enhancements.dismiss`, `enhancements.undo` |
| Automatic scheduling | `automation.get`, `automation.put`, `automation.list`, `automation.prepare` |
| Run records | `runs.prepare`, `runs.claim`, `runs.started`, `runs.protocol`, `runs.finish`, `runs.cancel`, `runs.get`, `runs.list` |
| Managed Codex (Go host) | `runs.managed.prepare`, `runs.managed.activate`, `runs.managed.stop` |
| Generation (Go host runner) | `enhancements.generate`, `enhancements.configuration` |

`.put` creates or replaces a record. Supply every editable field that should
survive replacement; omitted optional fields return to their defaults. It is not
a partial patch, with these exceptions on `items.put`: `locked_fields`, `logs`,
`archived`, `checklist`, `links` and `scope` keep their stored value when omitted
or null, so a host that does not edit a field (an agent's status move, a merge, an
older code path) cannot silently unlock, unarchive, uncheck, unlink or rescope a
task. That set is exact: `labels` and every other editable field are replaced
when omitted. A test pins the set (omit each preserved field and read it back
unchanged; omit `labels` and read it back cleared).
Supply `[]`, `""` or `false` to clear explicitly. `completed_at` belongs to the
store; an `items.put` naming it is refused as an unknown field. Automation
settings also preserve the provider/model pair when
both are omitted; see the scheduling contract below. Mutations require
`request_id`, `id` and `expected_revision`.
Revision zero creates a new record. An edit or deletion must match the latest
revision. IDs contain 1–128 ASCII letters, digits, hyphens or underscores.

An accepted mutation, its revision snapshot, event and idempotency receipt commit
together. Retry an uncertain request with exactly the same request ID, method and
JSON field order/content; insignificant JSON whitespace is normalized. Replays
return the original receipt even after later edits. Reusing that ID for another
payload returns `idempotency_conflict`. Refused transactions consume neither IDs
nor event sequence entries. There is no automatic retry on transport failure.
The deliberate exception is `runs.claim`: replay of an accepted claim returns
`claim_consumed`, never a second launch authorization. Read `runs.get` to reconcile
an uncertain claim; do not treat an old successful receipt as permission to spawn.

```json
{"id":"register","op":"work.repositories.put","params":{"request_id":"repo-1","id":"manvi","expected_revision":0,"name":"Manvi","identity_key":"clone-uuid-1"}}
{"id":"group","op":"work.workspaces.put","params":{"request_id":"group-1","id":"devtools","expected_revision":0,"name":"Devtools","repository_ids":["manvi"]}}
{"id":"create","op":"work.items.put","params":{"request_id":"task-1","id":"startup","expected_revision":0,"title":"Reduce board startup latency","kind":"improvement","description":"Measure and reduce the time to the first usable board.","acceptance_criteria":["Record cold and warm measurements."],"repository_ids":["manvi"],"primary_repository_id":"manvi","home_workspace_id":"devtools"}}
{"id":"board","op":"work.items.list","params":{"workspace_id":"devtools","archived":false,"limit":100}}
{"id":"archive","op":"work.items.list","params":{"workspace_id":"devtools","archived":true,"order":"completed","limit":100}}
```

Omitting `archived` lists both sides, so a board reads `archived:false`.

Successful host responses wrap the store document in `result`. That document
contains `ok:true`, `item` and `sequence` for a mutation, or the page fields below
for a list. A refusal uses the host's outer `ok:false` and typed `error` field.
Logical refusals preserve the running session and its store child.

## Canonical task briefs

`items.brief.get` accepts only `id` and a positive `expected_revision`. It reads
the live task, every ordered repository link, and the optional home workspace in
one SQLite read transaction. A stale task revision returns `revision_conflict`;
a deleted task returns `not_found`. It writes no event, receipt or task revision.

The result's `item` contains `id`, `revision`, `updated_at`, `format_version:1`,
the exact full `task`, ordered `repositories`, optional `workspace`, and canonical
`markdown`. Repository/workspace references contain only ID, revision, update
time and name. Repository identities and remote URLs are omitted from the export.
Changing a repository name updates that reference's revision even when the task
revision stays unchanged. The snapshot is consistent at read time; it is not a
launch authorization or proof that the underlying checkout remains unchanged.

Markdown preserves the saved title, description and acceptance criteria and
includes task metadata, repository revisions and primary-repository designation.
Missing criteria remain explicitly missing. A task with a checklist gains a
`## Checklist` section with each entry's done state; a task with links gains
`## Linked tasks`, listing the links it names and the live tasks that name it
(at most 128 lines, a linked task deleted since shown as deleted). Export refuses inconsistent links
and oversized replies instead of dropping repositories or truncating instructions.
The complete response fits the existing 2 MiB transport budget; large valid briefs
can exceed both the 256 KiB request limit and an individual terminal argument.

GitPulse's **Copy saved brief** reads this operation and validates the returned
task/repository revision vector. Unsaved edits are excluded and identified in the
copy result; uncertain saves and active enhancement writes disable copying.
Clipboard content is the store's Markdown, with no second frontend formatter.
Saved-file terminal handoff and managed Codex both consume that immutable brief;
see their launch contracts below.

## Records and board scopes

Workspaces have a name, description, icon, color, ordering position, archive/pin
flags and an ordered repository membership array. Repositories may belong to
multiple groups; each workspace supports up to 10,000 membership entries, subject
to the overall request byte limit. Deleting a group preserves repositories and
items. It clears affected home-workspace references, increments those item
revisions and records their history in the same transaction.

Repositories have a name, immutable unique `identity_key` and optional `remote_url`.
The host owns identity assignment; a matching remote does not merge clones.
`repositories.relink` points an existing repository at the checkout it moved to:
it takes `identity_key` and optional `name`, keeps the ID (so task links,
memberships and runs stay attached) and keeps the old identity in history. It
refuses while a run is active on the repository (`repository_busy`). An identity
another record holds is refused unless that record is named in `absorb_id` and
holds no task links, memberships or runs (otherwise `repository_not_empty`); it
is then removed in the same transaction. Two distinct clones are never merged.
Checkout discovery and checkout aliases are not yet part of this API. Remote
URLs are metadata and are never executed by these operations.

Items support title, description, kind, status, priority, optional severity, owner,
due date, labels, acceptance criteria, ordering position, repository links,
pasted `logs` (up to 256 KiB), and the schema 11 fields below: `archived`,
`checklist`, `links` and the store-maintained `completed_at`.
Saved items need 1–64 distinct registered repositories, including their primary
repository. A home workspace is optional. Titles are limited to 300 characters
and 1,200 UTF-8 bytes; descriptions to 64 KiB. Status is one of `inbox`, `backlog`,
`ready`, `in_progress`, `review`, `done`; priority is 0–3. Due dates are optional
nonnegative epoch integers. Dates and revisions must fit the supported exact
integer range. Custom nonblank kind strings are accepted.

`locked_fields` contains distinct `title` and/or `description` entries. Locks
prevent enhancement requests, acceptance and undo from changing those fields;
they do not prevent a human's ordinary task edit. The native task editor exposes
the locks alongside the description, and they take effect after saving.

### Schema eleven: archive, checklists, links and restore

`archived` is a boolean independent of `status`: Done is a column, not the
archive. Changing `archived` does not change `status`, so a restored task
returns to the column it left.
`completed_at` (Unix seconds) is set when a task enters `done`, kept while it
stays there and cleared to null when it leaves; no request can set it.

`checklist` is an array of at most 128 `{"text","done"}` objects with nonblank
text of at most 4,096 bytes and a boolean `done`. `links` is an array of at most
64 distinct `{"kind","item_id"}` objects; kinds are `parent`, `blocks`,
`related` and `duplicate_of`. A task cannot link to itself, has at most one
parent, and cannot take one of its own descendants as parent. A new parent link
is refused if the chain it would make — the parent's ancestors, the parent, the
task and its deepest subtask — holds more than 256 tasks; a parent the task
already has is not checked again. A new link must name a live task;
a link the task already holds survives its target's deletion, so deleting a
task never makes the tasks that link to it unsaveable. Links are indexed in
`work_item_links`; the task body remains the record of truth.

`items.restore` takes only `request_id`, `id` and `expected_revision`, where the
revision is the one the deletion produced. It clears the deleted flag at the
next revision, keeping repository links, task links and history; queued
enhancement suggestions discarded by the delete are not re-queued. A live task
returns `invalid_state` rather than a revision conflict. `items.put` on a
deleted ID is still refused; restore it instead.

The 10 → 11 upgrade (`items.sql`) adds `archived` and `completed_at` as
indexed VIRTUAL generated columns, adds `work_item_links`, and marks every Done
task archived, because every earlier reader treated Done as the archive. Each
migrated task's `completed_at` is recovered from the start of its current Done
streak in history, falling back to `updated_at`. Every other task gets
`archived:false`, `completed_at:null`, an empty checklist and no links, so
pre- and post-upgrade rows read the same shape. No revision is bumped, so a
host's pending write is not invalidated.

## Durable terminal attempt records

Schema five adds `work_runs` and immutable `work_run_inputs`. Upgrade is
transactional and preserves existing tasks and automation. Hosts must use the
same supported store version; older binaries refuse the newer profile.
These APIs record intent and host observations. They do not launch Claude Code or
Codex, enforce provider permissions, or prove process termination.

`runs.prepare` requires mutation identity with revision zero, `task_id`,
`source_revision`, `repository_id`, `repository_revision`, `provider` (`codex` or
`claude`), `permission_mode`, absolute `cwd`, `git_dir`, `git_common_dir`, and an
optional full `head_oid`. `head_ref` records `refs/heads/...`; null means detached
or unobserved for direct store callers. Modes are `inspect`, `ask`, `edit`,
`auto_review`, `preapproved`, and `bypass`. `acknowledge_bypass:true` is required
exactly when requesting bypass. These are requested modes, not verified effective
provider capabilities or grants. An optional `model_choice` object (at most eight
lowercase field names with nonblank string values of at most 128 bytes, such as
`{"model":"opus","effort":"high"}`) records what the launching host chose for
this attempt; it is stored as null when omitted.

The selected repository must be linked to the saved task at the supplied
revision and registered as `local:<git_common_dir>`. Storage cannot verify the
supplied paths. The native host must inspect them. Preparation captures the
canonical task brief once in the same transaction as the run and receipt. Only
`runs.get` adds `item.brief`. Run lists omit output and effective configuration;
protocol mutation receipts and stored history can retain those bounded fields. Task deletion preserves the snapshot and run history.

Preparations expire after five minutes. One prepared/active/unresolved attempt
may reserve a checkout (a linked worktree is its own checkout). Across the
profile, the host names the limit in `runs.prepare`'s `max_active_runs`,
between 1 and 64; a host that names none gets 8. Expired unclaimed
preparations release capacity; expiry never releases a claimed or unresolved
run.

| Method | Required state and result |
|---|---|
| `runs.claim` | Unexpired `prepared`; supplies `owner_id` and `session_id`, rechecks the complete task/repository/workspace snapshot, enters `starting` exactly once |
| `runs.started` | `starting`; matching owner/session, positive `process_id` and nonblank `process_start`; records `running` |
| `runs.finish` | Matching owner/session; `failed` only for a known pre-start failure; `exited` requires a recorded process; `unresolved` preserves uncertain starting/running work and its capacity |
| `runs.cancel` | Only unclaimed `prepared`; records `cancelled` |

`runs.finish` requires `outcome` and `reason`; `exit_code` is nullable and only
valid for `exited`. A terminal outcome never marks a task Done or accepts its work.
Recovery of an unresolved start without recorded process identity still needs a
native termination-proof workflow; it cannot be cleared as a successful exit.
`runs.list` supports `task_id`, `repository_id`, `state`, `limit`, `newest`, and
opaque `cursor`, with exact counts and bounded metadata pages. `newest: true`
orders creation position and ID descending; the default remains ascending.
Keep the same filters and direction when following a cursor. Equal timestamps
use the ID as a stable tie-breaker, so pagination does not omit tied attempts.

GitPulse adds native `runs.prepare_terminal` through its existing workbench IPC.
It accepts the preparation IDs/revisions/provider/mode and `repo_path`, validates
an actual working checkout, and supplies observed Git directories, commit and
branch to `runs.prepare`. It accepts linked worktrees and refuses mismatched
clones, bare/missing repositories and broken HEAD. Native `runs.claim` rechecks
these observations before the canonical transaction consumes the claim, including
a branch switch at the same commit. Malformed native inputs are refused before
filesystem/profile I/O. Filesystem checks are observations, not locks against
external Git tools or proof of checkout identity after a directory is replaced
with an otherwise identical clone. Launch-time fencing remains required.

GitPulse's Runs panel now prepares and opens these attempts through its existing
PTY manager, with private brief files, CLI help negotiation, child environment
normalization and process outcome receipts. Terminal handoffs delegate requests
to the provider terminal. Managed Codex and Claude have the separate protocol below; complete
crash recovery and code review remain open.

## Managed provider connection

Schema nine introduced immutable `kind`: `external_terminal` (the default) or `managed`.
`runs.claim` must specify the same kind; an old terminal adapter cannot claim a
managed attempt. Schema ten requires native process identity before provider
initialization and permits a failure receipt without an invented thread. Both
binaries must support schema ten. Its transactional contract-version migration
preserves existing attempts and does not reclaim unresolved legacy launches.

GitPulse adds `runs.prepare_managed`, `runs.launch_managed`,
`runs.stop_managed` and `runs.follow_managed` through its existing native IPC.
Preparation reuses the same Git checkout validation as terminals; Codex and
Claude are supported, with native attempt IDs of at most 100 characters.
Stop accepts only the saved ID. Launch accepts the saved ID and, only for a
follow-up, `parent_run_id` of another saved run. Follow accepts the saved ID
and an optional message, and GitPulse sends it only to a worker that is
already live. The native host serializes launch preparation and supplies the
OS-observed process birth; renderer-supplied process identity, a provider
thread id, raw `runs.started`, `runs.finish`, `runs.protocol`, and the managed
prepare/activate host operations are refused.

The owning `manvi serve --workbench-db` host advertises six methods (the sixth,
`runs.managed.continue`, is described after the list):

1. `runs.managed.prepare` takes `id`, `request_id`, `expected_revision:1`. It
   consumes the canonical claim and starts only the stdio process. It returns
   `protocol_version:2`, `phase:awaiting_activation` and the same owner/session/PID
   for an exact retry in that live host. It has not initialized a provider thread
   or sent the task. GitPulse rejects incompatible receipts and requests cleanup.
   Unactivated helpers expire after five
   minutes. GitPulse uses a stable `managed_launch_<run ID>` request identity.
   The claim's `owner_id` is `manvi-{pid}-{unix nanos}-{random}`: this Manvi
   process and the moment it claimed. A host reconciling after a crash
   releases an attempt that never recorded a provider process only when no
   process holds that pid, or the one that does started after the stamp.
2. `runs.managed.activate` takes the prepared `id`, `owner_id`, `session_id`,
   `process_id` and native `process_start`. It first records the running process,
   then initializes an ephemeral thread, verifies the returned checkout/approval/
   sandbox settings, saves `runs.protocol ready`, and sends one model turn.
   A follow-up is the exception, and only when prepare named a `parent_run_id`:
   the new process resumes that saved session and its one turn is the queued
   message. A first turn is unchanged.
   An uncertain activation consumes its latch;
   it cannot authorize a second turn. GitPulse rechecks the checkout immediately
   before this call and stops the prepared session if activation cannot complete.
3. `runs.managed.stop` accepts `id` and requests cancellation of the exact locally
   owned session. Its reply confirms the request, not process termination.
   Cancellation is the job context, which kills the provider process group.
4. `runs.managed.wake` takes an optional `id`. An empty id wakes every live
   managed job on this host; an unknown or ended run still answers ok. It does
   not read the store. The drive loop reads saved decisions when it is woken,
   and again before it fails a run whose unanswered callback has reached its
   own deadline. GitPulse calls it after a successful `decisions.decide`, on
   the worker that is already live.
5. `runs.managed.follow` takes `id` and an optional `message`. One nonblank
   message of at most 128 KiB may be queued while that run is still going. A
   second message while one is pending is refused, not replaced. A body with
   no message is a status read and does not write the queue. The queue is
   armed only after the provider process closes cleanly, the protocol records
   `completed`, `failed`, or `interrupted`, and `runs.finish` succeeds. A
   timeout is never armed, even when the recorded provider state is `failed`.
   A queued message whose run settled without arming is reported as not kept,
   not as still queued. The next `runs.managed.prepare` passes `parent_run_id`
   (the saved run id, never a thread id). The new process resumes that session
   (`claude --resume`, or Codex `thread/resume`) and its one StartTurn is the
   queued message. The queue lives in the sidecar process; when that process
   exits, a message that was not launched is gone. Bypass on the parent is not
   copied onto the new attempt.

`runs.managed.continue` (wire op `work.runs.managed.continue`) is the sixth
method: another turn for a run, in the same process and on the same session,
after its previous turn completed. It is opt-in per attempt. `runs.managed.prepare`
takes an optional `max_continuations` (0 to 8, absent is 0). A run prepared with
more than 0 does not end when a turn completes: the drive loop settles that
turn's callbacks, checkpoints the run as `running`, and parks with the provider
process alive. A run prepared without it behaves exactly as above. Prepare
refuses `max_continuations` for a provider without the mechanism
(`E_CONTINUE_UNSUPPORTED`) before the claim, so the saved attempt stays prepared.
Claude Code 2.1.316 and codex-cli 0.153.4 both support it, measured against the
installed CLIs: Claude takes a second `user` frame on the same stream-json
process (and repeats `system/init` for it); Codex takes a second `turn/start`
for the same `threadId`, ephemeral threads included.

The body is `{id, request_id, message?}`. With a `message` it delivers one
follow-up; without one it reads the run's state and writes nothing. A message is
nonblank, at most 32 KiB, and needs a `request_id`. The answer is
`{state, request_id?, turn, max_turns, idle_deadline?}` with `state` one of
`delivered` (the message was written to the provider; that does not say the
new turn has begun, which the state read shows), `parked`, `running`,
`unresolved` or `ended`. `running` is a turn in flight, and is also what a
run reports before its first turn has begun (`turn` 0) and in the moment after
a turn completes while its callbacks are settled and the checkpoint is written,
before it parks. `turn` counts the turns started, the first
included; `max_turns` is `1 + max_continuations`; `idle_deadline` (Unix
seconds) is when a parked run ends if nothing arrives (10 minutes). A run on its
last permitted turn ends when that turn completes.

Refusals are typed and, except where marked, write nothing to the provider:
`E_CONTINUE_UNSUPPORTED` (the provider, or a run prepared without
`max_continuations`), `E_TURN_IN_FLIGHT` (retryable; a turn is running),
`E_CONTINUE_LIMIT` (all turns used, or the run's saved output has no room for
the claim), `E_CONTINUE_RATE` (retryable; follow-ups of one run are at least two
seconds apart), `E_RUN_ENDED` (the run ended, was stopped, or is not this host's),
`E_CONTINUE_PENDING` (retryable; the wait for the outcome ran out, ask again with
the same `request_id`) and `E_BAD_REQUEST`. `E_CONTINUE_UNRESOLVED` is the one
that is not a refusal: the delivery cannot be told from a lost one. The message
is not sent again, the run ends with `provider_state` `unresolved`, and every
later call for that run, the same `request_id` included, answers the same way.
A retry of an answered `request_id` returns the first answer and sends nothing;
reusing it for a different message is refused.

A refusal that wrote nothing is not always one the host may retry. Only a turn
in flight, the rate window, and an outcome not yet known are marked retryable.
A provider that answered that it started no turn, and a refusal the host does
not recognise, are `E_DEPENDENCY` and not retryable: the host is not told that a
retry will help when it does not know why it was refused. A retryable refusal
that wrote nothing leaves the rate window as it was, so the host may offer the
follow-up again at once; a delivery uses it up, and so does a refusal that
cannot succeed. A host that loops on one of those is turned away with
`E_CONTINUE_RATE` before the provider and before the saved output, so it adds one
`[follow-up N not sent: ...]` note to the record, not one per attempt.

A provider process that exits while the run is parked is noticed when it
happens, not when the idle budget would have ended the run: the run ends as
`failed`, with the reason "the provider process exited while the run was parked
between turns" and a note in its saved output, and a follow-up claimed in the
same instant is answered `E_RUN_ENDED`. It is not recorded as `completed`: the
last turn did complete, but nobody asked for the run to end.

Which of the two follow-up mechanisms to use. `runs.managed.follow` queues one
message for a later attempt (`parent_run_id`, `resume`), which is a new process
that resumes the saved session after this run has ended. `runs.managed.continue`
takes the next turn in the same process of a run that is still alive and
parked. A host that prepared the run with `max_continuations` and sees `parked`
should use `continue`. A `follow` queued on a parked run is armed when that run
later ends and is launched as a new attempt then; it does not reach the parked
process. The two do not share a queue, so a host should choose one per run.

Before the message is written, a claim line (`[follow-up N claimed: request
"...", B bytes, sha256 ...]`) is appended to the run's saved output and the
checkpoint carrying it is acknowledged by the store; the store has no other
field a follow-up could be recorded in. What the store shows differs from a
one-turn run in two ways a reader must know. A parked run stays `running` with
provider state `running`, because the store never lets a provider state leave
`completed`; the state read above is how a host learns it is parked. And
`provider_turn_id` stays the first turn's id for the whole run (it is
immutable), while a follow-up turn's id is carried by the approvals raised in
it: each decision row is bound to the follow-up turn, and the adapter refuses
an answer whose request, turn, thread or payload digest differs. Permission
mode, sandbox and the effective configuration are fixed for the run and do not
change per turn.

Budgets that span turns: the two-hour bound on a managed job covers the whole
run, parked time included, so eight 45-minute turns are not reachable; each
turn gets its own 45-minute active-time budget inside it. The parked idle
deadline is set once when the run parks and is not extended by a refused
follow-up. The session's callback budget (2048 requests, 32 open) and the 32 MiB
provider-output cap are per process, so they are shared by all of its turns.
A callback of a finished turn is settled in the store when the run parks (a
decision already written is recorded as delivered, any other as cancelled). Claude
also drops the finished turn's answered entries from its callback map at a
follow-up; Codex removes an entry when the provider reports it resolved, so
neither provider carries an answered callback into the next turn.

Executable lookup, validation and spawn errors are classified as known unstarted
only by the process adapter. They finish as `failed` and release the repository
reservation. A generic factory error stays `unresolved`; an error result containing
a typed nil session is never dereferenced. A handshake failure retains native
process identity and sends no model turn. Confirmed direct-child reaping records
`exited` separately from `provider_state:failed` or `interrupted`; unconfirmed
reaping stays `unresolved` and holds capacity. The original failure reason remains
visible, and a failed provider produces a failure notice even with exit code zero.
The task's revision/status are unchanged. Retry creates an explicit new attempt
with its own permission selection; no failure automatically relaunches a provider.

For an uninitialized managed attempt, host-only `runs.finish` may supply terminal
`provider_state` (`failed`, `interrupted`, `unresolved`). It cannot introduce a
successful provider state, overwrite an existing protocol state, or attach managed
evidence to a terminal attempt. Initialized providers continue to own their state
through `runs.protocol`. Abandoned preparations and host-crash outcomes without
sufficient process evidence remain unresolved; timeout does not prove termination.

`MANVI_CODEX_BINARY` selects the Codex executable through the existing tool resolver;
otherwise the host resolves `codex` on PATH. `MANVI_CLAUDE_BINARY` does the same
for Claude, otherwise `claude` on PATH, and it must not be a shell wrapper. The
Codex adapter launches `app-server` with argument arrays, never shell
interpolation. stdio is that process's default; `--listen stdio://` is not
passed, because current Codex exits without a session when it is. The Claude
adapter launches `claude --print` with stream-json input and output,
`--permission-prompt-tool stdio`, `--setting-sources user`, and the run's
permission mode and session id. A new session still passes `--session-id`.
A follow-up swaps that flag for `--resume` and does not pass both. A follow-up
Codex launch calls `thread/resume` with the stored thread id and does not mark
the thread ephemeral.
It retains the initialized provider user agent and returned effective model,
provider, approval reviewer, approval policy, sandbox and thread identifiers.
The observed configuration and provider thread become immutable for this attempt;
a turn ID can be filled once. No global permission preference is changed.

Unlike Claude, managed Codex has no launch-shape refusal and no provider-reported process
identity check: the `initialize` reply is read only for `userAgent`, so a wrapper passes the
handshake. The process id the host records and the native host observes at activation is the
process Manvi spawned. Point `MANVI_CODEX_BINARY` at the native `codex` executable (for
example the standalone install's `~/.codex/packages/standalone/current/bin/codex`) rather than
a shell wrapper or an npm node launcher: if a launcher runs the native binary as a child, the
recorded id and start time describe the launcher, not the process running the model.

Requested modes map to Codex's advertised protocol settings. Inspect uses
read-only/never; Ask uses read-only/on-request; Edit uses workspace-write/on-request;
Preapproved uses workspace-write/never; Automatic review uses
workspace-write/on-request with the provider's `auto_review` reviewer; Bypass uses
danger-full-access/never and requires explicit acknowledgment for this attempt.
The adapter refuses extra configured writable roots or sandbox network access in
non-bypass modes. Reported temporary-root exclusions are retained. This is policy
verification, not proof of OS containment, containment of external MCP services,
or termination of escaped descendants.

Command and file approval callbacks retain their complete payload (file requests
also require a captured proposed change). Only one-time accept or decline is
mapped; session grants, rule amendments, `grantRoot`, unknown callback methods and
secret questions are refused without a grant. Up to three questions are presented
with separate fields and suggested options. Multi-question answers are serialized
by question ID internally; users do not enter protocol JSON.

Before consuming a durable decision claim, the adapter validates the answer and
drains its bounded buffered events so an already-resolved request cannot receive
a new approval. It repeats validation before the one-use write. Remote resolution
can still race that write, so uncertain delivery is never retried. A
`serverRequest/resolved` event confirms callback resolution, not tool success.
Provider turn completion, direct-child reaping and human task acceptance are
separate facts. Model completion never moves or accepts the task.

Limits: 64 active managed sessions per runner (`MaxManagedSessions`, equal to
the store's ceiling, so it never refuses a run the store admitted under the
host's limit); a 45-minute turn budget; 256 KiB protocol
frames; 32 MiB stdout per session (cumulative over the whole session and
counting every frame the provider writes, including frames the retained record
does not keep in full; a session that reads a great deal can reach it, and its failure
reason says so) and 64 KiB discarded stderr; 32 pending callbacks and 2,048
callback IDs per connection; a 64 KiB complete request; 128 KiB retained output
and 64 KiB effective configuration. The retained output is the turn text. When
a tool snapshot fits entirely in what remains of that cap it is appended after
a `[tools]` line as one JSON array, replaced by tool id rather than logged, and
a snapshot that would be cut mid-JSON is omitted and the record marked
truncated. A permission denial stays one text line and is not a tool row. Run
history excludes the output and the effective configuration; GitPulse loads one
detailed record on demand. Output truncation is explicit.

Verified with installed Codex 0.153.4: ephemeral read-only handshake, canonical
checkout comparison, one completed marker turn, and the native GitPulse → Manvi
→ Codex → profile-store path. Scripted protocol processes verify file/question
callbacks, foreign identities, widened settings, duplicate delivery, resolved
requests and frame floods. Real approval/account variants, unsupported callbacks,
process-tree reconciliation and full host-crash recovery still need qualification.

## Activity inbox

Schema six adds `work_attention`. Eligible run outcomes and enhancement results
create a single inbox entry in the same transaction as their source event and
receipt. The source event sequence is unique; replaying a mutation cannot emit
another notice. Migration preserves previous history without backfilling old
banners. Run and enhancement history remain available through their existing APIs.

The inbox currently records process exits (including nonzero exits), failed
starts, uncertain process outcomes, ready/failed enhancements and interrupted
enhancement recovery, plus captured coding-agent permission requests and questions.
It stores generic preview text and exact task/target
identities; source tasks, prompts, output and model proposals are not copied into
notification previews. Additional review/CI/reminder event types require
their producing workflows to be implemented before they can emit notices.

- `attention.list`: optional `task_id`, `workspace_id` or `repository_id` (one
  scope), `filter` (`active`, `unread`, `all`), `limit` 1–200 and cursor. Default
  active hides dismissed and currently snoozed notices; unread additionally
  filters read entries. All includes their history. Newest-first pages use the
  source sequence, carry exact totals, and retain the existing response byte cap.
  Keep the same scope/filter with a cursor. Workspace membership is evaluated at
  read time and notices are deduplicated by identity across shared repositories.
- `attention.get`: `id`; the returned record resolves the current target again.
  `target_status` is `current`, `changed`, `task_changed`, `task_deleted` or
  `unavailable`. It does not establish an approval, acceptance or verification.
- `attention.update`: the normal `id`, `request_id`, `expected_revision`, plus
  `action`: `read`, `unread`, `dismiss`, `restore`, `snooze`, `unsnooze`. Snooze
  alone takes `seconds` (1–604800). The timestamp uses the store clock. Retries
  reuse the complete request. These actions change only the notice and its
  history; they cannot grant permissions, dismiss a proposal or accept a task.

GitPulse exposes the inbox in each board and mounts at most one page of 30
notices. Closed inboxes do no reads; open inboxes use coalesced change/focus hints,
with no per-notice timer. Native delivery settings and receipts are described
below; OS behavior is owned by the desktop adapter and requires platform tests.

## Native notification records

Schema seven adds `work_notification_settings`, `work_notification_deliveries`
and an index for the recent activity window. The schema upgrade is transactional.
The profile defaults to disabled with a stable, randomly generated profile ID;
the ID is routing identity, not an authorization credential.

- `notifications.settings.get` reads `id: profile`. `notifications.settings.put`
  uses the normal revision and request identity and replaces enabled, sound,
  background, local quiet-hour minutes and the three muted-ID arrays. Each mute
  array is unique and bounded to 64 known workspace/repository/task identities.
  Known identities include deleted tasks/workspaces, so an old mute cannot
  prevent unrelated preference changes. Unknown identities are still refused;
  clearing the arrays removes saved mutes without changing other settings.
  Enabling from disabled captures the event watermark and skips older notices.
- `notifications.pending.list` requires the native host's current local
  `minute_of_day` (0–1439), with the normal bounded limit/cursor. The store clock
  owns snooze and the one-hour activity window. Reads and claims share one
  eligibility expression; changed, deleted, read, dismissed, snoozed and muted
  notices are excluded. Quiet hours can cross midnight. Older activity remains
  in the inbox even when it is no longer eligible for a desktop banner.
- `notifications.claim` uses the attention ID, expected revision zero and local
  minute. It atomically records an uncertain delivery before an OS side effect.
  Replay returns `claim_consumed`, never another submission permit.
  `notifications.finish` accepts only definite `submitted`/`failed` outcomes.
  A missing callback or failed receipt write remains uncertain with no automatic
  retry. `notifications.delivery.get` reads the saved outcome. None proves display.
- `notifications.activate` validates the exact saved native ID; activation is
  independent of submission state, because a click may race receipt persistence.
  `notifications.activations.list` returns unacknowledged activations, oldest
  first. `notifications.ack` acknowledges navigation without marking attention
  read, accepting a proposal or changing a task. Repeated activation does not
  reopen an acknowledged record.

These methods are available through the existing store CLI and Go profile
client. The GitPulse renderer cannot author claim, finish or activation receipts;
its native coordinator owns those operations and public OS calls. The initial
adapter supports macOS, private previews, explicit OS authorization and Open
task activation. Physical delivery, click-after-quit and complete callback
recovery qualification remain separate gates.

## Structured agent request bindings

Schema eight adds `work_decisions`. A request, its event, private activity notice
and mutation receipt commit together. These records bind a human response to one
callback; they do not launch an agent, execute a tool or accept a task. The current
terminal handoffs still handle permissions in the provider terminal. Managed
Codex/Claude transports must be connected before live callbacks reach this UI.

- `decisions.create` is a host capture: normal mutation identity, `run_id`,
  `owner_id`, `session_id`, opaque `provider_thread_id`, `provider_turn_id`,
  `protocol_request_id`, `kind` (`permission` or `question`), complete JSON-object
  `payload`, lowercase SHA-256 `payload_digest`, and `deadline`. Each provider
  identity is bounded to 256 bytes; payload is bounded to 64 KiB. The store
  requires the matching running attempt, captures its immutable task/repository
  revisions, cwd and requested permission mode, and clamps expiry to five minutes
  from its own clock. One run has at most 32 unresolved callbacks and 2048 total.
  A provider request identity cannot be recaptured under a second decision ID.
- `decisions.decide` takes revision/request identity, the captured digest and
  `decision`: `allow_once`, `deny`, or `answer`. Only a question accepts `answer`,
  with nonblank text bounded to 16 KiB; a question cannot become a tool approval.
  The store rechecks run/owner/session/cwd/mode and task/repository revisions.
  Saving changes `pending` to `decided`; it is not proof of provider delivery.
- `decisions.claim` is reserved for the owning host. It requires all captured
  callback identities and digest, then rechecks validity before changing
  `decided` to `dispatching`. The claim is consumable once. Replaying even the
  same successful mutation receipt returns `claim_consumed`, never another
  permission to send. An uncertain write or process restart cannot resend it.
- `decisions.resolve` accepts the owning run/session and a definite `resolved`
  or `cancelled` state with a bounded reason. It can close an expired or ended
  callback because it grants no authority. The adapter must have actual provider
  resolution evidence before recording `resolved`; database storage cannot
  prove what an external provider received or did.
- `decisions.get` reads one ID. `decisions.list` requires `run_id`, with an
  optional state filter and ordinary bounded pagination/exact counts. The
  `actionable` projection is true only for a pending, current, unexpired request.
  Inbox status and native eligibility use that same validity predicate, including
  repository revision changes. Notifications use generic permission/question
  titles and never copy the payload into a desktop preview.

Go `Client.CreateDecision` hashes the exact payload using the standard library.
`Client.ClaimDecision` hashes the retained callback again. The protocol owner must
also confirm that the same callback is still open before claiming; a stored
`running` state alone does not prove a process survived a crash. `policy_revision`
is currently 1 because run permission policy is immutable. Effective provider
configuration and a future policy-changing protocol need separate receipts.

GitPulse's renderer can list/read requests and save a human decision. It cannot
create callbacks, consume delivery claims or write provider resolution records.
The inspector verifies the digest, preserves the captured payload text exactly,
retains the original request ID after an uncertain response, and distinguishes
saved, dispatching and provider-confirmed states. Only one run's request panel
mounts at a time, at 30 records per page and 180 loaded records maximum; it reuses
the existing visible run refresh rather than starting another timer.

Nine canonical Rust tests cover expiry, changed identities/repositories/tasks,
one-use claims, questions, notification privacy/validity, capacity and atomic
migration/receipt failure. A Go test crosses the real process boundary and
rejects modified payloads and replay after restarting the client. These checks
qualify the binding contract.

Live managed provider delivery was exercised once, on 2026-10-21, through
`manvi serve --workbench-db` against the installed Claude Code 2.1.306 on its
own subscription login, in a throwaway git repository with a `repo` harness spec.
One inspect-mode run was prepared with that harness, activated and driven to
`exited` / `completed`. The harness block was in `CLAUDE.md` before the process
started, the agent's reply quoted the spec's section, the file was byte-identical
(same sha256) and `git status` clean afterwards, and `harness_digest` matched in
the prepare result, the effective configuration and `harness.resolve`. This
covers Claude only; it was one run, in inspect mode, with no approval, question or
file callback and no Codex. The CLI reported no cost or usage, so none is
recorded. The first attempt, through the `claude` shell wrapper on `PATH`, failed
closed ("Claude reported a process identity other than the one this host
started"): the wrapper starts the CLI as a child, so the reported pid is not the
one the host launched. `MANVI_CLAUDE_BINARY` has to name the CLI executable itself.

Since then the host refuses that case at `work.runs.managed.prepare`, before
activation and before anything is run. It reads at most 256 bytes of the
configured Claude program (`MANVI_CLAUDE_BINARY`, else `claude` on `PATH`, with
symlinks resolved). A Mach-O (thin or universal), ELF or PE image is accepted. A
`#!` script is judged by its interpreter line alone: `node`, `bun` or `deno`
(directly or through `env`, including `env -S`) is accepted, because the kernel
runs the interpreter in place under the same pid, which is what an npm-installed
`claude` is. A shell (`sh`, `bash`, `zsh`, `dash`, `ksh`, `fish`, `csh`, `tcsh`)
is refused as a wrapper, and any other interpreter is refused as a launch shape
Manvi cannot verify, naming the interpreter. An empty or unreadable file, a
directory, a FIFO, a file without an execute bit or a file with a byte before the
`#!` is refused too. The error names `MANVI_CLAUDE_BINARY` and the resolved path,
and the saved attempt stays prepared. The check never runs the program.

A host whose `claude` is a shell wrapper therefore has to set `MANVI_CLAUDE_BINARY`
to the Claude Code executable itself (for an installer that keeps versions, the
file under its `versions` directory) and put anything the wrapper used to set,
such as `CLAUDE_CONFIG_DIR`, in the host's own environment. The check is early
help, not the security property: the process-identity check after start is
unchanged and fail-closed, a reported pid other than the one the host started
still ends the run (now with a message that names the likely wrapper cause), and
it is the backstop for a file swapped after the prepare-time read and for a
launcher the interpreter line does not reveal.

## Enhancement lifecycle

Schema version four adds durable automation settings and a task enhancement
queue to the version-three worker ownership and version-two proposal lifecycle.
Migration preserves existing tasks, revisions, groups and leases; it does not
queue existing tasks. Unknown versions and failed migrations are errors. Upgrade
every host's store together: an older binary refuses a newer profile with
`schema_unsupported`. The current schema is fourteen. Five through eight add the
terminal attempts, activity inbox, native notification records and structured
agent requests described above. Nine and ten change no tables: nine adds the
managed/terminal run kinds and their claim/protocol invariants, and ten
requires native process identity before provider initialization, so a failed
initialization records no fabricated thread or configuration. Eleven adds the
task archive, completion time, checklists and links described under
[Records and board scopes](#schema-eleven-archive-checklists-links-and-restore);
a host that builds a task body without them would unarchive and unlink every
task it saved, which is why the rung is a hard fence. Manvi's release pin
(`scripts/module-pins.txt`) moved from DevCouncil `a29a11b`, which carried
schema ten, to `ad5c941c`, which carries eleven; a Manvi built before that
move refuses an upgraded profile.
Twelve adds refinement proposals (below): enhancement bodies gain `kind`, and
`acceptance_criteria` joins the lockable fields. It changes no table, and is a
hard fence for the same reason: a host that does not know it would complete a
refinement as text or ignore a criteria lock.
Thirteen shortens request receipts: a receipt keeps the event sequence instead of
a copy of the response and a retry rebuilds the response from the event. It
changes no table, and is a hard fence because a schema twelve host would replay
the short receipt verbatim as the response. Receipts stored in full by schema
twelve keep replaying as written. Manvi's release pin moved to a DevCouncil that
carries thirteen; a Manvi built before that move refuses an upgraded profile.
Fourteen gives items a validated `scope` (planned files, forbidden changes,
allowed commands, expected tests, requirement links). It changes no table, and
is a hard fence because a host that builds an item body without it would erase
the scope of every task it saves.
Fifteen admits CI results: `signals.record` (a GitPulse write through the Rust
store, not a Go or `manvi serve` op) stores `run_ci_passed`, `run_ci_failed` and
`run_ci_cancelled` attention rows that a schema fourteen host cannot read. It
changes no table, and is a hard fence for that reason. The release pin
(`scripts/module-pins.txt`, DevCouncil `1f6d5452`) carries workbench 15 with
`dc-store` 11, as recorded in `CHANGELOG.md`; a Manvi built before that move
refuses an upgraded profile.

`enhancements.create` requires the normal mutation identity plus `task_id`,
`source_revision`, a nonempty `fields` subset of title/description, and nonblank
`provider` and `model` metadata. It stores the complete source task snapshot and
returns a pending proposal. Creation refuses stale source revisions and locked
fields. There is at most one unexpired pending or active generation per profile.
Its pending deadline is 120 seconds; `automatic:true` additionally counts toward
20 automatic reservations in a rolling hour. Idempotent retries do not consume
another slot or quota entry. Running and cancellation-requested attempts reserve
the slot even after expiry: a deadline does not prove the provider stopped.

`enhancements.claim` atomically checks the pending proposal, current source task
revision and locks, then sets `running`, `worker_id`, `started_at` and a new
120-second deadline. Supply an opaque worker ID and the normal mutation identity.
Only the same worker can complete a claimed attempt. This is a trusted host
coordination primitive; the worker ID is not an agent authorization credential.
Automatic proposals also capture the settings revision. Claim refuses changed
settings or disabled automation; pre-upgrade automatic proposals without that
snapshot cannot start silently.

`enhancements.complete` requires the current proposal revision and all requested
fields, using the same title/description bounds as tasks. It marks the proposal
ready without changing the task. An alternative nonblank `failure` reason marks
it failed and cannot be combined with suggested changes. Success after the
deadline or cancellation is refused. The owning worker can acknowledge failure
after expiry; that records an actual return rather than inferring termination.
Unclaimed expired pending records no longer reserve a slot. Trusted hosts can
still supply externally produced proposals by completing pending records; stored
provider/model labels alone do not prove inference took place.

`enhancements.revise` requires the current proposal revision and at least one
requested title/description field. Only ready proposals can be edited; current
task locks still apply. The first edit preserves `original_proposed`, and
`edited_fields` identifies differences from that original model text. Reverting
to the original clears the field marker while retaining revision history. This
operation changes the proposal only and uses the normal transactional receipt.

`enhancements.accept` requires the current proposal revision,
`expected_task_revision` and the selected `fields`. Every selected field must
still equal its source snapshot and be unlocked. Unselected fields retain their
latest values, even when a human changed them while the proposal was pending.
The task revision/history/event and proposal acceptance/history/event commit with
one idempotency receipt in one transaction. A conflict consumes nothing.

`enhancements.undo` requires the current proposal and task revisions. It restores
only the accepted fields, and only if they still equal the accepted proposal and
are unlocked. It preserves other human edits and creates another task revision.
`enhancements.dismiss` dismisses pending, ready or failed proposals without
modifying the task. For running work it persists `cancel_requested`; only the
owning worker's failure acknowledgement makes that `cancelled`. Accepted/undone
proposals cannot be reused as new attempts.

`enhancements.recover` is an explicit recovery decision for expired running or
cancellation-requested work. It requires the latest revision, matching worker ID
and `acknowledge_uncertain:true`. The result is `interrupted` with
`outcome_uncertain:true`; another attempt may still incur an additional provider
charge. Recovery neither kills nor proves termination of the old call. Late
completion from that attempt is refused. There is no automatic recovery/retry.

`enhancements.get` returns the complete proposal including source and proposed
text. `enhancements.list` supports optional `task_id`, cursor and limit, uses the
shared page byte/count limits and omits source, proposed and original proposed
text and rationale from
summaries. History remains available even when the task is deleted; accepting
changes for a deleted task is refused.

## Durable automatic scheduling

Saving a new task or changing its title/description through `items.put` schedules
one queue entry per task, due one second after the most recent text save, when
automation is enabled and at least one enhancement field is unlocked. The task,
queue entry, history and idempotency receipt commit together. The response adds
`automatic_enhancement_queued`, which is true only when this request scheduled
text work. An exact retry returns the original receipt without enqueuing again.
This records intent only; no provider call occurs in storage.

Card moves and other non-text saves preserve existing queued work without
resetting its deadline. Queue reads project the task's latest revision, including
revision changes caused by workspace deletion. Locking both enhancement fields
or deleting the task removes its queue entry. Accepting or undoing a suggestion
does not enter the save scheduling path and never creates another queue entry.

`automation.get` takes `{"id":"profile"}` and returns versioned settings. The
initial revision is one, enabled is true, and provider/model are null, meaning
the worker must use the host's explicitly configured selection. `automation.put`
uses the usual mutation identity with `id:"profile"` and requires an explicit
boolean `enabled`. Omit both provider/model to preserve selection, supply both
nonblank strings to override it, or supply both null to clear the override.
Partial or mixed overrides are refused. Disabling clears queued work, dismisses
unexpired pending automatic proposals and requests cancellation of running
automatic proposals in the same transaction. A running attempt remains
`cancel_requested` until its worker acknowledges return. Manual work and ready
suggestions retain their existing lifecycle.

`automation.list` supports cursor/limit and optional `task_id`, returning bounded
summaries with task ID, current revision and `not_before_ms`, ordered by deadline
then ID. `automation.prepare` requires a new proposal ID, mutation identity,
`task_id`, `expected_task_revision`, `expected_settings_revision`, and the chosen
provider/model. It checks enablement, selection, revisions and the debounce,
derives the unlocked fields, then uses the existing enhancement creation path.
The one-job limit and 20-per-hour quota remain authoritative. Consuming the queue
entry and creating the pending proposal are atomic; conflicts, busy/quota
refusals and storage failures preserve the queued work. Retrying the identical
request reconciles the original preparation receipt.

Successful manual `enhancements.create` consumes this task's queued suggestion
in the same transaction, avoiding a second automatic model call for that source.
Other tasks remain queued. Replaying the manual receipt does not consume text
saved later; failed preparation rolls back queue removal with the proposal.

With a configured runner, `work.enhancements.wake` accepts `{}` and starts or
wakes one coalescing coordinator. `work.enhancements.worker` accepts `{}` and
reads cached coordinator status without opening storage or starting inference.
Both return `ok`, `state`, `reason`, `task_id`, `proposal_id` and
`next_check_at` (milliseconds, or zero). States are `not_started`, `checking`,
`idle`, `disabled`, `waiting`, `generating`, `stopping`, `paused` and `stopped`.
These describe this coordinator, not proof of another host or provider's liveness.

The Go host wakes after committed text saves and automation setting changes.
GitPulse persists through the canonical native store, then sends a coalesced wake
hint; a failed hint is displayed separately and never turns a saved task into a
failed write. Opening an active board also resumes durable saved work. Empty
queues block without an idle timer. Only eligible queued tasks are prepared;
missing model selection pauses without consuming work or choosing a fallback.
Pending automatic proposals resume after restart. Running or cancellation-requested
claims are observed, never replayed; expired unresolved claims pause for recovery.
Known contention waits are bounded, and failures/uncertain delivery never trigger
another provider attempt. Shutdown cancels the coordinator and active generation
within one shared five-second acknowledgment window.

GitPulse exposes profile enablement, inherited or explicit provider/model selection,
queued count, stop and resume controls. Visible boards share one two-second status
timer while work is active; idle, paused and hidden boards have none. Automatic
results appear in the task review panel without replacing an edited suggestion.
Enhancement history defaults to newest first in the UI. Store `enhancements.list`
also accepts `newest`, `automatic` and distinct nonempty `states` filters; default
store ordering remains oldest first, and all filters apply to counts and cursors.

## Asynchronous provider generation

`work.enhancements.configuration` accepts an empty object and returns `ok`,
`provider`, `model`, `model_source` and the configured `providers` list. It is
advertised when the host supplies configuration alongside its runner. The CLI
uses its effective provider setting, the explicit `MANVI_MODEL` override when
present, or the local provider's configured model. On `anthropic` the model is
the one `manvi run` sends: `MANVI_MODEL`, then the main tier (`llm.tier.main`,
reported as source `llm.tier.main` when the operator set it and as `default`
when it is the built-in choice, which interactive ops use but automatic
enhancement refuses as it refuses an empty selection); serve calls the same
resolver, and a tier
profile it cannot use never stops it starting — the op that needs the model
refuses with the tier named. Any other cloud provider without an explicit model
returns an empty model with source `none`; the user must choose one. This endpoint reports selection only. It neither opens profile storage nor
resolves a provider, checks credentials, discovers models or starts inference.
Unknown request fields are refused.

GitPulse opens its separate profile host lazily through the shared sidecar
transport. Unsupported operations produce an upgrade error. Opening the panel
loads configuration and proposal history; manual generation remains an explicit action.
The editor reviews the original and suggested fields, edits suggestions while
retaining the original model output, accepts selected fields,
undoes accepted fields, and exposes cancellation and expired-attempt recovery.
An uncertain action retains its request identity and pauses task editing until
reconciled. Automatic saved-text suggestions use the same review lifecycle.

`work.enhancements.refine` (host-only, registered with the enhancement
configuration) proposes acceptance criteria, splits and `duplicate_of` links for
one task. Schema twelve stores the proposal as an enhancement of `kind: "refine"`
whose `fields` are among `acceptance_criteria`, `split_into` and `duplicate_of`.
It keeps every invariant above: the complete source snapshot, a refused stale
`source_revision` or locked field (`locked_fields` may now name
`acceptance_criteria`), the single profile-wide generation slot, dismiss to
reject and expiry of an unclaimed pending record. Refine reserves the slot
before its model call, completes the record with the proposal and its grounding,
and returns `enhancement_id`; a failed or cancelled run records the failure and
frees the slot. `enhancements.accept` applies it additively through the
revisioned task update: criteria are appended, `duplicate_of` links merged into
`links`, and each split (`split_ids` supplies the new identifiers) becomes an
inbox task in the same repositories with a `parent` link. It is never automatic
and cannot be revised or undone. Schema twelve is a hard fence like eleven:
Manvi's pin must move to a DevCouncil that carries it, and a build that knows
only eleven refuses an upgraded profile. See the changelog entry for the output
contract and caps.

The CLI's explicitly enabled workbench module also advertises
`work.enhancements.generate`. Storage-only embedders advertise this method only
when supplied an `EnhancementRunner` sharing the module's profile client.
Generate accepts `id`, `request_id` and `expected_revision` for an existing pending
proposal. It returns after the durable claim, while inference runs outside the
serial dispatcher. Repeated starts observe an existing non-pending attempt and
cannot replay its model call. The first result is the claim receipt; subsequent
observations have the `.get` response shape. Poll `.get` or durable events for the
result. No unsolicited protocol lines are written.

The worker resolves the proposal's explicit provider/model through the CLI's
existing provider factory and credential resolver. It never substitutes a model,
downloads one or falls back to cloud. Provider/model capability must be available.
The full source is limited to 32 KiB and conservatively checked against the model's
declared context window; it is never silently truncated. Generation offers no
tools and grants no coding-agent permissions or repository write access.

One worker runs per host; durable claims exclude other hosts sharing the profile.
Inference has a 110-second context deadline, at most 8,192 output tokens (or a
smaller declared model cap), 8,192 stream events and 256 KiB of streamed/final
content. These application checks supplement the adapter's own transport bounds;
they do not bound an uncooperative external provider process. A one-second watch
checks cancellation/ownership while work is active; storage failure cancels the
worker. There is no per-task idle timer. Closing the host cancels work and waits
up to five seconds for actual return. Unacknowledged work remains unresolved in
storage. Process crash or uncertain completion never triggers another model call.

Output must be a complete text result with a confirmed output-token bound and a
single JSON object, optionally wrapped in one complete Markdown code fence with
either no language label or the `json` label. Leading/trailing prose, multiple
fences and other language labels are refused; the original envelope is size
checked before unwrapping. Duplicate/unknown keys, unrequested fields, tool calls,
malformed Unicode, truncated results and excessive output are refused. Literal
checks preserve quoted text, code, URLs, path references, issue references and
error identifiers, plus lines with explicit constraint language. A small set of
unsupported completion/cause phrases is also refused in proposed fields and the
explanation. The prompt includes the exact literals and full constraint lines
from the same extractor used by validation, with at most 512 of each per field.
These bounds are checked before provider resolution; the duplicated preservation
text also counts against the model context budget. A full line containing a
constraint may span several sentences or the entire field. The prompt explicitly
allows returning that field unchanged while clarifying other requested text.
These checks are heuristics, not proof of semantic equivalence or
complete hallucination detection. Live model evaluation and human diff review
remain required; only explicit acceptance changes the task.

Provider errors are redacted through the existing credential scrubber and bounded
before persistence. Failure to persist a result leaves the running claim
unresolved. Only a definite revision conflict permits rebuilding a completion
request; transport uncertainty never silently retries it. Managed coding-agent
integration remains outstanding: the owner decided on 2026-11-07 that generation
moves to managed coding agents (see
[Owner decisions of 2026-11-07](#owner-decisions-of-2026-11-07)); this section
describes the direct provider path that still ships. Configured selection, automatic saved-text
generation and the GitPulse generation/review UI are implemented. Board movement,
selection and acceptance do not enqueue inference. Board activation may resume
previously saved work.

The automatic worker checkpoint adds real-store Go race checks for text-save
wake-up, restart, two competing hosts, no replay after failure or a prior claim,
missing selection, disabling during inference, shutdown and maximum-length IDs.
Fifteen Rust automatic scheduling tests include manual supersession, exact receipt
replay and injected transaction failure. Nine browser interaction checks use the
actual Go host and canonical store with a scripted model, including automatic
generation after Save, field locks, unchanged saved text and persisted Stop.
A separate real native-to-Go test exercises worker controls, shared settings,
stale-generation refusal and shutdown without provider inference. Installed native
UI behavior, whole-process resource bounds and broader semantic quality remain
unverified. The full `verify.sh` for this worker checkpoint finished with a
qualified pass: 82.1% Go statement coverage, 4,085 collected Go tests (five skipped),
869 benchmark-instrument checks and a successful local-provider wire probe.
Missing analysis tools, incumbent Python lease interop, the stale flat graph
artifact (generation one versus index eight), and live cloud-provider checks
remain unverified. The following evidence describes earlier checkpoints.

Verification: 12 Rust enhancement lifecycle cases, 13 automatic scheduling cases,
the complete `dc-store` suite and Clippy passed at the scheduling checkpoint.
Go store/serve/CLI race suites passed. Worker tests use real
storage with controlled provider streams to check responsiveness, cross-host
repeat starts, cancellation, restart persistence, redaction and output refusals.
A separate real CLI test reaches its configured local HTTP adapter against a
scripted endpoint; this proves transport wiring, not live model quality.
The output fuzz target passed 198,929 executions. The full `verify.sh` completed
with a qualified pass: five skipped Go cases, missing analysis tools, unavailable
incumbent lease interoperability, a stale navigation graph artifact and scripted
cloud-provider coverage remain explicit gaps. Its live local-provider wire check
passed separately; task-enhancement semantic quality still requires evaluation.

## Owner decisions of 2026-11-07

Recorded from board card ft-10d901e9. Both are decisions, and each is built by
its own implementation card. **Decision 1 is built and wired, as its in-process release
only** (card ft-ad5dfc70; see "What shipped: the in-process release" below).
Decision 2 is not built: no code for it ships, and its sequencing note stands.
Evidence labels: **verified** (read in code or schema at this commit),
**inferred**, **gap** (no mechanism exists).

### Decision 1: an agent-facing `work.*` API, including writes

The owner decided yes. **This widens what an agent can write.** Before the
in-process release the operations were reachable only over the host's one
controlling connection
(`manvi/serve/workbench.go`, registered on the serve router; the socket server
authenticates by peer uid alone and admits one controller, `serve/socket.go`
security model). Nothing in `manvi/devcouncil`, `manvi/tools` or `manvi/agent`
named a `work.` op (the card's `rg -uu` check, 2026-10-09). With the in-process
release an agent process whose host opted in can change board records of its own
repository's store; the sections below are the specification and the statement it
was built to, and "What shipped" says where the build differs from them.

#### What shipped: the in-process release

**This is the in-process release.** The tools are `work_list_items`,
`work_get_item`, `work_get_brief`, `work_get_history`, `work_create_item` and
`work_update_own_item`, in `manvi/devcouncil/work_tools.go` (adapter, tools) and
`work_log.go` (the tagged log region and the untrusted-data fence), registered
in the in-process `devcouncil.Registry` beside the `devcouncil_*` tools. They are
not exposed to managed Codex or Claude runs, which are separate processes with no
channel back to Manvi; that exposure needs a per-run host-brokered channel and
its own security-impact statement, and is **not shipped**. `hello.ops` and the
host connection's `work.*` set are unchanged (`serve/` gained tests only).

**On for two commands, off everywhere else (wired 2026-11-07, card
ft-ad5dfc70, owner decision "Wire it now").** The tools exist only when the host
sets `devcouncil.Deps.Work` (a `WorkBinding`); with it nil the Registry registers
none of them, as it registers no fetch tool without a configured host. One
constructor sets it: `agentTools` in `cmd/manvi/harness.go`, called by `manvi run`
(`run.go`) and by the TUI's per-session state (`tui.go`, `newSessionState`), and
by the `manvi tools` listing, which prints what those two offer.
`TestWorkBindingIsReachedOnlyFromRunAndTUI` pins that list by parsing the source.

- *Run id.* `WorkBinding.RunID` is the host's session id: `manvi run`'s stored
  session id (the resumed one when it continues a stored session, so a continuation
  keeps one run id) and the TUI session's `storeID` (`session.NewID`, sixteen hex
  characters). It is used only when it fits the binding's rule (at most 32 letters,
  digits, hyphen, underscore, `devcouncil.WorkBindingFor`); a stored id that does
  not fit (the store accepts up to 64) leaves it empty and the Registry mints a
  random one, so a long resumed id costs an audit-line name and never the tools.
  The model never supplies it.
- *Roles.* Only the top-level agent holds the tools. **No sub-agent holds any
  `work_*` tool, reads included:** the sub-agent floor (`subAgentRunner.prepare`)
  removes them by name from every child registry (untyped, read-only, and a role
  whose `allowed_tools` names them, in the parent's registry or an isolated
  worktree's), so a child cannot reach the board on its parent's lease and the
  adapter's own `inherited_session` refusal is a second line, not the first. A
  read-only child therefore sees neither write tool, and neither does a
  `--chat-only` run, which is offered no tool at all. The parent reads the board
  and hands a child what it needs in the prompt.
- *Not wired.* The `manvi tool` command (an operator's direct call), `manvi
  reflect`, the verify critic and `manvi serve` build through `nativeToolsWith`,
  which sets no binding. (The TUI's `/tool` command dispatches through the TUI
  session's own pipeline, which is the agent's, so an operator who types
  `/tool work_create_item ...` there calls the board tools as themselves, under
  the same caps and labelling; that is an operator action, not an agent one.) `manvi serve`'s enhancement runs are one provider completion with no
  tool schemas, and their request-building files do not import the tool package
  (`TestEnhancementRunHoldsNoWorkTool`). Managed Codex and Claude runs get nothing:
  the `codingagent` package imports neither the tool surface nor the registry
  (`TestManagedRunsHaveNoPathToTheBoardTools`).
- *Counts.* The documented tool counts are the agent surface's and moved by six:
  51 native tools, 50 with no skills (`docs/TOOLS_REFERENCE.md` section 14 specifies
  the six; `TestDocumentedToolCountMatchesRegistry` measures them). The six are
  Extended, so the lean dynamic profile's offered set is unchanged.

**How the two mappings the earlier draft called gaps resolve, and where they do
not.** Both were checked in code, not assumed.

- *Repository.* The Registry's `Store` is the repository's own store
  (`.devcouncil/state.sqlite` by default). Every task in that store names the
  legacy local repository `this-repo` (`dc_store::workbench::LOCAL_REPOSITORY_ID`)
  as its primary repository. The adapter binds to that constant and **only reads
  it** (`repositories.get`, once, never from an argument). It never calls
  `store.Client.LocalRepository`, which creates the row in whatever database it is
  pointed at, and no tool ever creates a repository. If the row is absent (a fresh
  database, or a profile board that has no such row), every tool refuses with
  `repository_unmapped`, which says there is no mapping, and makes zero writes.
  **On a GitPulse or profile board database the tools therefore bind to the legacy
  `this-repo` constant and cannot see tasks linked to a real repository.** Reaching
  that board needs the `identity_key` mapping (a path-to-`repository_id` match
  through `repositories.put`), which does not exist. It is a follow-up and part of
  the owner's wiring decision, which wired the tools without it (below).
- *Own task.* Since dc-store schema 10 a DevCouncil task is a workbench item in
  the same database (`lease.schema.md`), so the task the session holds the lease
  on (`Session.State().TaskID`, set by `devcouncil_checkout_task`) is a board item.
  The own task is that one, derived from the lease and not from an argument. It
  counts as the agent's own task only while it is a live item linked to exactly the
  session's repository; otherwise `work_update_own_item` refuses with
  `own_task_unmapped` naming the task, and creation still works. The agent chooses
  which ready task to check out, as it always has; the repository link check
  bounds what that choice can reach.

**Criteria that do not apply in the in-process release.** The managed-run
liveness rule (run state `running` with matching `owner_id` and `session_id`) and
the `runs.get` projection need a run record, and the in-process agent has none:
no `runs.prepare` precedes it. No tool takes a run id, and none returns a run.
The launch UI's provenance banner on agent-proposed or agent-logged tasks is
GitPulse's, not Manvi's; Manvi supplies what it keys on (the `agent-proposed`
label and the tagged log block) and does not claim the banner.

**Where the build differs from the specification below, on purpose.**

- *Counters are not a file.* The caps are read from the store on every write, so a
  host restart cannot reset them and nothing can drift from them. Writes and
  created tasks per task are the entries in the own task's tagged log block, each
  entry carrying its kind; a create writes its own task's entry before it creates
  the task, so a failure leaves a counted slot rather than an uncounted task. The
  open and hourly counts page the repository's tasks and filter on the
  `agent-proposed` label client-side (`items.list` has no label filter; the
  interim does not use a label query). The hourly count is an upper bound, because
  a task's update time moves forward on an edit; a person moving a proposal out of
  the inbox frees an open slot but not an hourly one. A count that cannot be made
  complete within twenty pages refuses (`count_unavailable`); it is never read as
  zero. A person who clears the log block clears that task's budget with it.
- *Host mutex, not a store-level atomic count.* Count-then-create is serialised by
  one process-wide mutex per (store, repository), shared by every Registry in the
  process. Two host processes sharing one store
  can both pass a count at the cap; the reconciliation above bounds the overshoot
  to the next write, and a store-level atomic count is the real fix.
- *Reads render everything in one fenced block.* `work_get_item`,
  `work_get_brief`, `work_get_history` and `work_list_items` return the store's
  reply as JSON inside a code fence under a standing header that says the content
  is data and not instructions. The fence is one backtick longer than the longest
  run of backticks in the text, and never shorter than three.
- *Agent text is plain.* Title, description, checklist and log text are refused
  if they hold a control character other than newline, carriage return and tab,
  the Unicode line and paragraph separators (U+2028, U+2029), NEL, vertical tab,
  form feed, or any Unicode format character (`Cf`: bidi overrides, zero-width
  characters, the byte-order mark; this also refuses emoji joiner sequences). A log
  note's line breaks of every kind are indented, so no line of agent text can begin
  with an audit prefix. A title is one line. A checklist entry is one line of
  at most 300 characters, at most 32 entries, and **markup is refused, not
  neutralised**: an HTML tag and Markdown link or image syntax are rejected,
  because a person's UI renders the checklist. A description keeps Markdown: it is
  free text the person reads as the proposal's body, and inside the fence an agent
  reads it as inert data.
- *`work_list_items` takes no arguments.* There is no cursor and no filter: it
  lists one page of at most 30 live, unarchived tasks linked to exactly this
  repository, and says whether there are more.
- *Request ids.* `agent_{run}_{session}_{n}`: the run id is the host's
  (`WorkBinding.RunID`, or minted at start), the session id is host-minted per
  `Session` (not the owner string, which `devcouncil_checkout_task` takes from the
  agent), and `n` is the first sixteen hex digits of a SHA-256 over the phase, the
  target, the revision written against and the canonical content. It is the same
  after a restart for the same write, differs for any other, and the whole id is
  checked against the identifier rule and 128 characters before it is sent. A
  request id already recorded in the task's agent log is refused as
  `request_replayed`; an `idempotency_conflict` from the store is refused as
  `request_conflict`; neither is treated as a write.
- *One refusal for every task the agent may not see.* An absent id, a deleted
  task, a task in another repository and a task linked to several repositories all
  answer the same `not_found` bytes, naming no id.

#### Security-impact statement

- **What widens.** An agent gains read access to board records of its own
  repository, and write access to a narrow set of fields (below). It gains no new
  file, command or network permission.
- **What is reachable now (wired).** Since the wiring change, the six `work_*`
  tools are reachable by **the top-level agent of `manvi run` and of the TUI**, and
  by no other command, role or process:
  - *Roles.* The top-level agent only. Every sub-agent (untyped, read-only, or a
    role naming the tools in `allowed_tools`) has the tools removed from its
    registry, reads included; a `--chat-only` run is offered no tool. Nothing in
    `manvi reflect`, the verify critic, `manvi tool`, `manvi serve` (host
    connection `hello.ops` and `work.*` set unchanged, enhancement runs tool-less)
    or a managed Codex or Claude run reaches them.
  - *Which board.* The tools bind to the repo-local `this-repo` repository (the
    constant every task in the repository's own `.devcouncil/state.sqlite` names)
    and **refuse with `repository_unmapped`, writing nothing, when that row is
    absent**.
  - *Which tasks.* **On a GitPulse or profile board database they cannot see
    real-repository tasks:** those are linked to a real `repository_id`, not to
    `this-repo`, so a read of one answers `not_found` and the list omits it (and
    when the profile database has no `this-repo` row at all, every tool refuses
    with `repository_unmapped`). Reaching
    that board needs the `identity_key` mapping, a follow-up that is not built.
    Until then the wired surface is useful on a repository-local store and inert
    on a profile board.
  - *What an agent can do there.* Read live tasks linked to exactly this
    repository; create inbox tasks (caps: 5 per held task, 10 per hour and 50 open
    per repository, title and description locked, `agent-proposed` label); and move
    the one task it holds the lease on to `in_progress` or `review`, tick its
    checklist, append to its tagged log region or add a `related` link, never
    `done` and never any other field. Choosing which task to hold remains the
    agent's, bounded by the repository link check.
- **Self-escalation is the main risk.** GitPulse builds the `HostScope` that
  bounds an agent's file writes and commands from the board task's `scope`
  (`serve/policy.go`, `HostScope`: planned files, forbidden changes, allowed
  commands; item `scope` is schema fourteen). An agent able to write its own
  task's `scope`, `locked_fields`, `acceptance_criteria`, `owner` or repository
  links could widen its own write policy or move its own goalposts. Those fields
  are therefore never agent-writable, on any task. The same class is already open
  for instruction files (board card "Dynamic harness 4/6 (security)").
- **Untrusted text.** Task titles, descriptions and logs are model- and
  user-supplied. An agent reading them is exposed to injected instructions from
  other tasks; a task an agent creates is read later by other agents and by
  automation. Agent-created text is treated as untrusted proposal text, never as
  instruction.
- **Amplification.** A saved task schedules an automatic enhancement when
  automation is enabled (`items.put`, "Durable automatic scheduling"). Under
  Decision 2 that is a managed-agent run. An agent that can create tasks could
  therefore spend provider quota and start runs. Agent-created tasks are saved
  with both enhancement fields locked, which removes the queue entry (verified,
  lock semantics above); a human unlocks to opt in.
- **Not a confidentiality boundary.** Same-uid processes can already read the
  profile SQLite file directly (socket security model). This API bounds what a
  cooperating agent can do through Manvi, not what a hostile same-uid process can.

#### Operations exposed

Names are the store methods; over `manvi serve` they would be `work.<method>`,
but the agent surface is a separate registration (see "Seam" below), so the host
connection's full set is never handed to an agent.

| Class | Exposed | Notes |
|---|---|---|
| Read | `items.list` | The adapter pins `repository_id` to the session's repository, `deleted:false`, `archived:false`, no `workspace_id`, and forces `limit` (page cap). It accepts no other filter from the agent. `events.list` is not exposed: it is profile-wide and has no repository filter. |
| Read | `items.get`, `items.brief.get`, `items.history` | Only for a task linked to the session's repository and no other (see "Scope check"). `items.history` covers linked, non-deleted tasks only. |
| Read | `runs.get` for the session's own run | A projection only: task id, repository id, state and run id. Never `output`, `effective_configuration`, `item.brief` or any other field. **Not built in the in-process release:** that agent has no run record, so no tool returns a run. It applies to the managed-run release. |
| Write | create a task | Closed allow-list: `title`, `description` and an optional plain-text `checklist`. See "Create allow-list". |
| Write | update the agent's own task | Field allow-list below. |
| Write | add a `related` link from the agent's own task | The target must pass the same repository check as a read. `duplicate_of`, `blocks` and `parent` are not exposed. |

**Stay host-only:** `items.delete` and `items.restore` (deletion is
soft but removes a record the user relies on; an agent withdrawing its own
proposal is done by the user), `workspaces.*`, `repositories.put` and `.relink`,
`automation.*`, every `runs.*` and `runs.managed.*` method, `enhancements.accept`,
`.undo`, `.dismiss`, `.recover`, `.claim` and `.complete`, `store.compact`,
attention and decision methods, and any merge of tasks. Merge and delete rewrite
or remove other people's records and are not argued into scope.

**Create allow-list.** An agent-created task carries `title`, `description` and
an optional checklist of plain-text entries (each `done:false`), and nothing else.
The adapter, not the agent, fixes every other `items.put` field: `expected_revision`
zero, status `inbox`, priority 2, kind `task`, no severity, owner or due date,
no `acceptance_criteria`, no `scope`, no `links`, no home workspace, one
repository link (the session's, also primary), `locked_fields` of both `title`
and `description`, and the `agent-proposed` label. A call that carries any other
field is rejected, not ignored: the agent's tool decodes with unknown fields
refused. Each forged field has its own test.

#### Scope check: which board and repository a call may touch

Scope is derived by the host from state the agent cannot supply, never from call
parameters:

1. **Identity of the session.** A managed run is prepared by the host against one
   task, one repository snapshot and one checkout (`runs.prepare`;
   `runs.claim` records `owner_id` and `session_id`, verified in
   `dc-store/src/workbench/runs.rs`). The API resolves `{task_id, repository_id}`
   from that run record. For the in-process Manvi agent the equivalent is its
   `devcouncil.Session` (task id, lease token, owner; `devcouncil/session.go`),
   carried on the call context, never in tool arguments.
2. **Board.** Always the single profile board; there is no per-agent board. The
   effective scope is the repository.
3. **Repository link check on every item id.** Every call that names an item id,
   whether a read (`items.get`, `items.brief.get`, `items.history`), a write
   target, or the target of a `related` link, first loads the task and requires
   that it is linked to the session's repository and to no other. Anything else
   answers a `not_found` that is byte-identical to the answer for an absent id,
   including for link targets (no existence oracle). A task linked to several
   repositories is refused, not redacted: refusal is simpler and cannot leak the
   other repositories' fields. Mutating a link set is not exposed, so a task
   cannot be moved into scope.
4. **Own task.** The one task the run was prepared on. All agent writes other than
   creation target it, and the call refuses any other id.
5. **Liveness, defined per path, and rechecked at the write.** A write is allowed
   only while the session is live, and "live" has a different meaning on each path:
   - *Managed run.* Managed runs hold no DevCouncil lease. Live means the run
     record's state is `running` and its `owner_id` and `session_id` match the
     session's. `unresolved` refuses, as does any other state.
   - *In-process session.* Live means `Store.Valid` on the session's lease, checked
     against the store on every write exactly as `devcouncil_write_file` does
     (`devcouncil/file_tools.go`, "The lease is checked against the store on every
     write, not once at checkout"). An inherited child session (`inherited` in
     `devcouncil/session.go`) may not write the board, because it does not own the
     lease; as shipped the host goes further and gives a child no board tool at all
     (see "On for two commands" below), so this refusal is the second line.

   The run or lease is re-read immediately before `items.put`, not trusted from the
   start of the call. A store error on that read, or the process exiting mid-call,
   refuses the write. Required test: the run ends between the liveness check and
   the put, and the write is refused.
6. **Concurrency.** Every write sends `expected_revision` from the agent's last
   read; a stale write is refused (existing behaviour of `items.put`).

#### Authority check: who may create, update, complete, delete

| Action | Agent | Human host |
|---|---|---|
| Create a task | Yes, inbox only, closed allow-list, bounded (below) | Yes |
| Edit its **own** task | `status` to `in_progress` or `review`, tick existing `checklist` entries, append to `logs`, add a `related` link | Everything |
| **Complete** its own task | Move to `review` only. Never `done`. | Moves to `done` |
| Edit **another** task | No write at all. It proposes by creating an inbox task. | Everything |
| Delete or restore | No | Yes |
| Merge | No | Yes |

Completion of the agent's own task differs from editing others' tasks, and
differs from a human move to Done: the agent's `review` move is a claim, not an
acceptance. This follows the existing rule that a run outcome "never marks a task
Done or accepts its work" (`runs.finish`) and that board status does not represent
verification or human acceptance (Board queries, "Deletion is soft"). A later
option, for the owner to decide separately, is letting a `work.verify.task`
verdict for the current revision authorise the agent's move to `done`. The agent
never writes `title`, `description`, `acceptance_criteria`, `scope`,
`locked_fields`, `priority`, `severity`, `owner`, `due`, `kind`, `archived`,
repository links or home workspace on any task.

The agent edits through a host adapter that reads the item at `expected_revision`,
applies only the allow-listed delta, and calls `items.put`. Which fields survive
an omitted value matters here, and is exactly this set: `locked_fields`, `logs`,
`archived`, `checklist`, `links` and `scope`. `labels` is **not** preserved, nor
is any other field, so the adapter supplies every other editable field from the
read (including `labels`, `owner`, `due` and the repository links) and an
adapter test asserts a status move leaves each of them unchanged.

**Status moves and enhancement.** `items.put` schedules automatic enhancement only
when the save changes title or description (Durable automatic scheduling), and the
host wakes the worker only when the receipt carries `automatic_enhancement_queued`
(`serve/workbench.go`, the `Queued` check). An agent status move changes neither
field. Required tests: an agent status move enqueues nothing and wakes nothing, and
the wake path stays conditional on `automatic_enhancement_queued`. A second
invariant, for Decision 2: an enhancement run holds no `work_*` tool. A test
asserts the enhancement run's tool surface contains none.

#### Prompt injection and provenance

Agent-authored `logs`, descriptions and titles are untrusted text that other
agents and humans will read later. `items.brief.get` and any agent-facing read
render agent-authored regions inside a fenced untrusted-data block under a
standing header stating the content is not instructions. The launch UI shows a
provenance banner on any task whose text carries the `agent-proposed` label or an
agent log region. Test: a log entry containing an injection string and a closing
fence stays inside the fence, and the header is present on every render.

#### Rate and size bounds

Existing bounds the API inherits (verified): requests at most 256 KiB;
descriptions 64 KiB; logs 256 KiB; checklist 128 entries; links 64; titles 300
characters; mutations need `request_id`, `id` and `expected_revision`; per-op
`lane` and `deadline_ms` in `hello.op_limits`; the store pool is at most eight
children with a ten-second call deadline.

Bounds the agent surface adds (acceptance criteria for the implementation card):

- Description at most 16 KiB and title at most 200 characters on agent-created
  tasks; one `logs` append at most 16 KiB.
- **Budgets are keyed by (repository, task), not by run**, so a continuation or a
  follow-up run on the same task shares them: at most 5 tasks created and 30
  writes per (repository, task).
- **Repository-wide creation rate cap**: at most 10 agent-created tasks per
  repository per hour, and at most 50 open `agent-proposed` inbox tasks per
  repository, refused beyond either with both numbers reported.
- **Serialised and persisted.** Count-then-create runs behind a per-repository host
  mutex so concurrent creates cannot both pass the check. The counters are
  persisted, not held in memory, so a host restart does not reset them.
- **Log space.** Total agent-appended bytes per task are capped at 64 KiB. The
  agent region of `logs` is tagged (a delimited block with a fixed marker) so a
  human can clear it, and the adapter never rewrites the human's prefix: it only
  appends inside the tagged region.
- A cap returns a typed error naming the cap; it is never applied by silent
  truncation.

**Gap:** no per-session or per-profile write quota exists in the store ("Quota
enforcement is still unspecified", in the retention paragraph on `store.compact`).
A store-level atomic count is the real fix, and it is a cross-app schema change
(DevCouncil owns it; GitPulse re-vendors; Manvi's pin moves) that needs approval;
the host mutex is the interim, and the counts are read from the store's own
records (the repository's listing and the tagged log block), not from a counter
file; see "What shipped". **Still open and needing approval:** the store-level
atomic count, because the mutex covers one host process only. `items.list` has no
label filter (documented filters are `status`, `query`, scope, `archived`,
`deleted`, `order`), so the open `agent-proposed` count cannot be a label query;
the listing is filtered client-side. Tests: concurrent creates at the cap, a
restart that keeps the count, and a continuation that shares the budget of its
first run.

#### Audit: recorded with the agent and session identity

Every accepted mutation already commits a revision snapshot, an event and an
idempotency receipt in one transaction (verified). **Gap:** `work_events` has
columns `sequence, kind, entity_id, revision, at, payload` and `work_revisions`
has `entity_type, entity_id, revision, body` (`schema.sql`); neither has an actor
column. `work_requests` maps a `request_id` to its event `sequence`, but receipts
are deleted past a seven-day replay window by `store.compact`. **Events and
revisions are pruned too**: `store.compact` removes events older than 30 days
(`event_retention_seconds`) beyond the newest `keep_events` (default 1,000), and
keeps only the newest `keep_revisions` (default 50) per record. The log prefix is
therefore the only durable trace of which agent acted.

Until the store records an actor, the adapter records identity by three means
that use only existing fields: (1) the host mints `request_id` (an agent never
chooses it); (2) each `logs` append is prefixed with the run and session ids and a
timestamp, inside the tagged agent region; (3) created tasks carry the
`agent-proposed` label and a description line naming the run.

**The `request_id` must be durable and replay-safe.** A counter held in memory
restarts at zero with the host, and the store answers a reused `request_id` with
the old receipt without writing anything (`mutate` in `dc-store/src/workbench/mod.rs`:
a prior row with the same method and input is replayed, and a different input is
`idempotency_conflict`). A restarted counter would make a new, different write
look like a replay or fail as a conflict, or worse silently report an old success.
So `n` is derived durably: a hash of the canonical request content together with
the own task's current revision, or a persisted counter, not an in-memory one.
The form is `agent_{run_id}_{session_id}_{n}` within the identifier rule (letters,
digits, hyphens, underscores) and the adapter asserts the whole id is at most 128
characters. The adapter fails closed and reports on any replayed receipt or
`idempotency_conflict`; it never treats either as a successful new write. Required
test: restart the host mid-run and write again.

A first-class actor on events and revisions is a DevCouncil schema change, which
this repository's rules treat as cross-app. It is not part of this decision and
needs its own approval.

#### Seam, and gaps in the existing mechanisms

- The agent surface is a new registration beside the host's, not a widening of it.
  The in-process Manvi agent already has the right seam: `devcouncil.Registry`
  holds a `*store.Client`, the same client type `WorkbenchModule` uses, and tools
  receive the acting `Session` on the call context. `work_*` tools register there.
- **Gap, external agents (open; the shipped release is in-process only).** Managed
  Codex and Claude run as child processes
  (`codingagent`). Nothing in this repository gives those processes a channel back
  to Manvi's ops; the host connection is single-controller and uid-authenticated.
  Exposing `work.*` to them needs a per-run, host-brokered tool channel (for
  example a per-run MCP endpoint) that does not exist. That design is a second
  security statement, not an extension of this one, and is not part of the
  in-process release.
- **Path to repository: closed for the repository-local store, open for a profile
  board.** The earlier draft read: a task record carries repository ids, not paths,
  and mapping `Deps.Root` to a `repository_id` needs the `identity_key` match on
  `repositories.put`. For the Registry's own store the mapping is the store's:
  every task in it names `this-repo`, which the tools read and never create (see
  "What shipped"). The `identity_key` match for a profile board still does not
  exist, and these tools do not claim it.
- **Lease to item: closed for the in-process agent.** The earlier draft read: a
  DevCouncil lease is on a DevCouncil task and a workbench item is a different
  record. Since dc-store schema 10 they are the same record, so the leased task id
  is the board item id (`lease.schema.md`: "A task is a live `work_items` row whose
  primary repository is `this-repo`"). The own task is derived from the lease. For
  managed runs the run record would supply it, and that path is not shipped.
- **Gap, `HostScope`.** It bounds file writes and commands only (verified, no
  board field). It cannot express board authority and is not extended for it.

Managed Codex and Claude exposure is not shipped; it needs the host-brokered
channel above and its own review. The in-process release is the one built, under
this section's acceptance criteria.

### Decision 2: enhancement generation moves to managed coding agents

The owner decided that generation moves from the direct provider HTTP path to
managed coding agents. The scope is the model-calling code in
`serve/enhancements.go`, `serve/enhancement_output.go` and `serve/automation.go`:
the title/description rewrite, `work.enhancements.refine` and the automatic
saved-text worker. `serve/scan.go` is described as a deterministic audit, and
`work.tasks.suggest` and the organise proposal make no model call (Suggesting
what to work on next), so there is nothing to route there unless a model step is
added to them.

This supersedes the earlier choice recorded on the refine card ("direct inference
through the existing enhancement runner, not a managed coding agent"). What ships
today stays as documented above until the implementation lands.

**What changes.** Today generation offers no tools and no repository access, runs
in one completion with a 110-second deadline, 8,192 output tokens and 256 KiB of
output, and uses the provider factory and credential resolver (verified, "Asynchronous provider
generation"; the request is built at `serve/enhancement_output.go`, an
`llm.Request`). After the move it is a managed run: a process, a session, a
checkout and an agent loop.

**Consequences.**

- **Cost.** A completion becomes an agent turn with its own startup and possibly
  several model calls, and it draws on the coding-agent account (a seat or login)
  rather than a provider API key. Not measured; none of the numbers here are. The
  automatic worker (one debounced run per text save, up to 20 per hour) is the
  multiplier, so the implementation must measure per-run cost before automatic
  mode is allowed on a managed provider, and keep the existing 20-per-hour cap.
- **Latency.** Process start, provider initialisation and activation precede the
  first token. A pending proposal expires after 120 seconds and a claim carries a
  120-second deadline; a prepared managed helper expires after five minutes. Those
  deadlines were sized for one completion and probably need revisiting. Unmeasured.
- **A checkout is required.** A managed run is prepared against a task, a
  repository snapshot and a validated checkout, and only one prepared, active or
  unresolved attempt may reserve a checkout (Run records). An enhancement of a
  task title does not need repository files, but the run model has nowhere else to
  stand. Consequences: an enhancement and a real run on the same checkout exclude
  each other; a task linked to no usable checkout, or to several, has no defined
  checkout; the harness block written into a checkout during a run
  (`ManagedRunner.HarnessBusy`) refuses preparation while it is present. The
  implementation must choose, and state, whether enhancement runs use the task's
  primary checkout read-only, a detached linked worktree of it, or an empty scratch
  directory with no repository content.
- **Tools reach untrusted text.** The direct path's strongest property, that it
  "offers no tools and grants no coding-agent permissions or repository write
  access", does not carry over. Task text is untrusted and would now be read by an
  agent that has tools. Required of the implementation: read-only sandbox and
  approval posture, every permission request or question answered by denial rather
  than left to time out, no access to the Decision 1 API (an enhancement run
  holds none of its write operations), and a post-run check that the checkout is
  unchanged. This sentence in "Asynchronous provider generation" must be corrected
  in the same change that lands the implementation.
- **Output validation stays.** The single-JSON-object contract, preservation
  literals, size caps and refusal rules in `enhancement_output.go` remain the
  canonical validator; the agent's final message is validated by it exactly as a
  completion is today. Proposals still change a task only on explicit acceptance,
  and `enhancements.recover` keeps its uncertain-outcome meaning.
- **Failure modes new to this path.** The coding-agent CLI missing, wrapped or not
  logged in (the open board cards on managed Claude and Codex launch identity
  apply); an agent that edits files despite the sandbox; an agent that never
  finishes while its tool grandchildren outlive it (board card "Managed coding
  agents are not process-group owned"); provider selection that is a coding agent
  (`claude`, `codex`) rather than a model, so the stored `provider` and `model`
  labels change meaning; and capacity contention with real runs
  (`max_active_runs`, default 8).
- **Open owner question (unanswered, not decided here).** The `local` provider (a
  configured local model) is not a coding agent. Does it keep a direct enhancement
  path? Recommendation: keep local direct, because it is not a coding agent and
  removing the path would remove local enhancement entirely. "Replace, do not
  accumulate" argues the other way. Until the owner answers, the implementation
  card must not delete the direct path.

**Sequencing.** The managed-agent surface is in flux. At this commit another
live session holds uncommitted changes in `manvi/codingagent/*` (`agy.go`,
`claude_seat.go`, `claude.go`, `codex.go`, `connection.go`, `continue.go`,
`executable.go` and tests) and `manvi/serve/managed.go`, plus `serve.go`,
`toolbinary.go` and `harness_materialise.go` (GitPulse collision scan,
2026-11-07: those 19 files, in the main checkout's worktree, no overlap with this
branch). The implementation must wait for that work to land, then start from the
resulting adapter surface. No code ships for Decision 2.

## Board queries

`items.list` without a scope is global. A `repository_id` filters by linked
repository. A `workspace_id` includes the union of items homed there and items
linked to any member repository. An item appears once even when several links
match. Workspace and repository scopes are mutually exclusive. An absent/deleted
scope returns `not_found`. Optional `status` and plain-text `query` filters apply
to the complete scope. Search uses quoted prefix tokens joined with AND in FTS5;
user text cannot supply FTS operators.

`archived` selects one side: `false` for the board, `true` for the archive.
Omitted or null lists both, so a board that omits it also shows archived tasks.
`deleted:true` reads deleted tasks instead of live ones (what a restore is
chosen from). `order` is `board` (the default: position then ID, ascending),
`completed` (`completed_at` then ID, descending; a task not in Done sorts as
zero) or `updated` (`updated_at` then ID, descending; for deleted tasks, when
they were deleted). `updated` is not indexed and is meant for the deleted list,
not a whole board.

Count and page predicates share one SQL builder with bound parameters. Repository
queries begin with indexed links; workspace queries union indexed home membership
and member-repository links before fetching task rows. Empty optional-filter OR
branches no longer force selective counts to visit unrelated profile tasks.
Global search counts and small result sets begin with FTS hits. When a global
search has more than 200 matches, its page query uses an FTS row-ID set with the
board ordering, avoiding a fresh MATCH query for every candidate. Scoped search
also builds its FTS hit set once and intersects it with indexed membership.

Pages select row IDs, position and ID before formatting task bodies. A bounded
materialized selection keeps JSON projection to at most the requested limit plus
one lookahead row, including when search or membership candidates arrive in the
opposite order. These are query changes against the existing schema and indexes;
no migration, auxiliary search engine or worker is introduced.

Deletion is soft, preserving history. Deleted IDs cannot be reused by
`items.put`; `items.restore` brings the same task back. An agent's
execution status, verification evidence and human acceptance are not represented
by changing this board status; their separate models remain to be implemented.

## Suggesting what to work on next

`work.tasks.suggest` (host only; nothing offers it to an agent) reads the open
board and ranks the tasks worth starting. It never writes, and it makes no model
call: the order is a fixed point table, so the same board gives the same list,
each point is returned as a named signal, and the one-line `reason` is the three
largest of them. Params are all optional: `request_id` (echoed back), `limit`
(default 5, at most 20), `max_priority` (0 urgent to 3 low; keep that priority or
more urgent) and `repository_id`.

Points: priority urgent 100 / high 60 / normal 25 / low 0; severity critical 40,
high 25, medium 10; due date overdue 50 plus a day each up to 14, within two
days 35, within a week 20; one point per nine idle days up to 10; 12 for each
open task it blocks (a link `{blocks, X}` held on a task means that task blocks
X), up to 36; 10 when already `ready`; 8 when a repository gap scan raised it;
a newest stored verification that failed or degraded adds 15 and one that
passed takes 25 off. Ties break on priority, then the earlier due date, then id.

Only `inbox`, `backlog` and `ready` tasks are suggested. Done and archived
cards are not read; `in_progress` and `review` tasks are counted, not suggested;
a task that duplicates an open task or is blocked by one is left out, listed
under `blocked` with its blockers, and counted. A card whose links could not be
read is treated as blocked rather than guessed at.

Reads are bounded: ten pages of 200 per status column, 150 link reads for cards
the list returned without links, and verification records for the best 12
candidates only. `board.complete`, `board.unhydrated` and `verification` carry
those numbers, so a capped read is never reported as the whole board.

## Organising the board by priority

`work.tasks.organize` (host only; nothing offers it to an agent) reads the open
board and returns a proposal: each task it would re-prioritise, with the
priority it has now, the priority it would have, and one named reason; an order
for each column; and groups of tasks joined by `duplicate_of`. It never writes
and it makes no model call. `method` is `deterministic`, so the same board
gives the same proposal. A person accepts a row before the host writes it,
through `items.put` with `expected_revision`. The op does not merge duplicates
and it does not store a column order.

A rule raises priority: critical severity to urgent, high severity to high, a
due date a week or more overdue to urgent, a shorter overdue or a due date
within two days to high, and a task that blocks other open work to high. The
only rule that lowers a priority is `stale_high`: an inbox or backlog task that
is urgent or high, untouched for 90 days, with no due date, no high or critical
or medium severity, and nothing blocked on it, drops one step. `in_progress`
and `review` are never lowered. A task whose links were not read is not lowered
and is not treated as blocking. A task with no revision is counted under
`coverage.unrevisioned` and left out of the proposal.

Reads are bounded: ten pages of 200 per status column, and 2000 link reads.
`coverage.read` is how many in-scope open tasks were fetched. `coverage.unread`
is how many rows were not, taken from `items.list` `total`; when a cut page has
no total the op fails rather than guess. `coverage.unhydrated` is how many
fetched open tasks' links were not read. `coverage.complete` is false when a
bound cuts the read. Column order is priority, then due date, on the proposed
priority. Priority first uses those two signals on the stored priority. Suggest
next also weighs severity, blockers, age, and verification (`differs`).

## Bounded reads and durable events

- Pages default to 100 rows, accept 1–200, and stop fetching before a 2 MiB response
  budget. Large history/events may therefore return fewer rows than requested.
- `total` counts all matching records; `shown` counts this page. `has_more` and
  `next_cursor` disclose omitted records. Never treat `shown` as the full count.
- Workspace navigation omits descriptions/membership arrays and returns
  `repository_count`. Item cards omit descriptions, acceptance criteria, logs,
  checklists and links.
  Fetch the corresponding `.get` record when opening details or replacing it.
- Workspace cursors and board-order item cursors order by `(position,id)`
  with the opaque prefix `1:`; `items.list` with `order:"completed"` uses `2:`
  and `order:"updated"` uses `3:`. A cursor from one order is refused under
  another with `invalid_input` rather than resuming at a
  position that means something else. Repository membership
  lists retain the workspace's ordering. Each page/count pair shares a read
  transaction. Pagination across multiple requests is not a snapshot: restart a
  board query after intervening mutations; do not reuse a cursor across filters.
- `events.list` accepts an integer `after`; `items.history` accepts `id` and
  `after_revision`. Their numeric `next_cursor` advances only through returned
  records. History remains readable after deletion.
- Workspace deletion emits one workspace event with `affected_item_count` and
  up to 200 `affected_item_ids`. `affected_item_ids_truncated:true` requires broad
  item invalidation, not an assumption that only the sampled items changed.
- Requests are at most 256 KiB. Unknown fields, duplicate JSON keys (including
  alternate escaped spellings), malformed types and oversized integers are
  rejected. A future schema version returns `schema_unsupported`.

Events, revisions and receipts are bounded by `store.compact`, a workbench
method (`work.store.compact` through `manvi serve`) that needs no schema change.
Nothing calls it automatically: a host schedules it. It deletes idempotency
receipts only past a replay window (default 7 days, never under an hour), never
deletes a launch-claim receipt, keeps the newest `keep_revisions` (default 50)
revisions of every record so `expected_revision` checks stay answerable, keeps
every revision of an open decision or a live run, and keeps any event a
surviving receipt or attention row points at. A pass is bounded per table and
reports `deleted`, `eligible` and `truncated`, so a capped pass is never read as
a complete one. The full policy, its consequences for retries and `events.list`
cursors, and its tests are in DevCouncil's `docs/workbench-retention.md`.
Quota enforcement is still unspecified. Unresolved decisions are never pruned.

## Verification

Run the repository gate, `./verify.sh`, and focused integration commands from the
repository root:

```sh
cargo test --manifest-path crates/Cargo.toml -p dc-store
cargo clippy --manifest-path crates/Cargo.toml -p dc-store --all-targets -- -D warnings
```

From `manvi/`, run `go test ./serve ./cmd/manvi` and `go test -race ./serve`; the
`dc/store` client lives in DevCouncil, so from
`DevCouncil/backend/go_orchestrator/` run `go test ./dc/store` and
`go test -race ./dc/store`. Real binaries exercise persistence across
restart, three board scopes, process reuse, lease isolation, idempotency, stale
edits, an eight-writer race, grouping, page byte bounds and logical refusal
recovery. `FuzzWorkbenchNeverAcceptsPartialOrFailedChildOutput` attacks the Go/Rust
response boundary; it is independent of whole-app profiling and provider testing.

`BenchmarkWorkbenchProfile` seeds 10,000 tasks, 100 repositories, ten workspaces
and 100,110 events through the real public API. `BenchmarkWorkbenchProfileStress`
spreads the same 100,000 task mutations across 100,000 distinct tasks with one
revision each. Both measure global/workspace/repository/history reads, broad and
rare search, scoped search, empty status and committed text edits. Read cases
check exact totals and page sizes before timing; edit cases verify one additional
event per mutation afterward. Run explicitly from
`DevCouncil/backend/go_orchestrator/`:

```sh
go test ./dc/store -run '^$' -bench '^BenchmarkWorkbenchProfile(Stress)?$' -benchtime=100x -count=1 -timeout=20m
```

Measured on Apple M5 Pro (`darwin/arm64`). Mean milliseconds are `ns/op / 1e6`. p95 is the benchmark's own `p95-ms`. `events.list` counts at most 10,000 rows and sets `total_exact` false above that; the fixture check uses `latest_sequence`, the AUTOINCREMENT high-water mark.

The 10,000-task run (10 revisions each, 100,110 events, seed 7m31s, `-benchtime=20x`) is the earlier run recorded in DevCouncil `docs/workbench-retention.md`. Mean / p95 in milliseconds: global list 3.32 / 3.79, workspace 8.29 / 9.31, repository 1.90 / 2.33, search 10.2 / 12.95, history 0.59 / 0.79, edit 2.16 / 2.83. One `store.compact {"keep_revisions":1,"limit":100000}` deleted 90,021 revisions (eligible 90,021, not truncated) in 2.94 s. Receipts and events were fresh, so none were eligible. That write-up does not include rare search, scoped search or empty status.

The 100,000-task run is from 2026-10-13, `-benchtime=20x -count=1`, invoked as:

```sh
go test -C manvi -run '^$' -bench '^BenchmarkWorkbenchProfileStress$' -benchtime=20x -count=1 -timeout=90m -v github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store
```

`manvi/go.mod` replaces that package with the live DevCouncil tree. Seed 8m28.6s. Fixture check: `latest_sequence` 100,110. Each printed sample is 20 iterations. The result names end in `-18` (GOMAXPROCS). Other local processes were up during the run.

| Case | ns/op | Mean ms | p95 ms |
|---|---:|---:|---:|
| global | 9630338 | 9.63 | 11.65 |
| workspace | 76226683 | 76.23 | 112.9 |
| repository | 7174469 | 7.17 | 9.944 |
| search | 83882712 | 83.88 | 120.5 |
| history | 606162 | 0.606 | 1.160 |
| rare_search | 2385340 | 2.39 | 5.646 |
| repository_search | 57804852 | 57.80 | 72.87 |
| workspace_search | 115900885 | 115.9 | 150.5 |
| empty_status | 2923192 | 2.92 | 7.977 |
| edit | 5102760 | 5.10 | 11.78 |

One `store.compact {"keep_revisions":1,"limit":100000}` on that fixture took 517 ms and deleted 21 revisions (eligible 21, truncated false). Receipts deleted 0, events deleted 0. The printed edit sample is 20 iterations. The high-water mark after the edits was 100,131, which is the 100,110 seed events plus 21 committed edits; the benchmark's revision counter includes any extra invocation of that sub-benchmark. Vacuum was incremental (free pages 5, then 0).

The two fixtures commit the same number of mutations. Workspace list and search are slower on the 100,000-task fixture (one revision per task) than on the 10,000-task fixture (ten revisions per task).

Setup is excluded from operation timing; the temporary fixture is removed after
the child closes. Report hardware, build mode and competing activity alongside
results. One hundred samples provide a preliminary p95, not desktop qualification
or an idle-memory measurement. Query-complexity tests separately count actual
bundled-SQLite VM operations, compare all 64 scope/status/search combinations
against an independent ordered reference, and use a projection tripwire to prove
discarded candidates do not read their bodies. VM operations exclude work inside
virtual-table callbacks and are not a substitute for elapsed-time measurements.

The query checkpoint passed all 93 store tests and Clippy. The full `verify.sh`
completed with 82.1% Go coverage, 184 Rust tests and 869 benchmark-instrument
checks; the store/serve/CLI race suites passed separately. Five of 4,085 collected
Go tests skipped. Lint, nil and Go vulnerability tools, incumbent lease
interoperability, the stale flat navigation artifact (generation one versus
index ten) and live cloud-provider checks remain unverified. The live local
wire probe passed. These verification results do not override the measured
100k-task search latency failures reported in GitPulse's benchmark document.
