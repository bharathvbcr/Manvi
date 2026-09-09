#![forbid(unsafe_code)]

use crate::platform::{SupervisedChild, inherit_lease, terminate, watch_parent};
use desktop_core::{
    Action, Attach, Delivery, DesktopError, Effect, InputStamp, MAX_REQUEST_BYTES,
    MAX_RESPONSE_BYTES, MAX_WORKER_REQUEST_BYTES, Observation, Request, ResolvedTarget, Response,
    Result, Selector, VisualAnchor, VisualMatch, Window, WorkerRequest, now_ms, resolve,
    resolve_visual, state_fingerprint,
};
use serde::de::DeserializeOwned;
use serde_json::{Value, json};
use std::{
    collections::{HashMap, HashSet},
    io::{self, BufRead, BufReader, Write},
    process::{Command, Stdio},
    sync::mpsc,
    thread,
    time::{Duration, Instant},
};

const HELPER_TIMEOUT: Duration = Duration::from_secs(12);
const OBSERVATION_TTL_MS: u64 = 120_000;
const MAX_SESSIONS: usize = 16;
const MAX_HELPERS: usize = 4;

struct Session {
    run_id: String,
    epoch: u64,
    paused: bool,
    window: Window,
    observation: Option<Observation>,
    input_stamp: Option<InputStamp>,
    visual_targets: HashMap<String, (VisualMatch, VisualAnchor)>,
    read_only_targets: Vec<Selector>,
    approvals: HashMap<String, String>,
    actions: HashMap<String, (String, Response)>,
    pending: Option<String>,
}
struct Pending {
    child: SupervisedChild,
    request: Request,
    started: Instant,
    action_binding: Option<String>,
}
enum Event {
    Request(Request),
    Invalid(DesktopError),
    Done(String, Result<Value>),
    Eof,
}

fn error(code: &str, message: &str) -> DesktopError {
    DesktopError::new(code, message)
}
fn decode<T: DeserializeOwned>(v: Value) -> Result<T> {
    serde_json::from_value(v).map_err(|e| DesktopError::new("invalid_request", e.to_string()))
}
fn output(response: Response) -> Result<()> {
    let mut bytes = serde_json::to_vec(&response)
        .map_err(|e| DesktopError::new("serialization", e.to_string()))?;
    if bytes.len() > MAX_RESPONSE_BYTES {
        return output(Response::failure(
            response.id,
            error("response_limit", "Response exceeded the byte limit"),
        ));
    }
    bytes.push(b'\n');
    let stdout = io::stdout();
    let mut writer = stdout.lock();
    writer
        .write_all(&bytes)
        .and_then(|()| writer.flush())
        .map_err(|e| DesktopError::new("transport", e.to_string()))
}
fn read_bounded<R: BufRead>(reader: &mut R, limit: usize) -> io::Result<Option<Vec<u8>>> {
    let mut result = Vec::new();
    loop {
        let chunk = reader.fill_buf()?;
        if chunk.is_empty() {
            return if result.is_empty() {
                Ok(None)
            } else {
                Ok(Some(result))
            };
        }
        let size = chunk
            .iter()
            .position(|b| *b == b'\n')
            .map_or(chunk.len(), |i| i + 1);
        if result.len().saturating_add(size) > limit {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "NDJSON frame exceeds byte limit",
            ));
        }
        let finished = chunk[size - 1] == b'\n';
        result.extend_from_slice(&chunk[..size]);
        reader.consume(size);
        if finished {
            return Ok(Some(result));
        }
    }
}

fn platform(request: WorkerRequest) -> Result<Value> {
    if request.op == "resolve_target" {
        let window = request
            .window
            .ok_or_else(|| error("invalid_request", "Window required"))?;
        let anchor = request
            .visual_anchor
            .ok_or_else(|| error("invalid_request", "Visual anchor required"))?;
        let capture = request
            .capture
            .ok_or_else(|| error("invalid_request", "Observation pixels required"))?;
        let observation_id = request
            .observation_id
            .ok_or_else(|| error("invalid_request", "Observation identity required"))?;
        let visual = resolve_visual(&window, &capture, &anchor, &observation_id)?;
        return serde_json::to_value(ResolvedTarget::Visual { visual })
            .map_err(|e| DesktopError::new("serialization", e.to_string()));
    }

    #[cfg(target_os = "macos")]
    {
        desktop_macos::execute(request)
    }
    #[cfg(target_os = "windows")]
    {
        desktop_windows::execute(request)
    }
    #[cfg(target_os = "linux")]
    {
        desktop_linux::execute(request)
    }
    #[cfg(not(any(target_os = "macos", target_os = "windows", target_os = "linux")))]
    {
        let _ = request;
        Err(error(
            "unsupported_platform",
            "This executable has no backend for this OS",
        ))
    }
}

pub fn run() -> Result<()> {
    match std::env::args().nth(1).as_deref() {
        Some("worker") => {
            let mut reader = BufReader::new(io::stdin());
            let bytes = read_bounded(&mut reader, MAX_WORKER_REQUEST_BYTES)
                .map_err(|e| DesktopError::new("transport", e.to_string()))?
                .ok_or_else(|| error("invalid_request", "Missing worker request"))?;
            let request: WorkerRequest = serde_json::from_slice(&bytes)
                .map_err(|e| DesktopError::new("invalid_request", e.to_string()))?;
            watch_parent(request.broker_pid.ok_or_else(|| {
                error(
                    "parent_missing",
                    "Native worker requires its supervisor identity",
                )
            })?)?;
            match platform(request) {
                Ok(value) => output(Response::success("worker", value)),
                Err(e) => output(Response::failure("worker", e)),
            }
        }
        Some("probe") => {
            let value = platform(WorkerRequest {
                op: "probe".into(),
                broker_pid: None,
                attach: None,
                window: None,
                action: None,
                target: None,
                visual_anchor: None,
                capture: None,
                observation_id: None,
                expected_state: None,
                expected_nodes: None,
                expected_input: None,
            })?;
            output(Response::success("probe", value))
        }
        Some("serve") => serve(),
        _ => Err(error("usage", "Usage: manvi-desktop serve | probe")),
    }
}

