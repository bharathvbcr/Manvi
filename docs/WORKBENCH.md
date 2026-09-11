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

The API is for the owning local host. Possession of that stdio connection permits
profile CRUD. These operations are not registered as agent tools, do not grant
execution permissions, and do not acquire or renew repository leases. A future
agent-facing API needs explicit scope and authority checks before exposing writes.

## Operations

All names below receive a `work.` prefix over `manvi serve`.

| Resource | Methods |
|---|---|
| Workspaces | `workspaces.list`, `workspaces.get`, `workspaces.put`, `workspaces.delete` |
| Repositories | `repositories.list`, `repositories.get`, `repositories.put` |
| Items | `items.list`, `items.get`, `items.brief.get`, `items.put`, `items.delete`, `items.history` |
| Changes | `events.list` |
| Enhancements | `enhancements.create`, `enhancements.claim`, `enhancements.complete`, `enhancements.recover`, `enhancements.get`, `enhancements.list`, `enhancements.accept`, `enhancements.revise`, `enhancements.dismiss`, `enhancements.undo` |
| Automatic scheduling | `automation.get`, `automation.put`, `automation.list`, `automation.prepare` |
| Run records | `runs.prepare`, `runs.claim`, `runs.started`, `runs.protocol`, `runs.finish`, `runs.cancel`, `runs.get`, `runs.list` |
| Managed Codex (Go host) | `runs.managed.prepare`, `runs.managed.activate`, `runs.managed.stop` |
| Generation (Go host runner) | `enhancements.generate`, `enhancements.configuration` |

`.put` creates or replaces a record. Supply every editable field that should
survive replacement; omitted optional fields return to their defaults. It is not
a partial patch. The `locked_fields` exception preserves existing locks when
omitted or null, so an older host cannot silently unlock a task. Supply `[]` to
unlock explicitly. Automation settings also preserve the provider/model pair when
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
{"id":"board","op":"work.items.list","params":{"workspace_id":"devtools","limit":100}}
```

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
Missing criteria remain explicitly missing. Export refuses inconsistent links
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
Checkout discovery, moved-path relinking and checkout aliases are not yet part of
this API. Remote URLs are metadata and are never executed by these operations.

Items support title, description, kind, status, priority, optional severity, owner,
due date, labels, acceptance criteria, ordering position and repository links.
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
provider capabilities or grants.

The selected repository must be linked to the saved task at the supplied
revision and registered as `local:<git_common_dir>`. Storage cannot verify the
supplied paths. The native host must inspect them. Preparation captures the
canonical task brief once in the same transaction as the run and receipt. Only
`runs.get` adds `item.brief`. Run lists omit output and effective configuration;
protocol mutation receipts and stored history can retain those bounded fields. Task deletion preserves the snapshot and run history.

Preparations expire after five minutes. At most two prepared/active/unresolved
attempts may reserve the profile, and one may reserve a repository. Expired
unclaimed preparations release capacity; expiry never releases a claimed or
unresolved run. Configurable limits remain implementation work.

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
to the provider terminal. Managed Codex has the separate protocol below; complete
crash recovery and code review remain open.

## Managed Codex connection

Schema nine introduced immutable `kind`: `external_terminal` (the default) or `managed`.
`runs.claim` must specify the same kind; an old terminal adapter cannot claim a
managed attempt. Schema ten requires native process identity before provider
initialization and permits a failure receipt without an invented thread. Both
binaries must support schema ten. Its transactional contract-version migration
preserves existing attempts and does not reclaim unresolved legacy launches.

GitPulse adds `runs.prepare_managed`, `runs.launch_managed` and
`runs.stop_managed` through its existing native IPC. Preparation reuses the same
Git checkout validation as terminals; only Codex is supported, with native attempt
IDs of at most 100 characters. Launch/stop accept only the saved ID. The native
host serializes launch preparation and supplies the OS-observed process birth;
renderer-supplied process identity, raw `runs.started`, `runs.finish`,
`runs.protocol`, and the managed prepare/activate host operations are refused.

The owning `manvi serve --workbench-db` host advertises three methods:

1. `runs.managed.prepare` takes `id`, `request_id`, `expected_revision:1`. It
   consumes the canonical claim and starts only the stdio process. It returns
   `protocol_version:2`, `phase:awaiting_activation` and the same owner/session/PID
   for an exact retry in that live host. It has not initialized a provider thread
   or sent the task. GitPulse rejects incompatible receipts and requests cleanup.
   Unactivated helpers expire after five
   minutes. GitPulse uses a stable `managed_launch_<run ID>` request identity.
2. `runs.managed.activate` takes the prepared `id`, `owner_id`, `session_id`,
   `process_id` and native `process_start`. It first records the running process,
   then initializes an ephemeral thread, verifies the returned checkout/approval/
   sandbox settings, saves `runs.protocol ready`, and sends one model turn.
   An uncertain activation consumes its latch;
   it cannot authorize a second turn. GitPulse rechecks the checkout immediately
   before this call and stops the prepared session if activation cannot complete.
3. `runs.managed.stop` accepts `id` and requests cancellation of the exact locally
   owned session. Its reply confirms the request, not process termination.

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

`MANVI_CODEX_BINARY` selects the executable through the existing tool resolver;
otherwise the host resolves `codex` on PATH. The adapter launches
`app-server --listen stdio://` with argument arrays, never shell interpolation.
It retains the initialized provider user agent and returned effective model,
provider, approval reviewer, approval policy, sandbox and thread identifiers.
The observed configuration and provider thread become immutable for this attempt;
a turn ID can be filled once. No global permission preference is changed.

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

