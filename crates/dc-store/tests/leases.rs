//! Lease semantics, tested against the behaviours an agent branches on.
//!
//! The concurrency test drives real OS threads against one file rather than one
//! connection, because a shared connection would serialise the writes itself
//! and prove something easier than what actually happens when a builder, a
//! watcher and a developer's own CLI all reach for the same task.

use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::{Arc, Barrier};

use dc_store::{AcquireRequest, LeaseCode, Store, StoreError, format_iso_utc, parse_iso_utc};

fn req(task: &str, owner: &str) -> AcquireRequest {
    AcquireRequest {
        task_id: task.to_string(),
        owner: owner.to_string(),
        ttl_seconds: Some(900),
        ..Default::default()
    }
}

#[test]
fn acquire_then_validate_then_release() {
    let store = Store::open_in_memory().unwrap();
    let lease = store.acquire(&req("TASK-001", "builder-1")).unwrap();

    assert!(store.validate("TASK-001", &lease.token).unwrap());
    assert_eq!(
        store.diagnose("TASK-001", &lease.token).unwrap(),
        LeaseCode::Valid
    );

    assert!(store.release("TASK-001", &lease.token).unwrap());
    assert!(store.active_lease("TASK-001").unwrap().is_none());
    // The token is now recoverable-expired rather than unknown: the agent is
    // told to check out again, not that it invented a token.
    assert_eq!(
        store.diagnose("TASK-001", &lease.token).unwrap(),
        LeaseCode::Expired
    );
}

#[test]
fn a_second_holder_is_refused_with_the_holders_name() {
    let store = Store::open_in_memory().unwrap();
    store.acquire(&req("TASK-001", "builder-1")).unwrap();

    match store.acquire(&req("TASK-001", "builder-2")) {
        Err(StoreError::LeaseHeld { task_id, holder }) => {
            assert_eq!(task_id, "TASK-001");
            assert_eq!(holder, "builder-1");
        }
        other => panic!("expected LeaseHeld, got {other:?}"),
    }
}

#[test]
fn another_agents_token_diagnoses_as_held_by_other() {
    let store = Store::open_in_memory().unwrap();
    store.acquire(&req("TASK-001", "builder-1")).unwrap();

    let code = store.diagnose("TASK-001", "some-other-token").unwrap();
    assert_eq!(code, LeaseCode::HeldByOther);
    assert_eq!(
        code.recovery(),
        Some(("pick_other_task", "devcouncil_next_task"))
    );
}

#[test]
fn an_unknown_token_is_invalid_not_expired() {
    let store = Store::open_in_memory().unwrap();
    assert_eq!(
        store.diagnose("TASK-404", "never-issued").unwrap(),
        LeaseCode::Invalid
    );
}

#[test]
fn expiry_is_observed_on_read_and_frees_the_task() {
    let mut store = Store::open_in_memory().unwrap();
    let clock = Arc::new(AtomicI64::new(1_700_000_000));
    let handle = Arc::clone(&clock);
    store.set_clock(move || handle.load(Ordering::SeqCst));

    let lease = store
        .acquire(&AcquireRequest {
            ttl_seconds: Some(60),
            ..req("TASK-001", "builder-1")
        })
        .unwrap();
    assert!(store.validate("TASK-001", &lease.token).unwrap());

    clock.fetch_add(61, Ordering::SeqCst);

    assert!(store.active_lease("TASK-001").unwrap().is_none());
    assert_eq!(
        store.diagnose("TASK-001", &lease.token).unwrap(),
        LeaseCode::Expired
    );
    // And the task is genuinely free, not merely reported free.
    store.acquire(&req("TASK-001", "builder-2")).unwrap();
}

#[test]
fn renew_extends_a_live_lease_and_refuses_a_dead_one() {
    let mut store = Store::open_in_memory().unwrap();
    let clock = Arc::new(AtomicI64::new(1_700_000_000));
    let handle = Arc::clone(&clock);
    store.set_clock(move || handle.load(Ordering::SeqCst));

    let lease = store
        .acquire(&AcquireRequest {
            ttl_seconds: Some(60),
            ..req("TASK-001", "builder-1")
        })
        .unwrap();

    clock.fetch_add(30, Ordering::SeqCst);
    assert!(
        store
            .renew("TASK-001", &lease.token, 600)
            .unwrap()
            .is_some()
    );

    clock.fetch_add(120, Ordering::SeqCst);
    assert!(
        store.validate("TASK-001", &lease.token).unwrap(),
        "renewal should have pushed the expiry out"
    );

    // Past the renewed expiry, renewal is not a resurrection.
    clock.fetch_add(600, Ordering::SeqCst);
    assert!(
        store
            .renew("TASK-001", &lease.token, 600)
            .unwrap()
            .is_none()
    );
}

