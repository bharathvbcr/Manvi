//! macOS operations run only in a disposable helper process. All AX references are
//! retained locally, checked by CoreFoundation type ID, and dropped before exit.
use crate::input_guard;
use base64::{Engine, engine::general_purpose::STANDARD};
use block2::RcBlock;
use desktop_core::{
    Action, ActionKind, Attach, Bounds, Delivery, DesktopError, InputStamp, MAX_DEPTH,
    MAX_IMAGE_PIXELS, MAX_NODES, MAX_TEXT_BYTES, Node, Observation, Receipt, ResolvedTarget,
    Result, Screenshot, VisualAnchor, VisualMatch, Window, WorkerRequest, now_ms,
    revalidate_visual, same_target, state_difference, state_fingerprint, validate_state,
};
use enigo::{Axis, Button, Coordinate, Direction, Enigo, Keyboard, Mouse, Settings};
use objc2::{AnyThread, MainThreadMarker};
use objc2_app_kit::{
    NSApplication, NSApplicationActivationOptions, NSApplicationActivationPolicy,
    NSBitmapImageFileType, NSBitmapImageRep, NSRunningApplication, NSWorkspace,
};
use objc2_application_services::{AXError, AXIsProcessTrusted, AXUIElement, AXValue, AXValueType};
use objc2_core_foundation::{CFArray, CFBoolean, CFRetained, CFString, CFType, CGPoint, CGSize};
use objc2_core_graphics::{CGImage, CGPreflightScreenCaptureAccess};
use objc2_foundation::{NSDictionary, NSError};
use objc2_screen_capture_kit::{
    SCContentFilter, SCScreenshotManager, SCShareableContent, SCStreamConfiguration,
};
use serde_json::{Value, json};
use std::{ptr::NonNull, sync::mpsc, time::Duration};

fn err(code: &str, message: &str) -> DesktopError {
    DesktopError::new(code, message)
}