Limits: two active managed sessions; a 45-minute turn budget; 256 KiB protocol
frames; 32 MiB stdout and 64 KiB discarded stderr; 32 pending callbacks and 2,048
callback IDs per connection; a 64 KiB complete request; 128 KiB retained output
and 64 KiB effective configuration. Run history excludes the latter two fields;
GitPulse loads one detailed record on demand. Output truncation is explicit.

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
qualify the binding contract; live managed provider delivery remains unverified.

## Enhancement lifecycle

Schema version four adds durable automation settings and a task enhancement
queue to the version-three worker ownership and version-two proposal lifecycle.
Migration preserves existing tasks, revisions, groups and leases; it does not
queue existing tasks. Unknown versions and failed migrations are errors. Upgrade
every host's store together: an older binary refuses a newer profile. The current
schema is eight, including terminal attempts, the activity inbox, native
notification records and structured agent requests described above.

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
present, or the local provider's configured model. A cloud provider without an
explicit model returns an empty model with source `none`; the user must choose
one. This endpoint reports selection only. It neither opens profile storage nor
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
integration remains outstanding. Configured selection, automatic saved-text
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

## Board queries

`items.list` without a scope is global. A `repository_id` filters by linked
repository. A `workspace_id` includes the union of items homed there and items
linked to any member repository. An item appears once even when several links
match. Workspace and repository scopes are mutually exclusive. An absent/deleted
scope returns `not_found`. Optional `status` and plain-text `query` filters apply
to the complete scope. Search uses quoted prefix tokens joined with AND in FTS5;
user text cannot supply FTS operators.

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

Deletion is soft, preserving history. Deleted IDs cannot be reused. An agent's
execution status, verification evidence and human acceptance are not represented
by changing this board status; their separate models remain to be implemented.

## Bounded reads and durable events

- Pages default to 100 rows, accept 1–200, and stop fetching before a 2 MiB response
  budget. Large history/events may therefore return fewer rows than requested.
- `total` counts all matching records; `shown` counts this page. `has_more` and
  `next_cursor` disclose omitted records. Never treat `shown` as the full count.
- Workspace navigation omits descriptions/membership arrays and returns
  `repository_count`. Item cards omit descriptions and acceptance criteria.
  Fetch the corresponding `.get` record when opening details or replacing it.
- Workspace and item cursors order by `(position,id)`; repository membership
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

Events, revisions and receipts currently retain all accepted changes. Retention,
quota enforcement and compaction need explicit policy before large-scale rollout;
unresolved decisions must never be pruned by a generic history cleanup.

## Verification

Run the repository gate, `./verify.sh`, and focused integration commands from the
repository root:

```sh
cargo test --manifest-path crates/Cargo.toml -p dc-store
cargo clippy --manifest-path crates/Cargo.toml -p dc-store --all-targets -- -D warnings
```

From `manvi/`, run `go test ./dc/store ./serve ./cmd/manvi` and
`go test -race ./dc/store ./serve`. Real binaries exercise persistence across
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
event per mutation afterward. Run explicitly from `manvi/`:

```sh
go test ./dc/store -run '^$' -bench '^BenchmarkWorkbenchProfile(Stress)?$' -benchtime=100x -count=1 -timeout=20m
```

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