fn start(
    request: Request,
    worker: WorkerRequest,
    tx: mpsc::SyncSender<Event>,
    binding: Option<String>,
    lease: Option<&std::fs::File>,
) -> Result<Pending> {
    let exe =
        std::env::current_exe().map_err(|e| DesktopError::new("helper_start", e.to_string()))?;
    if worker.op != "probe" && lease.is_none() {
        return Err(error(
            "input_lease",
            "Attached native work requires an input lease",
        ));
    }
    let mut command = Command::new(exe);
    command
        .arg("worker")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null());
    inherit_lease(&mut command, lease);
    let mut child = SupervisedChild::new(
        command
            .spawn()
            .map_err(|e| DesktopError::new("helper_start", e.to_string()))?,
    )?;
    let mut bytes = serde_json::to_vec(&worker)
        .map_err(|e| DesktopError::new("serialization", e.to_string()))?;
    bytes.push(b'\n');
    if bytes.len() > MAX_WORKER_REQUEST_BYTES {
        let _killed = child.kill();
        let _reaped = child.wait();
        return Err(error("request_limit", "Worker request exceeds byte limit"));
    }
    let mut input = child
        .stdin
        .take()
        .ok_or_else(|| error("helper_start", "Missing helper input pipe"))?;
    let write_tx = tx.clone();
    let write_id = request.id.clone();
    thread::spawn(move || {
        if let Err(e) = input.write_all(&bytes) {
            let _closed = write_tx.send(Event::Done(
                write_id,
                Err(DesktopError::new("helper_transport", e.to_string())),
            ));
        }
    });
    let stdout = child
        .stdout
        .take()
        .ok_or_else(|| error("helper_start", "Missing helper output pipe"))?;
    let id = request.id.clone();
    thread::spawn(move || {
        let mut reader = BufReader::new(stdout);
        let result = (|| -> Result<Value> {
            let bytes = read_bounded(&mut reader, MAX_RESPONSE_BYTES)
                .map_err(|e| DesktopError::new("helper_transport", e.to_string()))?
                .ok_or_else(|| error("helper_exit", "Native helper exited without a response"))?;
            let response: Response = serde_json::from_slice(&bytes)
                .map_err(|e| DesktopError::new("helper_protocol", e.to_string()))?;
            if response.ok {
                response
                    .result
                    .ok_or_else(|| error("helper_protocol", "Missing helper result"))
            } else {
                Err(response
                    .error
                    .unwrap_or_else(|| error("helper_protocol", "Missing helper error")))
            }
        })();
        let _closed = tx.send(Event::Done(id, result));
    });
    Ok(Pending {
        child,
        request,
        started: Instant::now(),
        action_binding: binding,
    })
}

fn context<'a>(
    sessions: &'a mut HashMap<String, Session>,
    request: &Request,
) -> Result<&'a mut Session> {
    let id = request
        .session_id
        .as_ref()
        .ok_or_else(|| error("session_required", "A session ID is required"))?;
    let session = sessions.get_mut(id).ok_or_else(|| {
        error(
            "session_missing",
            "Session does not exist in this broker instance",
        )
    })?;
    if request.run_id.as_ref() != Some(&session.run_id) {
        return Err(error("run_mismatch", "Session belongs to another run"));
    }
    if request.epoch != Some(session.epoch) {
        return Err(error("stale_epoch", "Control epoch changed"));
    }
    Ok(session)
}
fn observation<'a>(session: &'a Session, id: &str) -> Result<&'a Observation> {
    let observed = session
        .observation
        .as_ref()
        .filter(|o| o.observation_id == id)
        .ok_or_else(|| {
            error(
                "stale_observation",
                "Observe the window again before resolving or acting",
            )
        })?;
    if now_ms().saturating_sub(observed.captured_at_ms) > OBSERVATION_TTL_MS {
        return Err(error(
            "stale_observation",
            "Observation age exceeded the allowed limit",
        ));
    }
    Ok(observed)
}

fn owns_input(request: &Request) -> bool {
    matches!(request.op.as_str(), "act" | "human_act" | "focus")
        || (request.op == "attach"
            && request.payload.get("focus").and_then(Value::as_bool) == Some(true))
}

