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
fn num(s: &Store, raw: &str, path: &str) -> i64 {
    s.connection()
        .query_row("SELECT json_extract(?1,?2)", [raw, path], |r| r.get(0))
        .unwrap()
}
fn fixture() -> (Store, Arc<AtomicI64>) {
    let mut s = Store::open_in_memory().unwrap();
    let clock = Arc::new(AtomicI64::new(1000));
    let time = clock.clone();
    s.set_clock(move || time.load(Ordering::SeqCst));
    call(
        &s,
        "repositories.put",
        r#"{"id":"r","request_id":"repo","expected_revision":0,"name":"Repo","identity_key":"local:repo"}"#,
    );
    call(
        &s,
        "items.put",
        r#"{"id":"t","request_id":"task","expected_revision":0,"title":"Private task","repository_ids":["r"],"primary_repository_id":"r"}"#,
    );
    (s, clock)
}
fn settings(s: &Store, revision: i64, extra: &str) -> String {
    call(
        s,
        "notifications.settings.put",
        &format!(
            r#"{{"id":"profile","request_id":"settings-{revision}","expected_revision":{revision},"enabled":true{extra}}}"#
        ),
    )
}
fn ready(s: &Store, id: &str) -> String {
    call(
        s,
        "enhancements.create",
        &format!(
            r#"{{"id":"{id}","request_id":"create-{id}","expected_revision":0,"task_id":"t","source_revision":1,"fields":["title"],"provider":"local","model":"configured"}}"#
        ),
    );
    let out = call(
        s,
        "enhancements.complete",
        &format!(
            r#"{{"id":"{id}","request_id":"ready-{id}","expected_revision":1,"title":"Better private title"}}"#
        ),
    );
    format!("event-{}", num(s, &out, "$.sequence"))
}
fn pending(s: &Store, minute: i64) -> String {
    call(
        s,
        "notifications.pending.list",
        &format!(r#"{{"minute_of_day":{minute},"limit":3}}"#),
    )
}
fn claim(s: &Store, id: &str) -> String {
    call(
        s,
        "notifications.claim",
        &format!(
            r#"{{"id":"{id}","request_id":"claim-{id}","expected_revision":0,"minute_of_day":720}}"#
        ),
    )
}

#[test]
fn disabled_and_enable_watermark_prevent_a_historical_banner_flood() {
    let (s, _) = fixture();
    let old = ready(&s, "old");
    assert_eq!(num(&s, &pending(&s, 720), "$.total"), 0);
    let initial = call(&s, "notifications.settings.get", r#"{"id":"profile"}"#);
    assert_eq!(num(&s, &initial, "$.item.enabled"), 0);
    settings(&s, 1, "");
    assert_eq!(num(&s, &pending(&s, 720), "$.total"), 0);
    let id = ready(&s, "new");
    let page = pending(&s, 720);
    assert_eq!(num(&s, &page, "$.total"), 1);
    assert_eq!(text(&s, &page, "$.items[0].id"), id);
    assert!(!page.contains("Private task") && !page.contains("Better private"));
    assert_eq!(
        s.workbench_request(
            "notifications.claim",
            &format!(
                r#"{{"id":"{old}","request_id":"old","expected_revision":0,"minute_of_day":720}}"#
            )
        )
        .unwrap_err()
        .code,
        "not_eligible"
    );
}

#[test]
fn uncertain_claim_is_atomic_unique_and_cannot_be_replayed_as_delivery_authority() {
    let (s, _) = fixture();
    settings(&s, 1, "");
    let id = ready(&s, "p");
    s.connection().execute_batch("CREATE TRIGGER broken_delivery_receipt BEFORE INSERT ON work_requests WHEN NEW.method='notifications.claim' BEGIN SELECT RAISE(ABORT,'disk failure'); END;").unwrap();
    let input = format!(
        r#"{{"id":"{id}","request_id":"claim-{id}","expected_revision":0,"minute_of_day":720}}"#
    );
    assert_eq!(
        s.workbench_request("notifications.claim", &input)
            .unwrap_err()
            .code,
        "store_error"
    );
    assert_eq!(num(&s, &pending(&s, 720), "$.total"), 1);
    s.connection()
        .execute_batch("DROP TRIGGER broken_delivery_receipt;")
        .unwrap();
    let record = claim(&s, &id);
    assert_eq!(text(&s, &record, "$.item.state"), "uncertain");
    assert_eq!(num(&s, &pending(&s, 720), "$.total"), 0);
    assert_eq!(
        s.workbench_request("notifications.claim", &input)
            .unwrap_err()
            .code,
        "claim_consumed"
    );
    let out = call(
        &s,
        "notifications.finish",
        &format!(
            r#"{{"id":"{id}","request_id":"finish","expected_revision":1,"state":"submitted"}}"#
        ),
    );
    assert_eq!(text(&s, &out, "$.item.state"), "submitted");
    assert_eq!(
        text(
            &s,
            &call(&s, "enhancements.get", r#"{"id":"p"}"#),
            "$.item.state"
        ),
        "ready"
    );
    assert_eq!(
        num(
            &s,
            &call(&s, "items.get", r#"{"id":"t"}"#),
            "$.item.revision"
        ),
        1
    );
}

#[test]
fn quiet_hours_scope_mutes_and_snooze_are_rechecked_at_claim() {
    let (s, time) = fixture();
    settings(
        &s,
        1,
        r#", "quiet_start":1320,"quiet_end":420,"muted_repository_ids":["r"]"#,
    );
    let id = ready(&s, "p");
    assert_eq!(num(&s, &pending(&s, 720), "$.total"), 0);
    settings(&s, 2, r#", "quiet_start":1320,"quiet_end":420"#);
    assert_eq!(num(&s, &pending(&s, 60), "$.total"), 0);
    assert_eq!(num(&s, &pending(&s, 1320), "$.total"), 0);
    assert_eq!(num(&s, &pending(&s, 420), "$.total"), 1);
    call(
        &s,
        "attention.update",
        &format!(
            r#"{{"id":"{id}","request_id":"snooze","expected_revision":1,"action":"snooze","seconds":60}}"#
        ),
    );
    assert_eq!(num(&s, &pending(&s, 720), "$.total"), 0);
    time.store(1060, Ordering::SeqCst);
    assert_eq!(num(&s, &pending(&s, 720), "$.total"), 1);
    call(
        &s,
        "attention.update",
        &format!(r#"{{"id":"{id}","request_id":"read","expected_revision":2,"action":"read"}}"#),
    );
    assert_eq!(s.workbench_request("notifications.claim",&format!(r#"{{"id":"{id}","request_id":"late-claim","expected_revision":0,"minute_of_day":720}}"#)).unwrap_err().code,"not_eligible");
}

#[test]
fn activation_identity_is_durable_and_acknowledgment_never_accepts_work() {
    let (s, _) = fixture();
    settings(&s, 1, "");
    let id = ready(&s, "p");
    let delivery = claim(&s, &id);
    let native = text(&s, &delivery, "$.item.native_id");
    let activate = format!(
        r#"{{"id":"{id}","request_id":"activate-{id}","expected_revision":1,"native_id":"{native}"}}"#
    );
    let forged = activate.replace(&native, "gitpulse.other.event-1");
    assert_eq!(
        s.workbench_request("notifications.activate", &forged)
            .unwrap_err()
            .code,
        "invalid_input"
    );
    let response = call(&s, "notifications.activate", &activate);
    assert_eq!(call(&s, "notifications.activate", &activate), response);
    assert_eq!(
        num(
            &s,
            &call(&s, "notifications.activations.list", "{}"),
            "$.total"
        ),
        1
    );
    call(
        &s,
        "notifications.ack",
        &format!(r#"{{"id":"{id}","request_id":"ack","expected_revision":2}}"#),
    );
    assert_eq!(
        num(
            &s,
            &call(&s, "notifications.activations.list", "{}"),
            "$.total"
        ),
        0
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
        num(
            &s,
            &call(&s, "attention.get", &format!(r#"{{"id":"{id}"}}"#)),
            "$.item.revision"
        ),
        1
    );
}

#[test]
fn stale_targets_expired_notices_and_invalid_settings_are_never_delivered() {
    let (s, time) = fixture();
    settings(&s, 1, "");
    let id = ready(&s, "p");
    for extra in [
        r#", "quiet_start":2"#,
        r#", "quiet_start":60,"quiet_end":60"#,
        r#", "quiet_start":1440,"quiet_end":60"#,
        r#", "muted_repository_ids":["absent"]"#,
        r#", "skip_permissions":true"#,
    ] {
        assert!(s.workbench_request("notifications.settings.put",&format!(r#"{{"id":"profile","request_id":"bad","expected_revision":2,"enabled":true{extra}}}"#)).is_err());
    }
    time.store(4601, Ordering::SeqCst);
    assert_eq!(num(&s, &pending(&s, 720), "$.total"), 0);
    time.store(1000, Ordering::SeqCst);
    call(
        &s,
        "enhancements.dismiss",
        r#"{"id":"p","request_id":"dismiss","expected_revision":2}"#,
    );
    assert_eq!(num(&s, &pending(&s, 720), "$.total"), 0);
    assert!(
        s.workbench_request(
            "notifications.claim",
            &format!(
                r#"{{"id":"{id}","request_id":"claim","expected_revision":0,"minute_of_day":720}}"#
            )
        )
        .is_err()
    );
}

#[test]
fn schema_six_migration_is_atomic_and_keeps_old_notices_without_enabling_delivery() {
    let (s, _) = fixture();
    let id = ready(&s, "p");
    s.connection().execute_batch("DROP TABLE work_decisions; DROP TABLE work_notification_deliveries; DROP TABLE work_notification_settings; DROP INDEX work_attention_delivery_time; UPDATE work_meta SET version=6; CREATE TABLE work_notification_deliveries(conflict TEXT);").unwrap();
    assert_eq!(
        s.workbench_request("notifications.settings.get", r#"{"id":"profile"}"#)
            .unwrap_err()
            .code,
        "store_error"
    );
    assert_eq!(
        s.connection()
            .query_row("SELECT version FROM work_meta", [], |r| r.get::<_, i64>(0))
            .unwrap(),
        6
    );
    assert_eq!(
        s.connection()
            .query_row(
                "SELECT count(*) FROM sqlite_master WHERE name='work_notification_settings'",
                [],
                |r| r.get::<_, i64>(0)
            )
            .unwrap(),
        0
    );
    s.connection()
        .execute_batch("DROP TABLE work_notification_deliveries;")
        .unwrap();
    let settings = call(&s, "notifications.settings.get", r#"{"id":"profile"}"#);
    assert_eq!(num(&s, &settings, "$.item.enabled"), 0);
    assert_eq!(num(&s, &settings, "$.item.updated_at"), 0);
    assert_eq!(
        text(
            &s,
            &call(&s, "attention.get", &format!(r#"{{"id":"{id}"}}"#)),
            "$.item.id"
        ),
        id
    );
    assert_eq!(num(&s, &pending(&s, 720), "$.total"), 0);
}

#[test]
fn workspace_and_task_mutes_cover_shared_repositories_without_duplicate_notices() {
    let (s, _) = fixture();
    call(
        &s,
        "workspaces.put",
        r#"{"id":"w","request_id":"w","expected_revision":0,"name":"Workspace","repository_ids":["r"]}"#,
    );
    call(
        &s,
        "workspaces.put",
        r#"{"id":"other","request_id":"other","expected_revision":0,"name":"Other","repository_ids":["r"]}"#,
    );
    settings(&s, 1, r#", "muted_workspace_ids":["w"]"#);
    ready(&s, "p");
    assert_eq!(num(&s, &pending(&s, 720), "$.total"), 0);
    settings(&s, 2, r#", "muted_task_ids":["t"]"#);
    assert_eq!(num(&s, &pending(&s, 720), "$.total"), 0);
    settings(&s, 3, "");
    assert_eq!(num(&s, &pending(&s, 720), "$.total"), 1);
}

#[test]
fn deleting_muted_scopes_does_not_block_other_preference_changes() {
    let (s, _) = fixture();
    call(
        &s,
        "workspaces.put",
        r#"{"id":"w","request_id":"w","expected_revision":0,"name":"Workspace","repository_ids":["r"]}"#,
    );
    settings(
        &s,
        1,
        r#", "muted_task_ids":["t"],"muted_workspace_ids":["w"]"#,
    );
    call(
        &s,
        "items.delete",
        r#"{"id":"t","request_id":"delete-muted","expected_revision":1}"#,
    );
    call(
        &s,
        "workspaces.delete",
        r#"{"id":"w","request_id":"delete-muted-workspace","expected_revision":1}"#,
    );
    let result = settings(
        &s,
        2,
        r#", "sound":true,"muted_task_ids":["t"],"muted_workspace_ids":["w"]"#,
    );
    assert_eq!(num(&s, &result, "$.item.sound"), 1);
    assert_eq!(text(&s, &result, "$.item.muted_task_ids[0]"), "t");
    assert_eq!(text(&s, &result, "$.item.muted_workspace_ids[0]"), "w");
    let cleared = settings(&s, 3, r#", "sound":true"#);
    assert_eq!(num(&s, &cleared, "$.item.sound"), 1);
    assert!(!cleared.contains("\"t\""));
    assert!(!cleared.contains("\"w\""));
}
