# Computer-use foundations

These changes extend the existing Go provider, tool, and session boundaries.
The separate `workflow` package supplies the pure capability compiler/reducer;
`computer` interprets its commands through the Rust native broker.

## Typed tool observations

`tools.Result.Content` and `session.ToolResultData.Content` use `llm.Content`,
a sequence of `llm.TextBlock` and `llm.ImageBlock`. Existing text tools continue
to use `Text`; a result with both representations is rejected. The registry
scrubs text blocks and copies image bytes before returning. Image input reaches
Gemini as `{type:"image", mime_type:..., data:...}` both in user input and in a
function result. Text-only adapters reject unsupported nested content.

The session projection reads the sanitized serialized event on both warm and
cold paths. Append callers, observers, event snapshots, restored logs, and
projected messages do not share mutable event or image bytes. Image comparisons
include SHA-256 of the bytes, including images inside tool results. A logged
compaction replaces the complete result with its logged text summary.

`Log.SetScrubber` receives decoded plain JSON strings, including escaped quotes,
backslashes, newlines and Unicode. It never rewrites canonical image bytes or
opaque signatures. `Events()` and session Store retain the private resumable
journal. `PublicEvents()` and `Event.Public()` remove provider replay state and
reasoning signatures; existing `Observe` callbacks receive these public copies.
Public events are for export/UI, not lossless provider continuation. Keep the
private journal outside exportable artifacts. A sanitized tool argument that
no longer agrees with signed private continuation causes Gemini to fail with a
payload-free diagnostic; use parameter references instead of sensitive literals.

`SetSensitiveScrubber` additionally replaces streaming fragments with a withheld
event marker. Chunk-by-chunk replacement cannot recognize a sensitive value
split across chunks. Completed messages retain sanitized text for projection;
ordinary logs keep their existing streaming behavior. Jarvis enables this policy
for both discovery and assisted recovery before appending new events.

Security impact: these APIs retain screenshot bytes in the session journal.
Text scrubbers cannot identify secrets inside pixels. Capture policy and image
redaction must run before constructing an image result. Sensitive tool inputs
must use parameter references: the agent journals assistant/tool-call arguments
before the tool approval pipeline runs. Installing a scrubber does not migrate
historical events. The generic session layer does not supply attachment expiry
or screenshot filtering. A computer-use host must bound observation size and
retention; `agent.CountRequestTokens` remains a text estimate, unsuitable for
pricing or bounding screenshot requests.

## Admission, cancellation, and accounting

`transport.WithAttemptGate(ctx, gate)` opts one request tree into admission
before every HTTP attempt, including HTTP retries and streaming preflight
retries. `AttemptInfo` exposes provider, method, path without query parameters,
attempt number, and serialized request size; it exposes no body or headers.
The returned callback runs once per admitted attempt. Accounting failures stop
retries.

`AttemptResult` is transport evidence, not a bill:

| State | Meaning |
| --- | --- |
| `not_sent` | No call to `http.Client.Do`; a reservation may be released. |
| `rejected` | HTTP rejection was observed; billed usage is not established. |
| `accepted` | HTTP/stream preflight accepted; final usage is not yet known. |
| `unknown` | Send or stream outcome is uncertain; retain the reservation. |

The host reserves a conservative model-specific upper bound before each attempt
and reconciles final provider usage separately. Cancellation, a missing usage
record, or a rejected response must not automatically refund a sent attempt.
No global quota or billing policy is enabled by this library change.

The optional `llm/budget.Provider` requires an inner
`transport.AttemptGatedProvider`; Gemini declares and tests this contract.
Ungated providers are rejected before `Stream` runs. The campaign ledger
validates contiguous attempt identities, state/usage/charge consistency and
overflow-safe totals on reopen. Failed persistence leaves the previous snapshot
intact, cancellation is checked again under the admission lock, and a closed
ledger cannot settle after releasing exclusive ownership. Only an accepted
attempt can settle once; unknown/rejected attempts keep full reservations.
The simple price model supports inclusive input tokens, uncached input pricing,
and separate output/reasoning tokens; nonzero cache-write usage is unsupported
and conservatively retains its reservation. Applications own exact model price
selection and the campaign limit.

The registry checks cancellation at entry, after approval, and immediately
before dispatch. It snapshots caller-owned argument bytes before approval.
This does not undo an effect already begun or replace a native broker's final
epoch fence. Empty provider responses now consume the agent's hard step budget.

## Gemini continuation

Indexed Interactions SSE responses retain ordered provider steps as version 2
opaque continuation state. Thought signatures remain attached to thought steps;
function calls and assistant prose remain in their original sequence. Unknown
step fields are retained. Unsupported continuation deltas fail explicitly.
Continuation content is checked against the neutral assistant message before
settling and before replay, so opaque state cannot silently override edited
tool arguments or prose. Provider/model ownership is checked at encoding.