fn attr(element: &AXUIElement, name: &str) -> Result<Option<CFRetained<CFType>>> {
    let mut raw = std::ptr::null();
    // AX owns the returned +1 reference. The out pointer is initialized and lives through the call.
    let status =
        unsafe { element.copy_attribute_value(&CFString::from_str(name), NonNull::from(&mut raw)) };
    if status == AXError::AttributeUnsupported || status == AXError::NoValue {
        return Ok(None);
    }
    if status != AXError::Success {
        return Err(DesktopError::new(
            "accessibility_read",
            format!("{name} failed with AX error {}", status.0),
        ));
    }
    Ok(NonNull::new(raw.cast_mut()).map(|p| unsafe { CFRetained::from_raw(p) }))
}
fn string_attr(element: &AXUIElement, name: &str) -> Result<Option<String>> {
    let Some(value) = attr(element, name)? else {
        return Ok(None);
    };
    let text = value
        .downcast::<CFString>()
        .map_err(|_| err("accessibility_type", "Expected a string attribute"))?
        .to_string();
    if text.len() > MAX_TEXT_BYTES {
        return Err(err(
            "attribute_limit",
            "Accessibility string exceeds the byte limit",
        ));
    }
    Ok(Some(text))
}
fn bool_attr(element: &AXUIElement, name: &str) -> Result<Option<bool>> {
    let Some(value) = attr(element, name)? else {
        return Ok(None);
    };
    Ok(Some(
        value
            .downcast::<CFBoolean>()
            .map_err(|_| err("accessibility_type", "Expected a boolean attribute"))?
            .value(),
    ))
}
fn typed_array(array: CFRetained<CFArray>) -> CFRetained<CFArray<CFType>> {
    // Apple AX attribute arrays contain retained CoreFoundation objects. Every
    // element is still checked by its concrete runtime type before use.
    unsafe { CFRetained::cast_unchecked::<CFArray<CFType>>(array) }
}
fn children(element: &AXUIElement, name: &str) -> Result<Vec<CFRetained<AXUIElement>>> {
    let Some(value) = attr(element, name)? else {
        return Ok(vec![]);
    };
    let array = value
        .downcast::<CFArray>()
        .map_err(|_| err("accessibility_type", "Expected an array attribute"))?;
    if array.len() > MAX_NODES {
        return Err(err(
            "node_limit",
            "Accessibility child array exceeds the node limit",
        ));
    }
    typed_array(array)
        .iter()
        .map(|v| {
            v.downcast::<AXUIElement>()
                .map_err(|_| err("accessibility_type", "Expected an AX element"))
        })
        .collect()
}
fn element_attr(element: &AXUIElement, name: &str) -> Result<Option<CFRetained<AXUIElement>>> {
    attr(element, name)?
        .map(|v| {
            v.downcast::<AXUIElement>()
                .map_err(|_| err("accessibility_type", "Expected an AX element"))
        })
        .transpose()
}
fn bounds(element: &AXUIElement) -> Result<Option<Bounds>> {
    let (Some(position), Some(size)) = (attr(element, "AXPosition")?, attr(element, "AXSize")?)
    else {
        return Ok(None);
    };
    let position = position
        .downcast::<AXValue>()
        .map_err(|_| err("accessibility_type", "Position is not an AXValue"))?;
    let size = size
        .downcast::<AXValue>()
        .map_err(|_| err("accessibility_type", "Size is not an AXValue"))?;
    let mut point = CGPoint::ZERO;
    let mut extent = CGSize::ZERO;
    // Pointers have the exact layouts specified by the requested AXValue types.
    if !unsafe { position.value(AXValueType::CGPoint, NonNull::from(&mut point).cast()) }
        || !unsafe { size.value(AXValueType::CGSize, NonNull::from(&mut extent).cast()) }
    {
        return Err(err("accessibility_type", "Invalid geometry value"));
    }
    Ok(Some(Bounds {
        x: point.x,
        y: point.y,
        width: extent.width,
        height: extent.height,
    }))
}
fn application(pid: u32) -> Result<CFRetained<AXUIElement>> {
    if pid == 0 || pid > i32::MAX as u32 {
        return Err(err("invalid_pid", "Invalid application process identifier"));
    }
    if !unsafe { AXIsProcessTrusted() } {
        return Err(err(
            "accessibility_permission",
            "Accessibility permission is not granted to the native helper",
        ));
    }
    let app = unsafe { AXUIElement::new_application(pid as i32) };
    let status = unsafe { app.set_messaging_timeout(0.75) };
    if status != AXError::Success {
        return Err(err(
            "accessibility_timeout",
            "Could not bound accessibility messaging timeout",
        ));
    }
    Ok(app)
}
fn process_identity(pid: u32) -> Result<String> {
    if pid == 0 || pid > i32::MAX as u32 {
        return Err(err("invalid_pid", "Invalid process identifier"));
    }
    // libproc's fixed-size structure is checked before assuming initialization.
    // Unlike NSRunningApplication.launchDate, this also identifies directly
    // launched Rust GUI executables that do not have a LaunchServices date.
    let mut info = std::mem::MaybeUninit::<libc::proc_bsdinfo>::uninit();
    let size = std::mem::size_of::<libc::proc_bsdinfo>();
    let count = unsafe {
        libc::proc_pidinfo(
            pid as i32,
            libc::PROC_PIDTBSDINFO,
            0,
            info.as_mut_ptr().cast(),
            size as i32,
        )
    };
    if count != size as i32 {
        return Err(err(
            "identity_unavailable",
            "Kernel process start time is unavailable",
        ));
    }
    let info = unsafe { info.assume_init() };
    let mut path = vec![0u8; libc::PROC_PIDPATHINFO_MAXSIZE as usize];
    let length =
        unsafe { libc::proc_pidpath(pid as i32, path.as_mut_ptr().cast(), path.len() as u32) };
    if length <= 0 {
        return Err(err(
            "identity_unavailable",
            "Kernel executable path is unavailable",
        ));
    }
    let end = path.iter().position(|b| *b == 0).unwrap_or(length as usize);
    let path = std::str::from_utf8(&path[..end])
        .map_err(|_| err("identity_unavailable", "Executable path is not UTF-8"))?;
    Ok(format!(
        "{pid}:{}:{}:{path}",
        info.pbi_start_tvsec, info.pbi_start_tvusec
    ))
}