fn dispatch(
    request: &Request,
    sessions: &mut HashMap<String, Session>,
    pending: &mut HashMap<String, Pending>,
    tx: &mpsc::SyncSender<Event>,
    counter: &mut u64,
    lease: &mut Option<std::fs::File>,
) -> Result<Option<Value>> {
    let make_worker = |op: &str| WorkerRequest {
        op: op.into(),
        broker_pid: Some(std::process::id()),
        attach: None,
        window: None,
        action: None,
        target: None,
        visual_anchor: None,
        capture: None,
        observation_id: None,
        expected_state: None,
        expected_nodes: None,
        expected_input: None,
    };
    if request.id.is_empty() || request.id.len() > 160 {
        return Err(error(
            "invalid_id",
            "Request ID must be nonempty and bounded",
        ));
    }
    if pending.len() >= MAX_HELPERS && !matches!(request.op.as_str(), "pause" | "detach") {
        return Err(error(
            "broker_busy",
            "Native helper concurrency limit reached",
        ));
    }
    if request.op == "probe" {
        pending.insert(
            request.id.clone(),
            start(
                request.clone(),
                make_worker("probe"),
                tx.clone(),
                None,
                None,
            )?,
        );
        return Ok(None);
    }
    if request.op == "attach" {
        if sessions.len()
            + pending
                .values()
                .filter(|p| p.request.op == "attach")
                .count()
            >= MAX_SESSIONS
        {
            return Err(error("session_limit", "Session limit reached"));
        }
        if request
            .run_id
            .as_ref()
            .is_none_or(|s| s.is_empty() || s.len() > 160)
        {
            return Err(error("run_required", "Attach requires a bounded run_id"));
        }
        let attach: Attach = decode(request.payload.clone())?;
        if attach.read_only_targets.len() > 64 {
            return Err(error("policy_limit", "Too many read-only target selectors"));
        }
        for selector in &attach.read_only_targets {
            selector.validate()?;
            if selector.visual.is_some() {
                return Err(error(
                    "invalid_policy",
                    "Visual input always requires explicit approval",
                ));
            }
        }
        if attach.focus && pending.values().any(|p| owns_input(&p.request)) {
            return Err(error(
                "input_owned",
                "Another operation currently owns desktop input",
            ));
        }
        let mut worker = make_worker("attach");
        worker.attach = Some(attach);
        if lease.is_none() {
            *lease = Some(input_lease()?);
        }
        pending.insert(
            request.id.clone(),
            start(request.clone(), worker, tx.clone(), None, lease.as_ref())?,
        );
        return Ok(None);
    }
    let session = context(sessions, request)?;
    if request.op == "pause" || request.op == "detach" {
        let mut uncertain = false;
        if let Some(id) = session.pending.take()
            && let Some(mut operation) = pending.remove(&id)
        {
            uncertain = matches!(operation.request.op.as_str(), "act" | "human_act");
            terminate(&mut operation.child)?;
            let mut failure = error(
                "cancelled",
                "Native operation cancelled during control transfer",
            );
            if uncertain {
                failure.delivery = Delivery::Unknown;
            }
            let response = Response::failure(operation.request.id.clone(), failure);
            if let (Some(binding), Ok(action)) = (
                operation.action_binding,
                decode::<Action>(operation.request.payload),
            ) {
                session
                    .actions
                    .insert(action.action_id, (binding, response.clone()));
            }
            output(response)?;
        }
        session.epoch = session
            .epoch
            .checked_add(1)
            .ok_or_else(|| error("epoch_exhausted", "Control epoch exhausted"))?;
        session.paused = true;
        session.observation = None;
        session.input_stamp = None;
        session.visual_targets.clear();
        session.approvals.clear();
        let result = json!({"epoch":session.epoch,"paused":true,"uncertain_delivery":uncertain});
        if request.op == "detach" {
            sessions.remove(
                request
                    .session_id
                    .as_ref()
                    .ok_or_else(|| error("session_required", "Session required"))?,
            );
        }
        return Ok(Some(result));
    }
    if session.pending.is_some() {
        return Err(error(
            "session_busy",
            "A native operation is already in progress for this session",
        ));
    }
    match request.op.as_str() {
        "focus" => {
            if pending.values().any(|p| owns_input(&p.request)) {
                return Err(error(
                    "input_owned",
                    "Another native operation currently owns desktop input",
                ));
            }
            session.observation = None;
            session.input_stamp = None;
            session.visual_targets.clear();
            session.approvals.clear();
            let mut worker = make_worker("focus");
            worker.window = Some(session.window.clone());
            pending.insert(
                request.id.clone(),
                start(request.clone(), worker, tx.clone(), None, lease.as_ref())?,
            );
            session.pending = Some(request.id.clone());
            Ok(None)
        }
        "observe" => {
            let mut worker = make_worker("observe");
            worker.window = Some(session.window.clone());
            if !session.paused {
                worker.expected_input = session.input_stamp.clone();
            }
            pending.insert(
                request.id.clone(),
                start(request.clone(), worker, tx.clone(), None, lease.as_ref())?,
            );
            session.pending = Some(request.id.clone());
            Ok(None)
        }
        "resolve" | "resolve_target" => {
            #[derive(serde::Deserialize)]
            #[serde(deny_unknown_fields)]
            struct Payload {
                observation_id: String,
                selector: Selector,
            }
            let payload: Payload = decode(request.payload.clone())?;
            let observed = observation(session, &payload.observation_id)?;
            if !observed.complete {
                return Err(error(
                    "observation_incomplete",
                    "Cannot claim selector uniqueness from a truncated tree",
                ));
            }
            payload.selector.validate()?;
            if let Some(anchor) = payload.selector.visual {
                if request.op != "resolve_target" {
                    return Err(error(
                        "visual_target_required",
                        "Visual anchors require resolve_target",
                    ));
                }
                if session.visual_targets.len() >= 64 {
                    return Err(error(
                        "target_limit",
                        "Visual target cache is full; observe again",
                    ));
                }
                let mut worker = make_worker("resolve_target");
                worker.window = Some(observed.window.clone());
                worker.capture = Some(observed.screenshot.clone());
                worker.observation_id = Some(payload.observation_id);
                worker.visual_anchor = Some(anchor);
                pending.insert(
                    request.id.clone(),
                    start(request.clone(), worker, tx.clone(), None, lease.as_ref())?,
                );
                session.pending = Some(request.id.clone());
                return Ok(None);
            }
            let node = resolve(&observed.nodes, &payload.selector)?;
            if request.op == "resolve_target" {
                return Ok(Some(json!({"kind":"semantic","node":node})));
            }
            Ok(Some(json!({"target_id":node.id,"node":node})))
        }
        "resume" => {
            #[derive(serde::Deserialize)]
            #[serde(deny_unknown_fields)]
            struct Payload {
                observation_id: String,
            }
            let payload: Payload = decode(request.payload.clone())?;
            observation(session, &payload.observation_id)?;
            if !session.paused {
                return Err(error(
                    "not_paused",
                    "Session is already controlled by automation",
                ));
            }
            session.paused = false;
            session.epoch = session
                .epoch
                .checked_add(1)
                .ok_or_else(|| error("epoch_exhausted", "Control epoch exhausted"))?;
            session.observation = None;
            session.visual_targets.clear();
            session.approvals.clear();
            Ok(Some(json!({"epoch":session.epoch,"paused":false})))
        }
        "approve" | "act" | "human_act" => {
            let action: Action = decode(request.payload.clone())?;
            action.validate()?;
            let session_id = request
                .session_id
                .as_deref()
                .ok_or_else(|| error("session_required", "Session required"))?;
            let binding = action.binding(session_id, &session.run_id, session.epoch)?;
            if matches!(request.op.as_str(), "act" | "human_act")
                && let Some((previous, response)) = session.actions.get(&action.action_id)
            {
                if previous != &binding {
                    return Err(error(
                        "action_id_conflict",
                        "Action ID was already used with different inputs or context",
                    ));
                }
                let mut response = response.clone();
                response.id = request.id.clone();
                output(response)?;
                return Ok(None);
            }
            if request.op == "act" && session.paused {
                return Err(error(
                    "human_control",
                    "Automation is paused; resume requires a fresh observation",
                ));
            }
            if request.op == "human_act" && !session.paused {
                return Err(error(
                    "automation_control",
                    "Human input requires pausing automation first",
                ));
            }
            let observed = observation(session, &action.observation_id)?;
            if !observed.complete {
                return Err(error(
                    "observation_incomplete",
                    "Cannot act using an incomplete observation",
                ));
            }
            let expected_state = state_fingerprint(&observed.nodes)?;
            let expected_nodes = observed.nodes.clone();
            let (target, visual_anchor) =
                if let Some((visual, anchor)) = session.visual_targets.get(&action.target_id) {
                    if !matches!(action.kind, desktop_core::ActionKind::Click)
                        || action.x.is_some()
                        || action.y.is_some()
                        || action.text.is_some()
                        || action.scroll_y.is_some()
                    {
                        return Err(error(
                            "invalid_action",
                            "Visual targets accept only their bound click offset",
                        ));
                    }
                    (
                        ResolvedTarget::Visual {
                            visual: visual.clone(),
                        },
                        Some(anchor.clone()),
                    )
                } else {
                    let node = observed
                        .nodes
                        .iter()
                        .find(|n| n.id == action.target_id)
                        .ok_or_else(|| {
                            error(
                                "target_missing",
                                "Target does not belong to this observation",
                            )
                        })?
                        .clone();
                    (ResolvedTarget::Semantic { node }, None)
                };
            let needs_approval = match &target {
                ResolvedTarget::Visual { .. } => true,
                ResolvedTarget::Semantic { node } => {
                    action.effect == Effect::Change
                        || !session
                            .read_only_targets
                            .iter()
                            .any(|s| resolve(&observed.nodes, s).is_ok_and(|n| n.id == node.id))
                }
            };
            if request.op == "approve" {
                if session.approvals.len() >= 16 {
                    return Err(error(
                        "approval_limit",
                        "Approval limit reached; observe again to invalidate old approvals",
                    ));
                }
                *counter += 1;
                let id = format!("approval-{}-{counter}", now_ms());
                session.approvals.insert(id.clone(), binding);
                return Ok(Some(
                    json!({"approval_id":id,"epoch":session.epoch,"single_use":true}),
                ));
            }
            if pending.values().any(|p| owns_input(&p.request)) {
                return Err(error(
                    "input_owned",
                    "Another session currently owns desktop input",
                ));
            }
            if needs_approval {
                let approval = action
                    .approval_id
                    .as_ref()
                    .and_then(|id| session.approvals.remove(id));
                if approval.as_ref() != Some(&binding) {
                    return Err(error(
                        "approval_required",
                        "A single-use approval for this exact action is required",
                    ));
                }
            }
            if session.actions.len() >= 4096 {
                return Err(error(
                    "action_limit",
                    "Session action journal is full; start a new session",
                ));
            }
            let mut worker = make_worker("act");
            worker.window = Some(session.window.clone());
            worker.action = Some(action);
            worker.target = Some(target);
            worker.visual_anchor = visual_anchor;
            worker.expected_state = Some(expected_state);
            worker.expected_nodes = Some(expected_nodes);
            worker.expected_input = session.input_stamp.clone();
            pending.insert(
                request.id.clone(),
                start(
                    request.clone(),
                    worker,
                    tx.clone(),
                    Some(binding),
                    lease.as_ref(),
                )?,
            );
            session.pending = Some(request.id.clone());
            Ok(None)
        }
        _ => Err(error("unknown_operation", "Unknown broker operation")),
    }
}

