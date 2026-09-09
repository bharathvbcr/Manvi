use dc_store::Store;
use rusqlite::params;

fn fixture() -> Store {
    let mut s = Store::open_in_memory().unwrap();
    s.set_clock(|| 1_000);
    call(
        &s,
        "repositories.put",
        r#"{"id":"r","request_id":"repo","expected_revision":0,"name":"Manvi","identity_key":"local:r"}"#,
    );
    call(
        &s,
        "items.put",
        r#"{"id":"t","request_id":"task","expected_revision":0,"title":"fix bug","description":"Exact error: E42. Preserve all constraints.","repository_ids":["r"],"primary_repository_id":"r","acceptance_criteria":["Original criterion"]}"#,
    );
    s
}
fn call(s: &Store, method: &str, raw: &str) -> String {
    s.workbench_request(method, raw)
        .unwrap_or_else(|e| panic!("{method}: {e}"))
}
fn text(s: &Store, raw: &str, field: &str) -> String {
    s.connection()
        .query_row("SELECT json_extract(?1,?2)", [raw, field], |r| r.get(0))
        .unwrap()
}
fn number(s: &Store, raw: &str, field: &str) -> i64 {
    s.connection()
        .query_row("SELECT json_extract(?1,?2)", [raw, field], |r| r.get(0))
        .unwrap()
}
fn refuse(s: &Store, method: &str, raw: &str, code: &str) {
    assert_eq!(
        s.workbench_request(method, raw).unwrap_err().code,
        code,
        "{method}: {raw}"
    );
}
fn create(s: &Store, id: &str) -> String {
    call(
        s,
        "enhancements.create",
        &format!(
            r#"{{"id":"{id}","request_id":"create-{id}","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title","description"],"provider":"local","model":"configured-model"}}"#
        ),
    )
}
fn ready(s: &Store, id: &str) {
    create(s, id);
    call(
        s,
        "enhancements.complete",
        &format!(
            r#"{{"id":"{id}","request_id":"complete-{id}","expected_revision":1,"title":"Resolve E42 while preserving task constraints","description":"Retain the exact error E42 and all existing constraints.","rationale":"Clarify the requested outcome"}}"#
        ),
    );
}

