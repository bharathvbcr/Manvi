//! UI Automation and GDI are isolated in a helper because providers may block.
use base64::{Engine, engine::general_purpose::STANDARD};
use desktop_core::{
    Action, ActionKind, Attach, Bounds, Delivery, DesktopError, MAX_DEPTH, MAX_IMAGE_PIXELS,
    MAX_NODES, MAX_TEXT_BYTES, Node, Observation, Receipt, ResolvedTarget, Result, Screenshot,
    VisualAnchor, VisualMatch, Window, WorkerRequest, now_ms, revalidate_visual, same_target,
    validate_state,
};
use image::{DynamicImage, ImageFormat, RgbaImage};
use serde_json::{Value, json};
use std::{ffi::c_void, io::Cursor, mem::size_of};
use windows::{
    Win32::{
        Foundation::{CloseHandle, E_POINTER, FILETIME, HWND, LPARAM, POINT, RECT},
        Graphics::Gdi::{
            BI_RGB, BITMAPINFO, BITMAPINFOHEADER, CreateCompatibleBitmap, CreateCompatibleDC,
            DIB_RGB_COLORS, DeleteDC, DeleteObject, GetDC, GetDIBits, HBITMAP, HDC, HGDIOBJ,
            ReleaseDC, SelectObject,
        },
        Storage::Xps::{PRINT_WINDOW_FLAGS, PrintWindow},
        System::{
            Com::{
                CLSCTX_INPROC_SERVER, COINIT_MULTITHREADED, CoCreateInstance, CoInitializeEx,
                CoUninitialize,
            },
            Threading::{
                GetProcessTimes, OpenProcess, PROCESS_NAME_WIN32,
                PROCESS_QUERY_LIMITED_INFORMATION, QueryFullProcessImageNameW,
            },
        },
        UI::{
            Accessibility::{
                CUIAutomation8, IUIAutomation, IUIAutomation2, IUIAutomationElement,
                IUIAutomationInvokePattern, IUIAutomationTreeWalker, IUIAutomationValuePattern,
                UIA_ButtonControlTypeId, UIA_CheckBoxControlTypeId, UIA_ComboBoxControlTypeId,
                UIA_EditControlTypeId, UIA_InvokePatternId, UIA_RadioButtonControlTypeId,
                UIA_TextControlTypeId, UIA_ValuePatternId, UIA_WindowControlTypeId,
            },
            HiDpi::{
                DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2, GetDpiForWindow,
                SetThreadDpiAwarenessContext,
            },
            Input::KeyboardAndMouse::{
                INPUT, INPUT_0, INPUT_KEYBOARD, INPUT_MOUSE, KEYBDINPUT, KEYEVENTF_KEYUP,
                KEYEVENTF_UNICODE, MOUSEEVENTF_ABSOLUTE, MOUSEEVENTF_LEFTDOWN, MOUSEEVENTF_LEFTUP,
                MOUSEEVENTF_MOVE, MOUSEEVENTF_VIRTUALDESK, MOUSEEVENTF_WHEEL, MOUSEINPUT,
                SendInput, VIRTUAL_KEY,
            },
            WindowsAndMessaging::{
                EnumWindows, GA_ROOT, GetAncestor, GetForegroundWindow, GetSystemMetrics,
                GetWindowRect, GetWindowTextW, GetWindowThreadProcessId, IsIconic, IsWindowVisible,
                SM_CXVIRTUALSCREEN, SM_CYVIRTUALSCREEN, SM_XVIRTUALSCREEN, SM_YVIRTUALSCREEN,
                SetForegroundWindow, WindowFromPoint,
            },
        },
    },
    core::{BOOL, BSTR, Interface},
};