#[test]
fn release_requires_the_holders_token() {
    let store = Store::open_in_memory().unwrap();
    store.acquire(&req("TASK-001", "builder-1")).unwrap();

    assert!(!store.release("TASK-001", "not-my-token").unwrap());
    assert!(
        store.active_lease("TASK-001").unwrap().is_some(),
        "a non-holder must not be able to release someone else's work"
    );
}

#[test]
fn force_steals_a_lease_and_records_the_old_one() {
    let store = Store::open_in_memory().unwrap();
    let first = store.acquire(&req("TASK-001", "builder-1")).unwrap();

    let second = store
        .acquire(&AcquireRequest {
            force: true,
            ..req("TASK-001", "operator")
        })
        .unwrap();

    assert_ne!(first.token, second.token);
    assert_eq!(
        store.active_lease("TASK-001").unwrap().unwrap().owner,
        "operator"
    );

    // The displaced holder gets held_by_other, not expired — and that ordering
    // is the incumbent's, not a choice made here. `diagnose_lease` checks for a
    // live lease before it looks the token up, so once the operator holds the
    // task the displaced agent is told to pick another task rather than to
    // check this one out again. That is the more useful answer: checking out
    // again would fail, because someone else genuinely holds it.
    assert_eq!(
        store.diagnose("TASK-001", &first.token).unwrap(),
        LeaseCode::HeldByOther
    );

    // Only once the operator finishes does the displaced token read as
    // recoverable.
    store.release("TASK-001", &second.token).unwrap();
    assert_eq!(
        store.diagnose("TASK-001", &first.token).unwrap(),
        LeaseCode::Expired
    );
}

