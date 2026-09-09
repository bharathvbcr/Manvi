use dc_store::Store;
use std::sync::{
    Arc,
    atomic::{AtomicI64, Ordering},
};

fn call(s: &Store, method: &str, input: &str) -> String {
    s.workbench_request(method, input)
        .unwrap_or_else(|e| panic!("{method}: {e}"))
}
fn text(s: &Store, raw: &str, path: &str) -> String {
    s.connection()
        .query_row("SELECT json_extract(?1,?2)", [raw, path], |r| r.get(0))
        .unwrap()
}
fn number(s: &Store, raw: &str, path: &str) -> i64 {
    s.connection()
        .query_row("SELECT json_extract(?1,?2)", [raw, path], |r| r.get(0))
        .unwrap()
}
fn fixture() -> (Store, Arc<AtomicI64>) {
    let mut s = Store::open_in_memory().unwrap();
    let clock = Arc::new(AtomicI64::new(1000));
    let time = clock.clone();
    s.set_clock(move || time.load(Ordering::SeqCst));
    let cwd = if cfg!(windows) {
        "C:/checkout"
    } else {
        "/checkout"
    };
    call(
        &s,
        "repositories.put",
        &format!(
            r#"{{"id":"r","request_id":"repo","expected_revision":0,"name":"Repository","identity_key":"local:{cwd}/.git"}}"#
        ),
    );
    call(
        &s,
        "items.put",
        r#"{"id":"t","request_id":"task","expected_revision":0,"title":"Private task title","repository_ids":["r"],"primary_repository_id":"r"}"#,
    );
    (s, clock)
}
fn ready(s: &Store, id: &str) -> String {
    call(
        s,
        "enhancements.create",
        &format!(
            r#"{{"id":"{id}","request_id":"create-{id}","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"configured"}}"#
        ),
    );
    call(
        s,
        "enhancements.complete",
        &format!(
            r#"{{"id":"{id}","request_id":"ready-{id}","expected_revision":1,"title":"Clarified private title","rationale":"Clarify intent"}}"#
        ),
    )
}
fn first(s: &Store) -> String {
    let page = call(s, "attention.list", "{}");
    text(s, &page, "$.items[0].id")
}

