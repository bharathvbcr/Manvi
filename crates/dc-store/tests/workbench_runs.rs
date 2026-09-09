use dc_store::Store;
use std::sync::{
    Arc,
    atomic::{AtomicI64, Ordering},
};

fn fixture() -> (Store, Arc<AtomicI64>) {
    let mut store = Store::open_in_memory().unwrap();
    let clock = Arc::new(AtomicI64::new(1000));
    let time = clock.clone();
    store.set_clock(move || time.load(Ordering::SeqCst));
    call(
        &store,
        "repositories.put",
        r#"{"id":"r","request_id":"r","expected_revision":0,"name":"Manvi","identity_key":"local:/checkout/.git"}"#,
    );
    call(
        &store,
        "items.put",
        r#"{"id":"t","request_id":"t","expected_revision":0,"title":"Keep E42","description":"Preserve exact evidence","repository_ids":["r"],"primary_repository_id":"r"}"#,
    );
    (store, clock)
}
fn call(store: &Store, method: &str, input: &str) -> String {
    request(store, method, input).unwrap_or_else(|e| panic!("{method}: {e}"))
}
fn request(store: &Store, method: &str, input: &str) -> Result<String, dc_store::workbench::Error> {
    // Forward slashes are accepted in absolute Windows paths and avoid adding
    // fixture-specific JSON escaping to every protocol case.
    let raw = if cfg!(windows) {
        input.replace("/checkout", "C:/checkout")
    } else {
        input.to_owned()
    };
    store.workbench_request(method, &raw)
}
fn text(store: &Store, raw: &str, path: &str) -> String {
    store
        .connection()
        .query_row("SELECT json_extract(?1,?2)", [raw, path], |r| r.get(0))
        .unwrap()
}
fn number(store: &Store, raw: &str, path: &str) -> i64 {
    store
        .connection()
        .query_row("SELECT json_extract(?1,?2)", [raw, path], |r| r.get(0))
        .unwrap()
}
const PREPARE: &str = r#"{"id":"run","request_id":"prepare","expected_revision":0,"task_id":"t","source_revision":1,"repository_id":"r","repository_revision":1,"provider":"codex","permission_mode":"ask","cwd":"/checkout","git_dir":"/checkout/.git","git_common_dir":"/checkout/.git","head_oid":null}"#;
const CLAIM: &str = r#"{"id":"run","request_id":"claim","expected_revision":1,"owner_id":"host-one","session_id":"terminal-one"}"#;