/// The load-bearing test. Twelve threads race for one task through separate
/// connections to one file. Exactly one may win, and the losers must all report
/// the conflict rather than any other error.
#[test]
fn concurrent_acquires_produce_exactly_one_winner() {
    let dir = std::env::temp_dir().join(format!("dc-store-race-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    let path = dir.join("state.sqlite");
    let _ = std::fs::remove_file(&path);

    // Create the schema once, so the race is over the lease and not over DDL.
    Store::open(&path).unwrap();

    const THREADS: usize = 12;
    let barrier = Arc::new(Barrier::new(THREADS));
    let mut handles = Vec::new();

    for i in 0..THREADS {
        let path = path.clone();
        let barrier = Arc::clone(&barrier);
        handles.push(std::thread::spawn(move || {
            let store = Store::open(&path).unwrap();
            barrier.wait();
            match store.acquire(&req("TASK-RACE", &format!("builder-{i}"))) {
                Ok(_) => Ok(()),
                Err(StoreError::LeaseHeld { .. }) => Err("held"),
                Err(other) => panic!("unexpected error under contention: {other}"),
            }
        }));
    }

    let results: Vec<_> = handles.into_iter().map(|h| h.join().unwrap()).collect();
    let winners = results.iter().filter(|r| r.is_ok()).count();
    assert_eq!(winners, 1, "exactly one acquire may succeed, got {winners}");

    let store = Store::open(&path).unwrap();
    let active = store.active_leases().unwrap();
    assert_eq!(active.len(), 1, "exactly one active lease may exist");

    // And the database itself agrees.
    let count: i64 = store
        .connection()
        .query_row(
            "SELECT COUNT(*) FROM task_leases WHERE task_id = 'TASK-RACE' AND status = 'active'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(count, 1);

    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn active_leases_excludes_expired_ones() {
    let mut store = Store::open_in_memory().unwrap();
    let clock = Arc::new(AtomicI64::new(1_700_000_000));
    let handle = Arc::clone(&clock);
    store.set_clock(move || handle.load(Ordering::SeqCst));

    store
        .acquire(&AcquireRequest {
            ttl_seconds: Some(60),
            ..req("TASK-SHORT", "b1")
        })
        .unwrap();
    store
        .acquire(&AcquireRequest {
            ttl_seconds: Some(6000),
            ..req("TASK-LONG", "b2")
        })
        .unwrap();

    clock.fetch_add(61, Ordering::SeqCst);
    let active = store.active_leases().unwrap();
    assert_eq!(active.len(), 1);
    assert_eq!(active[0].task_id, "TASK-LONG");
}

/// An unreadable expiry must surface as an error. Treating it as "not expired"
/// would turn a corrupt row into a lease nobody can ever take.
#[test]
fn an_unreadable_expiry_is_an_error_not_an_immortal_lease() {
    let store = Store::open_in_memory().unwrap();
    let lease = store.acquire(&req("TASK-001", "b1")).unwrap();
    store
        .connection()
        .execute(
            "UPDATE task_leases SET expires_at = 'not a timestamp' WHERE id = ?1",
            [&lease.id],
        )
        .unwrap();

    match store.active_lease("TASK-001") {
        Err(StoreError::BadTimestamp { value }) => assert_eq!(value, "not a timestamp"),
        other => panic!("expected BadTimestamp, got {other:?}"),
    }
}

/// Timestamps must round-trip through the exact shape DevCouncil writes, or the
/// Python side will read a lease's expiry differently from the harness.
#[test]
fn timestamps_round_trip_and_parse_python_shapes() {
    let epoch = 1_755_470_472;
    let iso = format_iso_utc(epoch);
    assert_eq!(iso, "2025-08-17T22:41:12+00:00");
    assert_eq!(parse_iso_utc(&iso), Some(epoch));

    // The shape datetime.now(timezone.utc).isoformat() actually produces,
    // including microseconds.
    assert_eq!(
        parse_iso_utc("2025-08-17T22:41:12.330953+00:00"),
        Some(epoch)
    );
    // Naive timestamps are read as UTC, matching the incumbent's
    // replace(tzinfo=timezone.utc).
    assert_eq!(parse_iso_utc("2025-08-17T22:41:12"), Some(epoch));
    // A non-UTC offset is honoured rather than assumed away.
    assert_eq!(
        parse_iso_utc("2025-08-17T23:41:12+01:00"),
        Some(epoch),
        "a +01:00 timestamp is the same instant"
    );
    assert_eq!(parse_iso_utc("garbage"), None);
    assert_eq!(parse_iso_utc(""), None);
}

#[test]
fn non_positive_ttl_is_refused_not_minted() {
    let store = Store::open_in_memory().unwrap();
    for ttl in [-3600i64, 0] {
        let err = store
            .acquire(&AcquireRequest {
                task_id: "TASK-TTL".into(),
                owner: "builder-1".into(),
                ttl_seconds: Some(ttl),
                ..Default::default()
            })
            .expect_err("a lease born expired must not report success");
        assert!(
            err.to_string().contains("unusable ttl"),
            "unexpected error for ttl {ttl}: {err}"
        );
        assert!(store.active_lease("TASK-TTL").unwrap().is_none());
    }
}

#[test]
fn absurd_ttl_is_refused_rather_than_overflowing() {
    // i64::MAX used to walk `now + ttl` off the end of i64: panic in debug,
    // nonsense expiry in release.
    let store = Store::open_in_memory().unwrap();
    let err = store
        .acquire(&AcquireRequest {
            task_id: "TASK-TTL".into(),
            owner: "builder-1".into(),
            ttl_seconds: Some(i64::MAX),
            ..Default::default()
        })
        .expect_err("an unbounded ttl must be refused");
    assert!(err.to_string().contains("unusable ttl"), "{err}");
}

#[test]
fn impossible_calendar_timestamps_are_unreadable_not_immortal() {
    // Digit-shaped garbage used to produce an epoch via the civil-date
    // arithmetic and read as a lease that effectively never expires. The
    // invariant is the opposite: an unreadable expiry surfaces as an error,
    // never as an indefinite lease.
    for bad in [
        "9999-99-99T99:99:99",
        "2099-13-01T00:00:00", // month 13
        "2099-01-32T00:00:00", // day 32
        "2099-01-01T24:00:00", // hour 24
        "2099-01-01T00:60:00", // minute 60
        "2099-01-01T00:00:61", // leap-second spelling stays out; it is not produced here
    ] {
        assert!(
            parse_iso_utc(bad).is_none(),
            "{bad} parsed as a readable timestamp"
        );
    }
    // And the readable shapes still parse, including the offset forms.
    assert!(parse_iso_utc("2099-01-31T23:59:59").is_some());
    assert!(parse_iso_utc("2099-01-31T23:59:59+05:30").is_some());
}
