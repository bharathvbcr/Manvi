//! Platform-neutral ordering of one observation and of the attach settle wait.
//!
//! The order in which a backend enumerates windows, walks the accessibility
//! tree and takes the image is what the privacy masking depends on: the image
//! must be bracketed by trees that are equal, so a node's bounds describe the
//! pixels that were captured. That order used to live inline in the macOS
//! backend, where it can only be exercised against a live screen. It lives here
//! so it is exercised with a scripted source instead; a backend only supplies
//! the three primitive reads.
use crate::{Node, Result, Screenshot, Window, err, state_difference, state_fingerprint};

/// Pause between the first tree walk and the image. Long enough that an
/// asynchronous repaint triggered by the walk has landed before the pixels are
/// taken, short enough not to dominate an observation.
pub const OBSERVE_SETTLE_MS: u64 = 150;

/// One accessibility walk: the nodes, whether the walk reached every node
/// within the node and depth limits, and whether the window was the foreground
/// window when it ended.
#[derive(Debug, Clone)]
pub struct TreeRead {
    pub nodes: Vec<Node>,
    pub complete: bool,
    pub foreground: bool,
}

/// The three primitive reads a backend supplies, plus a clock. Nothing here
/// retains a read: the flow keeps only what it returns.
pub trait CaptureSource {
    /// One window enumeration; metadata only, no image.
    fn window(&mut self) -> Result<Window>;
    /// One accessibility walk of `window`.
    fn tree(&mut self, window: &Window) -> Result<TreeRead>;
    /// One window enumeration plus one image of the window it selected.
    fn image(&mut self, window: &Window) -> Result<(Window, Screenshot)>;
    fn now_ms(&self) -> u64;
    fn pause_ms(&mut self, ms: u64);
}

/// An observation whose tree and image passed the bracketing checks.
#[derive(Debug)]
pub struct BracketedObservation {
    pub window: Window,
    pub nodes: Vec<Node>,
    pub complete: bool,
    pub tree_at_ms: u64,
    pub captured_at_ms: u64,
    pub screenshot: Screenshot,
}

fn same_tree(a: &TreeRead, b: &TreeRead) -> Result<bool> {
    Ok(a.complete == b.complete && state_fingerprint(&a.nodes)? == state_fingerprint(&b.nodes)?)
}

/// Observe once: window metadata, tree, pause, image, tree.
///
/// Two window enumerations (one for the metadata the walk needs, one inside the
/// image capture), two walks and one image. The image that is returned is
/// bracketed by two equal walks at least [`OBSERVE_SETTLE_MS`] apart, so the
/// bounds of every node describe the pixels that were taken. No image is taken
/// that is not returned.
pub fn observe_bracketed<S: CaptureSource>(
    src: &mut S,
    expected_identity: &str,
) -> Result<BracketedObservation> {
    let mut window = src.window()?;
    if window.process_identity != expected_identity {
        return Err(err(
            "process_changed",
            "Application process identity changed",
        ));
    }
    let tree_at_ms = src.now_ms();
    let first = src.tree(&window)?;
    window.foreground = first.foreground;
    src.pause_ms(OBSERVE_SETTLE_MS);
    let (captured, screenshot) = src.image(&window)?;
    if captured.process_identity != window.process_identity
        || !captured.bounds.near(window.bounds, 0.0)
        || captured.scale != window.scale
    {
        return Err(err(
            "observation_inconsistent",
            "Window geometry changed between accessibility and image capture",
        ));
    }
    let stable = src.tree(&window)?;
    if !same_tree(&first, &stable)? {
        return Err(err(
            "observation_inconsistent",
            format!(
                "Accessibility state changed between two scoped captures: {}",
                state_difference(&first.nodes, &stable.nodes)
            ),
        ));
    }
    window.foreground = stable.foreground;
    Ok(BracketedObservation {
        window,
        nodes: first.nodes,
        complete: first.complete,
        tree_at_ms,
        captured_at_ms: src.now_ms(),
        screenshot,
    })
}

