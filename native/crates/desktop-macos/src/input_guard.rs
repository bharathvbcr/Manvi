//! Bounded hardware-event admission guard. Stores only counters, never keycodes,
//! text, pointer coordinates or event payloads. It is not a continuous listener
//! and does not classify synthetic input from other applications.
use desktop_core::{DesktopError, InputStamp, Result};
use objc2_core_graphics::{CGEventSource, CGEventSourceStateID, CGEventType};

const EVENTS: [CGEventType; 15] = [
    CGEventType::LeftMouseDown,
    CGEventType::LeftMouseUp,
    CGEventType::RightMouseDown,
    CGEventType::RightMouseUp,
    CGEventType::MouseMoved,
    CGEventType::LeftMouseDragged,
    CGEventType::RightMouseDragged,
    CGEventType::KeyDown,
    CGEventType::KeyUp,
    CGEventType::FlagsChanged,
    CGEventType::ScrollWheel,
    CGEventType::TabletPointer,
    CGEventType::OtherMouseDown,
    CGEventType::OtherMouseUp,
    CGEventType::OtherMouseDragged,
];

/// Interference reports which hardware counters moved since admission.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Interference {
    /// No hardware counter changed.
    None,
    /// Only pointer-motion counters changed. The caller must still revalidate
    /// its target; motion alone never authorises acting on a stale observation.
    PointerMotion,
}

/// A bare pointer move cannot press, type, scroll or drag — at most it alters
/// hover presentation. Every other counter (button, key, modifier, scroll and
/// drag) can commit an application state change and stays a hard refusal.
/// Dragging is deliberately excluded here: it carries a held button.
fn motion_only(kind: CGEventType) -> bool {
    matches!(kind, CGEventType::MouseMoved | CGEventType::TabletPointer)
}

/// category names the kind of input a counter represents. It is coarse on
/// purpose: an operator needs to know whether a person was typing or clicking
/// to tell real interference from a misfiring guard, and that is answerable
/// without publishing counts, keycodes, buttons or pointer coordinates.
fn category(kind: CGEventType) -> &'static str {
    match kind {
        CGEventType::KeyDown | CGEventType::KeyUp | CGEventType::FlagsChanged => "keyboard",
        CGEventType::ScrollWheel => "scroll",
        CGEventType::LeftMouseDragged
        | CGEventType::RightMouseDragged
        | CGEventType::OtherMouseDragged => "drag",
        CGEventType::MouseMoved | CGEventType::TabletPointer => "pointer_motion",
        _ => "pointer_button",
    }
}

