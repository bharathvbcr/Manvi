//! Linux execution is explicitly X11 + AT-SPI; Wayland sessions fail closed.
use atspi::{
    AccessibilityConnection, CoordType, Interface, ObjectRefOwned, Role, State,
    proxy::{
        accessible::ObjectRefExt, action::ActionProxy, component::ComponentProxy,
        editable_text::EditableTextProxy, text::TextProxy,
    },
    zbus::{self, proxy::CacheProperties},
};
use base64::{Engine, engine::general_purpose::STANDARD};
use desktop_core::{
    Action, ActionKind, Attach, Bounds, Delivery, DesktopError, MAX_DEPTH, MAX_IMAGE_PIXELS,
    MAX_NODES, MAX_TEXT_BYTES, Node, Observation, Receipt, ResolvedTarget, Result, Screenshot,
    VisualAnchor, VisualMatch, Window, WorkerRequest, now_ms, revalidate_visual, same_target,
    validate_state,
};
use image::{DynamicImage, ImageFormat, RgbaImage};
use serde_json::{Value, json};
use std::{collections::HashSet, io::Cursor, time::Duration};
use x11rb::{
    connection::Connection,
    protocol::{
        xproto::{
            AtomEnum, ClientMessageEvent, ConnectionExt, EventMask, ImageFormat as XImageFormat,
            ImageOrder, MapState, WindowClass,
        },
        xtest::ConnectionExt as XTestExt,
    },
    rust_connection::RustConnection,
};