The adapter continues to use `store:false` and does not use
`previous_interaction_id`. Existing unindexed fixtures and old journals retain
the legacy signature reader, including its historical loss of unsigned calls
and assistant prose in tool-using history. Such historical records cannot be
made lossless retroactively. Native computer-use hosts should use newly recorded
version 2 continuations, and treat a legacy continuation as a migration boundary.

The contract was checked against Google's [stateless function calling
guide](https://ai.google.dev/gemini-api/docs/function-calling) and
[Interactions API reference](https://ai.google.dev/api/interactions-api) on
2026-09-09. Local HTTP/SSE fixtures verify encoding, decoding, cancellation,
opaque-state retention, and argument consistency. These tests are not live
provider or native desktop verification.

## Provider fixtures

`llm/replay` owns fixture, request, response, and recording snapshots. Canceled
requests do not consume fixture turns, and canceled streams cannot settle.
`Fixture.StrictRequests` requires each `Turn.Request` and compares the complete
serialized request before consuming a turn. Request drift and a missing expected
request both fail; legacy fixtures remain permissive unless strict mode is set.
This is provider-response fixture replay, distinct from an approved deterministic
workflow that executes without a model.

## Workflow and native boundary

The computer runner retries input only after a broker receipt explicitly states
`not_sent` with `window_changed` or `state_changed`. The workflow event retains
the failure code. Each retry advances `State.ActionAttempt`, uses a distinct
`:attempt:N` action ID, focuses the attached window, captures fresh state, and
resolves a unique target. Change effects require another human approval. Sent,
unknown, contradictory, and unclassified failures never take this retry path.
The capability's observation-attempt limit also bounds input attempts per step;
exhaustion pauses and fences the run, and cannot be bypassed by resume. Additional
attempts also consume the overall action limit. Read/extract observation retries
retain their original action ID, and post-delivery capture never repeats input.

`workflow.Compile` owns artifact bytes and rejects duplicate keys, cycles,
unreachable steps, duplicate output definitions and references not available on
every incoming branch. Final steps must be observed checkpoints. `wait` uses the
same typed predicate as `assert`, reobserves with a 200 ms command delay, and
pauses after the configured observation-attempt bound. The pure reducer contains
no provider, driver, network or clock imports; offline replay cannot send input.

`computer.StartRun` owns input/privacy snapshots and serializes state transitions.
A sent receipt requires a fresh post-action capture. Pausing at this stage saves
`ResumePhase=post_action`; resume recaptures without repeating the input. Safe
capture-only `observation_inconsistent`/`window_changed` failures retry within the
observation bound; uncertain input never retries. Failed, cancelled and unknown
runs fence the broker and record its newer epoch, including the terminal `fence`
event. Clean completion keeps its last capture epoch. Recorder failures remain
errors and any unresolved delivery remains `outcome_unknown`.

Approval binds epoch, action and reviewed observation. After explicit approval,
the runner focuses the attached window, captures it again and compares private
semantic form state, including protected values. Focus, timestamp, observation
IDs and geometry are excluded; the native broker rechecks actionability and
geometry. A change requests a new approval without granting or sending input.
Equivalent state receives one native grant bound to the new observation. The
private semantic digest stays in memory and is never exported. Extracted values
come from the sanitized tree, including numeric outputs.

`AttachFocused` explicitly requests initial native focus; existing `Attach`
keeps its original behavior. `focus`/`refresh` require an idle paused run.
`human_act` journals intent, receipt and sanitized post-capture. `RunOptions.Assisted`
opts into one `assisted_action` per run: a read-only semantic Back/Dismiss button,
resolved against the current private native observation. Its actor is
`model_recovery`; success leaves the run paused. The host still needs explicit
resume and canonical workflow checkpoints. Read-only means no business/account
change: reviewed form entry stays read-only under the broker's trusted target
allowlist. No model is available to deterministic replay or to the native broker.

The client bounds request size and pending calls. A cancelled or short blocked
pipe write retires the broker process; a partial send remains uncertain. Closing
the client does not wait behind the blocked write lock. Native behavior and OS
permissions require separate physical platform qualification.

`workflow/catalog` publishes synchronized staging files through atomic
non-replacing links, rejects symlinks and mismatched artifact/path identities,
and reserves `current.json` for promotion metadata. Reads and directory sizes
are bounded. Rooted filesystem handles prevent path escape. Publication is
atomic to readers; power-loss durability of directory metadata is not guaranteed
without a platform-specific directory-sync contract.

## Local checks

From `manvi/`:

```sh
CGO_ENABLED=0 go test ./... -count=1
CGO_ENABLED=1 go test -race ./workflow/... ./computer ./llm/replay ./session ./tools ./llm/transport -count=1
```

Regressions cover sanitized warm/cold projections, mutable image/event aliases,
changed screenshot bytes, nested image capability and progress accounting,
approval-time argument mutation, cancellation around approval, strict fixture
matching, indexed Gemini step stops, multimodal requests, and retry admission.
The repository-wide `verify.sh` additionally checks Rust, lint, coverage, and
cross-language boundaries; the focused commands above do not replace that gate.