#[test]
fn failed_managed_initialization_keeps_process_evidence_and_never_forges_a_thread() {
    let (store, _) = fixture();
    call(
        &store,
        "runs.prepare",
        &PREPARE.replace("\"provider\":", "\"kind\":\"managed\",\"provider\":"),
    );
    call(
        &store,
        "runs.claim",
        &CLAIM.replace("\"owner_id\":", "\"kind\":\"managed\",\"owner_id\":"),
    );
    call(
        &store,
        "runs.started",
        r#"{"id":"run","request_id":"started","expected_revision":2,"owner_id":"host-one","session_id":"terminal-one","process_id":42,"process_start":"native-birth"}"#,
    );
    let finish = r#"{"id":"run","request_id":"finish","expected_revision":3,"owner_id":"host-one","session_id":"terminal-one","outcome":"exited","exit_code":0,"reason":"Handshake rejected; process reaped","provider_state":"failed"}"#;
    for status in ["ready", "running", "completed"] {
        assert!(
            request(
                &store,
                "runs.finish",
                &finish.replace("\"failed\"", &format!("\"{status}\""))
            )
            .is_err()
        );
    }
    let result = call(&store, "runs.finish", finish);
    assert_eq!(text(&store, &result, "$.item.provider_state"), "failed");
    assert_eq!(
        text(&store, &result, "$.item.process_start"),
        "native-birth"
    );
    assert!(!result.contains("provider_thread_id"));
    let notices = call(&store, "attention.list", r#"{"task_id":"t"}"#);
    assert_eq!(text(&store, &notices, "$.items[0].kind"), "run_exit_failed");
    let task = call(&store, "items.get", r#"{"id":"t"}"#);
    assert_eq!(number(&store, &task, "$.item.revision"), 1);
    call(
        &store,
        "runs.prepare",
        &PREPARE
            .replace("\"run\"", "\"retry\"")
            .replace("\"prepare\"", "\"prepare-retry\""),
    );
}

#[test]
fn terminal_finish_cannot_introduce_managed_provider_evidence() {
    let (store, _) = fixture();
    call(&store, "runs.prepare", PREPARE);
    call(&store, "runs.claim", CLAIM);
    assert!(request(&store, "runs.finish", r#"{"id":"run","request_id":"finish","expected_revision":2,"owner_id":"host-one","session_id":"terminal-one","outcome":"failed","reason":"Not started","provider_state":"failed"}"#).is_err());
}

#[test]
fn managed_kind_cannot_be_claimed_as_a_terminal_and_protocol_receipts_are_owned() {
    let (store, _) = fixture();
    let prepared = call(
        &store,
        "runs.prepare",
        &PREPARE.replace("\"provider\":", "\"kind\":\"managed\",\"provider\":"),
    );
    assert_eq!(text(&store, &prepared, "$.item.kind"), "managed");
    assert_eq!(
        request(&store, "runs.claim", CLAIM).unwrap_err().code,
        "run_kind_mismatch"
    );
    call(
        &store,
        "runs.claim",
        &CLAIM.replace("\"owner_id\":", "\"kind\":\"managed\",\"owner_id\":"),
    );
    let protocol = r#"{"id":"run","request_id":"protocol","expected_revision":2,"owner_id":"host-one","session_id":"terminal-one","provider_thread_id":"thread","provider_turn_id":null,"provider_state":"ready","effective_configuration":"{\"cwd\":\"/checkout\",\"sandbox\":{\"type\":\"readOnly\",\"networkAccess\":false}}","output":"","output_truncated":false}"#;
    assert!(
        request(
            &store,
            "runs.protocol",
            &protocol.replace("host-one", "other")
        )
        .is_err()
    );
    assert_eq!(
        request(&store, "runs.protocol", protocol).unwrap_err().code,
        "invalid_state"
    );
    call(
        &store,
        "runs.started",
        r#"{"id":"run","request_id":"started","expected_revision":2,"owner_id":"host-one","session_id":"terminal-one","process_id":42,"process_start":"native-birth"}"#,
    );
    let protocol = protocol.replace("\"expected_revision\":2", "\"expected_revision\":3");
    call(&store, "runs.protocol", &protocol);
    let page = call(&store, "runs.list", "{}");
    assert!(
        !page.contains("effective_configuration"),
        "history transferred full provider settings"
    );
    assert!(
        !page.contains("\"output\":"),
        "history transferred full provider output"
    );
    assert!(call(&store, "runs.get", r#"{"id":"run"}"#).contains("effective_configuration"));
    assert_eq!(
        number(
            &store,
            &call(&store, "items.get", r#"{"id":"t"}"#),
            "$.item.revision"
        ),
        1
    );
    let changed = protocol
        .replace("\"protocol\"", "\"different\"")
        .replace("\"expected_revision\":3", "\"expected_revision\":4")
        .replace("\"thread\"", "\"other-thread\"");
    assert!(request(&store, "runs.protocol", &changed).is_err());
}

#[test]
fn newest_run_pages_keep_equal_timestamp_ties_and_exact_totals() {
    let (store, _) = fixture();
    for name in ["a", "b", "c"] {
        call(
            &store,
            "runs.prepare",
            &PREPARE
                .replace("\"run\"", &format!("\"{name}\""))
                .replace("\"prepare\"", &format!("\"prepare-{name}\"")),
        );
        call(
            &store,
            "runs.cancel",
            &format!(r#"{{"id":"{name}","request_id":"cancel-{name}","expected_revision":1}}"#),
        );
    }
    let first = call(
        &store,
        "runs.list",
        r#"{"task_id":"t","newest":true,"limit":1}"#,
    );
    assert_eq!(text(&store, &first, "$.items[0].id"), "c");
    assert_eq!(number(&store, &first, "$.total"), 3);
    let cursor = text(&store, &first, "$.next_cursor");
    let next = call(
        &store,
        "runs.list",
        &format!(r#"{{"task_id":"t","newest":true,"limit":2,"cursor":"{cursor}"}}"#),
    );
    assert_eq!(text(&store, &next, "$.items[0].id"), "b");
    assert_eq!(text(&store, &next, "$.items[1].id"), "a");
    assert_eq!(number(&store, &next, "$.total"), 3);
    assert_eq!(number(&store, &next, "$.has_more"), 0);
    assert_eq!(
        text(
            &store,
            &call(&store, "runs.list", r#"{"limit":1}"#),
            "$.items[0].id"
        ),
        "a"
    );
}

#[test]
fn run_snapshots_preserve_branch_identity_and_refuse_malformed_refs() {
    let (store, _) = fixture();
    let input = PREPARE.replace(
        "\"head_oid\":null",
        "\"head_oid\":null,\"head_ref\":\"refs/heads/main\"",
    );
    let prepared = call(&store, "runs.prepare", &input);
    assert_eq!(
        text(&store, &prepared, "$.item.head_ref"),
        "refs/heads/main"
    );
    assert_eq!(call(&store, "runs.prepare", &input), prepared);
    for branch in [
        "main",
        "refs/tags/main",
        "refs/heads/",
        "refs/heads/a..b",
        "refs/heads/with space",
    ] {
        let (store, _) = fixture();
        assert_eq!(
            request(
                &store,
                "runs.prepare",
                &input.replace("refs/heads/main", branch)
            )
            .unwrap_err()
            .code,
            "invalid_input"
        );
        assert_eq!(
            number(&store, &call(&store, "runs.list", "{}"), "$.total"),
            0
        );
    }
}

#[test]
fn a_launch_snapshot_is_durable_and_claim_receipts_never_authorize_a_second_spawn() {
    let (store, _) = fixture();
    let prepared = call(&store, "runs.prepare", PREPARE);
    assert_eq!(text(&store, &prepared, "$.item.state"), "prepared");
    assert_eq!(call(&store, "runs.prepare", PREPARE), prepared);
    let captured = call(&store, "runs.get", r#"{"id":"run"}"#);
    assert_eq!(
        text(&store, &captured, "$.item.brief.task.description"),
        "Preserve exact evidence"
    );
    let claimed = call(&store, "runs.claim", CLAIM);
    assert_eq!(text(&store, &claimed, "$.item.state"), "starting");
    assert_eq!(
        request(&store, "runs.claim", CLAIM).unwrap_err().code,
        "claim_consumed"
    );
    assert_eq!(
        text(
            &store,
            &call(&store, "runs.get", r#"{"id":"run"}"#),
            "$.item.session_id"
        ),
        "terminal-one"
    );
    assert_eq!(
        number(
            &store,
            &call(&store, "items.get", r#"{"id":"t"}"#),
            "$.item.revision"
        ),
        1
    );
}

#[test]
fn launch_claims_refuse_changed_task_or_repository_and_expired_preparation() {
    for change in [0, 1, 2] {
        let (store, time) = fixture();
        call(&store, "runs.prepare", PREPARE);
        match change {
            0 => {
                call(
                    &store,
                    "items.put",
                    r#"{"id":"t","request_id":"edit","expected_revision":1,"title":"Changed task","repository_ids":["r"],"primary_repository_id":"r"}"#,
                );
            }
            1 => {
                call(
                    &store,
                    "repositories.put",
                    r#"{"id":"r","request_id":"rename","expected_revision":1,"name":"Renamed repo","identity_key":"local:/checkout/.git"}"#,
                );
            }
            _ => time.store(1300, Ordering::SeqCst),
        }
        let error = request(&store, "runs.claim", CLAIM).unwrap_err();
        assert_eq!(
            error.code,
            if change == 2 {
                "expired"
            } else {
                "revision_conflict"
            }
        );
        let stored = call(&store, "runs.get", r#"{"id":"run"}"#);
        assert_eq!(text(&store, &stored, "$.item.state"), "prepared");
        assert_eq!(text(&store, &stored, "$.item.brief.task.title"), "Keep E42");
    }
}

#[test]
fn only_the_claim_owner_can_report_process_lifecycle_and_exit_never_accepts_a_task() {
    let (store, _) = fixture();
    call(&store, "runs.prepare", PREPARE);
    call(&store, "runs.claim", CLAIM);
    let start = r#"{"id":"run","request_id":"started","expected_revision":2,"owner_id":"host-one","session_id":"terminal-one","process_id":42,"process_start":"boot-one:birth-two"}"#;
    assert_eq!(
        request(
            &store,
            "runs.started",
            &start.replace("host-one", "host-two")
        )
        .unwrap_err()
        .code,
        "owner_mismatch"
    );
    let started = call(&store, "runs.started", start);
    let events = call(&store, "events.list", "{}");
    assert_eq!(call(&store, "runs.started", start), started);
    assert_eq!(call(&store, "events.list", "{}"), events);
    call(
        &store,
        "runs.finish",
        r#"{"id":"run","request_id":"exited","expected_revision":3,"owner_id":"host-one","session_id":"terminal-one","outcome":"exited","exit_code":0,"reason":"Process exited normally"}"#,
    );
    let result = call(&store, "runs.get", r#"{"id":"run"}"#);
    assert_eq!(text(&store, &result, "$.item.state"), "exited");
    assert_eq!(number(&store, &result, "$.item.exit_code"), 0);
    let task = call(&store, "items.get", r#"{"id":"t"}"#);
    assert_eq!(text(&store, &task, "$.item.status"), "inbox");
    assert_eq!(number(&store, &task, "$.item.revision"), 1);
}

#[test]
fn deleting_a_task_keeps_its_run_history_and_snapshots_available() {
    let (store, _) = fixture();
    call(&store, "runs.prepare", PREPARE);
    call(
        &store,
        "items.delete",
        r#"{"id":"t","request_id":"delete","expected_revision":1}"#,
    );
    let history = call(&store, "runs.list", r#"{"task_id":"t","limit":1}"#);
    assert_eq!(number(&store, &history, "$.total"), 1);
    assert_eq!(text(&store, &history, "$.items[0].task_title"), "Keep E42");
    assert!(
        !history.contains("Preserve exact evidence"),
        "run listing fetched the full source brief"
    );
    let saved = call(&store, "runs.get", r#"{"id":"run"}"#);
    assert_eq!(
        text(&store, &saved, "$.item.brief.task.description"),
        "Preserve exact evidence"
    );
    assert_eq!(
        request(&store, "runs.claim", CLAIM).unwrap_err().code,
        "not_found"
    );
}

#[test]
fn claim_event_failure_rolls_back_the_execution_claim_and_its_receipt() {
    let (store, _) = fixture();
    call(&store, "runs.prepare", PREPARE);
    store.connection().execute_batch("CREATE TRIGGER fail_claim BEFORE INSERT ON work_events WHEN new.kind='runs.claim' BEGIN SELECT RAISE(ABORT,'fixture claim event failure'); END").unwrap();
    assert_eq!(
        request(&store, "runs.claim", CLAIM).unwrap_err().code,
        "store_error"
    );
    assert_eq!(
        text(
            &store,
            &call(&store, "runs.get", r#"{"id":"run"}"#),
            "$.item.state"
        ),
        "prepared"
    );
    store
        .connection()
        .execute_batch("DROP TRIGGER fail_claim")
        .unwrap();
    call(&store, "runs.claim", CLAIM);
    assert_eq!(
        request(&store, "runs.claim", CLAIM).unwrap_err().code,
        "claim_consumed"
    );
}

#[test]
fn uncertain_processes_keep_the_repository_reserved_and_cannot_turn_into_success() {
    let (store, clock) = fixture();
    call(&store, "runs.prepare", PREPARE);
    call(&store, "runs.claim", CLAIM);
    call(
        &store,
        "runs.finish",
        r#"{"id":"run","request_id":"uncertain","expected_revision":2,"owner_id":"host-one","session_id":"terminal-one","outcome":"unresolved","reason":"Host connection lost before spawn confirmation"}"#,
    );
    clock.store(100_000, Ordering::SeqCst);
    let other = PREPARE
        .replace("\"run\"", "\"second\"")
        .replace("\"prepare\"", "\"second-prepare\"");
    assert_eq!(
        request(&store, "runs.prepare", &other).unwrap_err().code,
        "repository_busy"
    );
    let false_success = r#"{"id":"run","request_id":"false-success","expected_revision":3,"owner_id":"host-one","session_id":"terminal-one","outcome":"exited","exit_code":0,"reason":"Time passed"}"#;
    assert_eq!(
        request(&store, "runs.finish", false_success)
            .unwrap_err()
            .code,
        "invalid_state"
    );
    assert_eq!(
        number(
            &store,
            &call(&store, "runs.get", r#"{"id":"run"}"#),
            "$.item.outcome_uncertain"
        ),
        1
    );
}

#[test]
fn schema_four_upgrade_rolls_back_on_conflict_and_preserves_existing_tasks() {
    let (store, _) = fixture();
    let task = call(&store, "items.get", r#"{"id":"t"}"#);
    // Retain an empty conflicting input table. The migration must roll back
    // the run table and indexes created before discovering that conflict.
    store
        .connection()
        .execute_batch("DROP TABLE work_decisions; DROP TABLE work_notification_deliveries; DROP TABLE work_notification_settings; DROP TABLE work_attention; DROP TABLE work_runs; UPDATE work_meta SET version=4 WHERE id=1")
        .unwrap();
    assert_eq!(
        request(&store, "runs.list", "{}").unwrap_err().code,
        "store_error"
    );
    let state: (i64,i64) = store.connection().query_row("SELECT version,(SELECT count(*) FROM sqlite_master WHERE name='work_runs') FROM work_meta WHERE id=1",[],|r|Ok((r.get(0)?,r.get(1)?))).unwrap();
    assert_eq!(state, (4, 0));
    store
        .connection()
        .execute_batch("DROP TABLE work_run_inputs")
        .unwrap();
    assert_eq!(call(&store, "items.get", r#"{"id":"t"}"#), task);
    assert_eq!(
        number(&store, &call(&store, "runs.list", "{}"), "$.total"),
        0
    );
    assert_eq!(
        store
            .connection()
            .query_row("SELECT version FROM work_meta WHERE id=1", [], |r| r
                .get::<_, i64>(0))
            .unwrap(),
        10
    );
}

#[test]
fn independent_connections_share_one_consumable_claim_and_reopening_cannot_reissue_it() {
    let (store, _) = fixture();
    call(&store, "runs.prepare", PREPARE);
    let dir = std::env::temp_dir().join(format!(
        "manvi-run-claim-{}-{}",
        std::process::id(),
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos()
    ));
    std::fs::create_dir(&dir).unwrap();
    let path = dir.join("profile.sqlite");
    store
        .connection()
        .execute("VACUUM INTO ?1", [path.to_str().unwrap()])
        .unwrap();
    let a = Store::open(&path).unwrap();
    let b = Store::open(&path).unwrap();
    let barrier = Arc::new(std::sync::Barrier::new(2));
    let outcomes = std::thread::scope(|scope| {
        let first = barrier.clone();
        let a = scope.spawn(move || {
            let mut a = a;
            a.set_clock(|| 1000);
            first.wait();
            request(&a, "runs.claim", CLAIM)
        });
        let b = scope.spawn(move || {
            let mut b = b;
            b.set_clock(|| 1000);
            barrier.wait();
            request(&b, "runs.claim", CLAIM)
        });
        [a.join().unwrap(), b.join().unwrap()]
    });
    assert_eq!(outcomes.iter().filter(|r| r.is_ok()).count(), 1);
    assert_eq!(
        outcomes
            .iter()
            .filter_map(|r| r.as_ref().err())
            .next()
            .unwrap()
            .code,
        "claim_consumed"
    );
    {
        let reopened = Store::open(&path).unwrap();
        assert_eq!(
            request(&reopened, "runs.claim", CLAIM).unwrap_err().code,
            "claim_consumed"
        );
        assert_eq!(
            text(
                &reopened,
                &call(&reopened, "runs.get", r#"{"id":"run"}"#),
                "$.item.state"
            ),
            "starting"
        );
        assert_eq!(
            reopened
                .connection()
                .query_row(
                    "SELECT count(*) FROM work_events WHERE kind='runs.claim'",
                    [],
                    |r| r.get::<_, i64>(0)
                )
                .unwrap(),
            1
        );
    }
    std::fs::remove_dir_all(&dir).unwrap();
}

#[test]
fn preparation_limits_and_pagination_preserve_counts_without_copying_source_bodies() {
    let (store, _) = fixture();
    for i in 0..3 {
        call(
            &store,
            "repositories.put",
            &format!(
                r#"{{"id":"r{i}","request_id":"repo-{i}","expected_revision":0,"name":"Repository {i}","identity_key":"local:/checkout/{i}/.git"}}"#
            ),
        );
        call(
            &store,
            "items.put",
            &format!(
                r#"{{"id":"t{i}","request_id":"task-{i}","expected_revision":0,"title":"Task {i}","description":"Large source text must stay outside lifecycle events","repository_ids":["r{i}"],"primary_repository_id":"r{i}"}}"#
            ),
        );
    }
    let prepare = |i: usize| {
        PREPARE
            .replace("\"run\"", &format!("\"run-{i}\""))
            .replace("\"prepare\"", &format!("\"prepare-{i}\""))
            .replace("\"t\"", &format!("\"t{i}\""))
            .replace("\"r\"", &format!("\"r{i}\""))
            .replace("/checkout", &format!("/checkout/{i}"))
    };
    call(&store, "runs.prepare", &prepare(0));
    call(&store, "runs.prepare", &prepare(1));
    assert_eq!(
        request(&store, "runs.prepare", &prepare(2))
            .unwrap_err()
            .code,
        "capacity_reached"
    );
    let first = call(&store, "runs.list", r#"{"limit":1}"#);
    assert_eq!(number(&store, &first, "$.total"), 2);
    assert_eq!(number(&store, &first, "$.shown"), 1);
    assert!(!first.contains("Large source text"));
    let cursor = text(&store, &first, "$.next_cursor");
    let second = call(
        &store,
        "runs.list",
        &format!(r#"{{"limit":1,"cursor":"{cursor}"}}"#),
    );
    assert_eq!(number(&store, &second, "$.total"), 2);
    assert_eq!(text(&store, &second, "$.items[0].id"), "run-1");
    assert_eq!(number(&store, &second, "$.has_more"), 0);
    call(
        &store,
        "runs.cancel",
        r#"{"id":"run-0","request_id":"cancel-zero","expected_revision":1}"#,
    );
    call(&store, "runs.prepare", &prepare(2));
    assert_eq!(
        number(
            &store,
            &call(&store, "runs.list", r#"{"state":"cancelled"}"#),
            "$.total"
        ),
        1
    );
    assert_eq!(store.connection().query_row("SELECT count(*) FROM work_events WHERE kind LIKE 'runs.%' AND instr(payload,'Large source text')>0",[],|r|r.get::<_,i64>(0)).unwrap(),0);
    assert_eq!(
        store
            .connection()
            .query_row("SELECT count(*) FROM work_run_inputs", [], |r| r
                .get::<_, i64>(0))
            .unwrap(),
        3
    );
    assert!(
        store
            .connection()
            .execute(
                "UPDATE work_run_inputs SET brief='{}' WHERE run_id='run-0'",
                []
            )
            .is_err()
    );
}

#[test]
fn malformed_or_unrelated_launch_inputs_never_reserve_a_slot() {
    let (store, _) = fixture();
    for input in [
        PREPARE.replace("\"ask\"", "\"unknown\""),
        PREPARE.replace("\"codex\"", "\"other\""),
        PREPARE.replace("\"repository_revision\":1", "\"repository_revision\":2"),
        PREPARE.replace("\"cwd\":\"/checkout\"", "\"cwd\":\"relative\""),
        PREPARE.replace("\"head_oid\":null", "\"head_oid\":\"short\""),
        PREPARE.replace(
            "\"git_common_dir\":\"/checkout/.git\"",
            "\"git_common_dir\":\"/other/.git\"",
        ),
        PREPARE.trim_end_matches('}').to_owned() + ",\"acknowledge_bypass\":true}",
        PREPARE.trim_end_matches('}').to_owned() + ",\"skip_permissions\":true}",
    ] {
        assert!(
            request(&store, "runs.prepare", &input).is_err(),
            "accepted {input}"
        );
        assert_eq!(
            number(&store, &call(&store, "runs.list", "{}"), "$.total"),
            0
        );
    }
}

#[test]
fn bypass_is_explicit_for_each_new_attempt_and_claimed_runs_cannot_be_cancelled_as_unstarted() {
    let (store, _) = fixture();
    let bypass = PREPARE.replace("\"ask\"", "\"bypass\"");
    assert_eq!(
        request(&store, "runs.prepare", &bypass).unwrap_err().code,
        "invalid_input"
    );
    let acknowledged = bypass.trim_end_matches('}').to_owned() + ",\"acknowledge_bypass\":true}";
    call(&store, "runs.prepare", &acknowledged);
    call(&store, "runs.claim", CLAIM);
    assert_eq!(
        request(
            &store,
            "runs.cancel",
            r#"{"id":"run","request_id":"cancel","expected_revision":2}"#
        )
        .unwrap_err()
        .code,
        "invalid_state"
    );
    assert_eq!(
        request(
            &store,
            "runs.prepare",
            &bypass
                .replace("\"run\"", "\"retry\"")
                .replace("\"prepare\"", "\"retry-prepare\"")
        )
        .unwrap_err()
        .code,
        "invalid_input"
    );
}