fn err(code: &str, message: impl Into<String>) -> DesktopError {
    DesktopError::new(code, message)
}
fn native(e: impl std::fmt::Display) -> DesktopError {
    err("native_api", e.to_string())
}
fn bounded(text: String) -> Result<String> {
    if text.len() > MAX_TEXT_BYTES {
        Err(err(
            "attribute_limit",
            "Accessibility text exceeds byte limit",
        ))
    } else {
        Ok(text)
    }
}
fn process_identity(pid: u32) -> Result<String> {
    let stat = std::fs::read_to_string(format!("/proc/{pid}/stat")).map_err(native)?;
    let end = stat
        .rfind(')')
        .ok_or_else(|| err("identity_invalid", "Invalid process stat record"))?;
    let start = stat[end + 1..]
        .split_whitespace()
        .nth(19)
        .ok_or_else(|| err("identity_invalid", "Process start time unavailable"))?;
    let executable = std::fs::read_link(format!("/proc/{pid}/exe")).map_err(native)?;
    Ok(format!("{pid}:{start}:{}", executable.display()))
}
struct Display {
    connection: RustConnection,
    root: u32,
}
impl Display {
    fn new() -> Result<Self> {
        if std::env::var("XDG_SESSION_TYPE").is_ok_and(|v| v == "wayland") {
            return Err(err(
                "unsupported_session",
                "Linux backend requires an X11 desktop session",
            ));
        }
        let (connection, screen) = x11rb::connect(None).map_err(native)?;
        let root = connection.setup().roots[screen].root;
        Ok(Self { connection, root })
    }
    fn atom(&self, name: &str) -> Result<u32> {
        Ok(self
            .connection
            .intern_atom(false, name.as_bytes())
            .map_err(native)?
            .reply()
            .map_err(native)?
            .atom)
    }
    fn cardinal(&self, window: u32, name: &str) -> Result<Vec<u32>> {
        let reply = self
            .connection
            .get_property(false, window, self.atom(name)?, AtomEnum::ANY, 0, 8192)
            .map_err(native)?
            .reply()
            .map_err(native)?;
        if reply.bytes_after != 0 {
            return Err(err(
                "property_limit",
                "X11 property exceeds bounded response",
            ));
        }
        Ok(reply.value32().map_or_else(Vec::new, Iterator::collect))
    }
    fn window(&self, id: u32) -> Result<Window> {
        let attrs = self
            .connection
            .get_window_attributes(id)
            .map_err(native)?
            .reply()
            .map_err(native)?;
        if attrs.map_state != MapState::VIEWABLE {
            return Err(err("window_missing", "Window is not viewable"));
        }
        let geometry = self
            .connection
            .get_geometry(id)
            .map_err(native)?
            .reply()
            .map_err(native)?;
        let point = self
            .connection
            .translate_coordinates(id, self.root, 0, 0)
            .map_err(native)?
            .reply()
            .map_err(native)?;
        let pid = self
            .cardinal(id, "_NET_WM_PID")?
            .first()
            .copied()
            .ok_or_else(|| err("identity_unavailable", "Window has no process ID"))?;
        let title = self
            .connection
            .get_property(
                false,
                id,
                self.atom("_NET_WM_NAME")?,
                AtomEnum::ANY,
                0,
                MAX_TEXT_BYTES as u32 / 4,
            )
            .map_err(native)?
            .reply()
            .map_err(native)?;
        if title.bytes_after != 0 {
            return Err(err("attribute_limit", "Window title exceeds limit"));
        }
        let title = String::from_utf8(title.value).map_err(native)?;
        let foreground = self
            .cardinal(self.root, "_NET_ACTIVE_WINDOW")?
            .first()
            .copied()
            == Some(id);
        Ok(Window {
            pid,
            window_id: u64::from(id),
            title,
            process_identity: process_identity(pid)?,
            bounds: Bounds {
                x: f64::from(point.dst_x),
                y: f64::from(point.dst_y),
                width: f64::from(geometry.width),
                height: f64::from(geometry.height),
            },
            foreground,
            scale: 1.0,
        })
    }
    fn attach(&self, payload: &Attach) -> Result<Window> {
        if let Some(id) = payload.window_id {
            let window = self.window(u32::try_from(id).map_err(native)?)?;
            if window.pid != payload.pid {
                return Err(err("pid_mismatch", "Window belongs to another process"));
            }
            return Ok(window);
        }
        let mut candidates = Vec::new();
        for id in self.cardinal(self.root, "_NET_CLIENT_LIST_STACKING")? {
            if self.cardinal(id, "_NET_WM_PID")?.first().copied() == Some(payload.pid) {
                candidates.push(self.window(id)?);
            }
        }
        if candidates.len() != 1 {
            return Err(err(
                if candidates.is_empty() {
                    "window_missing"
                } else {
                    "window_ambiguous"
                },
                "Attach requires one exact visible application window",
            ));
        }
        Ok(candidates.remove(0))
    }
    fn visible_window_count(&self, pid: u32) -> Result<usize> {
        let mut count = 0;
        for id in self.cardinal(self.root, "_NET_CLIENT_LIST_STACKING")? {
            if self.cardinal(id, "_NET_WM_PID")?.first().copied() != Some(pid) {
                continue;
            }
            let attributes = self
                .connection
                .get_window_attributes(id)
                .map_err(native)?
                .reply()
                .map_err(native)?;
            if attributes.map_state == MapState::VIEWABLE {
                count += 1;
            }
        }
        Ok(count)
    }
    fn unobscured(&self, window: &Window) -> Result<()> {
        let mut top = u32::try_from(window.window_id).map_err(native)?;
        for _ in 0..64 {
            let tree = self
                .connection
                .query_tree(top)
                .map_err(native)?
                .reply()
                .map_err(native)?;
            if tree.parent == self.root {
                break;
            }
            if tree.parent == 0 {
                return Err(err("window_missing", "Window has no desktop ancestor"));
            }
            top = tree.parent;
        }
        let children = self
            .connection
            .query_tree(self.root)
            .map_err(native)?
            .reply()
            .map_err(native)?
            .children;
        let position = children.iter().position(|id| *id == top).ok_or_else(|| {
            err(
                "visibility_unknown",
                "Cannot establish window stacking order",
            )
        })?;
        for id in &children[position + 1..] {
            let attrs = self
                .connection
                .get_window_attributes(*id)
                .map_err(native)?
                .reply()
                .map_err(native)?;
            if attrs.map_state != MapState::VIEWABLE || attrs.class == WindowClass::INPUT_ONLY {
                continue;
            }
            let geometry = self
                .connection
                .get_geometry(*id)
                .map_err(native)?
                .reply()
                .map_err(native)?;
            let b = Bounds {
                x: f64::from(geometry.x),
                y: f64::from(geometry.y),
                width: f64::from(geometry.width),
                height: f64::from(geometry.height),
            };
            let a = window.bounds;
            if a.x < b.x + b.width
                && b.x < a.x + a.width
                && a.y < b.y + b.height
                && b.y < a.y + a.height
            {
                #[cfg(test)]
                eprintln!(
                    "occlusion_diagnostic={}",
                    json!({"attached_window":window.window_id,"attached_frame":top,"above_window":id,"above_bounds":b,"above_attributes":format!("{attrs:?}"),"above_pid":self.cardinal(*id,"_NET_WM_PID").ok(),"screen":self.connection.setup().roots.iter().map(|root| (root.width_in_pixels,root.height_in_pixels)).collect::<Vec<_>>()})
                );
                return Err(err(
                    "window_occluded",
                    "X11 cannot prove a clean window capture while another window overlaps it",
                ));
            }
        }
        Ok(())
    }
    fn capture(&self, window: &Window) -> Result<Screenshot> {
        self.unobscured(window)?;
        let id = u32::try_from(window.window_id).map_err(native)?;
        let width = window.bounds.width as u16;
        let height = window.bounds.height as u16;
        if width == 0 || height == 0 || usize::from(width) * usize::from(height) > MAX_IMAGE_PIXELS
        {
            return Err(err("image_limit", "Window dimensions exceed image limit"));
        }
        let reply = self
            .connection
            .get_image(XImageFormat::Z_PIXMAP, id, 0, 0, width, height, u32::MAX)
            .map_err(native)?
            .reply()
            .map_err(native)?;
        let setup = self.connection.setup();
        let format = setup
            .pixmap_formats
            .iter()
            .find(|f| f.depth == reply.depth)
            .ok_or_else(|| err("pixel_format", "Unknown X11 pixel format"))?;
        if ![16, 24, 32].contains(&format.bits_per_pixel) {
            return Err(err("pixel_format", "Unsupported X11 pixel depth"));
        }
        let visual = setup
            .roots
            .iter()
            .flat_map(|s| &s.allowed_depths)
            .flat_map(|d| &d.visuals)
            .find(|v| v.visual_id == reply.visual)
            .ok_or_else(|| err("pixel_format", "Unknown X11 visual"))?;
        let bytes = usize::from(format.bits_per_pixel) / 8;
        let pad = usize::from(format.scanline_pad);
        let stride =
            (usize::from(width) * usize::from(format.bits_per_pixel)).div_ceil(pad) * pad / 8;
        if reply.data.len() != stride * usize::from(height) {
            return Err(err("capture_incomplete", "X11 capture has incomplete rows"));
        }
        let mut pixels = Vec::with_capacity(usize::from(width) * usize::from(height) * 4);
        let channel = |pixel: u32, mask: u32| -> u8 {
            if mask == 0 {
                return 0;
            }
            let shift = mask.trailing_zeros();
            let max = mask >> shift;
            ((((pixel & mask) >> shift) as u64 * 255) / u64::from(max)) as u8
        };
        for y in 0..usize::from(height) {
            for x in 0..usize::from(width) {
                let start = y * stride + x * bytes;
                let chunk = &reply.data[start..start + bytes];
                let mut pixel = 0u32;
                if setup.image_byte_order == ImageOrder::LSB_FIRST {
                    for (i, b) in chunk.iter().enumerate() {
                        pixel |= u32::from(*b) << (i * 8);
                    }
                } else {
                    for b in chunk {
                        pixel = (pixel << 8) | u32::from(*b);
                    }
                }
                pixels.extend([
                    channel(pixel, visual.red_mask),
                    channel(pixel, visual.green_mask),
                    channel(pixel, visual.blue_mask),
                    255,
                ]);
            }
        }
        self.unobscured(window)?;
        let image = RgbaImage::from_raw(u32::from(width), u32::from(height), pixels)
            .ok_or_else(|| err("image_encoding", "Invalid pixel buffer"))?;
        let mut encoded = Cursor::new(Vec::new());
        DynamicImage::ImageRgba8(image)
            .write_to(&mut encoded, ImageFormat::Png)
            .map_err(native)?;
        if encoded.get_ref().len() > 12 * 1024 * 1024 {
            return Err(err("image_limit", "Encoded screenshot exceeds byte limit"));
        }
        Ok(Screenshot {
            mime_type: "image/png".into(),
            base64: STANDARD.encode(encoded.into_inner()),
            width: u32::from(width),
            height: u32::from(height),
        })
    }
    fn exact_pointer_recipient(&self, window: &Window, x: i16, y: i16) -> Result<()> {
        let target = u32::try_from(window.window_id).map_err(native)?;
        let mut destination = self.root;
        let mut seen = HashSet::new();
        for _ in 0..64 {
            if !seen.insert(destination) {
                break;
            }
            let hit = self
                .connection
                .translate_coordinates(self.root, destination, x, y)
                .map_err(native)?
                .reply()
                .map_err(native)?;
            if !hit.same_screen {
                break;
            }
            if hit.child == 0 {
                if destination == target {
                    return Ok(());
                }
                break;
            }
            destination = hit.child;
        }
        Err(err(
            "hit_test_unknown",
            "Native pointer recipient is not the exact attached X11 window",
        ))
    }
    fn pointer(
        &self,
        window: &Window,
        b: Bounds,
        action: &Action,
        after_move: impl FnOnce() -> Result<()>,
    ) -> Result<()> {
        let fresh = self.window(u32::try_from(window.window_id).map_err(native)?)?;
        if fresh.process_identity != window.process_identity
            || !fresh.bounds.near(window.bounds, 0.0)
        {
            return Err(err(
                "window_changed",
                "Window identity or geometry changed before input",
            ));
        }
        if !fresh.foreground {
            return Err(err(
                "foreground_mismatch",
                "Physical input requires the attached foreground window",
            ));
        }
        self.unobscured(&fresh)?;
        let x = action.x.unwrap_or(b.x + b.width / 2.);
        let y = action.y.unwrap_or(b.y + b.height / 2.);
        if !b.contains(x, y) || !window.bounds.contains(x, y) {
            return Err(err(
                "outside_target",
                "Input point is outside attached target",
            ));
        }
        let px = i16::try_from(x.round() as i64).map_err(native)?;
        let py = i16::try_from(y.round() as i64).map_err(native)?;
        self.exact_pointer_recipient(&fresh, px, py)?;
        self.connection
            .xtest_fake_input(6, 0, 0, self.root, px, py, 0)
            .map_err(|e| native(e).uncertain())?
            .check()
            .map_err(|e| native(e).uncertain())?;
        after_move().map_err(DesktopError::uncertain)?;
        self.exact_pointer_recipient(&fresh, px, py)
            .map_err(DesktopError::uncertain)?;
        let (button, count) = if matches!(action.kind, ActionKind::Click) {
            (1, 1)
        } else {
            let amount = action.scroll_y.unwrap_or(0);
            (if amount < 0 { 4 } else { 5 }, amount.unsigned_abs())
        };
        for _ in 0..count {
            for event in [4, 5] {
                self.connection
                    .xtest_fake_input(event, button, 0, self.root, 0, 0, 0)
                    .map_err(|e| native(e).uncertain())?
                    .check()
                    .map_err(|e| native(e).uncertain())?;
            }
        }
        self.connection.flush().map_err(|e| native(e).uncertain())?;
        Ok(())
    }
}