fn err(code: &str, message: impl Into<String>) -> DesktopError {
    DesktopError::new(code, message)
}
fn native(e: windows::core::Error) -> DesktopError {
    err("native_api", e.to_string())
}
fn bounded(s: String) -> Result<String> {
    if s.len() > MAX_TEXT_BYTES {
        Err(err(
            "attribute_limit",
            "Accessibility text exceeds byte limit",
        ))
    } else {
        Ok(s)
    }
}
fn hwnd(id: u64) -> Result<HWND> {
    let value = usize::try_from(id).map_err(|_| {
        err(
            "invalid_window",
            "Window handle does not fit this architecture",
        )
    })?;
    if value == 0 {
        return Err(err("invalid_window", "Window handle is null"));
    }
    Ok(HWND(value as *mut c_void))
}
fn identity(pid: u32) -> Result<String> {
    unsafe {
        let process = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, pid).map_err(native)?;
        let result = (|| -> Result<String> {
            let (mut creation, mut exit, mut kernel, mut user) = (
                FILETIME::default(),
                FILETIME::default(),
                FILETIME::default(),
                FILETIME::default(),
            );
            GetProcessTimes(process, &mut creation, &mut exit, &mut kernel, &mut user)
                .map_err(native)?;
            let mut name = vec![0u16; 32_768];
            let mut length = name.len() as u32;
            QueryFullProcessImageNameW(
                process,
                PROCESS_NAME_WIN32,
                windows::core::PWSTR(name.as_mut_ptr()),
                &mut length,
            )
            .map_err(native)?;
            let created =
                (u64::from(creation.dwHighDateTime) << 32) | u64::from(creation.dwLowDateTime);
            Ok(format!(
                "{pid}:{created}:{}",
                String::from_utf16_lossy(&name[..length as usize])
            ))
        })();
        CloseHandle(process).map_err(native)?;
        result
    }
}
fn window_info(handle: HWND) -> Result<Window> {
    unsafe {
        let mut pid = 0;
        GetWindowThreadProcessId(handle, Some(&mut pid));
        if pid == 0 || !IsWindowVisible(handle).as_bool() || IsIconic(handle).as_bool() {
            return Err(err(
                "window_missing",
                "Window is hidden, minimized, or unavailable",
            ));
        }
        let mut rect = RECT::default();
        GetWindowRect(handle, &mut rect).map_err(native)?;
        let mut title = vec![0u16; 8192];
        let length = GetWindowTextW(handle, &mut title);
        if length == 8191 {
            return Err(err("attribute_limit", "Window title is truncated"));
        }
        Ok(Window {
            pid,
            window_id: handle.0 as usize as u64,
            title: String::from_utf16_lossy(&title[..length.max(0) as usize]),
            process_identity: identity(pid)?,
            bounds: Bounds {
                x: rect.left as f64,
                y: rect.top as f64,
                width: (rect.right - rect.left) as f64,
                height: (rect.bottom - rect.top) as f64,
            },
            foreground: GetForegroundWindow() == handle,
            scale: f64::from(GetDpiForWindow(handle)) / 96.0,
        })
    }
}
unsafe extern "system" fn collect_window(handle: HWND, param: LPARAM) -> BOOL {
    // EnumWindows invokes synchronously; the Vec outlives this callback.
    let handles = unsafe { &mut *(param.0 as *mut Vec<HWND>) };
    if handles.len() < 8192 {
        handles.push(handle);
        true.into()
    } else {
        false.into()
    }
}
fn attach(payload: &Attach) -> Result<Window> {
    if let Some(id) = payload.window_id {
        let window = window_info(hwnd(id)?)?;
        if window.pid != payload.pid {
            return Err(err("pid_mismatch", "Window belongs to another application"));
        }
        return Ok(window);
    }
    let mut handles = Vec::<HWND>::new();
    unsafe {
        EnumWindows(
            Some(collect_window),
            LPARAM((&mut handles as *mut Vec<HWND>) as isize),
        )
        .map_err(native)?;
    }
    let mut candidates = Vec::new();
    for handle in handles {
        let mut pid = 0;
        unsafe {
            GetWindowThreadProcessId(handle, Some(&mut pid));
        }
        if pid == payload.pid {
            if let Ok(window) = window_info(handle) {
                candidates.push(window);
            }
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
struct Apartment;
impl Drop for Apartment {
    fn drop(&mut self) {
        unsafe {
            CoUninitialize();
        }
    }
}
struct Automation {
    client: IUIAutomation,
    walker: IUIAutomationTreeWalker,
    _apartment: Apartment,
}
impl Automation {
    fn new() -> Result<Self> {
        unsafe {
            CoInitializeEx(None, COINIT_MULTITHREADED)
                .ok()
                .map_err(native)?;
            let apartment = Apartment;
            let timeout_client: IUIAutomation2 =
                CoCreateInstance(&CUIAutomation8, None, CLSCTX_INPROC_SERVER).map_err(native)?;
            timeout_client.SetConnectionTimeout(750).map_err(native)?;
            timeout_client.SetTransactionTimeout(1500).map_err(native)?;
            let client: IUIAutomation = timeout_client.cast().map_err(native)?;
            let walker = client.ControlViewWalker().map_err(native)?;
            Ok(Self {
                client,
                walker,
                _apartment: apartment,
            })
        }
    }
    fn root(&self, window: &Window) -> Result<IUIAutomationElement> {
        unsafe {
            self.client
                .ElementFromHandle(hwnd(window.window_id)?)
                .map_err(native)
        }
    }
    fn children(&self, element: &IUIAutomationElement) -> Result<Vec<IUIAutomationElement>> {
        unsafe {
            let mut children = Vec::new();
            let mut child = match self.walker.GetFirstChildElement(element) {
                Ok(v) => Some(v),
                Err(e) if e.code() == E_POINTER => None,
                Err(e) => return Err(native(e)),
            };
            while let Some(current) = child {
                if children.len() >= MAX_NODES {
                    return Err(err("node_limit", "Child array exceeds limit"));
                }
                child = match self.walker.GetNextSiblingElement(&current) {
                    Ok(v) => Some(v),
                    Err(e) if e.code() == E_POINTER => None,
                    Err(e) => return Err(native(e)),
                };
                children.push(current);
            }
            Ok(children)
        }
    }
    fn describe(
        &self,
        element: &IUIAutomationElement,
        path: Vec<u32>,
        id: String,
        parent: Option<String>,
    ) -> Result<Node> {
        unsafe {
            let control = element.CurrentControlType().map_err(native)?;
            let role = if control == UIA_ButtonControlTypeId {
                "button"
            } else if control == UIA_EditControlTypeId {
                "text_field"
            } else if control == UIA_TextControlTypeId {
                "text"
            } else if control == UIA_CheckBoxControlTypeId {
                "checkbox"
            } else if control == UIA_RadioButtonControlTypeId {
                "radio"
            } else if control == UIA_ComboBoxControlTypeId {
                "combo_box"
            } else if control == UIA_WindowControlTypeId {
                "window"
            } else {
                "group"
            };
            let name = bounded(element.CurrentName().map_err(native)?.to_string())?;
            let identifier = bounded(element.CurrentAutomationId().map_err(native)?.to_string())?;
            let rect = element.CurrentBoundingRectangle().map_err(native)?;
            let value_pattern = element
                .GetCurrentPatternAs::<IUIAutomationValuePattern>(UIA_ValuePatternId)
                .ok();
            let editable = match &value_pattern {
                Some(p) => !p.CurrentIsReadOnly().map_err(native)?.as_bool(),
                None => false,
            };
            let value = value_pattern
                .as_ref()
                .map(|p| {
                    p.CurrentValue()
                        .map_err(native)
                        .and_then(|v| bounded(v.to_string()))
                })
                .transpose()?;
            let mut actions = Vec::new();
            if element
                .GetCurrentPatternAs::<IUIAutomationInvokePattern>(UIA_InvokePatternId)
                .is_ok()
            {
                actions.push("press".into());
            }
            if editable {
                actions.extend(["set_value".into(), "type_text".into()]);
            }
            Ok(Node {
                id,
                role: role.into(),
                name,
                value,
                identifier: if identifier.is_empty() {
                    None
                } else {
                    Some(identifier)
                },
                parent,
                bounds: Some(Bounds {
                    x: rect.left as f64,
                    y: rect.top as f64,
                    width: (rect.right - rect.left) as f64,
                    height: (rect.bottom - rect.top) as f64,
                }),
                enabled: element.CurrentIsEnabled().map_err(native)?.as_bool(),
                editable,
                actions,
                native_path: path,
            })
        }
    }
    fn walk(
        &self,
        element: &IUIAutomationElement,
        path: Vec<u32>,
        parent: Option<String>,
        nodes: &mut Vec<Node>,
        complete: &mut bool,
    ) -> Result<()> {
        if path.len() > MAX_DEPTH || nodes.len() >= MAX_NODES {
            *complete = false;
            return Ok(());
        }
        let id = format!("n{}", nodes.len());
        nodes.push(self.describe(element, path.clone(), id.clone(), parent)?);
        for (index, child) in self.children(element)?.into_iter().enumerate() {
            let mut next = path.clone();
            next.push(index as u32);
            self.walk(&child, next, Some(id.clone()), nodes, complete)?;
            if !*complete {
                break;
            }
        }
        Ok(())
    }
    fn tree(&self, window: &Window) -> Result<(Vec<Node>, bool)> {
        let root = self.root(window)?;
        let mut nodes = Vec::new();
        let mut complete = true;
        self.walk(&root, vec![], None, &mut nodes, &mut complete)?;
        Ok((nodes, complete))
    }
    fn target(&self, window: &Window, node: &Node) -> Result<IUIAutomationElement> {
        let mut element = self.root(window)?;
        for index in &node.native_path {
            element = self
                .children(&element)?
                .into_iter()
                .nth(*index as usize)
                .ok_or_else(|| err("target_missing", "Target path disappeared"))?;
        }
        let fresh = self.describe(
            &element,
            node.native_path.clone(),
            node.id.clone(),
            node.parent.clone(),
        )?;
        if !same_target(node, &fresh) {
            return Err(err(
                "target_changed",
                "Target identity, geometry, or enabled state changed",
            ));
        }
        Ok(element)
    }
}
struct Bitmap {
    window: HWND,
    dc: HDC,
    memory: HDC,
    bitmap: HBITMAP,
    previous: HGDIOBJ,
}
impl Drop for Bitmap {
    fn drop(&mut self) {
        unsafe {
            SelectObject(self.memory, self.previous);
            let _released_bitmap = DeleteObject(self.bitmap.into());
            let _released_dc = DeleteDC(self.memory);
            let _released_window = ReleaseDC(Some(self.window), self.dc);
        }
    }
}
fn capture(window: &Window) -> Result<Screenshot> {
    unsafe {
        let width = window.bounds.width as i32;
        let height = window.bounds.height as i32;
        if width <= 0
            || height <= 0
            || (width as usize)
                .checked_mul(height as usize)
                .is_none_or(|n| n > MAX_IMAGE_PIXELS)
        {
            return Err(err("image_limit", "Invalid screenshot dimensions"));
        }
        let handle = hwnd(window.window_id)?;
        let dc = GetDC(Some(handle));
        if dc.is_invalid() {
            return Err(err("capture_failed", "Could not acquire target window DC"));
        }
        let memory = CreateCompatibleDC(Some(dc));
        if memory.is_invalid() {
            ReleaseDC(Some(handle), dc);
            return Err(err("capture_failed", "Could not create capture DC"));
        }
        let bitmap = CreateCompatibleBitmap(dc, width, height);
        if bitmap.is_invalid() {
            let _released = DeleteDC(memory);
            ReleaseDC(Some(handle), dc);
            return Err(err("capture_failed", "Could not create capture bitmap"));
        }
        let previous = SelectObject(memory, bitmap.into());
        let guard = Bitmap {
            window: handle,
            dc,
            memory,
            bitmap,
            previous,
        };
        if !PrintWindow(handle, memory, PRINT_WINDOW_FLAGS(2)).as_bool() {
            return Err(err(
                "capture_failed",
                "PrintWindow failed for the attached window",
            ));
        }
        SelectObject(memory, previous);
        let mut info = BITMAPINFO {
            bmiHeader: BITMAPINFOHEADER {
                biSize: size_of::<BITMAPINFOHEADER>() as u32,
                biWidth: width,
                biHeight: -height,
                biPlanes: 1,
                biBitCount: 32,
                biCompression: BI_RGB.0,
                ..Default::default()
            },
            ..Default::default()
        };
        let mut pixels = vec![0u8; width as usize * height as usize * 4];
        if GetDIBits(
            guard.dc,
            bitmap,
            0,
            height as u32,
            Some(pixels.as_mut_ptr().cast()),
            &mut info,
            DIB_RGB_COLORS,
        ) != height
        {
            return Err(err(
                "capture_failed",
                "Capture returned incomplete pixel rows",
            ));
        }
        for pixel in pixels.chunks_exact_mut(4) {
            pixel.swap(0, 2);
            pixel[3] = 255;
        }
        let image = RgbaImage::from_raw(width as u32, height as u32, pixels)
            .ok_or_else(|| err("image_encoding", "Invalid capture buffer"))?;
        let mut encoded = Cursor::new(Vec::new());
        DynamicImage::ImageRgba8(image)
            .write_to(&mut encoded, ImageFormat::Png)
            .map_err(|e| err("image_encoding", e.to_string()))?;
        if encoded.get_ref().len() > 12 * 1024 * 1024 {
            return Err(err("image_limit", "Encoded screenshot exceeds limit"));
        }
        Ok(Screenshot {
            mime_type: "image/png".into(),
            base64: STANDARD.encode(encoded.into_inner()),
            width: width as u32,
            height: height as u32,
        })
    }
}
fn send(events: &[INPUT]) -> Result<()> {
    let sent = unsafe { SendInput(events, size_of::<INPUT>() as i32) };
    if sent != events.len() as u32 {
        return Err(err(
            "input_failed",
            format!(
                "Inserted {sent} of {} input events; delivery is uncertain",
                events.len()
            ),
        )
        .uncertain());
    }
    Ok(())
}
fn pointer_events(x: f64, y: f64, kind: &ActionKind, scroll_y: Option<i32>) -> Result<Vec<INPUT>> {
    if !matches!(kind, ActionKind::Click | ActionKind::Scroll) {
        return Err(err("invalid_action", "Pointer event kind required"));
    }
    unsafe {
        let left = GetSystemMetrics(SM_XVIRTUALSCREEN);
        let top = GetSystemMetrics(SM_YVIRTUALSCREEN);
        let width = GetSystemMetrics(SM_CXVIRTUALSCREEN);
        let height = GetSystemMetrics(SM_CYVIRTUALSCREEN);
        if width <= 1 || height <= 1 {
            return Err(err(
                "geometry_invalid",
                "Virtual desktop dimensions invalid",
            ));
        }
        let dx = (((x - left as f64) * 65535.) / (width - 1) as f64).round() as i32;
        let dy = (((y - top as f64) * 65535.) / (height - 1) as f64).round() as i32;
        let event = |flags, data| INPUT {
            r#type: INPUT_MOUSE,
            Anonymous: INPUT_0 {
                mi: MOUSEINPUT {
                    dx,
                    dy,
                    mouseData: data,
                    dwFlags: flags,
                    time: 0,
                    dwExtraInfo: 0,
                },
            },
        };
        let mut events = vec![event(
            MOUSEEVENTF_MOVE | MOUSEEVENTF_ABSOLUTE | MOUSEEVENTF_VIRTUALDESK,
            0,
        )];
        if matches!(kind, ActionKind::Click) {
            events.push(event(MOUSEEVENTF_LEFTDOWN, 0));
            events.push(event(MOUSEEVENTF_LEFTUP, 0));
        } else {
            events.push(event(
                MOUSEEVENTF_WHEEL,
                (scroll_y.unwrap_or(0) * 120) as u32,
            ));
        }
        Ok(events)
    }
}

fn visual_recipient(window: &Window, x: f64, y: f64) -> Result<()> {
    let expected = hwnd(window.window_id)?;
    unsafe {
        if GetForegroundWindow() != expected {
            return Err(err(
                "foreground_mismatch",
                "Visual click requires the exact foreground window",
            ));
        }
        let hit = WindowFromPoint(POINT {
            x: x as i32,
            y: y as i32,
        });
        let mut pid = 0;
        GetWindowThreadProcessId(hit, Some(&mut pid));
        if hit.0.is_null() || pid != window.pid || GetAncestor(hit, GA_ROOT) != expected {
            return Err(err(
                "target_occluded",
                "Visual click would reach another application or window",
            ));
        }
    }
    Ok(())
}

fn visual_act(
    automation: &Automation,
    window: &Window,
    visual: &VisualMatch,
    anchor: &VisualAnchor,
    action: &Action,
    expected_state: &str,
    expected_nodes: &[Node],
) -> Result<Receipt> {
    let fresh = window_info(hwnd(window.window_id)?)?;
    if fresh.process_identity != window.process_identity
        || !fresh.bounds.near(window.bounds, 0.0)
        || fresh.scale != window.scale
        || !fresh.foreground
    {
        return Err(err(
            "window_changed",
            "Visual window identity, geometry, scale or foreground changed",
        ));
    }
    let (nodes, complete) = automation.tree(&fresh)?;
    if !complete {
        return Err(err(
            "tree_incomplete",
            "Visual action requires a complete observation",
        ));
    }
    validate_state(expected_nodes, &nodes, expected_state)?;
    let image = capture(&fresh)?;
    let (x, y) = revalidate_visual(&fresh, &image, anchor, visual, action)?;
    visual_recipient(&fresh, x, y)?;
    let events = pointer_events(x, y, &ActionKind::Click, None)?;
    let dispatched_at_ms = now_ms();
    send(&events[..1])?;
    let after_move = window_info(hwnd(window.window_id)?).map_err(DesktopError::uncertain)?;
    if after_move.process_identity != fresh.process_identity
        || !after_move.bounds.near(fresh.bounds, 0.0)
    {
        return Err(err(
            "window_changed",
            "Visual window changed after pointer movement",
        )
        .uncertain());
    }
    let image = capture(&after_move).map_err(DesktopError::uncertain)?;
    revalidate_visual(&after_move, &image, anchor, visual, action)
        .map_err(DesktopError::uncertain)?;
    visual_recipient(&after_move, x, y).map_err(DesktopError::uncertain)?;
    send(&events[1..])?;
    Ok(Receipt {
        action_id: action.action_id.clone(),
        delivery: Delivery::Sent,
        dispatched_at_ms,
        verified: false,
    })
}

fn act(
    automation: &Automation,
    window: &Window,
    target: &Node,
    action: &Action,
    expected_state: &str,
    expected_nodes: &[Node],
) -> Result<Receipt> {
    action.validate()?;
    let fresh = window_info(hwnd(window.window_id)?)?;
    if fresh.pid != window.pid
        || fresh.process_identity != window.process_identity
        || !fresh.bounds.near(window.bounds, 1.0)
    {
        return Err(err(
            "window_changed",
            "Attached window changed; observe again",
        ));
    }
    let (nodes, complete) = automation.tree(&fresh)?;
    if !complete {
        return Err(err(
            "tree_incomplete",
            "Cannot act on incomplete accessibility tree",
        ));
    }
    validate_state(expected_nodes, &nodes, expected_state)?;
    let element = automation.target(&fresh, target)?;
    let dispatched_at_ms = now_ms();
    unsafe {
        match action.kind {
            ActionKind::Press => element
                .GetCurrentPatternAs::<IUIAutomationInvokePattern>(UIA_InvokePatternId)
                .map_err(native)?
                .Invoke()
                .map_err(|e| native(e).uncertain())?,
            ActionKind::SetValue => {
                let text = BSTR::from(
                    action
                        .text
                        .as_deref()
                        .ok_or_else(|| err("invalid_action", "Text required"))?,
                );
                element
                    .GetCurrentPatternAs::<IUIAutomationValuePattern>(UIA_ValuePatternId)
                    .map_err(native)?
                    .SetValue(&text)
                    .map_err(|e| native(e).uncertain())?;
            }
            ActionKind::TypeText | ActionKind::Click | ActionKind::Scroll => {
                if !fresh.foreground || GetForegroundWindow() != hwnd(window.window_id)? {
                    return Err(err(
                        "foreground_mismatch",
                        "Physical input requires the exact foreground window",
                    ));
                }
                if matches!(action.kind, ActionKind::TypeText) {
                    if !element.CurrentHasKeyboardFocus().map_err(native)?.as_bool() {
                        return Err(err(
                            "focus_required",
                            "Typing requires the target field already focused",
                        ));
                    }
                    let text = action
                        .text
                        .as_deref()
                        .ok_or_else(|| err("invalid_action", "Text required"))?;
                    let mut events = Vec::new();
                    for unit in text.encode_utf16() {
                        for flags in [KEYEVENTF_UNICODE, KEYEVENTF_UNICODE | KEYEVENTF_KEYUP] {
                            events.push(INPUT {
                                r#type: INPUT_KEYBOARD,
                                Anonymous: INPUT_0 {
                                    ki: KEYBDINPUT {
                                        wVk: VIRTUAL_KEY(0),
                                        wScan: unit,
                                        dwFlags: flags,
                                        time: 0,
                                        dwExtraInfo: 0,
                                    },
                                },
                            });
                        }
                    }
                    send(&events)?;
                } else {
                    let b = target
                        .bounds
                        .ok_or_else(|| err("geometry_unavailable", "Target has no bounds"))?;
                    let x = action.x.unwrap_or(b.x + b.width / 2.);
                    let y = action.y.unwrap_or(b.y + b.height / 2.);
                    if !b.contains(x, y) || !fresh.bounds.contains(x, y) {
                        return Err(err("outside_target", "Physical point is outside target"));
                    }
                    let hit = automation
                        .client
                        .ElementFromPoint(POINT {
                            x: x as i32,
                            y: y as i32,
                        })
                        .map_err(native)?;
                    if !automation
                        .client
                        .CompareElements(&element, &hit)
                        .map_err(native)?
                        .as_bool()
                    {
                        return Err(err(
                            "target_occluded",
                            "Input hit test reaches another element",
                        ));
                    }
                    send(&pointer_events(x, y, &action.kind, action.scroll_y)?)?;
                }
            }
        }
    }
    Ok(Receipt {
        action_id: action.action_id.clone(),
        delivery: Delivery::Sent,
        dispatched_at_ms,
        verified: false,
    })
}

fn focus_window(window: &Window) -> Result<Window> {
    let fresh = window_info(hwnd(window.window_id)?)?;
    if fresh.process_identity != window.process_identity {
        return Err(err(
            "process_changed",
            "Application process identity changed",
        ));
    }
    if !unsafe { SetForegroundWindow(hwnd(window.window_id)?) }.as_bool() {
        return Err(err(
            "foreground_denied",
            "Windows declined foreground activation",
        ));
    }
    let focused = window_info(hwnd(window.window_id)?)?;
    if !focused.foreground {
        return Err(err(
            "foreground_denied",
            "Exact attached window did not become foreground",
        ));
    }
    Ok(focused)
}
pub fn execute(request: WorkerRequest) -> Result<Value> {
    unsafe {
        SetThreadDpiAwarenessContext(DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2);
    }
    if request.op == "probe" {
        let _automation = Automation::new()?;
        return Ok(
            json!({"protocol_version":1,"platform":"windows","architecture":std::env::consts::ARCH,"accessibility":{"backend":"uia","state":"available"},"screen_capture":{"backend":"gdi_print_window","scope":"window","state":"not_tested"},"external_input":{"implementation":"unavailable","continuous":false,"qualification":"not_tested"},"qualification":"not_tested"}),
        );
    }
    let automation = Automation::new()?;
    match request.op.as_str() {
        "attach" => {
            let payload = request
                .attach
                .ok_or_else(|| err("invalid_request", "Attach payload required"))?;
            let window = attach(&payload)?;
            automation.root(&window)?;
            let window = if payload.focus {
                focus_window(&window)?
            } else {
                window
            };
            serde_json::to_value(window).map_err(|e| err("serialization", e.to_string()))
        }
        "focus" => {
            let window = request
                .window
                .ok_or_else(|| err("invalid_request", "Window required"))?;
            serde_json::to_value(focus_window(&window)?)
                .map_err(|e| err("serialization", e.to_string()))
        }

        "observe" => {
            let expected = request
                .window
                .ok_or_else(|| err("invalid_request", "Window required"))?;
            let window = window_info(hwnd(expected.window_id)?)?;
            if window.process_identity != expected.process_identity {
                return Err(err(
                    "process_changed",
                    "Application process identity changed",
                ));
            }
            let tree_at_ms = now_ms();
            let (nodes, complete) = automation.tree(&window)?;
            let screenshot = capture(&window)?;
            let after = window_info(hwnd(window.window_id)?)?;
            if !after.bounds.near(window.bounds, 1.0) {
                return Err(err(
                    "observation_inconsistent",
                    "Window moved during capture",
                ));
            }
            serde_json::to_value(Observation {
                observation_id: String::new(),
                epoch: 0,
                captured_at_ms: now_ms(),
                tree_at_ms,
                window,
                nodes,
                screenshot,
                complete,
                input_stamp: None,
                truncated_reason: if complete {
                    None
                } else {
                    Some("node_or_depth_limit".into())
                },
            })
            .map_err(|e| err("serialization", e.to_string()))
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
                ResolvedTarget::Semantic { node } => act(
                    &automation,
                    &window,
                    &node,
                    &action,
                    &expected_state,
                    &expected_nodes,
                )?,
                ResolvedTarget::Visual { visual } => visual_act(
                    &automation,
                    &window,
                    &visual,
                    &request
                        .visual_anchor
                        .ok_or_else(|| err("invalid_request", "Visual anchor required"))?,
                    &action,
                    &expected_state,
                    &expected_nodes,
                )?,
            };
            serde_json::to_value(receipt).map_err(|e| err("serialization", e.to_string()))
        }
        _ => Err(err("unknown_operation", "Unknown native helper operation")),
    }
}