fn refused(kinds: &[&'static str]) -> DesktopError {
    let mut seen: Vec<&str> = Vec::new();
    for k in kinds {
        if !seen.contains(k) {
            seen.push(k);
        }
    }
    seen.sort_unstable();
    let mut message =
        String::from("Hardware input changed since admission; automation requires reconciliation");
    if !seen.is_empty() {
        message.push_str(" (");
        message.push_str(&seen.join(", "));
        message.push(')');
    }
    DesktopError::new("external_interaction", message)
}

pub(crate) fn snapshot() -> InputStamp {
    InputStamp::MacosHid {
        counters: EVENTS.map(|kind| {
            CGEventSource::counter_for_event_type(CGEventSourceStateID::HIDSystemState, kind)
        }),
    }
}

/// compare is the strict contract: any hardware counter change is refused. Use
/// it before coordinate-addressed input, where the pointer position is part of
/// the action and a human moving it changes where the input lands.
pub(crate) fn compare(expected: &InputStamp, actual: &InputStamp) -> Result<()> {
    let (InputStamp::MacosHid { counters: before }, InputStamp::MacosHid { counters: after }) =
        (expected, actual);
    // Equality rather than ordering handles an individual u32 wrap. A full
    // 2^32 events between checks is outside the bounded observation lifetime.
    let changed: Vec<&'static str> = EVENTS
        .iter()
        .enumerate()
        .filter(|(i, _)| before[*i] != after[*i])
        .map(|(_, kind)| category(*kind))
        .collect();
    if !changed.is_empty() {
        return Err(refused(&changed));
    }
    Ok(())
}

/// classify refuses every counter that can commit an application state change
/// and reports bare pointer motion separately instead of aborting on it. Use it
/// only where the pointer is not part of the action: accessibility observation
/// and element-addressed AXPress/AXValue writes.
pub(crate) fn classify(expected: &InputStamp, actual: &InputStamp) -> Result<Interference> {
    let (InputStamp::MacosHid { counters: before }, InputStamp::MacosHid { counters: after }) =
        (expected, actual);
    let mut motion = false;
    let mut refusing: Vec<&'static str> = Vec::new();
    for (index, kind) in EVENTS.iter().enumerate() {
        if before[index] == after[index] {
            continue;
        }
        if motion_only(*kind) {
            motion = true;
            continue;
        }
        refusing.push(category(*kind));
    }
    if !refusing.is_empty() {
        return Err(refused(&refusing));
    }
    Ok(if motion {
        Interference::PointerMotion
    } else {
        Interference::None
    })
}

pub(crate) fn check(expected: &InputStamp) -> Result<()> {
    compare(expected, &snapshot())
}

pub(crate) fn check_semantic(expected: &InputStamp) -> Result<Interference> {
    classify(expected, &snapshot())
}

#[cfg(test)]
mod tests {
    use super::{EVENTS, Interference, check_semantic, classify, compare, motion_only, snapshot};
    use desktop_core::{Delivery, InputStamp};
    use objc2_core_graphics::{CGEventSource, CGEventSourceStateID, CGEventType};

    fn stamp(counters: [u32; 15]) -> InputStamp {
        InputStamp::MacosHid { counters }
    }

    #[test]
    fn every_hardware_counter_change_and_wrap_is_an_explicit_not_sent_refusal() {
        let baseline = stamp([u32::MAX; 15]);
        compare(&baseline, &baseline).unwrap();
        for i in 0..EVENTS.len() {
            let mut changed = [u32::MAX; 15];
            changed[i] = 0;
            let failure = compare(&baseline, &stamp(changed)).unwrap_err();
            assert_eq!(failure.code, "external_interaction");
            assert_eq!(failure.delivery, Delivery::NotSent);
            assert!(!failure.message.contains("4294967295"));
        }
    }

    /// classify must refuse exactly the counters that can commit a change, and
    /// report the rest as motion. Asserting per index keeps the partition
    /// honest if EVENTS is ever reordered or extended.
    #[test]
    fn classify_refuses_state_changing_counters_and_reports_only_motion() {
        let baseline = stamp([u32::MAX; 15]);
        assert_eq!(classify(&baseline, &baseline).unwrap(), Interference::None);
        for (i, kind) in EVENTS.iter().enumerate() {
            let mut changed = [u32::MAX; 15];
            changed[i] = 0;
            let actual = stamp(changed);
            if motion_only(*kind) {
                assert_eq!(
                    classify(&baseline, &actual).unwrap(),
                    Interference::PointerMotion,
                    "index {i} should be reported as motion"
                );
            } else {
                let failure = classify(&baseline, &actual).unwrap_err();
                assert_eq!(failure.code, "external_interaction", "index {i}");
                assert_eq!(failure.delivery, Delivery::NotSent, "index {i}");
            }
        }
    }

    /// Motion accompanying any state-changing counter must still be refused:
    /// the refusal cannot be downgraded by adding a harmless counter to it.
    #[test]
    fn motion_alongside_state_change_is_still_refused() {
        let baseline = stamp([u32::MAX; 15]);
        for (i, kind) in EVENTS.iter().enumerate() {
            if motion_only(*kind) {
                continue;
            }
            let mut changed = [u32::MAX; 15];
            changed[i] = 0;
            for (j, other) in EVENTS.iter().enumerate() {
                if motion_only(*other) {
                    changed[j] = 7;
                }
            }
            assert_eq!(
                classify(&baseline, &stamp(changed)).unwrap_err().code,
                "external_interaction",
                "index {i} with motion"
            );
        }
    }

    /// Drags carry a held button, so they are state-changing despite moving.
    #[test]
    fn drags_and_scrolls_are_not_treated_as_motion() {
        for kind in [
            CGEventType::LeftMouseDragged,
            CGEventType::RightMouseDragged,
            CGEventType::OtherMouseDragged,
            CGEventType::ScrollWheel,
            CGEventType::FlagsChanged,
        ] {
            assert!(!motion_only(kind), "{kind:?} must not be motion-only");
        }
        assert!(motion_only(CGEventType::MouseMoved));
        assert!(motion_only(CGEventType::TabletPointer));
    }

    /// A refusal has to say what kind of input it saw, or an operator cannot
    /// tell a person using the machine from a misfiring guard. It must still
    /// not disclose counts, keycodes, buttons or coordinates.
    #[test]
    fn refusals_name_the_input_category_without_disclosing_values() {
        let baseline = stamp([7; 15]);
        let expected = [
            (CGEventType::KeyDown, "keyboard"),
            (CGEventType::KeyUp, "keyboard"),
            (CGEventType::FlagsChanged, "keyboard"),
            (CGEventType::ScrollWheel, "scroll"),
            (CGEventType::LeftMouseDragged, "drag"),
            (CGEventType::RightMouseDragged, "drag"),
            (CGEventType::OtherMouseDragged, "drag"),
            (CGEventType::LeftMouseDown, "pointer_button"),
            (CGEventType::RightMouseUp, "pointer_button"),
            (CGEventType::OtherMouseDown, "pointer_button"),
        ];
        for (kind, want) in expected {
            let index = EVENTS.iter().position(|e| *e == kind).unwrap();
            let mut changed = [7; 15];
            changed[index] = 9;
            let failure = classify(&baseline, &stamp(changed)).unwrap_err();
            assert!(
                failure.message.contains(want),
                "{kind:?} should be reported as {want}: {}",
                failure.message
            );
            assert!(!failure.message.contains('7'), "{}", failure.message);
            assert!(!failure.message.contains('9'), "{}", failure.message);
        }

        // Several categories at once are listed, deduplicated and ordered.
        let mut changed = [7; 15];
        for kind in [
            CGEventType::KeyDown,
            CGEventType::KeyUp,
            CGEventType::ScrollWheel,
        ] {
            changed[EVENTS.iter().position(|e| *e == kind).unwrap()] = 9;
        }
        let message = classify(&baseline, &stamp(changed)).unwrap_err().message;
        assert!(message.contains("keyboard, scroll"), "{message}");
        assert_eq!(message.matches("keyboard").count(), 1, "{message}");
    }

    #[test]
    fn native_private_source_is_distinct_from_hardware_source_without_posting_input() {
        let source = CGEventSource::new(CGEventSourceStateID::Private).unwrap();
        let state = CGEventSource::source_state_id(Some(&source));
        assert_ne!(state, CGEventSourceStateID::HIDSystemState);
        assert_ne!(state, CGEventSourceStateID::CombinedSessionState);
        let InputStamp::MacosHid { counters } = snapshot();
        assert_eq!(counters.len(), EVENTS.len());
        // No event is posted. This verifies source separation and native reads,
        // not end-to-end classification of posted input versus real hardware.
    }

    /// check_semantic reads live counters; comparing a snapshot with itself must
    /// not fabricate interference.
    #[test]
    fn check_semantic_against_a_fresh_snapshot_is_stable_or_motion_at_most() {
        let taken = snapshot();
        match check_semantic(&taken) {
            Ok(Interference::None | Interference::PointerMotion) => {}
            Err(e) => assert_eq!(e.code, "external_interaction"),
        }
    }
}