async fn component<'a>(
    connection: &'a AccessibilityConnection,
    object: &'a ObjectRefOwned,
) -> Result<ComponentProxy<'a>> {
    ComponentProxy::builder(connection.connection())
        .destination(
            object
                .name()
                .ok_or_else(|| err("target_missing", "Missing accessibility bus identity"))?
                .clone(),
        )
        .map_err(native)?
        .path(object.path())
        .map_err(native)?
        .cache_properties(CacheProperties::No)
        .build()
        .await
        .map_err(native)
}
async fn action_proxy<'a>(
    connection: &'a AccessibilityConnection,
    object: &'a ObjectRefOwned,
) -> Result<ActionProxy<'a>> {
    ActionProxy::builder(connection.connection())
        .destination(
            object
                .name()
                .ok_or_else(|| err("target_missing", "Missing accessibility bus identity"))?
                .clone(),
        )
        .map_err(native)?
        .path(object.path())
        .map_err(native)?
        .cache_properties(CacheProperties::No)
        .build()
        .await
        .map_err(native)
}
async fn text_proxy<'a>(
    connection: &'a AccessibilityConnection,
    object: &'a ObjectRefOwned,
) -> Result<TextProxy<'a>> {
    TextProxy::builder(connection.connection())
        .destination(
            object
                .name()
                .ok_or_else(|| err("target_missing", "Missing accessibility bus identity"))?
                .clone(),
        )
        .map_err(native)?
        .path(object.path())
        .map_err(native)?
        .cache_properties(CacheProperties::No)
        .build()
        .await
        .map_err(native)
}
async fn editable_proxy<'a>(
    connection: &'a AccessibilityConnection,
    object: &'a ObjectRefOwned,
) -> Result<EditableTextProxy<'a>> {
    EditableTextProxy::builder(connection.connection())
        .destination(
            object
                .name()
                .ok_or_else(|| err("target_missing", "Missing accessibility bus identity"))?
                .clone(),
        )
        .map_err(native)?
        .path(object.path())
        .map_err(native)?
        .cache_properties(CacheProperties::No)
        .build()
        .await
        .map_err(native)
}
async fn extents(connection: &AccessibilityConnection, object: &ObjectRefOwned) -> Result<Bounds> {
    let (x, y, w, h) = component(connection, object)
        .await?
        .get_extents(CoordType::Screen)
        .await
        .map_err(native)?;
    Ok(Bounds {
        x: f64::from(x),
        y: f64::from(y),
        width: f64::from(w),
        height: f64::from(h),
    })
}
fn root_matches(
    window: &Window,
    role: Role,
    name: &str,
    bounds: Bounds,
    visible_windows: usize,
) -> bool {
    matches!(role, Role::Frame | Role::Window)
        && bounds.near(window.bounds, 0.0)
        && if name.is_empty() {
            visible_windows == 1
        } else {
            name == window.title
        }
}