fn cache_visual_target(
    session: &mut Session,
    mut visual: VisualMatch,
    anchor: VisualAnchor,
    counter: &mut u64,
) -> Result<Value> {
    *counter += 1;
    // Neither the public identity nor any public digest may encode raw pixels:
    // protected regions could otherwise be guessed via a hash oracle.
    visual.target_id = format!("visual-{}-{counter}", now_ms());
    let mut public = visual.clone();
    public.frame_sha256.clear();
    let value = serde_json::to_value(ResolvedTarget::Visual { visual: public })
        .map_err(|e| DesktopError::new("serialization", e.to_string()))?;
    session
        .visual_targets
        .insert(visual.target_id.clone(), (visual, anchor));
    Ok(value)
}

fn cache_observation(session: &mut Session, mut observed: Observation) -> Result<Value> {
    // Input counters never cross the native privacy boundary. Keep the baseline
    // after our own actions so intervening hardware input cannot be forgotten by
    // merely requesting another observation.
    session.input_stamp = observed.input_stamp.take();
    session.window = observed.window.clone();
    session.approvals.clear();
    session.visual_targets.clear();
    let value = serde_json::to_value(&observed)
        .map_err(|e| DesktopError::new("serialization", e.to_string()))?;
    session.observation = Some(observed);
    Ok(value)
}

fn suspend_external_interaction(session: &mut Session, response: &Response) {
    if response
        .error
        .as_ref()
        .is_some_and(|e| e.code == "external_interaction")
    {
        // Do not invisibly advance epoch: the host receives the explicit error
        // and calls pause to acquire the next epoch for reconciliation.
        session.paused = true;
        session.observation = None;
        session.input_stamp = None;
        session.visual_targets.clear();
        session.approvals.clear();
    }
}

