use dc_store::Store;
use std::sync::{
    Arc,
    atomic::{AtomicI64, Ordering},
};

fn fixture() -> (Store, Arc<AtomicI64>) {
    let mut store = Store::open_in_memory().unwrap();
    let clock = Arc::new(AtomicI64::new(1000));
    let source = clock.clone();
    store.set_clock(move || source.load(Ordering::SeqCst));
    call(
        &store,
        "repositories.put",
        r#"{"id":"r","request_id":"repo","expected_revision":0,"name":"Manvi","identity_key":"fixture:r"}"#,
    );
    (store, clock)
}
fn call(store: &Store, method: &str, input: &str) -> String {
    store
        .workbench_request(method, input)
        .unwrap_or_else(|error| panic!("{method}: {error}"))
}
fn number(store: &Store, response: &str, path: &str) -> i64 {
    store
        .connection()
        .query_row("SELECT json_extract(?1,?2)", [response, path], |row| {
            row.get(0)
        })
        .unwrap()
}
fn text(store: &Store, response: &str, path: &str) -> String {
    store
        .connection()
        .query_row("SELECT json_extract(?1,?2)", [response, path], |row| {
            row.get(0)
        })
        .unwrap()
}
fn save(store: &Store, revision: i64, title: &str, status: &str, locks: &str) -> String {
    call(
        store,
        "items.put",
        &format!(
            r#"{{"id":"t","request_id":"save-{revision}","expected_revision":{revision},"title":"{title}","description":"Original evidence","status":"{status}","locked_fields":{locks},"repository_ids":["r"],"primary_repository_id":"r"}}"#
        ),
    )
}
fn count(store: &Store) -> i64 {
    number(store, &call(store, "automation.list", "{}"), "$.total")
}
const PREPARE: &str = r#"{"id":"e","request_id":"prepare","expected_revision":0,"task_id":"t","expected_task_revision":1,"expected_settings_revision":1,"provider":"local","model":"fixture-model"}"#;