async fn screen_reader_enabled() -> Result<bool> {
    let session = zbus::Connection::session().await.map_err(native)?;
    atspi::proxy::bus::StatusProxy::new(&session)
        .await
        .map_err(native)?
        .screen_reader_enabled()
        .await
        .map_err(native)
}

async fn root(
    display: &Display,
    connection: &AccessibilityConnection,
    window: &Window,
) -> Result<ObjectRefOwned> {
    let visible_windows = display.visible_window_count(window.pid)?;
    let registry = connection
        .root_accessible_on_registry()
        .await
        .map_err(native)?;
    let apps = registry.get_children().await.map_err(native)?;
    if apps.len() > MAX_NODES {
        return Err(err("node_limit", "Accessibility registry exceeds limit"));
    }
    let bus = zbus::fdo::DBusProxy::new(connection.connection())
        .await
        .map_err(native)?;
    let mut candidates = Vec::new();
    let mut selected_apps = 0;
    for app in apps {
        let Some(name) = app.name() else {
            continue;
        };
        let pid = bus
            .get_connection_unix_process_id(name.clone().into())
            .await
            .map_err(native)?;
        if pid != window.pid {
            continue;
        }
        selected_apps += 1;
        let proxy = app
            .as_accessible_proxy(connection.connection())
            .await
            .map_err(native)?;
        let children = proxy.get_children().await.map_err(native)?;
        if children.len() > MAX_NODES {
            return Err(err("node_limit", "Application child array exceeds limit"));
        }
        for child in children {
            let Some(child_name) = child.name() else {
                continue;
            };
            if child_name != name
                && bus
                    .get_connection_unix_process_id(child_name.clone().into())
                    .await
                    .map_err(native)?
                    != window.pid
            {
                continue;
            }
            let accessible = child
                .as_accessible_proxy(connection.connection())
                .await
                .map_err(native)?;
            let role = accessible.get_role().await.map_err(native)?;
            if !matches!(role, Role::Frame | Role::Window) {
                continue;
            }
            if root_matches(
                window,
                role,
                &bounded(accessible.name().await.map_err(native)?)?,
                extents(connection, &child).await?,
                visible_windows,
            ) {
                candidates.push(child);
            }
        }
    }
    if selected_apps == 0 {
        let active = screen_reader_enabled().await?;
        return Err(err(
            if active {
                "accessibility_pending"
            } else {
                "accessibility_inactive"
            },
            if active {
                "Selected process has not registered its AT-SPI application"
            } else {
                "Selected process is absent from AT-SPI; ScreenReaderEnabled is false (AccessKit activation requires it)"
            },
        ));
    }
    if candidates.len() != 1 {
        return Err(err(
            "window_ambiguous",
            "Cannot uniquely bind the X11 window to its AT-SPI root",
        ));
    }
    Ok(candidates.remove(0))
}
async fn describe(
    connection: &AccessibilityConnection,
    object: &ObjectRefOwned,
    path: Vec<u32>,
    id: String,
    parent: Option<String>,
) -> Result<Node> {
    let proxy = object
        .as_accessible_proxy(connection.connection())
        .await
        .map_err(native)?;
    let role = proxy.get_role().await.map_err(native)?;
    let state = proxy.get_state().await.map_err(native)?;
    let interfaces = proxy.get_interfaces().await.map_err(native)?;
    let role_name = match role {
        Role::Button => "button",
        Role::Entry | Role::Text | Role::PasswordText => "text_field",
        Role::Frame | Role::Window => "window",
        Role::Label => "text",
        Role::CheckBox => "checkbox",
        Role::RadioButton => "radio",
        Role::ComboBox => "combo_box",
        Role::ScrollPane => "scroll_area",
        Role::Table => "table",
        Role::TableRow => "row",
        _ => "group",
    };
    let name = bounded(proxy.name().await.map_err(native)?)?;
    let identifier = bounded(proxy.accessible_id().await.map_err(native)?)?;
    // AccessKit currently exposes Editable + Text without EditableText. Such
    // controls require the separately fenced physical keyboard path below.
    let editable = state.contains(State::Editable)
        && (interfaces.contains(Interface::EditableText) || interfaces.contains(Interface::Text));
    let mut actions = Vec::new();
    if interfaces.contains(Interface::Action) {
        let advertised = action_proxy(connection, object)
            .await?
            .get_actions()
            .await
            .map_err(native)?;
        if advertised
            .iter()
            .any(|a| ["click", "press", "activate"].contains(&a.name.as_str()))
        {
            actions.push("press".into());
        }
    }
    if editable {
        actions.extend(["set_value".into(), "type_text".into()]);
    }
    let value = if interfaces.contains(Interface::Text) && role != Role::PasswordText {
        let text = text_proxy(connection, object).await?;
        let count = text.character_count().await.map_err(native)?;
        if count > MAX_TEXT_BYTES as i32 {
            return Err(err("attribute_limit", "Text exceeds character limit"));
        }
        Some(bounded(text.get_text(0, count).await.map_err(native)?)?)
    } else {
        None
    };
    let bounds = if interfaces.contains(Interface::Component) {
        Some(extents(connection, object).await?)
    } else {
        None
    };
    Ok(Node {
        id,
        role: role_name.into(),
        name,
        value,
        identifier: if identifier.is_empty() {
            None
        } else {
            Some(identifier)
        },
        parent,
        bounds,
        enabled: state.contains(State::Enabled) && state.contains(State::Sensitive),
        editable,
        actions,
        native_path: path,
    })
}
async fn tree(
    display: &Display,
    connection: &AccessibilityConnection,
    window: &Window,
) -> Result<(Vec<Node>, Vec<ObjectRefOwned>, bool)> {
    let root = root(display, connection, window).await?;
    let mut stack = vec![(root, Vec::<u32>::new(), None)];
    let mut nodes = Vec::new();
    let mut objects = Vec::new();
    let mut seen = HashSet::new();
    let mut complete = true;
    while let Some((object, path, parent)) = stack.pop() {
        if nodes.len() >= MAX_NODES || path.len() > MAX_DEPTH {
            complete = false;
            break;
        }
        let key = (
            object.name_as_str().map(str::to_owned),
            object.path_as_str().to_owned(),
        );
        if !seen.insert(key) {
            return Err(err(
                "tree_cycle",
                "Accessibility tree contains a cycle or shared child",
            ));
        }
        let id = format!("n{}", nodes.len());
        nodes.push(describe(connection, &object, path.clone(), id.clone(), parent).await?);
        let proxy = object
            .as_accessible_proxy(connection.connection())
            .await
            .map_err(native)?;
        let children = proxy.get_children().await.map_err(native)?;
        if children.len() > MAX_NODES {
            return Err(err("node_limit", "Child array exceeds limit"));
        }
        for (index, child) in children.into_iter().enumerate().rev() {
            let mut next = path.clone();
            next.push(index as u32);
            stack.push((child, next, Some(id.clone())));
        }
        objects.push(object);
    }
    Ok((nodes, objects, complete))
}
async fn act(
    display: &Display,
    connection: &AccessibilityConnection,
    window: &Window,
    node: &Node,
    action: &Action,
    expected_state: &str,
    expected_nodes: &[Node],
) -> Result<Receipt> {
    action.validate()?;
    let fresh = display.window(u32::try_from(window.window_id).map_err(native)?)?;
    if fresh.process_identity != window.process_identity || !fresh.bounds.near(window.bounds, 1.0) {
        return Err(err("window_changed", "Window changed; observe again"));
    }
    let (nodes, objects, complete) = tree(display, connection, &fresh).await?;
    if !complete {
        return Err(err(
            "tree_incomplete",
            "Cannot act using an incomplete tree",
        ));
    }
    validate_state(expected_nodes, &nodes, expected_state)?;
    let index = nodes
        .iter()
        .position(|n| n.native_path == node.native_path)
        .ok_or_else(|| err("target_missing", "Target path disappeared"))?;
    if !same_target(node, &nodes[index]) {
        return Err(err("target_changed", "Target identity or geometry changed"));
    }
    let object = &objects[index];
    let dispatched_at_ms = now_ms();
    match action.kind {
        ActionKind::Press => {
            let proxy = action_proxy(connection, object).await?;
            let actions = proxy.get_actions().await.map_err(native)?;
            let indices: Vec<_> = actions
                .iter()
                .enumerate()
                .filter(|(_, a)| ["click", "press", "activate"].contains(&a.name.as_str()))
                .map(|(i, _)| i)
                .collect();
            if indices.len() != 1 {
                return Err(err(
                    "action_ambiguous",
                    "Target requires one exact activation action",
                ));
            }
            if !proxy
                .do_action(indices[0] as i32)
                .await
                .map_err(|e| native(e).uncertain())?
            {
                return Err(err("action_failed", "AT-SPI rejected activation").uncertain());
            }
        }
        ActionKind::SetValue => {
            if !nodes[index].editable {
                return Err(err("not_editable", "Target is not editable"));
            }
            let interfaces = object
                .as_accessible_proxy(connection.connection())
                .await
                .map_err(native)?
                .get_interfaces()
                .await
                .map_err(native)?;
            if !interfaces.contains(Interface::EditableText) {
                keyboard_text(display, connection, &fresh, object, node, action).await?;
            } else if !editable_proxy(connection, object)
                .await?
                .set_text_contents(
                    action
                        .text
                        .as_deref()
                        .ok_or_else(|| err("invalid_action", "Text required"))?,
                )
                .await
                .map_err(|e| native(e).uncertain())?
            {
                return Err(err("action_failed", "AT-SPI rejected text replacement").uncertain());
            }
        }
        ActionKind::TypeText => {
            if !nodes[index].editable {
                return Err(err("not_editable", "Target is not editable"));
            }
            let proxy = object
                .as_accessible_proxy(connection.connection())
                .await
                .map_err(native)?;
            if !proxy
                .get_state()
                .await
                .map_err(native)?
                .contains(State::Focused)
            {
                return Err(err(
                    "focus_required",
                    "Text insertion requires the target already focused",
                ));
            }
            let text = action
                .text
                .as_deref()
                .ok_or_else(|| err("invalid_action", "Text required"))?;
            let caret = text_proxy(connection, object)
                .await?
                .caret_offset()
                .await
                .map_err(native)?;
            let interfaces = proxy.get_interfaces().await.map_err(native)?;
            if !interfaces.contains(Interface::EditableText) {
                keyboard_text(display, connection, &fresh, object, node, action).await?;
            } else if !editable_proxy(connection, object)
                .await?
                .insert_text(caret, text, text.chars().count() as i32)
                .await
                .map_err(|e| native(e).uncertain())?
            {
                return Err(err("action_failed", "AT-SPI rejected text insertion").uncertain());
            }
        }
        ActionKind::Click | ActionKind::Scroll => {
            let b = node
                .bounds
                .ok_or_else(|| err("geometry_unavailable", "Target has no bounds"))?;
            let x = action.x.unwrap_or(b.x + b.width / 2.);
            let y = action.y.unwrap_or(b.y + b.height / 2.);
            let root = &objects[0];
            let hit = component(connection, root)
                .await?
                .get_accessible_at_point(x as i32, y as i32, CoordType::Screen)
                .await
                .map_err(native)?;
            if &hit != object {
                return Err(err(
                    "hit_test_unknown",
                    "Cannot establish that the exact target receives pointer input",
                ));
            }
            display.pointer(&fresh, b, action, || Ok(()))?;
        }
    }
    Ok(Receipt {
        action_id: action.action_id.clone(),
        delivery: Delivery::Sent,
        dispatched_at_ms,
        verified: false,
    })
}
async fn keyboard_text(
    display: &Display,
    connection: &AccessibilityConnection,
    window: &Window,
    object: &ObjectRefOwned,
    node: &Node,
    action: &Action,
) -> Result<()> {
    let first = display.connection.setup().min_keycode;
    let count = display
        .connection
        .setup()
        .max_keycode
        .checked_sub(first)
        .and_then(|n| n.checked_add(1))
        .ok_or_else(|| err("keyboard_mapping_unavailable", "Invalid X11 keycode range"))?;
    let mapping = display
        .connection
        .get_keyboard_mapping(first, count)
        .map_err(native)?
        .reply()
        .map_err(native)?;
    let modifiers = display
        .connection
        .get_modifier_mapping()
        .map_err(native)?
        .reply()
        .map_err(native)?;
    let text = action
        .text
        .as_deref()
        .ok_or_else(|| err("invalid_action", "Text required"))?;
    // Build the complete sequence before focus or any physical input. A single
    // unsupported character rejects the entire operation without partial typing.
    let strokes = crate::keyboard::plan(
        first,
        mapping.keysyms_per_keycode,
        &mapping.keysyms,
        &modifiers.keycodes,
        text,
        matches!(action.kind, ActionKind::SetValue),
    )?;
    let bounds = node
        .bounds
        .ok_or_else(|| err("geometry_unavailable", "Editable target has no bounds"))?;
    let x = i16::try_from((bounds.x + bounds.width / 2.0).round() as i64).map_err(native)?;
    let y = i16::try_from((bounds.y + bounds.height / 2.0).round() as i64).map_err(native)?;
    display.exact_pointer_recipient(window, x, y)?;
    let proxy = object
        .as_accessible_proxy(connection.connection())
        .await
        .map_err(native)?;
    let target_component = component(connection, object).await?;
    if !target_component.grab_focus().await.map_err(native)? {
        return Err(err("focus_required", "Editable target refused focus"));
    }
    let mut delivered = false;
    for stroke in strokes {
        let guard = async {
            let current = display.window(u32::try_from(window.window_id).map_err(native)?)?;
            if current.process_identity != window.process_identity
                || !current.foreground
                || !current.bounds.near(window.bounds, 0.0)
            {
                return Err(err(
                    "foreground_mismatch",
                    "Text target lost exact foreground window ownership",
                ));
            }
            display.unobscured(&current)?;
            let state = proxy.get_state().await.map_err(native)?;
            if !extents(connection, object).await?.near(bounds, 0.0)
                || ![
                    State::Focused,
                    State::Editable,
                    State::Enabled,
                    State::Sensitive,
                ]
                .into_iter()
                .all(|required| state.contains(required))
            {
                return Err(err(
                    "focus_required",
                    "Exact editable target is no longer focused",
                ));
            }
            let focus = display
                .connection
                .get_input_focus()
                .map_err(native)?
                .reply()
                .map_err(native)?;
            if u64::from(focus.focus) != window.window_id {
                return Err(err(
                    "focus_required",
                    "Native keyboard recipient is not the attached X11 window",
                ));
            }
            let keys = display
                .connection
                .query_keymap()
                .map_err(native)?
                .reply()
                .map_err(native)?;
            let pointer = display
                .connection
                .query_pointer(display.root)
                .map_err(native)?
                .reply()
                .map_err(native)?;
            if keys.keys.iter().any(|v| *v != 0) || u16::from(pointer.mask) != 0 {
                return Err(err(
                    "keyboard_busy",
                    "User input or modifiers are held; physical text is refused",
                ));
            }
            let fresh = display
                .connection
                .get_keyboard_mapping(first, count)
                .map_err(native)?
                .reply()
                .map_err(native)?;
            let mods = display
                .connection
                .get_modifier_mapping()
                .map_err(native)?
                .reply()
                .map_err(native)?;
            if fresh.keysyms_per_keycode != mapping.keysyms_per_keycode
                || fresh.keysyms != mapping.keysyms
                || mods.keycodes != modifiers.keycodes
            {
                return Err(err(
                    "keyboard_mapping_changed",
                    "Keyboard mapping changed before typing",
                ));
            }
            Ok(())
        }
        .await;
        if let Err(e) = guard {
            return Err(if delivered { e.uncertain() } else { e });
        }
        let mut pressed = Vec::new();
        let mut cookies = Vec::new();
        let mut failure = None;
        for code in stroke
            .modifier
            .into_iter()
            .chain(std::iter::once(stroke.code))
        {
            pressed.push(code);
            match display
                .connection
                .xtest_fake_input(2, code, 0, display.root, 0, 0, 0)
            {
                Ok(cookie) => cookies.push(cookie),
                Err(e) => {
                    failure = Some(native(e));
                    break;
                }
            }
        }
        // Queue releases before waiting for any server acknowledgement. A
        // stalled response must not leave our modifier pressed while we wait.
        for code in pressed.into_iter().rev() {
            match display
                .connection
                .xtest_fake_input(3, code, 0, display.root, 0, 0, 0)
            {
                Ok(cookie) => cookies.push(cookie),
                Err(e) => failure = Some(native(e)),
            }
        }
        if let Err(e) = display.connection.flush() {
            failure = Some(native(e));
        }
        for cookie in cookies {
            if let Err(e) = cookie.check() {
                failure = Some(native(e));
            }
        }
        delivered = true;
        if let Some(e) = failure {
            return Err(e.uncertain());
        }
        display
            .connection
            .flush()
            .map_err(|e| native(e).uncertain())?;
    }
    Ok(())
}
fn act_visual(
    display: &Display,
    window: &Window,
    visual: &VisualMatch,
    anchor: &VisualAnchor,
    action: &Action,
) -> Result<Receipt> {
    let fresh = display.window(u32::try_from(window.window_id).map_err(native)?)?;
    if fresh.process_identity != window.process_identity
        || !fresh.bounds.near(window.bounds, 0.0)
        || fresh.scale != window.scale
    {
        return Err(err(
            "window_changed",
            "Visual window identity or geometry changed",
        ));
    }
    let capture = display.capture(&fresh)?;
    let (x, y) = revalidate_visual(&fresh, &capture, anchor, visual, action)?;
    let mut derived = action.clone();
    derived.x = Some(x);
    derived.y = Some(y);
    let dispatched_at_ms = now_ms();
    display.pointer(&fresh, visual.screen_bounds, &derived, || {
        let after = display.window(u32::try_from(window.window_id).map_err(native)?)?;
        if after.process_identity != fresh.process_identity || !after.foreground {
            return Err(err(
                "foreground_mismatch",
                "Visual target lost foreground ownership after pointer movement",
            ));
        }
        revalidate_visual(&after, &display.capture(&after)?, anchor, visual, action).map(|_| ())
    })?;
    Ok(Receipt {
        action_id: action.action_id.clone(),
        delivery: Delivery::Sent,
        dispatched_at_ms,
        verified: false,
    })
}
async fn execute_async(request: WorkerRequest) -> Result<Value> {
    if request.op == "probe" {
        let display = Display::new();
        let a11y = atspi::connection::read_session_accessibility().await;
        let screen_reader = screen_reader_enabled().await;
        return Ok(
            json!({"protocol_version":1,"platform":"linux","architecture":std::env::consts::ARCH,"session":"x11","external_input":{"implementation":"unavailable","continuous":false,"qualification":"not_tested"},"screen_capture":{"backend":"x11_get_image","scope":"unoccluded_window","state":if display.is_ok(){"available"}else{"unavailable"}},"accessibility":{"backend":"atspi","state":match a11y{Ok(true)=>"enabled",Ok(false)=>"disabled",Err(_)=>"unavailable"},"screen_reader_enabled":screen_reader.as_ref().ok(),"accesskit_activation":match screen_reader{Ok(true)=>"enabled",Ok(false)=>"inactive",Err(_)=>"unavailable"}},"qualification":"not_tested"}),
        );
    }
    let display = Display::new()?;
    if !atspi::connection::read_session_accessibility()
        .await
        .map_err(native)?
    {
        return Err(err(
            "accessibility_disabled",
            "AT-SPI accessibility is not enabled in this desktop session",
        ));
    }
    let connection = AccessibilityConnection::new().await.map_err(native)?;
    match request.op.as_str() {
        "attach" => {
            let payload = request
                .attach
                .ok_or_else(|| err("invalid_request", "Attach required"))?;
            let window = display.attach(&payload)?;
            root(&display, &connection, &window).await?;
            let window = if payload.focus {
                focus_window(&display, &window).await?
            } else {
                window
            };
            serde_json::to_value(window).map_err(native)
        }
        "focus" => {
            let window = request
                .window
                .ok_or_else(|| err("invalid_request", "Window required"))?;
            serde_json::to_value(focus_window(&display, &window).await?).map_err(native)
        }

        "observe" => {
            let expected = request
                .window
                .ok_or_else(|| err("invalid_request", "Window required"))?;
            let window = display.window(u32::try_from(expected.window_id).map_err(native)?)?;
            if window.process_identity != expected.process_identity {
                return Err(err(
                    "process_changed",
                    "Application process identity changed",
                ));
            }
            let tree_at_ms = now_ms();
            let (nodes, _, complete) = tree(&display, &connection, &window).await?;
            let screenshot = display.capture(&window)?;
            let after = display.window(u32::try_from(window.window_id).map_err(native)?)?;
            if !after.bounds.near(window.bounds, 1.0) {
                return Err(err(
                    "observation_inconsistent",
                    "Window moved during capture",
                ));
            }
            serde_json::to_value(Observation {
                input_stamp: None,
                observation_id: String::new(),
                epoch: 0,
                captured_at_ms: now_ms(),
                tree_at_ms,
                window,
                nodes,
                screenshot,
                complete,
                truncated_reason: if complete {
                    None
                } else {
                    Some("node_or_depth_limit".into())
                },
            })
            .map_err(native)
        }
        "act" => {
            let window = request
                .window
                .ok_or_else(|| err("invalid_request", "Window required"))?;
            let action = request
                .action
                .ok_or_else(|| err("invalid_request", "Action required"))?;
            let target = request
                .target
                .ok_or_else(|| err("invalid_request", "Target required"))?;
            let expected_nodes = request
                .expected_nodes
                .ok_or_else(|| err("invalid_request", "Expected nodes required"))?;
            let expected_state = request
                .expected_state
                .ok_or_else(|| err("invalid_request", "Expected observation state required"))?;
            let receipt = match target {
                ResolvedTarget::Semantic { node } => {
                    act(
                        &display,
                        &connection,
                        &window,
                        &node,
                        &action,
                        &expected_state,
                        &expected_nodes,
                    )
                    .await?
                }
                ResolvedTarget::Visual { visual } => act_visual(
                    &display,
                    &window,
                    &visual,
                    &request
                        .visual_anchor
                        .ok_or_else(|| err("invalid_request", "Visual anchor required"))?,
                    &action,
                )?,
            };
            serde_json::to_value(receipt).map_err(native)
        }
        _ => Err(err("unknown_operation", "Unknown native helper operation")),
    }
}
async fn focus_window(display: &Display, window: &Window) -> Result<Window> {
    let id = u32::try_from(window.window_id).map_err(native)?;
    let fresh = display.window(id)?;
    if fresh.process_identity != window.process_identity {
        return Err(err(
            "process_changed",
            "Application process identity changed",
        ));
    }
    let event =
        ClientMessageEvent::new(32, id, display.atom("_NET_ACTIVE_WINDOW")?, [2, 0, 0, 0, 0]);
    display
        .connection
        .send_event(
            false,
            display.root,
            EventMask::SUBSTRUCTURE_REDIRECT | EventMask::SUBSTRUCTURE_NOTIFY,
            event,
        )
        .map_err(native)?
        .check()
        .map_err(native)?;
    display.connection.flush().map_err(native)?;
    for _ in 0..50 {
        let focused = display.window(id)?;
        if focused.foreground {
            return Ok(focused);
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    Err(err(
        "foreground_denied",
        "Window manager declined exact window activation",
    ))
}
pub fn execute(request: WorkerRequest) -> Result<Value> {
    tokio::runtime::Builder::new_current_thread()
        .enable_time()
        .build()
        .map_err(native)?
        .block_on(async {
            tokio::time::timeout(Duration::from_secs(10), execute_async(request))
                .await
                .map_err(|_| {
                    err(
                        "accessibility_timeout",
                        "AT-SPI operation deadline exceeded",
                    )
                })?
        })
}

#[cfg(test)]
#[test]
fn unnamed_root_requires_unique_native_window_and_exact_window_role_and_geometry() {
    let window = Window {
        pid: 1,
        window_id: 2,
        title: "selected".into(),
        process_identity: "fixture".into(),
        bounds: Bounds {
            x: 5.0,
            y: 56.0,
            width: 953.0,
            height: 758.0,
        },
        foreground: true,
        scale: 1.0,
    };
    assert!(root_matches(&window, Role::Frame, "", window.bounds, 1));
    assert!(root_matches(
        &window,
        Role::Window,
        "selected",
        window.bounds,
        2
    ));
    assert!(!root_matches(&window, Role::Frame, "", window.bounds, 2));
    assert!(!root_matches(
        &window,
        Role::Frame,
        "other",
        window.bounds,
        1
    ));
    assert!(!root_matches(
        &window,
        Role::Button,
        "selected",
        window.bounds,
        1
    ));
    let mut moved = window.bounds;
    moved.x += 1.0;
    assert!(!root_matches(&window, Role::Frame, "selected", moved, 1));
    let mut untitled = window;
    untitled.title.clear();
    assert!(!root_matches(
        &untitled,
        Role::Frame,
        "",
        untitled.bounds,
        2
    ));
}

#[cfg(test)]
#[test]
#[ignore = "requires MANVI_LINUX_DIAGNOSTIC_PID for one owned synthetic X11 window"]
fn diagnostic_selected_window_roots() {
    let pid: u32 = std::env::var("MANVI_LINUX_DIAGNOSTIC_PID")
        .expect("explicit synthetic window PID required")
        .parse()
        .expect("PID must be numeric");
    tokio::runtime::Builder::new_current_thread().enable_time().build().unwrap().block_on(async {
        tokio::time::timeout(Duration::from_secs(10), async {
            let display = Display::new().unwrap();
            let window = display.attach(&Attach { pid, window_id: None, focus: false, read_only_targets: Vec::new() }).unwrap();
            println!("selected_x11_window={}", serde_json::to_string(&window).unwrap());
            let connection = AccessibilityConnection::new().await.unwrap();
            let registry = connection.root_accessible_on_registry().await.unwrap();
            let apps = registry.get_children().await.unwrap();
            assert!(apps.len() <= MAX_NODES);
            let bus = zbus::fdo::DBusProxy::new(connection.connection()).await.unwrap();
            let mut matched_apps = 0;
            for app in apps {
                let Some(name) = app.name() else { continue; };
                let owner = bus.get_connection_unix_process_id(name.clone().into()).await.unwrap();
                if owner != pid { continue; }
                matched_apps += 1;
                let proxy = app.as_accessible_proxy(connection.connection()).await.unwrap();
                let children = proxy.get_children().await.unwrap();
                assert!(children.len() <= MAX_NODES);
                println!("selected_atspi_application={}",json!({"pid":owner,"bus":name.to_string(),"path":app.path().to_string(),"children":children.len()}));
                for child in children {
                    let accessible = child.as_accessible_proxy(connection.connection()).await.unwrap();
                    let name = accessible.name().await;
                    let role = accessible.get_role().await;
                    let bounds = extents(&connection,&child).await;
                    println!("selected_atspi_child={}",json!({"path":child.path().to_string(),"name":format!("{name:?}"),"role":format!("{role:?}"),"bounds":format!("{bounds:?}")}));
                }
            }
            println!("selected_atspi_app_count={matched_apps}");
            let resolved = root(&display,&connection,&window).await;
            println!("selected_root_result={resolved:?}");
            assert!(resolved.is_ok(),"selected synthetic X11 window must bind one exact AT-SPI root");
            let visible = display.unobscured(&window);
            println!("selected_visibility_result={visible:?}");
            assert!(visible.is_ok(), "selected synthetic window capture must be unobscured");
        }).await.expect("selected window diagnosis exceeded ten seconds");
    });
}