fn completed(
    mut operation: Pending,
    result: Result<Value>,
    sessions: &mut HashMap<String, Session>,
    counter: &mut u64,
    lease: &mut Option<std::fs::File>,
    pending: &HashMap<String, Pending>,
) -> Result<()> {
    // The helper emits its final response only after native work finishes. Reap it
    // without allowing a stuck teardown to freeze the broker.
    terminate(&mut operation.child)?;
    let request = operation.request;
    let result = (|| -> Result<Value> {
        let value = result?;
        if request.op == "attach" {
            let window: Window = decode(value)?;
            let attach: Attach = decode(request.payload.clone())?;
            *counter += 1;
            let id = format!("session-{}-{counter}", now_ms());
            let run_id = request
                .run_id
                .clone()
                .ok_or_else(|| error("run_required", "Missing run ID"))?;
            sessions.insert(
                id.clone(),
                Session {
                    run_id,
                    epoch: 1,
                    paused: false,
                    window: window.clone(),
                    observation: None,
                    input_stamp: None,
                    visual_targets: HashMap::new(),
                    read_only_targets: attach.read_only_targets,
                    approvals: HashMap::new(),
                    actions: HashMap::new(),
                    pending: None,
                },
            );
            return Ok(json!({"session_id":id,"epoch":1,"window":window}));
        }
        if let Some(id) = &request.session_id {
            let session = sessions
                .get_mut(id)
                .ok_or_else(|| error("session_missing", "Session was detached"))?;
            if request.epoch != Some(session.epoch) {
                return Err(error(
                    "stale_epoch",
                    "Native response belongs to an old control epoch",
                ));
            }
            if request.op == "focus" {
                let window: Window = decode(value)?;
                session.window = window.clone();
                return Ok(json!({"window": window, "epoch": session.epoch}));
            }
            if request.op == "resolve_target" {
                let target: ResolvedTarget = decode(value.clone())?;
                let ResolvedTarget::Visual { visual } = target else {
                    return Err(error(
                        "invalid_target",
                        "Visual helper returned a semantic target",
                    ));
                };
                let selector: Selector = decode(
                    request
                        .payload
                        .get("selector")
                        .cloned()
                        .ok_or_else(|| error("invalid_request", "Selector required"))?,
                )?;
                let anchor = selector
                    .visual
                    .ok_or_else(|| error("invalid_request", "Visual anchor required"))?;
                let observed_id = request
                    .payload
                    .get("observation_id")
                    .and_then(Value::as_str)
                    .ok_or_else(|| error("invalid_request", "Observation identity required"))?;
                observation(session, observed_id)?;
                return cache_visual_target(session, visual, anchor, counter);
            }
            if request.op == "observe" {
                let mut observation: Observation = decode(value)?;
                *counter += 1;
                observation.observation_id = format!("observation-{}-{counter}", now_ms());
                observation.epoch = session.epoch;
                // Keep one bounded raw frame only in memory for supervised visual
                // matching. Observation replacement/control transfer drops it.
                return cache_observation(session, observation);
            }
        }
        Ok(value)
    })();
    let response = match result {
        Ok(value) => Response::success(request.id.clone(), value),
        Err(mut e) => {
            if matches!(request.op.as_str(), "act" | "human_act") && e.code.starts_with("helper_") {
                e.delivery = Delivery::Unknown;
            }
            Response::failure(request.id.clone(), e)
        }
    };
    if let Some(id) = &request.session_id
        && let Some(session) = sessions.get_mut(id)
    {
        session.pending = None;
        suspend_external_interaction(session, &response);
        if matches!(request.op.as_str(), "act" | "human_act") {
            session.observation = None;
            session.visual_targets.clear();
            session.approvals.clear();
            if let (Some(binding), Ok(action)) =
                (operation.action_binding, decode::<Action>(request.payload))
            {
                session
                    .actions
                    .insert(action.action_id, (binding, response.clone()));
            }
        }
    }
    release_idle_lease(lease, sessions, pending);
    output(response)
}

fn input_lease() -> Result<std::fs::File> {
    let home_key = if cfg!(windows) {
        "LOCALAPPDATA"
    } else {
        "HOME"
    };
    let profile = std::env::var_os(home_key)
        .ok_or_else(|| error("input_lease", "User profile directory is unavailable"))?;
    let dir = std::path::PathBuf::from(profile).join(if cfg!(windows) {
        "Manvi/native"
    } else {
        ".cache/manvi/native"
    });
    let mut builder = std::fs::DirBuilder::new();
    builder.recursive(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        builder.mode(0o700);
    }
    builder
        .create(&dir)
        .map_err(|e| DesktopError::new("input_lease", e.to_string()))?;
    let mut options = std::fs::OpenOptions::new();
    options.read(true).write(true).create(true).truncate(false);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let file = options
        .open(dir.join("desktop.lock"))
        .map_err(|e| DesktopError::new("input_lease", e.to_string()))?;
    file.try_lock().map_err(|e| {
        DesktopError::new(
            "input_owned",
            format!("Cannot acquire the account desktop input lease: {e}"),
        )
    })?;
    Ok(file)
}

fn release_idle_lease(
    lease: &mut Option<std::fs::File>,
    sessions: &HashMap<String, Session>,
    pending: &HashMap<String, Pending>,
) {
    if sessions.is_empty() && pending.values().all(|p| p.request.op == "probe") {
        *lease = None;
    }
}

