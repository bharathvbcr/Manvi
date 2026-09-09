# Linux adapter

This adapter requires an X11 desktop, the selected process's AT-SPI application,
and an unobscured application window. Wayland is refused. An untitled AT-SPI
Frame/Window is accepted only when its bus owner PID and geometry exactly match
the sole visible X11 window for that PID. A contradictory nonempty title is
refused; geometry tolerance is not widened to make association succeed.

AccessKit 0.21 activates its AT-SPI adapter when `org.a11y.Status.ScreenReaderEnabled`
is true. `IsEnabled` alone does not activate it. `probe` reports both values and
does not change desktop preferences. Missing registration with the screen reader
enabled returns `accessibility_pending`; a missing process with that property
false returns `accessibility_inactive`. The Jarvis isolated XFCE VM seed enables
this property and uses the verified Virtual-1 1440x900 mode. The original
1280x800 display clipped the scaled bank into the bottom panel; capture correctly
refused that overlapping window.

AT-SPI EditableText is preferred. Editable Text controls without that interface
(including the current AccessKit adapter) use XTest through the existing keymap.
The complete text is mapped before input. It is limited to 256 non-control
characters and refuses characters absent from the existing unshifted/shifted
mapping. There is no clipboard access, keymap replacement, or layout switch.
Replacement uses Control+A; empty replacement sends Backspace. Before each
stroke the adapter checks focused AT-SPI identity and geometry, exact foreground
X11 keyboard recipient, visibility, unchanged keymap, and absence of held user
keys or modifiers. Release events are queued before awaiting acknowledgements.
Any failure after a physical stroke is delivery-unknown and cannot be retried.
Physical input cannot be atomic with independent desktop users/processes.

Visual targets use core's exact, bounded RGBA matcher and an approved click only.
The adapter captures and revalidates before moving the pointer, then captures and
revalidates again before pressing the button. It requires the exact attached X11
window as the native point recipient. A hover-induced pixel change is refused;
any failure after pointer movement is delivery-unknown.

`cargo test --locked -p desktop-linux` runs pure mapping/root invariants on Linux.
The ignored `diagnostic_selected_window_roots` test requires
`MANVI_LINUX_DIAGNOSTIC_PID` for an explicitly owned synthetic window. It reads
the actual registry, window role/title/geometry, and occlusion state without
dispatching input. Run with `-- --ignored --nocapture` in that desktop session.
Completed Go workflows, verifier acceptance, and account-state oracles must be
reported separately; a passing unit test is not native qualification.