/// How long and how often attach re-reads the tree before it calls the target
/// ready.
#[derive(Debug, Clone, Copy)]
pub struct SettlePolicy {
    pub interval_ms: u64,
    pub deadline_ms: u64,
}

/// Attach readiness: the target's tree is read until two reads
/// [`SettlePolicy::interval_ms`] apart are equal (same completeness, same state
/// fingerprint), and that stable read is returned. Tree walks only: no window
/// enumeration, no image. A tree that is still changing when
/// [`SettlePolicy::deadline_ms`] passes is reported as `accessibility_pending`
/// (undelivered, so the host's bounded attach retry applies). Completeness is
/// part of the comparison, not a precondition: two reads cut at the same node
/// limit compare equal, and that stable read is returned with `complete` false. Every walk re-checks the process and window
/// identity, so a window that changed identity fails instead of settling.
/// `focus_raised` only words the refusal; it does not change the reads.
pub fn settle_attach<S: CaptureSource>(
    src: &mut S,
    window: &Window,
    policy: SettlePolicy,
    focus_raised: bool,
) -> Result<TreeRead> {
    let started = src.now_ms();
    let mut previous = src.tree(window)?;
    loop {
        src.pause_ms(policy.interval_ms);
        let current = src.tree(window)?;
        if same_tree(&previous, &current)? {
            return Ok(current);
        }
        // The deadline is checked only after a comparison. A tree whose walk
        // alone takes longer than the deadline is stable, not unsettled, and
        // checking first refused it on every retry without ever comparing.
        if src.now_ms().saturating_sub(started) >= policy.deadline_ms {
            // Settling follows the focus step on purpose (an unfocused window's
            // tree is not the tree that will be observed), so a refusal here
            // can come after the window was already raised. Still undelivered:
            // no input was sent; the host's bounded retry applies.
            return Err(err(
                "accessibility_pending",
                if focus_raised {
                    "The accessibility tree was still changing when attach checked it; \
                     the window focus had already been raised by this attempt"
                } else {
                    "The accessibility tree was still changing when attach checked it"
                },
            ));
        }
        previous = current;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::Bounds;
    use std::collections::VecDeque;

    fn node(path: &[u32], name: &str) -> Node {
        Node {
            id: format!("n{}", path.len()),
            role: "button".into(),
            name: name.into(),
            value: None,
            identifier: None,
            parent: None,
            bounds: None,
            enabled: true,
            editable: false,
            actions: vec![],
            native_path: path.to_vec(),
        }
    }
    fn nodes(count: usize) -> Vec<Node> {
        (0..count).map(|i| node(&[i as u32], "item")).collect()
    }
    fn window() -> Window {
        Window {
            pid: 7,
            window_id: 9,
            title: "w".into(),
            process_identity: "7:1:2:/x".into(),
            bounds: Bounds {
                x: 0.0,
                y: 0.0,
                width: 100.0,
                height: 50.0,
            },
            foreground: false,
            scale: 2.0,
        }
    }
    fn shot() -> Screenshot {
        Screenshot {
            mime_type: "image/png".into(),
            base64: "AAAA".into(),
            width: 200,
            height: 100,
        }
    }
    fn read(count: usize) -> TreeRead {
        TreeRead {
            nodes: nodes(count),
            complete: true,
            foreground: true,
        }
    }

    /// Scripted source: every primitive is counted; the last scripted tree
    /// repeats. A walk costs `walk_cost_ms` on the fake clock.
    struct Fake {
        trees: VecDeque<TreeRead>,
        last: Option<TreeRead>,
        enumerations: usize,
        walks: usize,
        images: usize,
        pauses: Vec<u64>,
        log: Vec<String>,
        clock: u64,
        walk_cost_ms: u64,
        image_window: Window,
    }
    impl Fake {
        fn new(trees: Vec<TreeRead>) -> Self {
            Self {
                trees: trees.into(),
                last: None,
                enumerations: 0,
                walks: 0,
                images: 0,
                pauses: vec![],
                log: vec![],
                clock: 1_000,
                walk_cost_ms: 0,
                image_window: window(),
            }
        }
    }
    impl CaptureSource for Fake {
        fn window(&mut self) -> Result<Window> {
            self.log.push("window".into());
            self.enumerations += 1;
            Ok(window())
        }
        fn tree(&mut self, _window: &Window) -> Result<TreeRead> {
            self.log.push("tree".into());
            self.walks += 1;
            self.clock += self.walk_cost_ms;
            if let Some(next) = self.trees.pop_front() {
                self.last = Some(next);
            }
            Ok(self.last.clone().expect("script has a tree"))
        }
        fn image(&mut self, _window: &Window) -> Result<(Window, Screenshot)> {
            self.log.push("image".into());
            self.enumerations += 1;
            self.images += 1;
            Ok((self.image_window.clone(), shot()))
        }
        fn now_ms(&self) -> u64 {
            self.clock
        }
        fn pause_ms(&mut self, ms: u64) {
            self.log.push(format!("pause:{ms}"));
            self.pauses.push(ms);
            self.clock += ms;
        }
    }

    #[test]
    fn observe_reads_in_exactly_this_order() {
        // The image sits between the two walks and after the pause; moving it
        // outside them, or pausing after it, breaks what masking relies on.
        let mut src = Fake::new(vec![read(20)]);
        observe_bracketed(&mut src, "7:1:2:/x").unwrap();
        assert_eq!(
            src.log,
            ["window", "tree", "pause:150", "image", "tree"],
            "window read, walk, pause, window read and image, walk"
        );
    }

    #[test]
    fn attach_settle_reads_in_exactly_this_order() {
        let mut src = Fake::new(vec![read(19), read(20), read(20)]);
        settle_attach(&mut src, &window(), POLICY, false).unwrap();
        assert_eq!(
            src.log,
            ["tree", "pause:150", "tree", "pause:150", "tree"],
            "walks only, one pause between each pair"
        );
    }

    #[test]
    fn an_unsettled_attach_after_focus_says_that_focus_had_moved() {
        let growing: Vec<_> = (1..200).map(read).collect();
        let mut src = Fake::new(growing.clone());
        let plain = settle_attach(&mut src, &window(), POLICY, false).unwrap_err();
        assert!(!plain.message.contains("focus"), "{}", plain.message);
        let mut src = Fake::new(growing);
        let raised = settle_attach(&mut src, &window(), POLICY, true).unwrap_err();
        assert_eq!(raised.code, "accessibility_pending");
        assert_eq!(raised.delivery, crate::Delivery::NotSent);
        assert!(
            raised.message.contains("already been raised"),
            "{}",
            raised.message
        );
    }

    const POLICY: SettlePolicy = SettlePolicy {
        interval_ms: 150,
        deadline_ms: 3_000,
    };

    #[test]
    fn one_observe_costs_two_enumerations_one_image_and_two_walks() {
        let mut src = Fake::new(vec![read(20)]);
        let observed = observe_bracketed(&mut src, "7:1:2:/x").unwrap();
        assert_eq!(observed.nodes.len(), 20);
        assert_eq!(src.images, 1, "an image that is not returned is not taken");
        assert_eq!(src.enumerations, 2);
        assert_eq!(src.walks, 2);
        assert_eq!(src.pauses, vec![OBSERVE_SETTLE_MS]);
    }

    #[test]
    fn a_tree_that_changes_across_the_image_is_refused() {
        // The recorded failure shape: 19 nodes, then 20.
        let mut src = Fake::new(vec![read(19), read(20)]);
        let failure = observe_bracketed(&mut src, "7:1:2:/x").unwrap_err();
        assert_eq!(failure.code, "observation_inconsistent");
    }

    #[test]
    fn moved_window_and_changed_scale_and_process_are_refused() {
        let mut src = Fake::new(vec![read(3)]);
        src.image_window.bounds.x += 1.0;
        assert_eq!(
            observe_bracketed(&mut src, "7:1:2:/x").unwrap_err().code,
            "observation_inconsistent"
        );
        let mut src = Fake::new(vec![read(3)]);
        src.image_window.scale = 1.0;
        assert_eq!(
            observe_bracketed(&mut src, "7:1:2:/x").unwrap_err().code,
            "observation_inconsistent"
        );
        let mut src = Fake::new(vec![read(3)]);
        assert_eq!(
            observe_bracketed(&mut src, "other").unwrap_err().code,
            "process_changed"
        );
        assert_eq!(src.images, 0, "identity is checked before any image");
    }

    #[test]
    fn attach_waits_out_a_tree_that_is_still_growing() {
        // The recorded first-observation failure: 19 nodes, then 20.
        let mut src = Fake::new(vec![read(19), read(20), read(20)]);
        let ready = settle_attach(&mut src, &window(), POLICY, false).unwrap();
        assert_eq!(
            ready.nodes.len(),
            20,
            "the stable tree, not the partial one"
        );
        assert_eq!(src.walks, 3);
        assert_eq!(src.images, 0, "attach settling reads trees only");
        assert_eq!(src.enumerations, 0);
    }

    #[test]
    fn attach_reports_a_tree_that_never_settles_as_pending() {
        let growing: Vec<_> = (1..200).map(read).collect();
        let mut src = Fake::new(growing);
        src.walk_cost_ms = 40;
        let failure = settle_attach(&mut src, &window(), POLICY, false).unwrap_err();
        assert_eq!(failure.code, "accessibility_pending");
        assert_eq!(failure.delivery, crate::Delivery::NotSent);
        assert!(
            src.clock - 1_000 <= POLICY.deadline_ms + POLICY.interval_ms + 40,
            "wait is bounded: {} ms",
            src.clock - 1_000
        );
    }

    #[test]
    fn attach_accepts_a_stable_tree_whose_walks_outlast_the_deadline() {
        // A slow window: each walk alone costs more than the whole deadline.
        // The tree is stable, so one comparison must still happen and pass; a
        // deadline checked before the first comparison refused this every time.
        let mut src = Fake::new(vec![read(5)]);
        src.walk_cost_ms = POLICY.deadline_ms + 500;
        let ready = settle_attach(&mut src, &window(), POLICY, false).unwrap();
        assert_eq!(ready.nodes.len(), 5);
        assert_eq!(src.walks, 2, "exactly one comparison walk");
    }

    #[test]
    fn a_slow_unsettled_tree_is_refused_after_one_comparison() {
        let growing: Vec<_> = (1..200).map(read).collect();
        let mut src = Fake::new(growing);
        src.walk_cost_ms = POLICY.deadline_ms + 500;
        let failure = settle_attach(&mut src, &window(), POLICY, false).unwrap_err();
        assert_eq!(failure.code, "accessibility_pending");
        assert_eq!(src.walks, 2, "one comparison, then the deadline");
    }

    #[test]
    fn attach_accepts_an_already_stable_tree_after_one_interval() {
        let mut src = Fake::new(vec![read(5)]);
        let ready = settle_attach(&mut src, &window(), POLICY, false).unwrap();
        assert_eq!(ready.nodes.len(), 5);
        assert_eq!(src.walks, 2);
        assert_eq!(src.pauses, vec![POLICY.interval_ms]);
    }

    #[test]
    fn attach_never_calls_a_truncated_to_complete_change_stable() {
        let mut partial = read(5);
        partial.complete = false;
        let mut src = Fake::new(vec![read(5), partial.clone(), partial]);
        let ready = settle_attach(&mut src, &window(), POLICY, false).unwrap();
        assert!(!ready.complete, "completeness comes from the stable pair");
        assert_eq!(src.walks, 3);
    }
}