#[test]
fn manual_preparation_supersedes_only_its_source_queue_and_replay_preserves_new_text() {
    let (store, _) = fixture();
    save(&store, 0, "Investigate E42", "backlog", "[]");
    call(
        &store,
        "items.put",
        r#"{"id":"other","request_id":"other","expected_revision":0,"title":"Another task","repository_ids":["r"],"primary_repository_id":"r"}"#,
    );
    let manual = r#"{"id":"manual","request_id":"manual","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"fixture-model"}"#;
    let response = call(&store, "enhancements.create", manual);
    assert_eq!(
        count(&store),
        1,
        "manual work left duplicate automatic inference queued"
    );
    assert_eq!(
        number(
            &store,
            &call(&store, "automation.list", r#"{"task_id":"other"}"#),
            "$.total"
        ),
        1
    );
    save(
        &store,
        1,
        "Investigate E42 with new evidence",
        "backlog",
        "[]",
    );
    assert_eq!(count(&store), 2);
    assert_eq!(call(&store, "enhancements.create", manual), response);
    assert_eq!(
        count(&store),
        2,
        "replaying an old receipt consumed new text"
    );
}

#[test]
fn failed_manual_preparation_preserves_queued_work_atomically() {
    let (store, _) = fixture();
    save(&store, 0, "Investigate E42", "backlog", "[]");
    let manual = r#"{"id":"manual","request_id":"manual","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"fixture-model"}"#;
    store.connection().execute_batch("CREATE TRIGGER refuse_manual_event BEFORE INSERT ON work_events BEGIN SELECT RAISE(ABORT,'fixture event failure'); END;").unwrap();
    assert!(
        store
            .workbench_request("enhancements.create", manual)
            .is_err()
    );
    assert_eq!(count(&store), 1);
    assert_eq!(
        number(&store, &call(&store, "enhancements.list", "{}"), "$.total"),
        0
    );
    store
        .connection()
        .execute_batch("DROP TRIGGER refuse_manual_event;")
        .unwrap();
    call(&store, "enhancements.create", manual);
    assert_eq!(
        count(&store),
        0,
        "successful retry did not supersede queued work"
    );
}

#[test]
fn disabling_after_preparation_prevents_a_model_claim() {
    let (store, clock) = fixture();
    save(&store, 0, "Investigate E42", "backlog", "[]");
    clock.store(1001, Ordering::SeqCst);
    call(&store, "automation.prepare", PREPARE);
    call(
        &store,
        "automation.put",
        r#"{"id":"profile","request_id":"disable","expected_revision":1,"enabled":false}"#,
    );
    let claim = r#"{"id":"e","request_id":"start","expected_revision":1,"worker_id":"worker"}"#;
    assert!(
        store
            .workbench_request("enhancements.claim", claim)
            .is_err(),
        "disabled automation claimed a provider call"
    );
    assert_eq!(
        text(
            &store,
            &call(&store, "enhancements.get", r#"{"id":"e"}"#),
            "$.item.state"
        ),
        "dismissed"
    );
}

#[test]
fn disabling_waits_for_a_running_automatic_worker_and_preserves_manual_work() {
    for automatic in [true, false] {
        let (store, clock) = fixture();
        save(&store, 0, "Investigate E42", "backlog", "[]");
        clock.store(1001, Ordering::SeqCst);
        if automatic {
            call(&store, "automation.prepare", PREPARE);
        } else {
            call(
                &store,
                "enhancements.create",
                r#"{"id":"e","request_id":"manual","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"fixture-model"}"#,
            );
        }
        call(
            &store,
            "enhancements.claim",
            r#"{"id":"e","request_id":"claim","expected_revision":1,"worker_id":"worker"}"#,
        );
        call(
            &store,
            "automation.put",
            r#"{"id":"profile","request_id":"disable","expected_revision":1,"enabled":false}"#,
        );
        let result = call(&store, "enhancements.get", r#"{"id":"e"}"#);
        assert_eq!(
            text(&store, &result, "$.item.state"),
            if automatic {
                "cancel_requested"
            } else {
                "running"
            }
        );
        if automatic {
            call(
                &store,
                "enhancements.complete",
                r#"{"id":"e","request_id":"ack","expected_revision":3,"worker_id":"worker","failure":"Provider returned after cancellation"}"#,
            );
            assert_eq!(
                text(
                    &store,
                    &call(&store, "enhancements.get", r#"{"id":"e"}"#),
                    "$.item.state"
                ),
                "cancelled"
            );
        }
    }
}

#[test]
fn provider_changes_before_claim_require_a_fresh_preparation() {
    let (store, clock) = fixture();
    save(&store, 0, "Investigate E42", "backlog", "[]");
    clock.store(1001, Ordering::SeqCst);
    call(&store, "automation.prepare", PREPARE);
    call(
        &store,
        "automation.put",
        r#"{"id":"profile","request_id":"change","expected_revision":1,"enabled":true,"provider":"local","model":"new-model"}"#,
    );
    assert_eq!(
        store
            .workbench_request(
                "enhancements.claim",
                r#"{"id":"e","request_id":"claim","expected_revision":1,"worker_id":"worker"}"#
            )
            .unwrap_err()
            .code,
        "revision_conflict"
    );
}

#[test]
fn text_saves_debounce_durably_while_board_moves_only_refresh_the_queued_revision() {
    let (store, clock) = fixture();
    let first = save(&store, 0, "Investigate E42", "backlog", "[]");
    assert_eq!(number(&store, &first, "$.automatic_enhancement_queued"), 1);
    let queue = call(&store, "automation.list", "{}");
    assert_eq!(
        number(&store, &queue, "$.items[0].not_before_ms"),
        1_001_000
    );
    let moved = save(&store, 1, "Investigate E42", "ready", "[]");
    assert_eq!(number(&store, &moved, "$.automatic_enhancement_queued"), 0);
    let queue = call(&store, "automation.list", "{}");
    assert_eq!(number(&store, &queue, "$.items[0].revision"), 2);
    assert_eq!(
        number(&store, &queue, "$.items[0].not_before_ms"),
        1_001_000
    );
    clock.store(1001, Ordering::SeqCst);
    save(&store, 2, "Investigate E42 on startup", "ready", "[]");
    let queue = call(&store, "automation.list", "{}");
    assert_eq!(
        number(&store, &queue, "$.items[0].not_before_ms"),
        1_002_000
    );
    assert_eq!(count(&store), 1);
    save(
        &store,
        3,
        "Investigate E42 on startup",
        "ready",
        r#"["title","description"]"#,
    );
    assert_eq!(count(&store), 0);
}

#[test]
fn preparing_is_atomic_idempotent_and_acceptance_never_enqueues_itself() {
    let (store, clock) = fixture();
    save(
        &store,
        0,
        "Investigate E42",
        "backlog",
        r#"["description"]"#,
    );
    assert_eq!(
        store
            .workbench_request("automation.prepare", PREPARE)
            .unwrap_err()
            .code,
        "not_ready"
    );
    assert_eq!(count(&store), 1);
    clock.store(1001, Ordering::SeqCst);
    let prepared = call(&store, "automation.prepare", PREPARE);
    assert_eq!(call(&store, "automation.prepare", PREPARE), prepared);
    assert_eq!(number(&store, &prepared, "$.item.automatic"), 1);
    assert_eq!(text(&store, &prepared, "$.item.fields[0]"), "title");
    assert_eq!(count(&store), 0);
    call(
        &store,
        "enhancements.complete",
        r#"{"id":"e","request_id":"complete","expected_revision":1,"title":"Investigate E42 during startup"}"#,
    );
    call(
        &store,
        "enhancements.accept",
        r#"{"id":"e","request_id":"accept","expected_revision":2,"expected_task_revision":1,"fields":["title"]}"#,
    );
    assert_eq!(count(&store), 0);
    assert_eq!(
        number(
            &store,
            &call(&store, "items.get", r#"{"id":"t"}"#),
            "$.item.revision"
        ),
        2
    );
}

#[test]
fn settings_changes_and_refused_preparation_preserve_authority_and_pending_work() {
    let (store, clock) = fixture();
    save(&store, 0, "Investigate E42", "backlog", "[]");
    let settings = call(
        &store,
        "automation.put",
        r#"{"id":"profile","request_id":"settings","expected_revision":1,"enabled":false}"#,
    );
    assert_eq!(number(&store, &settings, "$.item.enabled"), 0);
    assert_eq!(count(&store), 0);
    assert_eq!(
        number(
            &store,
            &save(&store, 1, "Investigate E42 again", "backlog", "[]"),
            "$.automatic_enhancement_queued"
        ),
        0
    );
    call(
        &store,
        "automation.put",
        r#"{"id":"profile","request_id":"enable","expected_revision":2,"enabled":true,"provider":"local","model":"explicit-model"}"#,
    );
    save(&store, 2, "Investigate E42 carefully", "backlog", "[]");
    clock.store(1002, Ordering::SeqCst);
    assert_eq!(
        store
            .workbench_request("automation.prepare", PREPARE)
            .unwrap_err()
            .code,
        "revision_conflict"
    );
    assert_eq!(count(&store), 1);
    assert_eq!(store.workbench_request("automation.put",r#"{"id":"profile","request_id":"invalid","expected_revision":3,"enabled":true,"provider":"local"}"#).unwrap_err().code,"invalid_input");
    call(
        &store,
        "items.delete",
        r#"{"id":"t","request_id":"delete","expected_revision":3}"#,
    );
    assert_eq!(count(&store), 0);
}

#[test]
fn storage_failure_rolls_back_task_queue_and_preparation_together() {
    let (store, clock) = fixture();
    store.connection().execute_batch("CREATE TRIGGER fail_save BEFORE INSERT ON work_requests BEGIN SELECT RAISE(ABORT,'injected commit failure'); END;").unwrap();
    assert_eq!(store.workbench_request("items.put",r#"{"id":"t","request_id":"save","expected_revision":0,"title":"Investigate E42","repository_ids":["r"],"primary_repository_id":"r"}"#).unwrap_err().code,"store_error");
    assert_eq!(count(&store), 0);
    assert_eq!(
        store
            .workbench_request("items.get", r#"{"id":"t"}"#)
            .unwrap_err()
            .code,
        "not_found"
    );
    store
        .connection()
        .execute_batch("DROP TRIGGER fail_save;")
        .unwrap();
    save(&store, 0, "Investigate E42", "backlog", "[]");
    clock.store(1001, Ordering::SeqCst);
    store.connection().execute_batch("CREATE TRIGGER fail_prepare BEFORE INSERT ON work_events WHEN new.kind='automation.prepare' BEGIN SELECT RAISE(ABORT,'injected prepare failure'); END;").unwrap();
    assert_eq!(
        store
            .workbench_request("automation.prepare", PREPARE)
            .unwrap_err()
            .code,
        "store_error"
    );
    assert_eq!(count(&store), 1);
    assert_eq!(
        store
            .workbench_request("enhancements.get", r#"{"id":"e"}"#)
            .unwrap_err()
            .code,
        "not_found"
    );
}

#[test]
fn failed_disabling_rolls_back_cancellation_settings_and_the_queue() {
    let (store, clock) = fixture();
    save(&store, 0, "Investigate E42", "backlog", "[]");
    clock.store(1001, Ordering::SeqCst);
    call(&store, "automation.prepare", PREPARE);
    save(&store, 1, "Investigate E42 again", "backlog", "[]");
    let before = call(&store, "automation.get", r#"{"id":"profile"}"#);
    store.connection().execute_batch("CREATE TRIGGER fail_disable BEFORE INSERT ON work_events WHEN new.kind='automation.put' BEGIN SELECT RAISE(ABORT,'injected settings failure'); END;").unwrap();
    assert_eq!(
        store
            .workbench_request(
                "automation.put",
                r#"{"id":"profile","request_id":"disable","expected_revision":1,"enabled":false}"#
            )
            .unwrap_err()
            .code,
        "store_error"
    );
    assert_eq!(
        call(&store, "automation.get", r#"{"id":"profile"}"#),
        before
    );
    let proposal = call(&store, "enhancements.get", r#"{"id":"e"}"#);
    assert_eq!(text(&store, &proposal, "$.item.state"), "pending");
    assert_eq!(number(&store, &proposal, "$.item.revision"), 1);
    assert_eq!(count(&store), 1);
}

#[test]
fn settings_reject_ambiguous_overrides_and_preserve_explicit_selection() {
    let (store, _) = fixture();
    let original = call(&store, "automation.get", r#"{"id":"profile"}"#);
    for fields in [
        r#""enabled":null"#,
        r#""enabled":"true""#,
        r#""enabled":true,"provider":"local""#,
        r#""enabled":true,"provider":"local","model":null"#,
        r#""enabled":true,"provider":"local","model":" ""#,
        r#""enabled":true,"provider":null"#,
        r#""enabled":true,"force":true"#,
    ] {
        let request =
            format!(r#"{{"id":"profile","request_id":"bad","expected_revision":1,{fields}}}"#);
        assert_eq!(
            store
                .workbench_request("automation.put", &request)
                .unwrap_err()
                .code,
            "invalid_input"
        );
        assert_eq!(
            call(&store, "automation.get", r#"{"id":"profile"}"#),
            original
        );
    }
    call(
        &store,
        "automation.put",
        r#"{"id":"profile","request_id":"choose","expected_revision":1,"enabled":true,"provider":"local","model":"chosen-model"}"#,
    );
    let disabled = call(
        &store,
        "automation.put",
        r#"{"id":"profile","request_id":"disable","expected_revision":2,"enabled":false}"#,
    );
    assert_eq!(text(&store, &disabled, "$.item.model"), "chosen-model");
    let cleared = call(
        &store,
        "automation.put",
        r#"{"id":"profile","request_id":"clear","expected_revision":3,"enabled":true,"provider":null,"model":null}"#,
    );
    assert!(cleared.contains(r#""provider":null,"model":null"#));
}

#[test]
fn automatic_preparation_reuses_the_durable_active_slot_and_hourly_limit() {
    let (store, clock) = fixture();
    save(&store, 0, "Investigate E42", "backlog", "[]");
    call(
        &store,
        "items.put",
        r#"{"id":"other","request_id":"other","expected_revision":0,"title":"Another task","repository_ids":["r"],"primary_repository_id":"r"}"#,
    );
    call(
        &store,
        "enhancements.create",
        r#"{"id":"manual","request_id":"manual","expected_revision":0,"task_id":"other","source_revision":1,"fields":["title"],"provider":"local","model":"fixture-model"}"#,
    );
    clock.store(1001, Ordering::SeqCst);
    assert_eq!(
        store
            .workbench_request("automation.prepare", PREPARE)
            .unwrap_err()
            .code,
        "busy"
    );
    assert_eq!(count(&store), 1);
    call(
        &store,
        "enhancements.dismiss",
        r#"{"id":"manual","request_id":"dismiss","expected_revision":1}"#,
    );
    for i in 0..20 {
        clock.store(1001 + i, Ordering::SeqCst);
        if i > 0 {
            save(
                &store,
                i,
                &format!("Investigate E42 attempt {i}"),
                "backlog",
                "[]",
            );
        }
        clock.store(1002 + i, Ordering::SeqCst);
        let revision = i + 1;
        let input = format!(
            r#"{{"id":"auto-{i}","request_id":"prepare-{i}","expected_revision":0,"task_id":"t","expected_task_revision":{revision},"expected_settings_revision":1,"provider":"local","model":"fixture-model"}}"#
        );
        let prepared = call(&store, "automation.prepare", &input);
        assert_eq!(call(&store, "automation.prepare", &input), prepared);
        call(
            &store,
            "enhancements.complete",
            &format!(
                r#"{{"id":"auto-{i}","request_id":"finish-{i}","expected_revision":1,"failure":"Fixture provider unavailable"}}"#
            ),
        );
    }
    save(&store, 20, "Investigate E42 after quota", "backlog", "[]");
    clock.store(1023, Ordering::SeqCst);
    let blocked = PREPARE.replace(
        "\"expected_task_revision\":1",
        "\"expected_task_revision\":21",
    );
    assert_eq!(
        store
            .workbench_request("automation.prepare", &blocked)
            .unwrap_err()
            .code,
        "enhancement_limit"
    );
    assert_eq!(count(&store), 1);
    clock.store(4602, Ordering::SeqCst);
    call(&store, "automation.prepare", &blocked);
    assert_eq!(count(&store), 0);
}

#[test]
fn queued_work_survives_restart_and_concurrent_hosts_prepare_only_once() {
    use std::sync::Barrier;
    let (store, _) = fixture();
    let saved = save(&store, 0, "Investigate E42", "backlog", "[]");
    let path = std::env::temp_dir().join(format!(
        "manvi-automatic-race-{}.sqlite",
        std::process::id()
    ));
    store
        .connection()
        .execute("VACUUM INTO ?1", [path.to_str().unwrap()])
        .unwrap();
    drop(store);
    let barrier = Arc::new(Barrier::new(2));
    let threads: Vec<_> = (0..2)
        .map(|i| {
            let path = path.clone();
            let barrier = barrier.clone();
            std::thread::spawn(move || {
                let mut store = Store::open(path).unwrap();
                store.set_clock(|| 1001);
                barrier.wait();
                let input = PREPARE
                    .replace("\"e\"", &format!("\"e-{i}\""))
                    .replace("\"prepare\"", &format!("\"prepare-{i}\""));
                (
                    input.clone(),
                    store.workbench_request("automation.prepare", &input),
                )
            })
        })
        .collect();
    let results: Vec<_> = threads
        .into_iter()
        .map(|thread| thread.join().unwrap())
        .collect();
    assert_eq!(
        results.iter().filter(|(_, result)| result.is_ok()).count(),
        1
    );
    let store = Store::open(&path).unwrap();
    let (input, result) = results.iter().find(|(_, result)| result.is_ok()).unwrap();
    assert_eq!(
        &call(&store, "automation.prepare", input),
        result.as_ref().unwrap()
    );
    assert_eq!(count(&store), 0);
    assert_eq!(save(&store, 0, "Investigate E42", "backlog", "[]"), saved);
    assert_eq!(count(&store), 0, "retrying a saved task enqueued it again");
    assert_eq!(
        number(&store, &call(&store, "enhancements.list", "{}"), "$.total"),
        1
    );
    drop(store);
    std::fs::remove_file(path).unwrap();
}

#[test]
fn queued_pages_are_bounded_and_workspace_changes_keep_current_task_revisions() {
    let (store, clock) = fixture();
    for i in 0..201 {
        call(
            &store,
            "items.put",
            &format!(
                r#"{{"id":"t-{i}","request_id":"save-{i}","expected_revision":0,"title":"Investigate E42","description":"Do not load all task details","repository_ids":["r"],"primary_repository_id":"r"}}"#
            ),
        );
    }
    let first = call(&store, "automation.list", r#"{"limit":200}"#);
    assert_eq!(number(&store, &first, "$.total"), 201);
    assert_eq!(number(&store, &first, "$.shown"), 200);
    assert_eq!(number(&store, &first, "$.has_more"), 1);
    assert!(!first.contains("Do not load all task details"));
    let cursor = text(&store, &first, "$.next_cursor");
    let last = call(
        &store,
        "automation.list",
        &format!(r#"{{"limit":200,"cursor":"{cursor}"}}"#),
    );
    assert_eq!(number(&store, &last, "$.shown"), 1);
    assert_eq!(number(&store, &last, "$.has_more"), 0);
    call(
        &store,
        "workspaces.put",
        r#"{"id":"w","request_id":"workspace","expected_revision":0,"name":"Devtools","repository_ids":["r"]}"#,
    );
    call(
        &store,
        "items.put",
        r#"{"id":"t","request_id":"task","expected_revision":0,"title":"Investigate E42","repository_ids":["r"],"primary_repository_id":"r","home_workspace_id":"w"}"#,
    );
    call(
        &store,
        "workspaces.delete",
        r#"{"id":"w","request_id":"delete","expected_revision":1}"#,
    );
    let queued = call(&store, "automation.list", r#"{"task_id":"t"}"#);
    assert_eq!(number(&store, &queued, "$.total"), 1);
    assert_eq!(number(&store, &queued, "$.items[0].revision"), 2);
    assert_eq!(
        number(&store, &queued, "$.items[0].not_before_ms"),
        1_001_000
    );
    clock.store(1001, Ordering::SeqCst);
    call(
        &store,
        "automation.prepare",
        &PREPARE.replace(
            "\"expected_task_revision\":1",
            "\"expected_task_revision\":2",
        ),
    );
}

#[test]
fn schema_three_upgrade_is_atomic_and_does_not_enqueue_existing_tasks() {
    let (store, _) = fixture();
    save(&store, 0, "Investigate E42", "backlog", "[]");
    let before = call(&store, "items.get", r#"{"id":"t"}"#);
    store
        .connection()
        .execute_batch("DROP TABLE work_decisions; DROP TABLE work_notification_deliveries; DROP TABLE work_notification_settings; DROP TABLE work_attention; DROP TABLE work_run_inputs; DROP TABLE work_runs; DROP TABLE work_automation; UPDATE work_meta SET version=3 WHERE id=1;")
        .unwrap();
    // The queue table already exists: creation must roll the settings table back.
    assert_eq!(
        store
            .workbench_request("automation.get", r#"{"id":"profile"}"#)
            .unwrap_err()
            .code,
        "store_error"
    );
    assert_eq!(
        store
            .connection()
            .query_row("SELECT version FROM work_meta WHERE id=1", [], |r| r
                .get::<_, i64>(0))
            .unwrap(),
        3
    );
    assert_eq!(
        store
            .connection()
            .query_row(
                "SELECT count(*) FROM sqlite_master WHERE name='work_automation'",
                [],
                |r| r.get::<_, i64>(0)
            )
            .unwrap(),
        0
    );
    store
        .connection()
        .execute_batch("DROP TABLE work_enhancement_queue;")
        .unwrap();
    assert_eq!(call(&store, "items.get", r#"{"id":"t"}"#), before);
    assert_eq!(count(&store), 0);
    assert_eq!(
        number(
            &store,
            &call(&store, "automation.get", r#"{"id":"profile"}"#),
            "$.item.revision"
        ),
        1
    );
}