/// ScreenCaptureKit enumerates metadata, then captures precisely the selected
/// window. No display-wide image or temporary image file is created.
fn capture(attach: &Attach, take_image: bool) -> Result<(Window, Option<Screenshot>)> {
    if !CGPreflightScreenCaptureAccess() {
        return Err(err(
            "capture_permission",
            "Screen Recording permission is not granted to the native helper",
        ));
    }
    let pid = attach.pid;
    let requested = attach.window_id;
    let identity = process_identity(pid)?;
    let (tx, rx) = mpsc::sync_channel(1);
    let block = RcBlock::new(
        move |content: *mut SCShareableContent, error: *mut NSError| {
            let result = (|| -> Result<()> {
                if !error.is_null() {
                    return Err(err(
                        "capture_unavailable",
                        "ScreenCaptureKit could not enumerate shareable windows",
                    ));
                }
                let content = unsafe { content.as_ref() }.ok_or_else(|| {
                    err(
                        "capture_unavailable",
                        "ScreenCaptureKit returned no content",
                    )
                })?;
                let windows = unsafe { content.windows() };
                let candidates: Vec<_> = windows
                    .iter()
                    .filter(|w| unsafe {
                        w.owningApplication()
                            .is_some_and(|a| a.processID() == pid as i32)
                            && w.windowLayer() == 0
                            && w.isOnScreen()
                            && requested.is_none_or(|id| u64::from(w.windowID()) == id)
                    })
                    .collect();
                if candidates.len() != 1 {
                    return Err(err(
                        if candidates.is_empty() {
                            "window_missing"
                        } else {
                            "window_ambiguous"
                        },
                        "An exact visible application window is required",
                    ));
                }
                let selected = &candidates[0];
                let frame = unsafe { selected.frame() };
                let filter = unsafe {
                    SCContentFilter::initWithDesktopIndependentWindow(
                        SCContentFilter::alloc(),
                        selected,
                    )
                };
                let scale = unsafe { filter.pointPixelScale() } as f64;
                let window = Window {
                    pid,
                    window_id: u64::from(unsafe { selected.windowID() }),
                    title: unsafe { selected.title() }.map_or_else(String::new, |s| s.to_string()),
                    process_identity: identity.clone(),
                    bounds: Bounds {
                        x: frame.origin.x,
                        y: frame.origin.y,
                        width: frame.size.width,
                        height: frame.size.height,
                    },
                    foreground: false,
                    scale,
                };
                if !window.bounds.valid() || !scale.is_finite() || scale <= 0.0 {
                    return Err(err(
                        "geometry_invalid",
                        "Invalid window dimensions or scale",
                    ));
                }
                if !take_image {
                    tx.send(Ok((window, None)))
                        .map_err(|_| err("cancelled", "Capture receiver closed"))?;
                    return Ok(());
                }
                let width = (frame.size.width * scale).ceil() as usize;
                let height = (frame.size.height * scale).ceil() as usize;
                if width
                    .checked_mul(height)
                    .is_none_or(|n| n > MAX_IMAGE_PIXELS)
                {
                    return Err(err(
                        "image_limit",
                        "Window screenshot exceeds the pixel limit",
                    ));
                }
                let config = unsafe { SCStreamConfiguration::new() };
                unsafe {
                    config.setWidth(width);
                    config.setHeight(height);
                    config.setShowsCursor(false);
                    config.setIgnoreShadowsSingleWindow(true);
                }
                let image_tx = tx.clone();
                let image_block = RcBlock::new(move |image: *mut CGImage, error: *mut NSError| {
                    let result = (|| -> Result<(Window, Option<Screenshot>)> {
                        if !error.is_null() {
                            return Err(err(
                                "capture_failed",
                                "ScreenCaptureKit could not capture the selected window",
                            ));
                        }
                        let image = unsafe { image.as_ref() }
                            .ok_or_else(|| err("capture_failed", "Capture returned no image"))?;
                        let rep =
                            NSBitmapImageRep::initWithCGImage(NSBitmapImageRep::alloc(), image);
                        let data = unsafe {
                            rep.representationUsingType_properties(
                                NSBitmapImageFileType::PNG,
                                &NSDictionary::new(),
                            )
                        }
                        .ok_or_else(|| err("image_encoding", "PNG encoding failed"))?;
                        let bytes = unsafe { data.as_bytes_unchecked() };
                        if bytes.len() > 12 * 1024 * 1024 {
                            return Err(err(
                                "image_limit",
                                "Encoded window screenshot exceeds the byte limit",
                            ));
                        }
                        Ok((
                            window.clone(),
                            Some(Screenshot {
                                mime_type: "image/png".into(),
                                base64: STANDARD.encode(bytes),
                                width: width as u32,
                                height: height as u32,
                            }),
                        ))
                    })();
                    let _closed = image_tx.send(result);
                });
                unsafe {
                    SCScreenshotManager::captureImageWithFilter_configuration_completionHandler(
                        &filter,
                        &config,
                        Some(&image_block),
                    );
                }
                Ok(())
            })();
            if let Err(error) = result {
                let _closed = tx.send(Err(error));
            }
        },
    );
    unsafe {
        SCShareableContent::getShareableContentExcludingDesktopWindows_onScreenWindowsOnly_completionHandler(true,true,&block);
    }
    rx.recv_timeout(Duration::from_secs(8))
        .map_err(|_| err("capture_timeout", "ScreenCaptureKit deadline exceeded"))?
}

