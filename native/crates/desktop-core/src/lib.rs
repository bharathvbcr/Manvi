//! Versioned native desktop contracts. Platform handles never cross this boundary.
mod visual;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::time::{SystemTime, UNIX_EPOCH};
pub use visual::{
    PixelPoint, PixelRect, ResolvedTarget, VisualAnchor, VisualMatch, resolve_visual,
    revalidate_visual,
};

pub const PROTOCOL_VERSION: u32 = 1;
pub const MAX_WORKER_REQUEST_BYTES: usize = 21 * 1024 * 1024;
pub const MAX_REQUEST_BYTES: usize = 256 * 1024;
pub const MAX_RESPONSE_BYTES: usize = 20 * 1024 * 1024;
pub const MAX_NODES: usize = 2_000;
pub const MAX_DEPTH: usize = 40;
pub const MAX_TEXT_BYTES: usize = 8_192;
pub const MAX_IMAGE_PIXELS: usize = 8_000_000;

pub fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_millis().min(u128::from(u64::MAX)) as u64)
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Request {
    pub id: String,
    pub op: String,
    #[serde(default)]
    pub session_id: Option<String>,
    #[serde(default)]
    pub run_id: Option<String>,
    #[serde(default)]
    pub epoch: Option<u64>,
    #[serde(default)]
    pub payload: serde_json::Value,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Response {
    pub id: String,
    pub ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub result: Option<serde_json::Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<DesktopError>,
}

impl Response {
    pub fn success(id: impl Into<String>, result: serde_json::Value) -> Self {
        Self {
            id: id.into(),
            ok: true,
            result: Some(result),
            error: None,
        }
    }
    pub fn failure(id: impl Into<String>, error: DesktopError) -> Self {
        Self {
            id: id.into(),
            ok: false,
            result: None,
            error: Some(error),
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Delivery {
    NotSent,
    Sent,
    Unknown,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DesktopError {
    pub code: String,
    pub message: String,
    pub delivery: Delivery,
}
impl DesktopError {
    pub fn new(code: &str, message: impl Into<String>) -> Self {
        Self {
            code: code.into(),
            message: message.into(),
            delivery: Delivery::NotSent,
        }
    }
    pub fn uncertain(mut self) -> Self {
        self.delivery = Delivery::Unknown;
        self
    }
}
impl std::fmt::Display for DesktopError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}: {}", self.code, self.message)
    }
}
impl std::error::Error for DesktopError {}
pub type Result<T> = std::result::Result<T, DesktopError>;

#[derive(Debug, Clone, Copy, Serialize, Deserialize, Default, PartialEq)]
pub struct Bounds {
    pub x: f64,
    pub y: f64,
    pub width: f64,
    pub height: f64,
}
impl Bounds {
    pub fn valid(self) -> bool {
        [self.x, self.y, self.width, self.height]
            .into_iter()
            .all(f64::is_finite)
            && self.width > 0.0
            && self.height > 0.0
    }
    pub fn near(self, other: Self, tolerance: f64) -> bool {
        [
            self.x - other.x,
            self.y - other.y,
            self.width - other.width,
            self.height - other.height,
        ]
        .into_iter()
        .all(|v| v.is_finite() && v.abs() <= tolerance)
    }
    pub fn contains(self, x: f64, y: f64) -> bool {
        self.valid()
            && x >= self.x
            && y >= self.y
            && x < self.x + self.width
            && y < self.y + self.height
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Window {
    pub pid: u32,
    pub window_id: u64,
    pub title: String,
    pub process_identity: String,
    pub bounds: Bounds,
    pub foreground: bool,
    pub scale: f64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Node {
    pub id: String,
    pub role: String,
    pub name: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub value: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub identifier: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub parent: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub bounds: Option<Bounds>,
    pub enabled: bool,
    pub editable: bool,
    pub actions: Vec<String>,
    pub native_path: Vec<u32>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Screenshot {
    pub mime_type: String,
    pub base64: String,
    pub width: u32,
    pub height: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub enum InputStamp {
    MacosHid { counters: [u32; 15] },
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Observation {
    pub observation_id: String,
    pub epoch: u64,
    pub captured_at_ms: u64,
    pub tree_at_ms: u64,
    pub window: Window,
    pub nodes: Vec<Node>,
    pub screenshot: Screenshot,
    pub complete: bool,
    /// Private helper/broker admission state; removed from public observations.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub input_stamp: Option<InputStamp>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub truncated_reason: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(deny_unknown_fields)]
pub struct Selector {
    pub role: Option<String>,
    pub name: Option<String>,
    pub identifier: Option<String>,
    pub ancestor_name: Option<String>,
    pub visual: Option<VisualAnchor>,
}

impl Selector {
    pub fn validate(&self) -> Result<()> {
        if let Some(anchor) = &self.visual {
            if [
                &self.role,
                &self.name,
                &self.identifier,
                &self.ancestor_name,
            ]
            .into_iter()
            .any(Option::is_some)
            {
                return Err(DesktopError::new(
                    "invalid_selector",
                    "Visual and semantic selectors are mutually exclusive",
                ));
            }
            return anchor.validate();
        }
        if self.name.is_none() && self.identifier.is_none() {
            return Err(DesktopError::new(
                "invalid_selector",
                "An exact name or identifier is required",
            ));
        }
        if [
            &self.role,
            &self.name,
            &self.identifier,
            &self.ancestor_name,
        ]
        .into_iter()
        .flatten()
        .any(|v| v.trim().is_empty() || v.len() > MAX_TEXT_BYTES || v.contains('\0'))
        {
            return Err(DesktopError::new(
                "invalid_selector",
                "Selector fields must be nonempty and bounded",
            ));
        }
        Ok(())
    }
}

pub fn resolve<'a>(nodes: &'a [Node], selector: &Selector) -> Result<&'a Node> {
    selector.validate()?;
    if selector.visual.is_some() {
        return Err(DesktopError::new(
            "visual_target_required",
            "Visual anchors require tagged target resolution",
        ));
    }
    let matches = |node: &&Node| {
        if selector.role.as_ref().is_some_and(|s| &node.role != s)
            || selector.name.as_ref().is_some_and(|s| &node.name != s)
            || selector
                .identifier
                .as_ref()
                .is_some_and(|s| node.identifier.as_ref() != Some(s))
        {
            return false;
        }
        if let Some(name) = &selector.ancestor_name {
            let mut parent = node.parent.as_ref();
            for _ in 0..MAX_DEPTH {
                let Some(id) = parent else {
                    return false;
                };
                let Some(ancestor) = nodes.iter().find(|n| &n.id == id) else {
                    return false;
                };
                if &ancestor.name == name {
                    return true;
                }
                parent = ancestor.parent.as_ref();
            }
            return false;
        }
        true
    };
    let mut found = nodes.iter().filter(matches);
    let first = found
        .next()
        .ok_or_else(|| DesktopError::new("target_missing", "No element matches the selector"))?;
    if found.next().is_some() {
        return Err(DesktopError::new(
            "target_ambiguous",
            "More than one element matches the selector",
        ));
    }
    Ok(first)
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Attach {
    pub pid: u32,
    #[serde(default)]
    pub window_id: Option<u64>,
    /// Trusted host navigation: raise the unique attached window before admission.
    #[serde(default)]
    pub focus: bool,
    /// Trusted host policy; only these exact named controls allow read-effect input without approval.
    #[serde(default)]
    pub read_only_targets: Vec<Selector>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ActionKind {
    Press,
    SetValue,
    TypeText,
    Click,
    Scroll,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Effect {
    Read,
    Change,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Action {
    pub action_id: String,
    pub observation_id: String,
    pub target_id: String,
    pub kind: ActionKind,
    #[serde(default)]
    pub text: Option<String>,
    #[serde(default)]
    pub x: Option<f64>,
    #[serde(default)]
    pub y: Option<f64>,
    #[serde(default)]
    pub scroll_y: Option<i32>,
    pub effect: Effect,
    #[serde(default)]
    pub approval_id: Option<String>,
}

impl Action {
    pub fn validate(&self) -> Result<()> {
        if [
            self.action_id.as_str(),
            self.observation_id.as_str(),
            self.target_id.as_str(),
        ]
        .into_iter()
        .any(|s| s.is_empty() || s.len() > 160)
        {
            return Err(DesktopError::new(
                "invalid_action",
                "Action and target identities must be nonempty and bounded",
            ));
        }
        if self
            .text
            .as_ref()
            .is_some_and(|s| s.len() > MAX_TEXT_BYTES || s.contains('\0'))
        {
            return Err(DesktopError::new(
                "invalid_action",
                "Text exceeds limit or contains a NUL",
            ));
        }
        if matches!(self.kind, ActionKind::SetValue | ActionKind::TypeText) && self.text.is_none() {
            return Err(DesktopError::new("invalid_action", "Text is required"));
        }
        if self.x.is_some_and(|v| !v.is_finite()) || self.y.is_some_and(|v| !v.is_finite()) {
            return Err(DesktopError::new(
                "invalid_action",
                "Coordinates must be finite",
            ));
        }
        if matches!(self.kind, ActionKind::Scroll)
            && !self
                .scroll_y
                .is_some_and(|v| (-20..=20).contains(&v) && v != 0)
        {
            return Err(DesktopError::new(
                "invalid_action",
                "Scroll must be between -20 and 20, excluding zero",
            ));
        }
        Ok(())
    }
    pub fn binding(&self, session: &str, run: &str, epoch: u64) -> Result<String> {
        let mut action = self.clone();
        action.approval_id = None;
        let bytes = serde_json::to_vec(&(session, run, epoch, action))
            .map_err(|e| DesktopError::new("serialization", e.to_string()))?;
        Ok(format!("{:x}", Sha256::digest(bytes)))
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Receipt {
    pub action_id: String,
    pub delivery: Delivery,
    pub dispatched_at_ms: u64,
    pub verified: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WorkerRequest {
    pub op: String,
    pub broker_pid: Option<u32>,
    pub attach: Option<Attach>,
    pub window: Option<Window>,
    pub action: Option<Action>,
    pub target: Option<ResolvedTarget>,
    pub visual_anchor: Option<VisualAnchor>,
    pub capture: Option<Screenshot>,
    pub observation_id: Option<String>,
    pub expected_state: Option<String>,
    pub expected_nodes: Option<Vec<Node>>,
    #[serde(default)]
    pub expected_input: Option<InputStamp>,
}

pub fn state_fingerprint(nodes: &[Node]) -> Result<String> {
    // IDs and parent IDs are traversal-generated observation handles, not native
    // state. native_path already represents stable traversal ancestry. Retain
    // every actual node and all semantic/state/geometry fields in the binding.
    let state: Vec<_> = nodes
        .iter()
        .map(|n| {
            (
                &n.native_path,
                &n.role,
                &n.name,
                &n.value,
                &n.identifier,
                n.bounds,
                n.enabled,
                n.editable,
                &n.actions,
            )
        })
        .collect();
    let bytes = serde_json::to_vec(&state)
        .map_err(|e| DesktopError::new("serialization", e.to_string()))?;
    Ok(format!("{:x}", Sha256::digest(bytes)))
}

/// Diagnostic contains only normalized paths and changed attribute names;
/// it never includes text values, names, identifiers, or their reversible encodings.
pub fn state_difference(expected: &[Node], actual: &[Node]) -> String {
    let mut changes = Vec::new();
    let mut total = 0usize;
    for before in expected {
        if let Some(after) = actual.iter().find(|n| n.native_path == before.native_path) {
            let mut fields = Vec::new();
            if before.id != after.id {
                fields.push("id");
            }
            if before.role != after.role {
                fields.push("role");
            }
            if before.name != after.name {
                fields.push("name");
            }
            if before.value != after.value {
                fields.push("value");
            }
            if before.identifier != after.identifier {
                fields.push("identifier");
            }
            if before.parent != after.parent {
                fields.push("parent");
            }
            if before.bounds != after.bounds {
                fields.push("bounds");
            }
            if before.enabled != after.enabled {
                fields.push("enabled");
            }
            if before.editable != after.editable {
                fields.push("editable");
            }
            if before.actions != after.actions {
                fields.push("actions");
            }
            if !fields.is_empty() {
                total += 1;
                if changes.len() < 20 {
                    changes.push(format!("{:?}:{}", before.native_path, fields.join(",")));
                }
            }
        } else {
            total += 1;
            if changes.len() < 20 {
                changes.push(format!("{:?}:removed", before.native_path));
            }
        }
    }
    for after in actual {
        if !expected.iter().any(|n| n.native_path == after.native_path) {
            total += 1;
            if changes.len() < 20 {
                changes.push(format!("{:?}:added", after.native_path));
            }
        }
    }
    format!(
        "nodes {} -> {}; changed {total}; sample {}: {}",
        expected.len(),
        actual.len(),
        changes.len(),
        changes.join("; ")
    )
}

pub fn validate_state(expected: &[Node], actual: &[Node], fingerprint: &str) -> Result<()> {
    if state_fingerprint(expected)? != fingerprint {
        return Err(DesktopError::new(
            "invalid_request",
            "Worker expected-state binding is inconsistent",
        ));
    }
    if state_fingerprint(actual)? != fingerprint {
        return Err(DesktopError::new(
            "state_changed",
            format!(
                "Application state changed after observation: {}",
                state_difference(expected, actual)
            ),
        ));
    }
    Ok(())
}

pub fn same_target(expected: &Node, actual: &Node) -> bool {
    expected.role == actual.role
        && expected.name == actual.name
        && expected.identifier == actual.identifier
        && expected.native_path == actual.native_path
        && actual.enabled
        && match (expected.bounds, actual.bounds) {
            (Some(a), Some(b)) => a.near(b, 1.0),
            (None, None) => true,
            _ => false,
        }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn node(id: &str, name: &str) -> Node {
        Node {
            id: id.into(),
            role: "button".into(),
            name: name.into(),
            value: None,
            identifier: None,
            parent: None,
            bounds: None,
            enabled: true,
            editable: false,
            actions: vec!["press".into()],
            native_path: vec![],
        }
    }
    #[test]
    fn rejects_ambiguous_and_empty_selectors() {
        let nodes = vec![node("1", "Save"), node("2", "Save")];
        assert_eq!(
            resolve(
                &nodes,
                &Selector {
                    name: Some("Save".into()),
                    ..Default::default()
                }
            )
            .unwrap_err()
            .code,
            "target_ambiguous"
        );
        assert!(resolve(&nodes, &Selector::default()).is_err());
    }
    #[test]
    fn bounds_reject_nan_and_offscreen_point() {
        assert!(
            !Bounds {
                x: f64::NAN,
                y: 0.,
                width: 5.,
                height: 5.
            }
            .valid()
        );
        assert!(
            !Bounds {
                x: 0.,
                y: 0.,
                width: 5.,
                height: 5.
            }
            .contains(5., 2.)
        );
    }
    #[test]
    fn approval_binds_input_epoch_and_target() {
        let mut a = Action {
            action_id: "a".into(),
            observation_id: "o".into(),
            target_id: "n".into(),
            kind: ActionKind::SetValue,
            text: Some("one".into()),
            x: None,
            y: None,
            scroll_y: None,
            effect: Effect::Change,
            approval_id: None,
        };
        let hash = a.binding("s", "r", 1).unwrap();
        assert_ne!(hash, a.binding("s", "r", 2).unwrap());
        a.text = Some("two".into());
        assert_ne!(hash, a.binding("s", "r", 1).unwrap());
    }
    #[test]
    fn state_binding_includes_other_form_values_and_geometry() {
        let mut nodes = vec![node("1", "Confirm"), node("2", "Amount")];
        nodes[1].value = Some("100".into());
        let before = state_fingerprint(&nodes).unwrap();
        nodes[1].value = Some("900".into());
        assert_ne!(before, state_fingerprint(&nodes).unwrap());
        nodes[1].value = Some("100".into());
        nodes[0].bounds = Some(Bounds {
            x: 1.,
            y: 1.,
            width: 10.,
            height: 10.,
        });
        assert_ne!(before, state_fingerprint(&nodes).unwrap());
    }
    #[test]
    fn ancestor_selector_disambiguates_and_bounds_cyclic_ancestry() {
        let mut first = node("1", "Save");
        first.parent = Some("a".into());
        let mut second = node("2", "Save");
        second.parent = Some("b".into());
        let mut a = node("a", "North");
        a.parent = Some("a".into());
        let nodes = vec![first, second, a, node("b", "South")];
        let selected = resolve(
            &nodes,
            &Selector {
                name: Some("Save".into()),
                ancestor_name: Some("North".into()),
                ..Default::default()
            },
        )
        .unwrap();
        assert_eq!(selected.id, "1");
        assert!(
            resolve(
                &nodes,
                &Selector {
                    name: Some("Save".into()),
                    ancestor_name: Some("Missing".into()),
                    ..Default::default()
                }
            )
            .is_err()
        );
    }
    #[test]
    fn generated_handles_do_not_change_semantic_state_binding() {
        let mut nodes = vec![node("n1", "Member ID")];
        nodes[0].native_path = vec![0, 4];
        nodes[0].parent = Some("n0".into());
        let before = state_fingerprint(&nodes).unwrap();
        nodes[0].id = "n500".into();
        nodes[0].parent = Some("n499".into());
        assert_eq!(before, state_fingerprint(&nodes).unwrap());
        nodes[0].value = Some("M-1002".into());
        assert_ne!(before, state_fingerprint(&nodes).unwrap());
    }
    #[test]
    fn diagnostic_identifies_fields_without_disclosing_values() {
        let before = vec![node("field", "Private customer name")];
        let mut after = before.clone();
        after[0].value = Some("SECRET-MEMBER".into());
        let difference = state_difference(&before, &after);
        assert!(difference.contains("value"));
        assert!(!difference.contains("SECRET"));
        assert!(!difference.contains("Private"));
    }
}