#[test]
fn proposal_filters_and_newest_pages_preserve_ties_counts_and_pending_recovery() {
    let s = fixture();
    for id in ["b", "a", "z"] {
        ready(&s, id);
    }
    let first = call(
        &s,
        "enhancements.list",
        r#"{"newest":true,"states":["ready"],"automatic":false,"limit":2}"#,
    );
    assert_eq!(text(&s, &first, "$.items[0].id"), "z");
    assert_eq!(text(&s, &first, "$.items[1].id"), "b");
    assert_eq!(number(&s, &first, "$.total"), 3);
    assert_eq!(number(&s, &first, "$.has_more"), 1);
    let cursor = text(&s, &first, "$.next_cursor");
    let next = call(
        &s,
        "enhancements.list",
        &format!(
            r#"{{"newest":true,"states":["ready"],"automatic":false,"limit":2,"cursor":"{cursor}"}}"#
        ),
    );
    assert_eq!(text(&s, &next, "$.items[0].id"), "a");
    assert_eq!(number(&s, &next, "$.shown"), 1);
    assert_eq!(number(&s, &next, "$.has_more"), 0);
    call(
        &s,
        "enhancements.create",
        r#"{"id":"automatic","request_id":"auto","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"configured-model","automatic":true}"#,
    );
    let pending = call(
        &s,
        "enhancements.list",
        r#"{"newest":true,"states":["pending","running","cancel_requested"],"automatic":true,"limit":1}"#,
    );
    assert_eq!(number(&s, &pending, "$.total"), 1);
    assert_eq!(text(&s, &pending, "$.items[0].id"), "automatic");
    assert!(!pending.contains("Exact error"));
    assert_eq!(
        number(
            &s,
            &call(
                &s,
                "enhancements.list",
                r#"{"states":["running"],"automatic":true}"#
            ),
            "$.total"
        ),
        0
    );
    for input in [
        r#"{"states":["invented"]}"#,
        r#"{"states":["pending","pending"]}"#,
        r#"{"states":[]}"#,
        r#"{"automatic":null}"#,
        r#"{"automatic":"true"}"#,
    ] {
        refuse(&s, "enhancements.list", input, "invalid_input");
    }
}
fn edit(s: &Store, patch: &str, receipt: &str) {
    let raw = call(s, "items.get", r#"{"id":"t"}"#);
    let input: String = s.connection().query_row("SELECT json_remove(json_patch(json_set(json_extract(?1,'$.item'),'$.request_id',?3,'$.expected_revision',json_extract(?1,'$.item.revision')),?2),'$.revision','$.updated_at','$.due_at')", params![raw, patch, receipt], |r| r.get(0)).unwrap();
    call(s, "items.put", &input);
}

#[test]
fn selected_acceptance_and_undo_preserve_other_fields_and_history() {
    let s = fixture();
    ready(&s, "e");
    edit(
        &s,
        r#"{"status":"review","description":"A newer human description"}"#,
        "human-edit",
    );
    let accepted = call(
        &s,
        "enhancements.accept",
        r#"{"id":"e","request_id":"accept","expected_revision":2,"expected_task_revision":2,"fields":["title"]}"#,
    );
    assert_eq!(text(&s, &accepted, "$.item.state"), "accepted");
    assert_eq!(number(&s, &accepted, "$.item.applied_task_revision"), 3);
    let current = call(&s, "items.get", r#"{"id":"t"}"#);
    assert_eq!(
        text(&s, &current, "$.item.title"),
        "Resolve E42 while preserving task constraints"
    );
    assert_eq!(
        text(&s, &current, "$.item.description"),
        "A newer human description"
    );
    assert_eq!(text(&s, &current, "$.item.status"), "review");
    assert_eq!(
        text(&s, &current, "$.item.acceptance_criteria[0]"),
        "Original criterion"
    );
    call(
        &s,
        "enhancements.undo",
        r#"{"id":"e","request_id":"undo","expected_revision":3,"expected_task_revision":3}"#,
    );
    let current = call(&s, "items.get", r#"{"id":"t"}"#);
    assert_eq!(text(&s, &current, "$.item.title"), "fix bug");
    assert_eq!(
        text(&s, &current, "$.item.description"),
        "A newer human description"
    );
    assert_eq!(
        number(&s, &call(&s, "items.history", r#"{"id":"t"}"#), "$.total"),
        4
    );
}

#[test]
fn revised_suggestions_keep_model_output_and_apply_only_after_acceptance() {
    let s = fixture();
    ready(&s, "e");
    let initial = call(&s, "enhancements.get", r#"{"id":"e"}"#);
    let task = call(&s, "items.get", r#"{"id":"t"}"#);
    let request = r#"{"id":"e","request_id":"revise","expected_revision":2,"title":"Investigate E42 without changing the original constraints"}"#;
    let revised = call(&s, "enhancements.revise", request);
    assert_eq!(call(&s, "enhancements.revise", request), revised);
    assert_eq!(text(&s, &revised, "$.item.state"), "ready");
    assert_eq!(number(&s, &revised, "$.item.revision"), 3);
    assert_eq!(
        text(&s, &revised, "$.item.original_proposed.title"),
        text(&s, &initial, "$.item.proposed.title")
    );
    assert_eq!(
        text(&s, &revised, "$.item.proposed.description"),
        text(&s, &initial, "$.item.proposed.description")
    );
    assert_eq!(text(&s, &revised, "$.item.edited_fields[0]"), "title");
    assert_eq!(call(&s, "items.get", r#"{"id":"t"}"#), task);
    let summary = call(&s, "enhancements.list", r#"{"task_id":"t"}"#);
    assert!(!summary.contains("original_proposed"));
    assert!(!summary.contains("source\""));
    call(
        &s,
        "enhancements.accept",
        r#"{"id":"e","request_id":"accept","expected_revision":3,"expected_task_revision":1,"fields":["title"]}"#,
    );
    let accepted = call(&s, "items.get", r#"{"id":"t"}"#);
    assert_eq!(
        text(&s, &accepted, "$.item.title"),
        "Investigate E42 without changing the original constraints"
    );
    assert_eq!(
        text(&s, &accepted, "$.item.description"),
        text(&s, &task, "$.item.description")
    );
    call(
        &s,
        "enhancements.undo",
        r#"{"id":"e","request_id":"undo","expected_revision":4,"expected_task_revision":2}"#,
    );
    assert_eq!(
        text(&s, &call(&s, "items.get", r#"{"id":"t"}"#), "$.item.title"),
        "fix bug"
    );
    let history_count: i64 = s
        .connection()
        .query_row(
            "SELECT count(*) FROM work_revisions WHERE entity_type='enhancement' AND entity_id='e'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(history_count, 5);
}

#[test]
fn suggestion_edits_reject_stale_invalid_locked_and_non_review_states_atomically() {
    let s = fixture();
    create(&s, "e");
    refuse(
        &s,
        "enhancements.revise",
        r#"{"id":"e","request_id":"early","expected_revision":1,"title":"Unfinished"}"#,
        "invalid_transition",
    );
    call(
        &s,
        "enhancements.complete",
        r#"{"id":"e","request_id":"complete","expected_revision":1,"title":"Resolve E42","description":"Preserve evidence"}"#,
    );
    let before = call(&s, "events.list", "{}");
    for input in [
        r#"{"id":"e","request_id":"bad","expected_revision":2}"#,
        r#"{"id":"e","request_id":"bad","expected_revision":2,"title":" "}"#,
        r#"{"id":"e","request_id":"bad","expected_revision":2,"title":"Changed","provider":"different"}"#,
        r#"{"id":"e","request_id":"bad","expected_revision":2,"title":"Changed","original_proposed":{"title":"Forged"}}"#,
        r#"{"id":"e","request_id":"bad","expected_revision":2,"title":null}"#,
        r#"{"id":"e","request_id":"bad","expected_revision":2,"title":"one","\u0074itle":"two"}"#,
    ] {
        refuse(&s, "enhancements.revise", input, "invalid_input");
    }
    refuse(
        &s,
        "enhancements.revise",
        &format!(
            r#"{{"id":"e","request_id":"long","expected_revision":2,"title":"{}"}}"#,
            "a".repeat(301)
        ),
        "invalid_input",
    );
    refuse(
        &s,
        "enhancements.revise",
        r#"{"id":"e","request_id":"stale","expected_revision":1,"title":"New"}"#,
        "revision_conflict",
    );
    assert_eq!(call(&s, "events.list", "{}"), before);
    edit(&s, r#"{"locked_fields":["title"]}"#, "lock");
    refuse(
        &s,
        "enhancements.revise",
        r#"{"id":"e","request_id":"locked","expected_revision":2,"title":"New"}"#,
        "field_locked",
    );
    call(
        &s,
        "enhancements.revise",
        r#"{"id":"e","request_id":"description","expected_revision":2,"description":"Updated review text"}"#,
    );
    let restored = call(
        &s,
        "enhancements.revise",
        r#"{"id":"e","request_id":"restore","expected_revision":3,"description":"Preserve evidence"}"#,
    );
    assert_eq!(number(&s, &restored, "$.item.revision"), 4);
    let edits: i64 = s
        .connection()
        .query_row(
            "SELECT json_array_length(?1,'$.item.edited_fields')",
            [&restored],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(edits, 0);
    assert_eq!(
        text(&s, &restored, "$.item.original_proposed.description"),
        "Preserve evidence"
    );
    call(
        &s,
        "enhancements.dismiss",
        r#"{"id":"e","request_id":"dismiss","expected_revision":4}"#,
    );
    refuse(
        &s,
        "enhancements.revise",
        r#"{"id":"e","request_id":"late","expected_revision":5,"description":"Too late"}"#,
        "invalid_transition",
    );
}

#[test]
fn stale_or_conflicting_acceptance_is_atomic_and_exact_retries_are_idempotent() {
    let s = fixture();
    ready(&s, "e");
    edit(&s, r#"{"title":"Human owns this title"}"#, "edit");
    let before = call(&s, "events.list", "{}");
    refuse(
        &s,
        "enhancements.accept",
        r#"{"id":"e","request_id":"stale","expected_revision":2,"expected_task_revision":1,"fields":["title"]}"#,
        "revision_conflict",
    );
    refuse(
        &s,
        "enhancements.accept",
        r#"{"id":"e","request_id":"conflict","expected_revision":2,"expected_task_revision":2,"fields":["title"]}"#,
        "field_conflict",
    );
    assert_eq!(call(&s, "events.list", "{}"), before);
    let input = r#"{"id":"e","request_id":"accept","expected_revision":2,"expected_task_revision":2,"fields":["description"]}"#;
    let accepted = call(&s, "enhancements.accept", input);
    assert_eq!(call(&s, "enhancements.accept", input), accepted);
    edit(
        &s,
        r#"{"description":"Human replaced the accepted description"}"#,
        "later",
    );
    refuse(
        &s,
        "enhancements.undo",
        r#"{"id":"e","request_id":"undo","expected_revision":3,"expected_task_revision":4}"#,
        "field_conflict",
    );
    let proposal = call(&s, "enhancements.get", r#"{"id":"e"}"#);
    assert_eq!(text(&s, &proposal, "$.item.state"), "accepted");
}

#[test]
fn locked_fields_are_enforced_at_request_and_acceptance() {
    let s = fixture();
    ready(&s, "e");
    edit(&s, r#"{"locked_fields":["title"]}"#, "lock");
    refuse(
        &s,
        "enhancements.accept",
        r#"{"id":"e","request_id":"a","expected_revision":2,"expected_task_revision":2,"fields":["title"]}"#,
        "field_locked",
    );
    refuse(
        &s,
        "enhancements.create",
        r#"{"id":"new","request_id":"new","expected_revision":0,"task_id":"t","source_revision":2,"fields":["title"],"provider":"local","model":"m"}"#,
        "field_locked",
    );
    refuse(
        &s,
        "items.put",
        r#"{"id":"t","request_id":"bad-lock","expected_revision":2,"title":"T","repository_ids":["r"],"primary_repository_id":"r","locked_fields":["push"]}"#,
        "invalid_input",
    );
    // A pre-lock-aware host omits the new field on a complete replacement.
    call(
        &s,
        "items.put",
        r#"{"id":"t","request_id":"old-host","expected_revision":2,"title":"Human edit","repository_ids":["r"],"primary_repository_id":"r"}"#,
    );
    let current = call(&s, "items.get", r#"{"id":"t"}"#);
    assert_eq!(text(&s, &current, "$.item.locked_fields[0]"), "title");
}

#[test]
fn an_event_or_proposal_failure_rolls_back_the_task_and_its_revision() {
    let s = fixture();
    ready(&s, "e");
    let before = call(&s, "events.list", "{}");
    s.connection().execute_batch("CREATE TRIGGER fail_proposal BEFORE UPDATE ON work_enhancements BEGIN SELECT RAISE(ABORT,'simulated storage failure'); END;").unwrap();
    refuse(
        &s,
        "enhancements.accept",
        r#"{"id":"e","request_id":"a","expected_revision":2,"expected_task_revision":1,"fields":["title"]}"#,
        "store_error",
    );
    assert_eq!(call(&s, "events.list", "{}"), before);
    assert_eq!(
        number(&s, &call(&s, "items.history", r#"{"id":"t"}"#), "$.total"),
        1
    );
    assert_eq!(
        text(&s, &call(&s, "items.get", r#"{"id":"t"}"#), "$.item.title"),
        "fix bug"
    );
    s.connection()
        .execute_batch("DROP TRIGGER fail_proposal")
        .unwrap();
    call(
        &s,
        "enhancements.accept",
        r#"{"id":"e","request_id":"a","expected_revision":2,"expected_task_revision":1,"fields":["title"]}"#,
    );
}

#[test]
fn v1_migration_preserves_existing_data_and_refuses_partial_or_future_schemas() {
    let s = fixture();
    let before = call(&s, "items.get", r#"{"id":"t"}"#);
    s.connection()
        .execute_batch("DROP TABLE work_decisions; DROP TABLE work_notification_deliveries; DROP TABLE work_notification_settings; DROP TABLE work_attention; DROP TABLE work_run_inputs; DROP TABLE work_runs; DROP TABLE work_enhancements; DROP TABLE work_enhancement_queue; DROP TABLE work_automation; UPDATE work_meta SET version=1 WHERE id=1;")
        .unwrap();
    assert_eq!(call(&s, "items.get", r#"{"id":"t"}"#), before);
    ready(&s, "migrated");
    assert_eq!(
        s.connection()
            .query_row("SELECT version FROM work_meta WHERE id=1", [], |r| r
                .get::<_, i64>(0))
            .unwrap(),
        10
    );
    s.connection()
        .execute_batch("UPDATE work_meta SET version=999 WHERE id=1")
        .unwrap();
    refuse(&s, "items.get", r#"{"id":"t"}"#, "schema_unsupported");

    let s = fixture();
    s.connection()
        .execute_batch("UPDATE work_meta SET version=1 WHERE id=1")
        .unwrap();
    // A preexisting table with an old version is inconsistent, never a migration
    // success. The version cannot advance while schema creation failed.
    refuse(&s, "items.get", r#"{"id":"t"}"#, "store_error");
    assert_eq!(
        s.connection()
            .query_row("SELECT version FROM work_meta WHERE id=1", [], |r| r
                .get::<_, i64>(0))
            .unwrap(),
        1
    );
}

#[test]
fn independent_connections_cannot_accept_twice_and_receipts_survive_restart() {
    use std::sync::{Arc, Barrier};
    let dir = std::env::temp_dir().join(format!("manvi-enhancement-race-{}", std::process::id()));
    std::fs::create_dir(&dir).unwrap();
    let path = dir.join("profile.sqlite");
    let s = fixture();
    ready(&s, "e");
    s.connection()
        .execute("VACUUM INTO ?1", [path.to_str().unwrap()])
        .unwrap();
    drop(s);
    let barrier = Arc::new(Barrier::new(2));
    let threads: Vec<_> = (0..2).map(|i| {
        let path = path.clone();
        let barrier = barrier.clone();
        std::thread::spawn(move || {
            let s = Store::open(path).unwrap();
            barrier.wait();
            let input = format!(r#"{{"id":"e","request_id":"accept-{i}","expected_revision":2,"expected_task_revision":1,"fields":["title"]}}"#);
            (input.clone(), s.workbench_request("enhancements.accept", &input))
        })
    }).collect();
    let results: Vec<_> = threads.into_iter().map(|t| t.join().unwrap()).collect();
    assert_eq!(results.iter().filter(|(_, r)| r.is_ok()).count(), 1);
    let s = Store::open(&path).unwrap();
    let (request, response) = results.iter().find(|(_, r)| r.is_ok()).unwrap();
    assert_eq!(
        &call(&s, "enhancements.accept", request),
        response.as_ref().unwrap()
    );
    assert_eq!(
        number(&s, &call(&s, "items.history", r#"{"id":"t"}"#), "$.total"),
        2
    );
    drop(s);
    std::fs::remove_dir_all(dir).unwrap();
}

#[test]
fn a_claimed_attempt_has_one_owner_and_expiry_does_not_prove_it_stopped() {
    let mut s = fixture();
    create(&s, "e");
    call(
        &s,
        "enhancements.claim",
        r#"{"id":"e","request_id":"claim","expected_revision":1,"worker_id":"owner"}"#,
    );
    refuse(
        &s,
        "enhancements.claim",
        r#"{"id":"e","request_id":"second","expected_revision":2,"worker_id":"other"}"#,
        "invalid_transition",
    );
    refuse(
        &s,
        "enhancements.complete",
        r#"{"id":"e","request_id":"wrong","expected_revision":2,"worker_id":"other","failure":"failed"}"#,
        "worker_mismatch",
    );
    s.set_clock(|| 1_121);
    refuse(
        &s,
        "enhancements.create",
        r#"{"id":"e2","request_id":"next","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"m"}"#,
        "busy",
    );
    call(
        &s,
        "enhancements.complete",
        r#"{"id":"e","request_id":"stopped","expected_revision":2,"worker_id":"owner","failure":"Generation deadline exceeded"}"#,
    );
    create(&s, "e2");
}

#[test]
fn cancellation_needs_worker_acknowledgment_and_recovery_preserves_uncertainty() {
    let mut s = fixture();
    create(&s, "e");
    call(
        &s,
        "enhancements.claim",
        r#"{"id":"e","request_id":"claim","expected_revision":1,"worker_id":"owner"}"#,
    );
    call(
        &s,
        "enhancements.dismiss",
        r#"{"id":"e","request_id":"dismiss","expected_revision":2}"#,
    );
    assert_eq!(
        text(
            &s,
            &call(&s, "enhancements.get", r#"{"id":"e"}"#),
            "$.item.state"
        ),
        "cancel_requested"
    );
    refuse(
        &s,
        "enhancements.complete",
        r#"{"id":"e","request_id":"late","expected_revision":3,"worker_id":"owner","title":"x","description":"y"}"#,
        "cancel_requested",
    );
    refuse(
        &s,
        "enhancements.recover",
        r#"{"id":"e","request_id":"early","expected_revision":3,"worker_id":"owner","acknowledge_uncertain":true}"#,
        "busy",
    );
    s.set_clock(|| 1_121);
    refuse(
        &s,
        "enhancements.recover",
        r#"{"id":"e","request_id":"bad","expected_revision":3,"worker_id":"owner"}"#,
        "invalid_input",
    );
    let result = call(
        &s,
        "enhancements.recover",
        r#"{"id":"e","request_id":"recover","expected_revision":3,"worker_id":"owner","acknowledge_uncertain":true}"#,
    );
    assert_eq!(text(&s, &result, "$.item.state"), "interrupted");
    assert_eq!(number(&s, &result, "$.item.outcome_uncertain"), 1);
    refuse(
        &s,
        "enhancements.complete",
        r#"{"id":"e","request_id":"stale","expected_revision":4,"worker_id":"owner","failure":"Stopped"}"#,
        "invalid_transition",
    );
    create(&s, "e2");
    call(
        &s,
        "enhancements.claim",
        r#"{"id":"e2","request_id":"claim2","expected_revision":1,"worker_id":"owner2"}"#,
    );
    call(
        &s,
        "enhancements.dismiss",
        r#"{"id":"e2","request_id":"dismiss2","expected_revision":2}"#,
    );
    let result = call(
        &s,
        "enhancements.complete",
        r#"{"id":"e2","request_id":"stopped2","expected_revision":3,"worker_id":"owner2","failure":"Cancelled by user"}"#,
    );
    assert_eq!(text(&s, &result, "$.item.state"), "cancelled");
}

#[test]
fn cancellation_expiry_and_incomplete_output_cannot_be_reported_ready() {
    let mut s = fixture();
    create(&s, "e");
    refuse(
        &s,
        "enhancements.complete",
        r#"{"id":"e","request_id":"partial","expected_revision":1,"title":"Only one field"}"#,
        "invalid_input",
    );
    call(
        &s,
        "enhancements.dismiss",
        r#"{"id":"e","request_id":"cancel","expected_revision":1}"#,
    );
    refuse(
        &s,
        "enhancements.complete",
        r#"{"id":"e","request_id":"late","expected_revision":2,"title":"x","description":"y"}"#,
        "invalid_transition",
    );
    create(&s, "e2");
    s.set_clock(|| 1_120);
    refuse(
        &s,
        "enhancements.complete",
        r#"{"id":"e2","request_id":"expired","expected_revision":1,"title":"x","description":"y"}"#,
        "expired",
    );
    create(&s, "e3");
    call(
        &s,
        "enhancements.complete",
        r#"{"id":"e3","request_id":"failed","expected_revision":1,"failure":"Provider timed out"}"#,
    );
    assert_eq!(
        text(
            &s,
            &call(&s, "enhancements.get", r#"{"id":"e3"}"#),
            "$.item.state"
        ),
        "failed"
    );
    refuse(
        &s,
        "enhancements.accept",
        r#"{"id":"e3","request_id":"bad","expected_revision":2,"expected_task_revision":1,"fields":["title"]}"#,
        "invalid_transition",
    );
}

#[test]
fn profile_capacity_and_automatic_quota_are_durable() {
    let s = fixture();
    let input = r#"{"id":"e","request_id":"create-e","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"m","automatic":true}"#;
    let first = call(&s, "enhancements.create", input);
    assert_eq!(call(&s, "enhancements.create", input), first);
    refuse(
        &s,
        "enhancements.create",
        r#"{"id":"other","request_id":"other","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"m"}"#,
        "busy",
    );
    call(
        &s,
        "enhancements.dismiss",
        r#"{"id":"e","request_id":"cancel-e","expected_revision":1}"#,
    );
    for n in 1..20 {
        call(
            &s,
            "enhancements.create",
            &format!(
                r#"{{"id":"e{n}","request_id":"create-e{n}","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"m","automatic":true}}"#
            ),
        );
        call(
            &s,
            "enhancements.dismiss",
            &format!(r#"{{"id":"e{n}","request_id":"cancel-e{n}","expected_revision":1}}"#),
        );
    }
    refuse(
        &s,
        "enhancements.create",
        r#"{"id":"overflow","request_id":"overflow","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"m","automatic":true}"#,
        "enhancement_limit",
    );
    let page = call(&s, "enhancements.list", r#"{"task_id":"t","limit":3}"#);
    assert_eq!(number(&s, &page, "$.total"), 20);
    assert_eq!(number(&s, &page, "$.shown"), 3);
    assert_eq!(number(&s, &page, "$.has_more"), 1);
}