fn serve() -> Result<()> {
    let mut input_lease = None;
    let (tx, rx) = mpsc::sync_channel(32);
    let input_tx = tx.clone();
    thread::spawn(move || {
        let mut reader = BufReader::new(io::stdin());
        loop {
            match read_bounded(&mut reader, MAX_REQUEST_BYTES) {
                Ok(Some(bytes)) => {
                    let event = match serde_json::from_slice(&bytes) {
                        Ok(request) => Event::Request(request),
                        Err(e) => Event::Invalid(DesktopError::new("invalid_json", e.to_string())),
                    };
                    if input_tx.send(event).is_err() {
                        break;
                    }
                }
                Ok(None) => {
                    let _closed = input_tx.send(Event::Eof);
                    break;
                }
                Err(e) => {
                    let _closed = input_tx.send(Event::Invalid(DesktopError::new(
                        "request_limit",
                        e.to_string(),
                    )));
                    let _closed = input_tx.send(Event::Eof);
                    break;
                }
            }
        }
    });
    let mut sessions = HashMap::new();
    let mut pending: HashMap<String, Pending> = HashMap::new();
    let mut ids = HashSet::new();
    let mut counter = 0;
    loop {
        match rx.recv_timeout(Duration::from_millis(20)) {
            Ok(Event::Request(request)) => {
                if ids.len() >= 100_000 {
                    output(Response::failure(
                        request.id,
                        error(
                            "request_limit",
                            "Broker request history limit reached; restart broker",
                        ),
                    ))?;
                    continue;
                }
                if !ids.insert(request.id.clone()) {
                    output(Response::failure(
                        request.id,
                        error("duplicate_request", "Request ID was already used"),
                    ))?;
                    continue;
                }
                let result = dispatch(
                    &request,
                    &mut sessions,
                    &mut pending,
                    &tx,
                    &mut counter,
                    &mut input_lease,
                );
                release_idle_lease(&mut input_lease, &sessions, &pending);
                match result {
                    Ok(Some(value)) => output(Response::success(request.id, value))?,
                    Ok(None) => {}
                    Err(e) => output(Response::failure(request.id, e))?,
                }
            }
            Ok(Event::Done(id, result)) => {
                if let Some(operation) = pending.remove(&id) {
                    completed(
                        operation,
                        result,
                        &mut sessions,
                        &mut counter,
                        &mut input_lease,
                        &pending,
                    )?;
                }
            }
            Ok(Event::Invalid(e)) => output(Response::failure("invalid", e))?,
            Ok(Event::Eof) => {
                pending.clear();
                break;
            }
            Err(mpsc::RecvTimeoutError::Timeout) => {}
            Err(mpsc::RecvTimeoutError::Disconnected) => break,
        }
        let expired: Vec<_> = pending
            .iter()
            .filter(|(_, p)| p.started.elapsed() > HELPER_TIMEOUT)
            .map(|(id, _)| id.clone())
            .collect();
        for id in expired {
            if let Some(mut operation) = pending.remove(&id) {
                let _killed = operation.child.kill();
                completed(
                    operation,
                    Err(error("helper_timeout", "Native helper deadline exceeded")),
                    &mut sessions,
                    &mut counter,
                    &mut input_lease,
                    &pending,
                )?;
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    // The lease deliberately crosses exec. Serialize fixture spawns so another
    // concurrent test cannot inherit a lease whose ownership it is not testing.
    use crate::platform::CHILD_FIXTURE_LOCK;
    #[test]
    fn bounds_protocol_frames() {
        assert!(read_bounded(&mut io::Cursor::new(b"12345\n"), 4).is_err());
        assert_eq!(
            read_bounded(&mut io::Cursor::new(b"{}\n"), 4).unwrap(),
            Some(b"{}\n".to_vec())
        );
    }
    #[test]
    fn run_and_epoch_are_mandatory() {
        let mut sessions = HashMap::new();
        sessions.insert(
            "s".into(),
            Session {
                run_id: "r".into(),
                epoch: 2,
                paused: false,
                window: Window {
                    pid: 1,
                    window_id: 1,
                    title: String::new(),
                    process_identity: "p".into(),
                    bounds: Default::default(),
                    foreground: false,
                    scale: 1.,
                },
                observation: None,
                input_stamp: None,
                visual_targets: HashMap::new(),
                read_only_targets: vec![],
                approvals: HashMap::new(),
                actions: HashMap::new(),
                pending: None,
            },
        );
        let request = Request {
            id: "x".into(),
            op: "observe".into(),
            session_id: Some("s".into()),
            run_id: Some("r".into()),
            epoch: Some(1),
            payload: Value::Null,
        };
        assert!(matches!(context(&mut sessions,&request),Err(e) if e.code=="stale_epoch"));
    }
    fn fixture() -> (HashMap<String, Session>, Request) {
        let window = Window {
            pid: 1,
            window_id: 1,
            title: "Bank".into(),
            process_identity: "process-1".into(),
            bounds: desktop_core::Bounds {
                x: 0.,
                y: 0.,
                width: 100.,
                height: 100.,
            },
            foreground: true,
            scale: 1.,
        };
        let node = desktop_core::Node {
            id: "field".into(),
            role: "text_field".into(),
            name: "Member ID".into(),
            value: Some(String::new()),
            identifier: None,
            parent: None,
            bounds: Some(window.bounds),
            enabled: true,
            editable: true,
            actions: vec!["set_value".into()],
            native_path: vec![],
        };
        let observation = Observation {
            observation_id: "observed".into(),
            epoch: 1,
            captured_at_ms: now_ms(),
            tree_at_ms: now_ms(),
            window: window.clone(),
            nodes: vec![node],
            screenshot: desktop_core::Screenshot {
                mime_type: "image/png".into(),
                base64: String::new(),
                width: 100,
                height: 100,
            },
            complete: true,
            input_stamp: None,
            truncated_reason: None,
        };
        let session = Session {
            run_id: "run".into(),
            epoch: 1,
            paused: false,
            window,
            observation: Some(observation),
            input_stamp: None,
            visual_targets: HashMap::new(),
            read_only_targets: vec![],
            approvals: HashMap::new(),
            actions: HashMap::new(),
            pending: None,
        };
        let request = Request {
            id: "request".into(),
            op: "act".into(),
            session_id: Some("session".into()),
            run_id: Some("run".into()),
            epoch: Some(1),
            payload: json!({"action_id":"action","observation_id":"observed","target_id":"field","kind":"set_value","text":"M-1001","effect":"read"}),
        };
        (HashMap::from([("session".into(), session)]), request)
    }
    fn dispatch_without_helper(
        request: &Request,
        sessions: &mut HashMap<String, Session>,
    ) -> Result<Option<Value>> {
        let (tx, _rx) = mpsc::sync_channel(32);
        dispatch(
            request,
            sessions,
            &mut HashMap::new(),
            &tx,
            &mut 0,
            &mut None,
        )
    }

    fn visual_fixture() -> (HashMap<String, Session>, Request) {
        let (mut sessions, mut request) = fixture();
        let session = sessions.get_mut("session").unwrap();
        let anchor = VisualAnchor {
            png_base64: "fixture".into(),
            sha256: "0".repeat(64),
            width: 8,
            height: 8,
            frame_width: 100,
            frame_height: 100,
            window_width: 100.,
            window_height: 100.,
            scale: 1.,
            search: desktop_core::PixelRect {
                x: 0,
                y: 0,
                width: 100,
                height: 100,
            },
            click: desktop_core::PixelPoint { x: 4, y: 4 },
        };
        // This policy fixture represents a matcher result already in the broker
        // cache. PNG decoding/uniqueness have separate core matcher regressions.
        let visual = VisualMatch {
            target_id: "visual-fixture".into(),
            anchor_sha256: anchor.sha256.clone(),
            frame_sha256: "1".repeat(64),
            matched: desktop_core::PixelRect {
                x: 0,
                y: 0,
                width: 8,
                height: 8,
            },
            screen_bounds: desktop_core::Bounds {
                x: 0.,
                y: 0.,
                width: 8.,
                height: 8.,
            },
        };
        session
            .visual_targets
            .insert(visual.target_id.clone(), (visual, anchor));
        session.read_only_targets.push(Selector {
            name: Some("Member ID".into()),
            ..Selector::default()
        });
        request.payload = json!({"action_id":"visual-action","observation_id":"observed","target_id":"visual-fixture","kind":"click","effect":"read"});
        (sessions, request)
    }

    #[test]
    fn private_input_counters_are_cached_but_never_published_or_stored_in_the_public_tree() {
        let (mut sessions, _) = fixture();
        let session = sessions.get_mut("session").unwrap();
        let mut observed = session.observation.clone().unwrap();
        observed.input_stamp = Some(InputStamp::MacosHid { counters: [73; 15] });
        let public = cache_observation(session, observed).unwrap();
        assert!(public.get("input_stamp").is_none());
        assert!(session.observation.as_ref().unwrap().input_stamp.is_none());
        let Some(InputStamp::MacosHid { counters }) = session.input_stamp.as_ref() else {
            panic!("private admission baseline must be retained")
        };
        assert_eq!(counters, &[73; 15]);
    }

    #[test]
    fn hardware_interference_suspends_without_hiding_epoch_change_and_pause_still_fences() {
        let (mut sessions, mut request) = visual_fixture();
        let session = sessions.get_mut("session").unwrap();
        session.input_stamp = Some(InputStamp::MacosHid { counters: [3; 15] });
        session
            .approvals
            .insert("pending-grant".into(), "binding".into());
        let refusal = Response::failure(
            "request",
            DesktopError::new("external_interaction", "reconcile"),
        );
        suspend_external_interaction(session, &refusal);
        assert_eq!(session.epoch, 1);
        assert!(session.paused);
        assert!(session.observation.is_none());
        assert!(session.input_stamp.is_none());
        assert!(session.approvals.is_empty());
        assert!(session.visual_targets.is_empty());
        assert_eq!(
            dispatch_without_helper(&request, &mut sessions)
                .unwrap_err()
                .code,
            "human_control"
        );
        request.op = "pause".into();
        request.payload = json!({});
        let paused = dispatch_without_helper(&request, &mut sessions)
            .unwrap()
            .unwrap();
        assert_eq!(paused["epoch"], 2);
        request.op = "resume".into();
        request.epoch = Some(2);
        request.payload = json!({"observation_id":"observed"});
        assert_eq!(
            dispatch_without_helper(&request, &mut sessions)
                .unwrap_err()
                .code,
            "stale_observation"
        );
    }

    #[test]
    fn ordinary_native_errors_do_not_claim_external_hardware_interference() {
        let (mut sessions, _) = fixture();
        let session = sessions.get_mut("session").unwrap();
        suspend_external_interaction(
            session,
            &Response::failure("r", DesktopError::new("state_changed", "different form")),
        );
        assert!(!session.paused);
        assert!(session.observation.is_some());
    }

    #[test]
    fn published_visual_target_does_not_leak_private_frame_digest_or_derived_identity() {
        let (mut sessions, _) = visual_fixture();
        let session = sessions.get_mut("session").unwrap();
        let (visual, anchor) = session.visual_targets.remove("visual-fixture").unwrap();
        let raw_digest = visual.frame_sha256.clone();
        let private_id = visual.target_id.clone();
        let mut counter = 70;
        let public = cache_visual_target(session, visual, anchor, &mut counter).unwrap();
        assert!(public["visual"].get("frame_sha256").is_none());
        let id = public["visual"]["target_id"].as_str().unwrap();
        assert_ne!(id, private_id);
        assert!(id.starts_with("visual-"));
        assert!(id.ends_with("-71"));
        assert_eq!(session.visual_targets[id].0.frame_sha256, raw_digest);
        assert_eq!(session.visual_targets[id].0.target_id, id);
        assert!(!public.to_string().contains(&raw_digest));
        let parsed: ResolvedTarget = serde_json::from_value(public).unwrap();
        let ResolvedTarget::Visual { visual } = parsed else {
            panic!("visual target expected")
        };
        assert!(visual.frame_sha256.is_empty());
    }

    #[test]
    fn visual_read_click_requires_approval_and_control_transfer_drops_it() {
        let (mut sessions, mut request) = visual_fixture();
        assert_eq!(
            dispatch_without_helper(&request, &mut sessions)
                .unwrap_err()
                .code,
            "approval_required"
        );
        request.op = "approve".into();
        let grant = dispatch_without_helper(&request, &mut sessions)
            .unwrap()
            .unwrap();
        assert!(grant["single_use"].as_bool().unwrap());
        request.op = "pause".into();
        request.payload = json!({});
        dispatch_without_helper(&request, &mut sessions).unwrap();
        assert!(sessions["session"].approvals.is_empty());
        assert!(sessions["session"].visual_targets.is_empty());
        assert!(sessions["session"].observation.is_none());
    }

    #[test]
    fn visual_targets_cannot_enter_trusted_read_allowance() {
        let (sessions, _) = visual_fixture();
        let anchor = &sessions["session"].visual_targets["visual-fixture"].1;
        let request = Request {
            id: "attach".into(),
            op: "attach".into(),
            session_id: None,
            run_id: Some("run".into()),
            epoch: None,
            payload: json!({"pid":42,"read_only_targets":[{"visual":anchor}]}),
        };
        assert_eq!(
            dispatch_without_helper(&request, &mut HashMap::new())
                .unwrap_err()
                .code,
            "invalid_policy"
        );
    }
    #[test]
    fn read_effect_cannot_bypass_form_input_policy() {
        let (mut sessions, request) = fixture();
        assert_eq!(
            dispatch_without_helper(&request, &mut sessions)
                .unwrap_err()
                .code,
            "approval_required"
        );
    }
    #[test]
    fn modified_approval_is_consumed_and_cannot_be_reused() {
        let (mut sessions, mut request) = fixture();
        request.op = "approve".into();
        let grant = dispatch_without_helper(&request, &mut sessions)
            .unwrap()
            .unwrap();
        request.op = "act".into();
        request.payload["approval_id"] = grant["approval_id"].clone();
        request.payload["text"] = json!("M-9999");
        assert_eq!(
            dispatch_without_helper(&request, &mut sessions)
                .unwrap_err()
                .code,
            "approval_required"
        );
        request.payload["text"] = json!("M-1001");
        assert_eq!(
            dispatch_without_helper(&request, &mut sessions)
                .unwrap_err()
                .code,
            "approval_required"
        );
        assert!(sessions["session"].approvals.is_empty());
    }
    #[test]
    fn pause_fences_old_epoch_observation_and_approval() {
        let (mut sessions, mut request) = fixture();
        request.op = "approve".into();
        dispatch_without_helper(&request, &mut sessions).unwrap();
        request.op = "pause".into();
        request.payload = json!({});
        let pause = dispatch_without_helper(&request, &mut sessions)
            .unwrap()
            .unwrap();
        assert_eq!(pause["epoch"], 2);
        assert!(sessions["session"].observation.is_none());
        assert!(sessions["session"].approvals.is_empty());
        request.op = "observe".into();
        assert_eq!(
            dispatch_without_helper(&request, &mut sessions)
                .unwrap_err()
                .code,
            "stale_epoch"
        );
        request.epoch = Some(2);
        request.op = "resume".into();
        request.payload = json!({"observation_id":"observed"});
        assert_eq!(
            dispatch_without_helper(&request, &mut sessions)
                .unwrap_err()
                .code,
            "stale_observation"
        );
    }
    #[test]
    fn human_and_automation_ownership_are_mutually_exclusive() {
        let (mut sessions, mut request) = fixture();
        request.op = "human_act".into();
        assert_eq!(
            dispatch_without_helper(&request, &mut sessions)
                .unwrap_err()
                .code,
            "automation_control"
        );
        sessions.get_mut("session").unwrap().paused = true;
        request.op = "act".into();
        assert_eq!(
            dispatch_without_helper(&request, &mut sessions)
                .unwrap_err()
                .code,
            "human_control"
        );
    }
    #[test]
    fn no_action_or_approval_from_truncated_or_expired_observation() {
        let (mut sessions, mut request) = fixture();
        request.op = "approve".into();
        sessions
            .get_mut("session")
            .unwrap()
            .observation
            .as_mut()
            .unwrap()
            .complete = false;
        assert_eq!(
            dispatch_without_helper(&request, &mut sessions)
                .unwrap_err()
                .code,
            "observation_incomplete"
        );
        sessions
            .get_mut("session")
            .unwrap()
            .observation
            .as_mut()
            .unwrap()
            .complete = true;
        sessions
            .get_mut("session")
            .unwrap()
            .observation
            .as_mut()
            .unwrap()
            .captured_at_ms = now_ms() - OBSERVATION_TTL_MS - 1;
        assert_eq!(
            dispatch_without_helper(&request, &mut sessions)
                .unwrap_err()
                .code,
            "stale_observation"
        );
    }
    #[test]
    fn action_id_reuse_after_control_transfer_is_rejected() {
        let (mut sessions, request) = fixture();
        sessions.get_mut("session").unwrap().actions.insert(
            "action".into(),
            (
                "different-binding".into(),
                Response::failure("old", error("cancelled", "cancelled").uncertain()),
            ),
        );
        assert_eq!(
            dispatch_without_helper(&request, &mut sessions)
                .unwrap_err()
                .code,
            "action_id_conflict"
        );
    }
    #[test]
    fn helper_fixture_blocks_until_parent_closes_pipe() {
        if std::env::var_os("MANVI_TEST_BLOCK_HELPER").is_none() {
            return;
        }
        let mut data = Vec::new();
        std::io::Read::read_to_end(&mut io::stdin(), &mut data).unwrap();
    }
    #[test]
    fn pause_interrupts_blocked_helper_and_journals_uncertainty() {
        let _fixture_guard = CHILD_FIXTURE_LOCK.lock().unwrap();
        let (mut sessions, mut pause) = fixture();
        let child = Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "broker::tests::helper_fixture_blocks_until_parent_closes_pipe",
            ])
            .env("MANVI_TEST_BLOCK_HELPER", "1")
            .stdin(Stdio::piped())
            .stdout(Stdio::null())
            .spawn()
            .unwrap();
        let mut pending = HashMap::from([(
            "inflight".into(),
            Pending {
                child: SupervisedChild::new(child).unwrap(),
                request: pause.clone(),
                started: Instant::now(),
                action_binding: Some("cancelled-binding".into()),
            },
        )]);
        sessions.get_mut("session").unwrap().pending = Some("inflight".into());
        pause.op = "pause".into();
        pause.payload = json!({});
        let (tx, _rx) = mpsc::sync_channel(32);
        let start = Instant::now();
        let response = dispatch(&pause, &mut sessions, &mut pending, &tx, &mut 0, &mut None)
            .unwrap()
            .unwrap();
        assert!(start.elapsed() < Duration::from_secs(1));
        assert_eq!(response["uncertain_delivery"], true);
        assert!(pending.is_empty());
        assert_eq!(
            sessions["session"].actions["action"]
                .1
                .error
                .as_ref()
                .unwrap()
                .delivery,
            Delivery::Unknown
        );
    }
    #[test]
    fn dropping_supervised_child_terminates_blocked_native_work() {
        let _fixture_guard = CHILD_FIXTURE_LOCK.lock().unwrap();
        let child = Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "broker::tests::helper_fixture_blocks_until_parent_closes_pipe",
            ])
            .env("MANVI_TEST_BLOCK_HELPER", "1")
            .stdin(Stdio::piped())
            .stdout(Stdio::null())
            .spawn()
            .unwrap();
        let started = Instant::now();
        drop(SupervisedChild::new(child).unwrap());
        assert!(started.elapsed() < Duration::from_secs(1));
    }
    #[cfg(unix)]
    #[test]
    fn orphan_supervisor_identity_is_rejected() {
        assert_eq!(watch_parent(u32::MAX).unwrap_err().code, "parent_missing");
    }
    #[cfg(unix)]
    #[test]
    fn diagnostic_helper_does_not_hold_attached_session_lease() {
        let _fixture_guard = CHILD_FIXTURE_LOCK.lock().unwrap();
        let path = std::env::temp_dir().join(format!(
            "manvi-probe-lease-{}-{}",
            std::process::id(),
            now_ms()
        ));
        let lease = std::fs::File::create_new(&path).unwrap();
        lease.try_lock().unwrap();
        let mut command = Command::new(std::env::current_exe().unwrap());
        command
            .args([
                "--exact",
                "broker::tests::helper_fixture_blocks_until_parent_closes_pipe",
            ])
            .env("MANVI_TEST_BLOCK_HELPER", "1")
            .stdin(Stdio::piped())
            .stdout(Stdio::null());
        inherit_lease(&mut command, None);
        let child = SupervisedChild::new(command.spawn().unwrap()).unwrap();
        drop(lease);
        let other = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(&path)
            .unwrap();
        other.try_lock().unwrap();
        drop(child);
        drop(other);
        std::fs::remove_file(path).unwrap();
    }
    #[test]
    fn final_detach_releases_lease_but_paused_session_retains_it() {
        let path = std::env::temp_dir().join(format!(
            "manvi-session-lease-{}-{}",
            std::process::id(),
            now_ms()
        ));
        let mut lease = Some(std::fs::File::create_new(&path).unwrap());
        let (mut sessions, mut request) = fixture();
        request.op = "pause".into();
        request.payload = json!({});
        dispatch_without_helper(&request, &mut sessions).unwrap();
        release_idle_lease(&mut lease, &sessions, &HashMap::new());
        assert!(lease.is_some());
        request.op = "detach".into();
        request.epoch = Some(2);
        dispatch_without_helper(&request, &mut sessions).unwrap();
        release_idle_lease(&mut lease, &sessions, &HashMap::new());
        assert!(lease.is_none());
        std::fs::remove_file(path).unwrap();
    }
}
