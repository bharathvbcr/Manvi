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

pub(crate) fn snapshot() -> InputStamp {
    InputStamp::MacosHid {
        counters: EVENTS.map(|kind| {
            CGEventSource::counter_for_event_type(CGEventSourceStateID::HIDSystemState, kind)
        }),
    }
}

pub(crate) fn compare(expected: &InputStamp, actual: &InputStamp) -> Result<()> {
    let (InputStamp::MacosHid { counters: before }, InputStamp::MacosHid { counters: after }) =
        (expected, actual);
    // Equality rather than ordering handles an individual u32 wrap. A full
    // 2^32 events between checks is outside the bounded observation lifetime.
    if before != after {
        return Err(DesktopError::new(
            "external_interaction",
            "Hardware input changed since admission; automation requires reconciliation",
        ));
    }
    Ok(())
}

pub(crate) fn check(expected: &InputStamp) -> Result<()> {
    compare(expected, &snapshot())
}

#[cfg(test)]
mod tests {
    use super::{EVENTS, compare, snapshot};
    use desktop_core::{Delivery, InputStamp};
    use objc2_core_graphics::{CGEventSource, CGEventSourceStateID};

    #[test]
    fn every_hardware_counter_change_and_wrap_is_an_explicit_not_sent_refusal() {
        let baseline = InputStamp::MacosHid {
            counters: [u32::MAX; 15],
        };
        compare(&baseline, &baseline).unwrap();
        for i in 0..EVENTS.len() {
            let mut changed = [u32::MAX; 15];
            changed[i] = 0;
            let failure =
                compare(&baseline, &InputStamp::MacosHid { counters: changed }).unwrap_err();
            assert_eq!(failure.code, "external_interaction");
            assert_eq!(failure.delivery, Delivery::NotSent);
            assert!(!failure.message.contains("4294967295"));
        }
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
}