#[test]
fn eligible_results_create_one_durable_private_notice_in_the_same_transaction() {
    let (s, _) = fixture();
    assert_eq!(number(&s, &call(&s, "attention.list", "{}"), "$.total"), 0);
    let result = ready(&s, "p");
    let input = r#"{"id":"p","request_id":"ready-p","expected_revision":1,"title":"Clarified private title","rationale":"Clarify intent"}"#;
    assert_eq!(call(&s, "enhancements.complete", input), result);
    let page = call(&s, "attention.list", "{}");
    assert_eq!(number(&s, &page, "$.total"), 1);
    assert_eq!(text(&s, &page, "$.items[0].task_id"), "t");
    assert_eq!(text(&s, &page, "$.items[0].kind"), "enhancement_ready");
    assert_eq!(
        number(&s, &page, "$.items[0].source_sequence"),
        number(&s, &result, "$.sequence")
    );
    assert!(!page.contains("Private task") && !page.contains("Clarified private"));
    s.connection().execute_batch("CREATE TRIGGER attention_disk_failure BEFORE INSERT ON work_attention BEGIN SELECT RAISE(ABORT,'disk failure'); END;").unwrap();
    call(
        &s,
        "enhancements.create",
        r#"{"id":"q","request_id":"q","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"configured"}"#,
    );
    let events = call(&s, "events.list", "{}");
    assert_eq!(
        s.workbench_request(
            "enhancements.complete",
            r#"{"id":"q","request_id":"q-ready","expected_revision":1,"title":"Better title"}"#
        )
        .unwrap_err()
        .code,
        "store_error"
    );
    assert_eq!(
        text(
            &s,
            &call(&s, "enhancements.get", r#"{"id":"q"}"#),
            "$.item.state"
        ),
        "pending"
    );
    assert_eq!(call(&s, "events.list", "{}"), events);
}

#[test]
fn reading_dismissing_and_snoozing_never_accepts_a_proposal_or_task() {
    let (s, time) = fixture();
    ready(&s, "p");
    let id = first(&s);
    let input =
        format!(r#"{{"id":"{id}","request_id":"seen","expected_revision":1,"action":"read"}}"#);
    let receipt = call(&s, "attention.update", &input);
    assert_eq!(call(&s, "attention.update", &input), receipt);
    assert_eq!(
        number(
            &s,
            &call(&s, "attention.list", r#"{"filter":"unread"}"#),
            "$.total"
        ),
        0
    );
    assert_eq!(number(&s, &call(&s, "attention.list", "{}"), "$.total"), 1);
    call(
        &s,
        "attention.update",
        &format!(
            r#"{{"id":"{id}","request_id":"snooze","expected_revision":2,"action":"snooze","seconds":60}}"#
        ),
    );
    assert_eq!(number(&s, &call(&s, "attention.list", "{}"), "$.total"), 0);
    time.store(1060, Ordering::SeqCst);
    assert_eq!(number(&s, &call(&s, "attention.list", "{}"), "$.total"), 1);
    call(
        &s,
        "attention.update",
        &format!(
            r#"{{"id":"{id}","request_id":"dismiss","expected_revision":3,"action":"dismiss"}}"#
        ),
    );
    assert_eq!(number(&s, &call(&s, "attention.list", "{}"), "$.total"), 0);
    assert_eq!(
        number(
            &s,
            &call(&s, "attention.list", r#"{"filter":"all"}"#),
            "$.total"
        ),
        1
    );
    assert_eq!(
        text(
            &s,
            &call(&s, "enhancements.get", r#"{"id":"p"}"#),
            "$.item.state"
        ),
        "ready"
    );
    assert_eq!(
        text(&s, &call(&s, "items.get", r#"{"id":"t"}"#), "$.item.status"),
        "inbox"
    );
}

#[test]
fn current_target_validation_survives_edits_and_deletion() {
    let (s, _) = fixture();
    ready(&s, "p");
    let id = first(&s);
    let get = format!(r#"{{"id":"{id}"}}"#);
    assert_eq!(
        text(&s, &call(&s, "attention.get", &get), "$.item.target_status"),
        "current"
    );
    call(
        &s,
        "enhancements.dismiss",
        r#"{"id":"p","request_id":"dismiss","expected_revision":2}"#,
    );
    assert_eq!(
        text(&s, &call(&s, "attention.get", &get), "$.item.target_status"),
        "changed"
    );
    call(
        &s,
        "items.delete",
        r#"{"id":"t","request_id":"delete","expected_revision":1}"#,
    );
    assert_eq!(
        text(&s, &call(&s, "attention.get", &get), "$.item.target_status"),
        "task_deleted"
    );
    assert_eq!(
        number(
            &s,
            &call(&s, "attention.list", r#"{"filter":"all"}"#),
            "$.total"
        ),
        1
    );
}

#[test]
fn workspaces_deduplicate_and_pages_do_not_repeat_after_new_arrivals() {
    let (s, _) = fixture();
    for group in ["a", "b"] {
        call(
            &s,
            "workspaces.put",
            &format!(
                r#"{{"id":"{group}","request_id":"group-{group}","expected_revision":0,"name":"Group {group}","repository_ids":["r"]}}"#
            ),
        );
    }
    for p in ["p", "q", "r"] {
        ready(&s, p);
    }
    let page = call(&s, "attention.list", r#"{"workspace_id":"a","limit":2}"#);
    assert_eq!(number(&s, &page, "$.total"), 3);
    let cursor = text(&s, &page, "$.next_cursor");
    ready(&s, "later");
    let next = call(
        &s,
        "attention.list",
        &format!(r#"{{"workspace_id":"a","limit":2,"cursor":"{cursor}"}}"#),
    );
    assert_eq!(number(&s, &next, "$.shown"), 1);
    assert_eq!(text(&s, &next, "$.items[0].target_id"), "p");
    assert_eq!(number(&s, &next, "$.total"), 4);
    assert_eq!(
        number(
            &s,
            &call(&s, "attention.list", r#"{"workspace_id":"b"}"#),
            "$.total"
        ),
        4
    );
    assert_eq!(
        number(
            &s,
            &call(&s, "attention.list", r#"{"repository_id":"r"}"#),
            "$.total"
        ),
        4
    );
}

#[test]
fn stale_updates_invalid_scopes_and_permission_fields_are_refused() {
    let (s, _) = fixture();
    ready(&s, "p");
    let id = first(&s);
    for action in [
        r#""action":"approve""#,
        r#""action":"snooze","seconds":0"#,
        r#""action":"snooze","seconds":604801"#,
        r#""action":"read","permission_mode":"bypass""#,
        r#""action":"dismiss","seconds":60"#,
    ] {
        assert_eq!(
            s.workbench_request(
                "attention.update",
                &format!(r#"{{"id":"{id}","request_id":"bad","expected_revision":1,{action}}}"#)
            )
            .unwrap_err()
            .code,
            "invalid_input"
        );
    }
    assert_eq!(
        s.workbench_request(
            "attention.update",
            &format!(
                r#"{{"id":"{id}","request_id":"stale","expected_revision":0,"action":"read"}}"#
            )
        )
        .unwrap_err()
        .code,
        "revision_conflict"
    );
    for raw in [
        r#"{"limit":0}"#,
        r#"{"limit":201}"#,
        r#"{"filter":"resolved"}"#,
        r#"{"workspace_id":"a","repository_id":"r"}"#,
        r#"{"cursor":"bad"}"#,
    ] {
        assert_eq!(
            s.workbench_request("attention.list", raw).unwrap_err().code,
            "invalid_input"
        );
    }
}

#[test]
fn native_run_outcome_is_a_notice_and_does_not_mean_task_completion() {
    let (s, _) = fixture();
    let cwd = if cfg!(windows) {
        "C:/checkout"
    } else {
        "/checkout"
    };
    call(
        &s,
        "runs.prepare",
        &format!(
            r#"{{"id":"run","request_id":"prepare","expected_revision":0,"task_id":"t","source_revision":1,"repository_id":"r","repository_revision":1,"provider":"codex","permission_mode":"ask","cwd":"{cwd}","git_dir":"{cwd}/.git","git_common_dir":"{cwd}/.git","head_oid":null}}"#
        ),
    );
    call(
        &s,
        "runs.claim",
        r#"{"id":"run","request_id":"claim","expected_revision":1,"owner_id":"host","session_id":"session"}"#,
    );
    call(
        &s,
        "runs.finish",
        r#"{"id":"run","request_id":"failed","expected_revision":2,"owner_id":"host","session_id":"session","outcome":"failed","reason":"Failed before spawn"}"#,
    );
    let page = call(&s, "attention.list", "{}");
    assert_eq!(text(&s, &page, "$.items[0].kind"), "run_failed");
    assert_eq!(text(&s, &page, "$.items[0].target_type"), "run");
    assert_eq!(
        text(&s, &call(&s, "items.get", r#"{"id":"t"}"#), "$.item.status"),
        "inbox"
    );
}

#[test]
fn schema_upgrade_preserves_existing_history_without_replaying_old_banners() {
    let (s, _) = fixture();
    ready(&s, "old");
    let events = call(&s, "events.list", "{}");
    s.connection().execute_batch("DROP TABLE work_decisions; DROP TABLE work_notification_deliveries; DROP TABLE work_notification_settings; DROP TABLE work_attention; DELETE FROM work_revisions WHERE entity_type='attention'; UPDATE work_meta SET version=5;").unwrap();
    assert_eq!(number(&s, &call(&s, "attention.list", "{}"), "$.total"), 0);
    assert_eq!(call(&s, "events.list", "{}"), events);
    assert_eq!(
        text(
            &s,
            &call(&s, "enhancements.get", r#"{"id":"old"}"#),
            "$.item.state"
        ),
        "ready"
    );
    ready(&s, "new");
    assert_eq!(number(&s, &call(&s, "attention.list", "{}"), "$.total"), 1);
}

#[test]
fn task_changes_are_explicit_and_acknowledgement_failure_rolls_back() {
    let (s, _) = fixture();
    ready(&s, "p");
    let id = first(&s);
    call(
        &s,
        "items.put",
        r#"{"id":"t","request_id":"edit","expected_revision":1,"title":"Revised task","repository_ids":["r"],"primary_repository_id":"r"}"#,
    );
    let before = call(&s, "attention.get", &format!(r#"{{"id":"{id}"}}"#));
    assert_eq!(text(&s, &before, "$.item.target_status"), "task_changed");
    s.connection().execute_batch("CREATE TRIGGER failed_receipt BEFORE INSERT ON work_requests BEGIN SELECT RAISE(ABORT,'receipt failed'); END;").unwrap();
    assert_eq!(
        s.workbench_request(
            "attention.update",
            &format!(
                r#"{{"id":"{id}","request_id":"read","expected_revision":1,"action":"read"}}"#
            )
        )
        .unwrap_err()
        .code,
        "store_error"
    );
    assert_eq!(
        call(&s, "attention.get", &format!(r#"{{"id":"{id}"}}"#)),
        before
    );
}

#[test]
fn large_result_metadata_does_not_copy_the_source_or_reject_a_valid_completion() {
    let (s, _) = fixture();
    let criteria = vec![format!("\"{}\"", "x".repeat(4090)); 50].join(",");
    call(
        &s,
        "items.put",
        &format!(
            r#"{{"id":"t","request_id":"large-task","expected_revision":1,"title":"Preserve detailed criteria","acceptance_criteria":[{criteria}],"repository_ids":["r"],"primary_repository_id":"r"}}"#
        ),
    );
    call(
        &s,
        "enhancements.create",
        r#"{"id":"large","request_id":"large","expected_revision":0,"task_id":"t","source_revision":2,"fields":["title","description"],"provider":"local","model":"configured"}"#,
    );
    let result = call(
        &s,
        "enhancements.complete",
        &format!(
            r#"{{"id":"large","request_id":"large-ready","expected_revision":1,"title":"Preserve detailed criteria","description":"{}"}}"#,
            "d".repeat(65_536)
        ),
    );
    assert!(result.len() > 256 * 1024);
    let page = call(&s, "attention.list", "{}");
    assert!(page.len() < 4096);
    assert_eq!(number(&s, &page, "$.total"), 1);
}