fn matching_ax_window(app: &AXUIElement, window: &Window) -> Result<CFRetained<AXUIElement>> {
    let mut found = Vec::new();
    for ax in children(app, "AXWindows")? {
        if string_attr(&ax, "AXTitle")?.as_deref() == Some(&window.title)
            && bounds(&ax)?.is_some_and(|b| b.near(window.bounds, 2.0))
        {
            found.push(ax);
        }
    }
    if found.len() != 1 {
        return Err(err(
            "window_ambiguous",
            "The capture window cannot be uniquely associated with its accessibility window",
        ));
    }
    Ok(found.remove(0))
}
/// Stage Manager may expose a thumbnail frame for an inactive window. During
/// explicit admission focus only, correlate by unique exact title within the
/// already unique PID/window candidate, then recapture and require full geometry.
fn focus_window(window: &Window, admission: bool) -> Result<Window> {
    if process_identity(window.pid)? != window.process_identity {
        return Err(err("process_changed", "Application process changed"));
    }
    let app = application(window.pid)?;
    let root = if admission {
        let mut candidates = Vec::new();
        for ax in children(&app, "AXWindows")? {
            if string_attr(&ax, "AXTitle")?.as_deref() == Some(&window.title) {
                candidates.push(ax);
            }
        }
        if candidates.len() != 1 {
            return Err(err(
                "window_ambiguous",
                "Admission requires a unique exact accessibility window title",
            ));
        }
        candidates.remove(0)
    } else {
        matching_ax_window(&app, window)?
    };
    let running = NSRunningApplication::runningApplicationWithProcessIdentifier(window.pid as i32)
        .ok_or_else(|| err("application_missing", "Application no longer exists"))?;
    #[allow(deprecated)]
    if !running.activateWithOptions(NSApplicationActivationOptions::ActivateIgnoringOtherApps) {
        return Err(err(
            "foreground_denied",
            "macOS declined application activation",
        ));
    }
    if unsafe { root.perform_action(&CFString::from_str("AXRaise")) } != AXError::Success {
        return Err(err(
            "foreground_denied",
            "macOS declined raising the attached window",
        )
        .uncertain());
    }
    let mut prior: Option<Bounds> = None;
    let mut consecutive = 0;
    for _ in 0..25 {
        let (mut fresh, _) = capture(
            &Attach {
                pid: window.pid,
                window_id: Some(window.window_id),
                focus: false,
                read_only_targets: vec![],
            },
            false,
        )?;
        if fresh.process_identity != window.process_identity || fresh.title != window.title {
            return Err(err(
                "window_changed",
                "Window identity changed during focus",
            ));
        }
        if exact_foreground(&app, &fresh)? {
            matching_ax_window(&app, &fresh)?;
            if prior.is_some_and(|b| b.near(fresh.bounds, 0.0)) {
                consecutive += 1;
            } else {
                consecutive = 1;
            }
            prior = Some(fresh.bounds);
            if consecutive >= 3 {
                fresh.foreground = true;
                return Ok(fresh);
            }
        } else {
            prior = None;
            consecutive = 0;
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    Err(err(
        "foreground_denied",
        "Exact window activation did not settle before deadline",
    ))
}
fn exact_foreground(app: &AXUIElement, window: &Window) -> Result<bool> {
    let front = NSWorkspace::sharedWorkspace().frontmostApplication();
    if front.is_none_or(|a| a.processIdentifier() != window.pid as i32) {
        return Ok(false);
    }
    let Some(focused) = element_attr(app, "AXFocusedWindow")? else {
        return Ok(false);
    };
    Ok(
        string_attr(&focused, "AXTitle")?.as_deref() == Some(&window.title)
            && bounds(&focused)?.is_some_and(|b| b.near(window.bounds, 2.0)),
    )
}
fn normalized_role(role: &str) -> String {
    match role {
        "AXButton" => "button",
        "AXTextField" | "AXTextArea" => "text_field",
        "AXStaticText" => "text",
        "AXWindow" => "window",
        "AXCheckBox" => "checkbox",
        "AXRadioButton" => "radio",
        "AXComboBox" => "combo_box",
        "AXScrollArea" => "scroll_area",
        "AXTable" => "table",
        "AXRow" => "row",
        _ => "group",
    }
    .into()
}
fn describe(
    element: &AXUIElement,
    path: Vec<u32>,
    id: String,
    parent: Option<String>,
) -> Result<Node> {
    let raw_role = string_attr(element, "AXRole")?.unwrap_or_default();
    let mut name = string_attr(element, "AXTitle")?.filter(|s| !s.is_empty());
    if name.is_none() {
        name = string_attr(element, "AXDescription")?.filter(|s| !s.is_empty());
    }
    if name.is_none()
        && let Some(label) = element_attr(element, "AXTitleUIElement")?
    {
        name = string_attr(&label, "AXValue")?.or(string_attr(&label, "AXTitle")?);
    }
    let text_role = raw_role == "AXTextField" || raw_role == "AXTextArea";
    let mut settable = 0u8;
    let settable_status = unsafe {
        element.is_attribute_settable(&CFString::from_str("AXValue"), NonNull::from(&mut settable))
    };
    let editable = text_role && settable_status == AXError::Success && settable != 0;
    let mut actions = Vec::new();
    let mut raw_actions = std::ptr::null();
    let action_status = unsafe { element.copy_action_names(NonNull::from(&mut raw_actions)) };
    if action_status == AXError::Success {
        if let Some(raw) = NonNull::new(raw_actions.cast_mut()) {
            let array = typed_array(unsafe { CFRetained::from_raw(raw) });
            for action in array.iter() {
                let name = action
                    .downcast::<CFString>()
                    .map_err(|_| err("accessibility_type", "Expected action name"))?
                    .to_string();
                if name == "AXPress" {
                    actions.push("press".into());
                }
            }
        }
    } else if action_status != AXError::NotImplemented
        && action_status != AXError::ActionUnsupported
    {
        return Err(err(
            "accessibility_read",
            "Could not read target action names",
        ));
    }
    if editable {
        actions.extend(["set_value".into(), "type_text".into()]);
    }
    let value = if text_role || raw_role == "AXStaticText" {
        string_attr(element, "AXValue")?
    } else {
        None
    };
    Ok(Node {
        id,
        role: normalized_role(&raw_role),
        name: name.unwrap_or_default(),
        value,
        identifier: string_attr(element, "AXIdentifier")?,
        parent,
        bounds: bounds(element)?,
        enabled: bool_attr(element, "AXEnabled")?.unwrap_or(false),
        editable,
        actions,
        native_path: path,
    })
}
fn walk(
    element: &AXUIElement,
    path: Vec<u32>,
    parent: Option<String>,
    nodes: &mut Vec<Node>,
    complete: &mut bool,
) -> Result<()> {
    if nodes.len() >= MAX_NODES || path.len() > MAX_DEPTH {
        *complete = false;
        return Ok(());
    }
    let id = format!("n{}", nodes.len());
    nodes.push(describe(element, path.clone(), id.clone(), parent)?);
    for (index, child) in children(element, "AXChildren")?.into_iter().enumerate() {
        let mut child_path = path.clone();
        child_path.push(index as u32);
        walk(&child, child_path, Some(id.clone()), nodes, complete)?;
        if !*complete {
            break;
        }
    }
    Ok(())
}
fn tree(window: &Window) -> Result<(CFRetained<AXUIElement>, Vec<Node>, bool, bool)> {
    if process_identity(window.pid)? != window.process_identity {
        return Err(err(
            "process_changed",
            "Application process identity changed",
        ));
    }
    let app = application(window.pid)?;
    let root = matching_ax_window(&app, window)?;
    let foreground = exact_foreground(&app, window)?;
    let mut nodes = Vec::new();
    let mut complete = true;
    walk(&root, Vec::new(), None, &mut nodes, &mut complete)?;
    Ok((root, nodes, complete, foreground))
}
fn current_window(expected: &Window) -> Result<Window> {
    let (window, _) = capture(
        &Attach {
            pid: expected.pid,
            window_id: Some(expected.window_id),
            read_only_targets: vec![],
            focus: false,
        },
        false,
    )?;
    if window.process_identity != expected.process_identity
        || !window.bounds.near(expected.bounds, 1.0)
        || window.title != expected.title
    {
        return Err(DesktopError::new(
            "window_changed",
            format!(
                "Window identity or geometry changed; observe again (previous bounds {:?}; current bounds {:?})",
                expected.bounds, window.bounds
            ),
        ));
    }
    Ok(window)
}
fn visual_recipient(root: &AXUIElement, window: &Window, x: f64, y: f64) -> Result<()> {
    let app = application(window.pid)?;
    if !exact_foreground(&app, window)? {
        return Err(err(
            "foreground_mismatch",
            "Visual input requires the exact attached foreground window",
        ));
    }
    let mut raw = std::ptr::null();
    let system = unsafe { AXUIElement::new_system_wide() };
    if unsafe { system.copy_element_at_position(x as f32, y as f32, NonNull::from(&mut raw)) }
        != AXError::Success
    {
        return Err(err(
            "hit_test_unknown",
            "Visual input recipient could not be established",
        ));
    }
    let hit = NonNull::new(raw.cast_mut())
        .map(|p| unsafe { CFRetained::from_raw(p) })
        .ok_or_else(|| {
            err(
                "hit_test_unknown",
                "Visual input hit test returned no recipient",
            )
        })?;
    let mut pid = 0;
    if unsafe { hit.pid(NonNull::from(&mut pid)) } != AXError::Success || pid != window.pid as i32 {
        return Err(err(
            "target_occluded",
            "Visual click would reach another application",
        ));
    }
    if &*hit != root && element_attr(&hit, "AXWindow")?.as_deref() != Some(root) {
        return Err(err(
            "target_occluded",
            "Visual click would reach another window",
        ));
    }
    Ok(())
}

fn visual_act(
    window: &Window,
    visual: &VisualMatch,
    anchor: &VisualAnchor,
    action: &Action,
    expected_state: &str,
    expected_nodes: &[Node],
    expected_input: &InputStamp,
) -> Result<Receipt> {
    input_guard::check(expected_input)?;
    let fresh = current_window(window)?;
    if !fresh.bounds.near(window.bounds, 0.0) || fresh.scale != window.scale {
        return Err(err("window_changed", "Visual window geometry changed"));
    }
    let (captured, image) = capture(
        &Attach {
            pid: window.pid,
            window_id: Some(window.window_id),
            focus: false,
            read_only_targets: vec![],
        },
        true,
    )?;
    if captured.process_identity != fresh.process_identity
        || !captured.bounds.near(fresh.bounds, 0.0)
        || captured.scale != fresh.scale
    {
        return Err(err(
            "window_changed",
            "Window changed during visual capture",
        ));
    }
    let (root, nodes, complete, foreground) = tree(&captured)?;
    if !complete || !foreground {
        return Err(err(
            "foreground_mismatch",
            "Visual action requires a complete observation of the foreground window",
        ));
    }
    validate_state(expected_nodes, &nodes, expected_state)?;
    let image = image.ok_or_else(|| {
        err(
            "capture_failed",
            "Visual revalidation requires scoped pixels",
        )
    })?;
    let (x, y) = revalidate_visual(&captured, &image, anchor, visual, action)?;
    visual_recipient(&root, &captured, x, y)?;
    let mut input = Enigo::new(&Settings {
        open_prompt_to_get_permissions: false,
        independent_of_keyboard_state: true,
        ..Settings::default()
    })
    .map_err(|e| DesktopError::new("input_unavailable", e.to_string()))?;
    let dispatched_at_ms = now_ms();
    input_guard::check(expected_input)?;
    input
        .move_mouse(x as i32, y as i32, Coordinate::Abs)
        .map_err(|e| DesktopError::new("input_failed", e.to_string()).uncertain())?;
    // Movement can trigger hover UI. Recheck the actual window recipient before
    // pressing; any failure after movement is explicitly uncertain delivery.
    let (after_move, image) = capture(
        &Attach {
            pid: window.pid,
            window_id: Some(window.window_id),
            focus: false,
            read_only_targets: vec![],
        },
        true,
    )
    .map_err(DesktopError::uncertain)?;
    let image =
        image.ok_or_else(|| err("capture_failed", "Post-movement frame missing").uncertain())?;
    revalidate_visual(&after_move, &image, anchor, visual, action)
        .map_err(DesktopError::uncertain)?;
    visual_recipient(&root, &after_move, x, y).map_err(DesktopError::uncertain)?;
    input_guard::check(expected_input).map_err(DesktopError::uncertain)?;
    input
        .button(Button::Left, Direction::Click)
        .map_err(|e| DesktopError::new("input_failed", e.to_string()).uncertain())?;
    Ok(Receipt {
        action_id: action.action_id.clone(),
        delivery: Delivery::Sent,
        dispatched_at_ms,
        verified: false,
    })
}

fn act(
    window: &Window,
    expected: &Node,
    action: &Action,
    expected_state: &str,
    expected_nodes: &[Node],
    expected_input: &InputStamp,
) -> Result<Receipt> {
    input_guard::check(expected_input)?;
    action.validate()?;
    let fresh = current_window(window)?;
    let (root, nodes, complete, foreground) = tree(&fresh)?;
    if !complete {
        return Err(err(
            "tree_incomplete",
            "Cannot act on an incomplete accessibility tree",
        ));
    }
    validate_state(expected_nodes, &nodes, expected_state)?;
    let actual = nodes
        .iter()
        .find(|n| n.native_path == expected.native_path)
        .ok_or_else(|| err("target_missing", "Target path no longer exists"))?;
    if !same_target(expected, actual) {
        return Err(err(
            "target_changed",
            "Target identity, state, or geometry changed",
        ));
    }
    let mut element = root;
    for index in &expected.native_path {
        element = children(&element, "AXChildren")?
            .into_iter()
            .nth(*index as usize)
            .ok_or_else(|| err("target_missing", "Target disappeared during resolution"))?;
    }
    // A second direct read closes the traversal-to-action gap as far as AX allows.
    let final_node = describe(
        &element,
        expected.native_path.clone(),
        expected.id.clone(),
        expected.parent.clone(),
    )?;
    if !same_target(expected, &final_node) {
        return Err(err(
            "target_changed",
            "Target changed during action preparation",
        ));
    }
    let dispatch = now_ms();
    match action.kind {
        ActionKind::Press => {
            input_guard::check(expected_input)?;
            let status = unsafe { element.perform_action(&CFString::from_str("AXPress")) };
            if status != AXError::Success {
                return Err(DesktopError::new(
                    "action_failed",
                    format!("AXPress returned {}", status.0),
                )
                .uncertain());
            }
        }
        ActionKind::SetValue => {
            if !actual.editable {
                return Err(err("not_editable", "Target is not an editable field"));
            }
            let text = CFString::from_str(
                action
                    .text
                    .as_deref()
                    .ok_or_else(|| err("invalid_action", "Text required"))?,
            );
            let status = unsafe {
                input_guard::check(expected_input)?;
                element.set_attribute_value(&CFString::from_str("AXValue"), text.as_ref())
            };
            if status != AXError::Success {
                return Err(DesktopError::new(
                    "action_failed",
                    format!("AXValue write returned {}", status.0),
                )
                .uncertain());
            }
        }
        ActionKind::TypeText | ActionKind::Click | ActionKind::Scroll => {
            if !foreground {
                return Err(err(
                    "foreground_mismatch",
                    "Physical input requires the exact target window in the foreground",
                ));
            }
            let bounds = actual.bounds.filter(|b| b.valid()).ok_or_else(|| {
                err(
                    "geometry_unavailable",
                    "Physical input requires target bounds",
                )
            })?;
            let x = action.x.unwrap_or(bounds.x + bounds.width / 2.0);
            let y = action.y.unwrap_or(bounds.y + bounds.height / 2.0);
            if !bounds.contains(x, y) || !window.bounds.contains(x, y) {
                return Err(err(
                    "outside_target",
                    "Input point must lie inside the target and attached window",
                ));
            }
            let mut hit = std::ptr::null();
            let system = unsafe { AXUIElement::new_system_wide() };
            let status = unsafe {
                system.copy_element_at_position(x as f32, y as f32, NonNull::from(&mut hit))
            };
            if status != AXError::Success {
                return Err(err(
                    "hit_test_unknown",
                    "Could not establish input recipient",
                ));
            }
            let hit = NonNull::new(hit.cast_mut())
                .map(|p| unsafe { CFRetained::from_raw(p) })
                .ok_or_else(|| err("hit_test_unknown", "No hit-test recipient"))?;
            if hit != element {
                return Err(err(
                    "target_occluded",
                    "Physical input would reach another element",
                ));
            }
            let settings = Settings {
                open_prompt_to_get_permissions: false,
                independent_of_keyboard_state: true,
                ..Settings::default()
            };
            let mut input = Enigo::new(&settings)
                .map_err(|e| DesktopError::new("input_unavailable", e.to_string()))?;
            match action.kind {
                ActionKind::TypeText => {
                    let focused = bool_attr(&element, "AXFocused")?.unwrap_or(false);
                    if !focused {
                        return Err(err(
                            "focus_required",
                            "Typing requires the target field already focused",
                        ));
                    }
                    input_guard::check(expected_input)?;
                    input
                        .text(
                            action
                                .text
                                .as_deref()
                                .ok_or_else(|| err("invalid_action", "Text required"))?,
                        )
                        .map_err(|e| {
                            DesktopError::new("input_failed", e.to_string()).uncertain()
                        })?;
                }
                ActionKind::Click => {
                    input_guard::check(expected_input)?;
                    input
                        .move_mouse(x.round() as i32, y.round() as i32, Coordinate::Abs)
                        .map_err(|e| {
                            DesktopError::new("input_failed", e.to_string()).uncertain()
                        })?;
                    input_guard::check(expected_input).map_err(DesktopError::uncertain)?;
                    input.button(Button::Left, Direction::Click).map_err(|e| {
                        DesktopError::new("input_failed", e.to_string()).uncertain()
                    })?;
                }
                ActionKind::Scroll => {
                    input_guard::check(expected_input)?;
                    input
                        .move_mouse(x.round() as i32, y.round() as i32, Coordinate::Abs)
                        .map_err(|e| {
                            DesktopError::new("input_failed", e.to_string()).uncertain()
                        })?;
                    input_guard::check(expected_input).map_err(DesktopError::uncertain)?;
                    input
                        .scroll(action.scroll_y.unwrap_or(0), Axis::Vertical)
                        .map_err(|e| {
                            DesktopError::new("input_failed", e.to_string()).uncertain()
                        })?;
                }
                _ => unreachable!(),
            }
        }
    }
    Ok(Receipt {
        action_id: action.action_id.clone(),
        delivery: Delivery::Sent,
        dispatched_at_ms: dispatch,
        verified: false,
    })
}

pub fn execute(request: WorkerRequest) -> Result<Value> {
    // A fresh CLI helper has no AppKit/WindowServer connection. ScreenCaptureKit
    // otherwise aborts inside CGS_REQUIRE_INIT instead of returning an error.
    // Prohibited activation initializes that connection without focusing us.
    let mtm = MainThreadMarker::new().ok_or_else(|| {
        err(
            "thread_invalid",
            "Native helper must execute on its main thread",
        )
    })?;
    let helper_application = NSApplication::sharedApplication(mtm);
    if helper_application.activationPolicy() != NSApplicationActivationPolicy::Prohibited
        && !helper_application.setActivationPolicy(NSApplicationActivationPolicy::Prohibited)
    {
        return Err(err(
            "appkit_initialization",
            "Cannot prohibit helper activation",
        ));
    }
    match request.op.as_str() {
        "probe" => Ok(
            json!({"protocol_version":1,"platform":"macos","architecture":std::env::consts::ARCH,"accessibility":{"state":if unsafe { AXIsProcessTrusted() } {"granted"} else {"denied"}},"screen_capture":{"state":if CGPreflightScreenCaptureAccess() {"granted"} else {"denied"},"backend":"screencapturekit","scope":"window"},"external_input":{"implementation":"bounded_hid_counter","hardware_qualification":"not_tested","synthetic_sources":"unclassified","continuous":false,"stored_data":"counters_only"},"qualification":"not_tested"}),
        ),
        "attach" => {
            let attach = request
                .attach
                .ok_or_else(|| err("invalid_request", "Attach payload required"))?;
            let (mut window, _) = capture(&attach, false)?;
            if attach.focus {
                window = focus_window(&window, true)?;
            }
            let app = application(window.pid)?;
            matching_ax_window(&app, &window)?;
            window.foreground = exact_foreground(&app, &window)?;
            serde_json::to_value(window)
                .map_err(|e| DesktopError::new("serialization", e.to_string()))
        }
        "focus" => {
            let window = request
                .window
                .ok_or_else(|| err("invalid_request", "Window required"))?;
            serde_json::to_value(focus_window(&window, false)?)
                .map_err(|e| DesktopError::new("serialization", e.to_string()))
        }

        "observe" => {
            let input_stamp = input_guard::snapshot();
            if let Some(expected) = request.expected_input.as_ref() {
                input_guard::compare(expected, &input_stamp)?;
            }
            let expected = request
                .window
                .ok_or_else(|| err("invalid_request", "Window required"))?;
            let (mut window, _) = capture(
                &Attach {
                    pid: expected.pid,
                    window_id: Some(expected.window_id),
                    read_only_targets: vec![],
                    focus: false,
                },
                false,
            )?;
            if window.process_identity != expected.process_identity {
                return Err(err(
                    "process_changed",
                    "Application process identity changed",
                ));
            }
            let tree_at_ms = now_ms();
            let (_, nodes, complete, foreground) = tree(&window)?;
            window.foreground = foreground;
            let (captured, image) = capture(
                &Attach {
                    pid: window.pid,
                    window_id: Some(window.window_id),
                    read_only_targets: vec![],
                    focus: false,
                },
                true,
            )?;
            if !captured.bounds.near(window.bounds, 1.0)
                || captured.process_identity != window.process_identity
            {
                return Err(err(
                    "observation_inconsistent",
                    "Window changed between accessibility and image capture",
                ));
            }
            // The first scoped frame is used only to establish stability. It is
            // never persisted or returned to the host.
            let first_screenshot =
                image.ok_or_else(|| err("capture_failed", "Missing screenshot"))?;
            let (_, after_nodes, after_complete, _) = tree(&window)?;
            if complete != after_complete
                || state_fingerprint(&nodes)? != state_fingerprint(&after_nodes)?
            {
                return Err(DesktopError::new(
                    "observation_inconsistent",
                    format!(
                        "Accessibility state changed during first scoped capture: {}",
                        state_difference(&nodes, &after_nodes)
                    ),
                ));
            }
            std::thread::sleep(Duration::from_millis(150));
            let (second_window, second_image) = capture(
                &Attach {
                    pid: window.pid,
                    window_id: Some(window.window_id),
                    focus: false,
                    read_only_targets: vec![],
                },
                true,
            )?;
            if second_window.process_identity != window.process_identity
                || !second_window.bounds.near(window.bounds, 0.0)
                || second_window.scale != window.scale
            {
                return Err(err(
                    "observation_inconsistent",
                    "Window geometry changed between two scoped captures",
                ));
            }
            let (_, stable_nodes, stable_complete, stable_foreground) = tree(&window)?;
            if stable_complete != complete
                || state_fingerprint(&nodes)? != state_fingerprint(&stable_nodes)?
            {
                return Err(DesktopError::new(
                    "observation_inconsistent",
                    format!(
                        "Accessibility state changed between two scoped captures: {}",
                        state_difference(&nodes, &stable_nodes)
                    ),
                ));
            }
            let screenshot = second_image
                .ok_or_else(|| err("capture_failed", "Second scoped capture missing"))?;
            if screenshot.width != first_screenshot.width
                || screenshot.height != first_screenshot.height
            {
                return Err(err(
                    "observation_inconsistent",
                    "Scoped image dimensions changed",
                ));
            }
            window.foreground = stable_foreground;
            input_guard::check(&input_stamp)?;
            let observation = Observation {
                observation_id: String::new(),
                epoch: 0,
                captured_at_ms: now_ms(),
                tree_at_ms,
                window,
                nodes,
                screenshot,
                complete,
                input_stamp: Some(input_stamp),
                truncated_reason: if complete {
                    None
                } else {
                    Some("node_or_depth_limit".into())
                },
            };
            serde_json::to_value(observation)
                .map_err(|e| DesktopError::new("serialization", e.to_string()))
        }
        "act" => {
            let expected_input = request.expected_input.ok_or_else(|| {
                err(
                    "input_guard_missing",
                    "Hardware admission baseline required",
                )
            })?;
            let window = request
                .window
                .ok_or_else(|| err("invalid_request", "Window required"))?;
            let target = request
                .target
                .ok_or_else(|| err("invalid_request", "Target required"))?;
            let action = request
                .action
                .ok_or_else(|| err("invalid_request", "Action required"))?;
            let expected_nodes = request
                .expected_nodes
                .ok_or_else(|| err("invalid_request", "Expected nodes required"))?;
            let expected_state = request
                .expected_state
                .ok_or_else(|| err("invalid_request", "Expected observation state required"))?;
            let receipt = match target {
                ResolvedTarget::Semantic { node } => act(
                    &window,
                    &node,
                    &action,
                    &expected_state,
                    &expected_nodes,
                    &expected_input,
                )?,
                ResolvedTarget::Visual { visual } => visual_act(
                    &window,
                    &visual,
                    &request
                        .visual_anchor
                        .ok_or_else(|| err("invalid_request", "Visual anchor required"))?,
                    &action,
                    &expected_state,
                    &expected_nodes,
                    &expected_input,
                )?,
            };
            serde_json::to_value(receipt)
                .map_err(|e| DesktopError::new("serialization", e.to_string()))
        }
        _ => Err(err("unknown_operation", "Unknown native helper operation")),
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn directly_launched_process_has_stable_kernel_identity() {
        let a = super::process_identity(std::process::id()).unwrap();
        let b = super::process_identity(std::process::id()).unwrap();
        assert_eq!(a, b);
        assert!(a.contains("desktop_macos"));
    }
}
