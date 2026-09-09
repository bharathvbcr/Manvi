//! Real process-boundary tests for the profile workbench, independent of repo leases.

use rusqlite::Connection;
use std::path::PathBuf;
use std::process::Command;
use std::sync::atomic::{AtomicU64, Ordering};

static NEXT: AtomicU64 = AtomicU64::new(1);

struct Fixture(PathBuf);

impl Fixture {
    fn new() -> Self {
        let path = std::env::temp_dir().join(format!(
            "manvi-workbench-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        std::fs::create_dir(&path).unwrap();
        Self(path)
    }

    fn call(&self, method: &str, input: &str) -> String {
        let out = Command::new(env!("CARGO_BIN_EXE_dcstore"))
            .args([
                "--db",
                self.0.join("profile.sqlite").to_str().unwrap(),
                "work",
                "--method",
                method,
                "--input",
                input,
            ])
            .output()
            .unwrap();
        let body = String::from_utf8(out.stdout).unwrap();
        assert!(
            out.status.success(),
            "{method}: {body}; {}",
            String::from_utf8_lossy(&out.stderr)
        );
        assert_eq!(number(&body, "$.ok"), 1, "{method}: {body}");
        body
    }

    fn refuse(&self, method: &str, input: &str, code: &str) {
        let out = Command::new(env!("CARGO_BIN_EXE_dcstore"))
            .args([
                "--db",
                self.0.join("profile.sqlite").to_str().unwrap(),
                "work",
                "--method",
                method,
                "--input",
                input,
            ])
            .output()
            .unwrap();
        let body = String::from_utf8(out.stdout).unwrap();
        assert_eq!(number(&body, "$.ok"), 0, "{method} {input}: {body}");
        assert_eq!(value(&body, "$.code"), code, "{body}");
    }

    fn repo(&self, id: &str) {
        self.call("repositories.put", &format!(r#"{{"request_id":"repo-{id}","id":"{id}","expected_revision":0,"name":"{id}","identity_key":"local:{id}"}}"#));
    }

    fn workspace(&self, id: &str, repos: &str) {
        self.call("workspaces.put", &format!(r#"{{"request_id":"ws-{id}","id":"{id}","expected_revision":0,"name":"{id}","repository_ids":{repos}}}"#));
    }

    fn task(&self, id: &str, repos: &str) {
        self.call("items.put", &format!(r#"{{"request_id":"task-{id}","id":"{id}","expected_revision":0,"title":"Fix {id}","repository_ids":{repos},"primary_repository_id":"r1"}}"#));
    }
}

impl Drop for Fixture {
    fn drop(&mut self) {
        std::fs::remove_dir_all(&self.0).unwrap();
    }
}

fn value(body: &str, path: &str) -> String {
    Connection::open_in_memory()
        .unwrap()
        .query_row("SELECT json_extract(?1, ?2)", [body, path], |row| {
            row.get(0)
        })
        .unwrap()
}

fn number(body: &str, path: &str) -> i64 {
    Connection::open_in_memory()
        .unwrap()
        .query_row("SELECT json_extract(?1, ?2)", [body, path], |row| {
            row.get(0)
        })
        .unwrap()
}

#[test]
fn groups_persist_across_processes_and_share_repositories() {
    let f = Fixture::new();
    f.repo("r1");
    f.repo("r2");
    f.workspace("devtools", r#"["r1","r2"]"#);
    f.workspace("research", r#"["r1"]"#);
    let result = f.call("workspaces.list", "{}");
    assert_eq!(number(&result, "$.total"), 2);
    assert_eq!(number(&f.call("repositories.list", "{}"), "$.total"), 2);
    assert_eq!(
        number(
            &f.call("repositories.list", r#"{"workspace_id":"devtools"}"#),
            "$.total"
        ),
        2
    );
    assert_eq!(
        number(
            &f.call("repositories.list", r#"{"workspace_id":"research"}"#),
            "$.total"
        ),
        1
    );
}

#[test]
fn one_task_has_three_board_scopes_without_duplicates() {
    let f = Fixture::new();
    f.repo("r1");
    f.repo("r2");
    f.workspace("both", r#"["r1","r2"]"#);
    f.task("t1", r#"["r1","r2"]"#);
    for scope in [
        "{}",
        r#"{"workspace_id":"both"}"#,
        r#"{"repository_id":"r1"}"#,
        r#"{"repository_id":"r2"}"#,
    ] {
        let result = f.call("items.list", scope);
        assert_eq!(number(&result, "$.total"), 1, "{result}");
        assert_eq!(value(&result, "$.items[0].id"), "t1");
    }
}

#[test]
fn stale_edits_and_reused_request_ids_cannot_overwrite_current_data() {
    let f = Fixture::new();
    f.repo("r1");
    let original = r#"{"request_id":"create","id":"t1","expected_revision":0,"title":"Keep quotes: \"x\" and 日本語","repository_ids":["r1"],"primary_repository_id":"r1"}"#;
    let receipt = f.call("items.put", original);
    assert_eq!(f.call("items.put", original), receipt);
    f.call("items.put",r#"{"request_id":"edit","id":"t1","expected_revision":1,"title":"Current title","repository_ids":["r1"],"primary_repository_id":"r1"}"#);
    assert_eq!(f.call("items.put", original), receipt);
    f.refuse("items.put",r#"{"request_id":"stale","id":"t1","expected_revision":1,"title":"Lost update","repository_ids":["r1"],"primary_repository_id":"r1"}"#,"revision_conflict");
    f.refuse("items.put",r#"{"request_id":"create","id":"t1","expected_revision":2,"title":"Different payload","repository_ids":["r1"],"primary_repository_id":"r1"}"#,"idempotency_conflict");
    assert_eq!(
        value(&f.call("items.get", r#"{"id":"t1"}"#), "$.item.title"),
        "Current title"
    );
}

#[test]
fn group_deletion_preserves_tasks_and_repositories() {
    let f = Fixture::new();
    f.repo("r1");
    f.workspace("ws", r#"["r1"]"#);
    f.call("items.put",r#"{"request_id":"task","id":"t1","expected_revision":0,"title":"Keep me","repository_ids":["r1"],"primary_repository_id":"r1","home_workspace_id":"ws"}"#);
    f.call(
        "workspaces.delete",
        r#"{"request_id":"delete","id":"ws","expected_revision":1}"#,
    );
    assert_eq!(number(&f.call("workspaces.list", "{}"), "$.total"), 0);
    assert_eq!(
        number(
            &f.call("repositories.list", r#"{"ungrouped":true}"#),
            "$.total"
        ),
        1
    );
    assert_eq!(number(&f.call("items.list", "{}"), "$.total"), 1);
    assert_eq!(number(&f.call("events.list", "{}"), "$.total"), 4);
}

#[test]
fn malformed_missing_and_duplicate_repository_links_are_refused_atomically() {
    let f = Fixture::new();
    f.repo("r1");
    for repos in [r#"[]"#, r#"["r1","r1"]"#, r#"["missing"]"#, r#"[4]"#] {
        f.refuse("items.put",&format!(r#"{{"request_id":"bad","id":"bad","expected_revision":0,"title":"No","repository_ids":{repos},"primary_repository_id":"r1"}}"#),"invalid_input");
    }
    assert_eq!(number(&f.call("items.list", "{}"), "$.total"), 0);
    f.task("good", r#"["r1"]"#);
    assert_eq!(number(&f.call("items.list", "{}"), "$.total"), 1);
}

#[test]
fn pagination_is_bounded_and_does_not_count_only_the_page() {
    let f = Fixture::new();
    f.repo("r1");
    for id in ["a", "b", "c"] {
        f.task(id, r#"["r1"]"#);
    }
    let first = f.call("items.list", r#"{"limit":2}"#);
    assert_eq!(number(&first, "$.shown"), 2);
    assert_eq!(number(&first, "$.total"), 3);
    assert_eq!(number(&first, "$.has_more"), 1);
    let cursor = value(&first, "$.next_cursor");
    let second = f.call(
        "items.list",
        &format!(r#"{{"limit":2,"cursor":"{cursor}"}}"#),
    );
    assert_eq!(number(&second, "$.shown"), 1);
    assert_eq!(number(&second, "$.total"), 3);
    f.refuse("items.list", r#"{"limit":1000000}"#, "invalid_input");
}

#[test]
fn boards_return_cards_and_details_remain_available() {
    let f = Fixture::new();
    f.repo("r1");
    f.workspace("ws", r#"["r1"]"#);
    f.call("items.put",r#"{"request_id":"detail","id":"t1","expected_revision":0,"title":"Fast card","description":"Detailed evidence","acceptance_criteria":["Measured"],"repository_ids":["r1"],"primary_repository_id":"r1"}"#);
    let cards = f.call("items.list", "{}");
    assert!(
        !cards.contains("Detailed evidence"),
        "board eagerly loaded description"
    );
    assert!(
        !cards.contains("Measured"),
        "board eagerly loaded acceptance criteria"
    );
    assert_eq!(
        value(&f.call("items.get", r#"{"id":"t1"}"#), "$.item.description"),
        "Detailed evidence"
    );
    let groups = f.call("workspaces.list", "{}");
    assert!(
        !groups.contains("repository_ids"),
        "navigation eagerly loaded membership"
    );
    assert_eq!(number(&groups, "$.items[0].repository_count"), 1);
}

#[test]
fn unavailable_scopes_and_invalid_numbers_are_not_empty_successes() {
    let f = Fixture::new();
    for (method, input) in [
        ("items.list", r#"{"repository_id":"missing"}"#),
        ("items.list", r#"{"workspace_id":"missing"}"#),
        ("repositories.list", r#"{"workspace_id":"missing"}"#),
        ("items.history", r#"{"id":"missing"}"#),
    ] {
        f.refuse(method, input, "not_found");
    }
    for input in [
        r#"{"status":"made_up"}"#,
        r#"{"limit":99999999999999999999999999}"#,
        r#"{"limit":1.5}"#,
        r#"{"limit":true}"#,
        r#"{"limit":1,"limit":2}"#,
        r#"{"limit":1,"\u006cimit":2}"#,
    ] {
        f.refuse("items.list", input, "invalid_input");
    }
}

#[test]
fn repository_membership_order_survives_pagination() {
    let f = Fixture::new();
    f.repo("r1");
    f.repo("r2");
    f.workspace("ws", r#"["r2","r1"]"#);
    let first = f.call("repositories.list", r#"{"workspace_id":"ws","limit":1}"#);
    assert_eq!(value(&first, "$.items[0].id"), "r2");
    let cursor = value(&first, "$.next_cursor");
    let next = f.call(
        "repositories.list",
        &format!(r#"{{"workspace_id":"ws","limit":1,"cursor":"{cursor}"}}"#),
    );
    assert_eq!(value(&next, "$.items[0].id"), "r1");
}

#[test]
fn concurrent_writers_cannot_both_replace_the_same_revision() {
    let f = Fixture::new();
    f.repo("r1");
    f.task("t1", r#"["r1"]"#);
    let barrier = std::sync::Barrier::new(8);
    let results = std::thread::scope(|scope| {
        let threads=(0..8).map(|i| {
            let barrier=&barrier; let path=&f.0;
            scope.spawn(move || {
                barrier.wait();
                let out=Command::new(env!("CARGO_BIN_EXE_dcstore")).args([
                    "--db",path.join("profile.sqlite").to_str().unwrap(),"work","--method","items.put","--input",
                    &format!(r#"{{"request_id":"race-{i}","id":"t1","expected_revision":1,"title":"Writer {i}","repository_ids":["r1"],"primary_repository_id":"r1"}}"#)
                ]).output().unwrap();
                assert!(out.status.success(),"{}",String::from_utf8_lossy(&out.stderr));
                String::from_utf8(out.stdout).unwrap()
            })
        }).collect::<Vec<_>>();
        threads
            .into_iter()
            .map(|t| t.join().unwrap())
            .collect::<Vec<_>>()
    });
    assert_eq!(
        results
            .iter()
            .filter(|body| number(body, "$.ok") == 1)
            .count(),
        1
    );
    for body in results.iter().filter(|body| number(body, "$.ok") == 0) {
        assert_eq!(value(body, "$.code"), "revision_conflict");
    }
    assert_eq!(
        number(&f.call("items.history", r#"{"id":"t1"}"#), "$.total"),
        2
    );
    assert_eq!(number(&f.call("events.list", "{}"), "$.total"), 3);
}

#[test]
fn large_history_pages_obey_byte_budget_and_resume_without_loss() {
    let f = Fixture::new();
    f.repo("r1");
    let store = dc_store::Store::open(f.0.join("profile.sqlite")).unwrap();
    let description = "d".repeat(60_000);
    for revision in 0..40 {
        store.workbench_request("items.put",&format!(r#"{{"request_id":"long-{revision}","id":"t1","expected_revision":{revision},"title":"Large item","description":"{description}","repository_ids":["r1"],"primary_repository_id":"r1"}}"#)).unwrap();
    }
    let mut after = 0;
    let mut seen = 0;
    loop {
        let body = store
            .workbench_request(
                "items.history",
                &format!(r#"{{"id":"t1","limit":200,"after_revision":{after}}}"#),
            )
            .unwrap();
        assert!(
            body.len() <= 2 * 1024 * 1024,
            "page exceeded 2 MiB: {}",
            body.len()
        );
        seen += number(&body, "$.shown");
        let next = number(&body, "$.next_cursor");
        assert!(next > after);
        after = next;
        if number(&body, "$.has_more") == 0 {
            break;
        }
    }
    assert_eq!(seen, 40);
    assert_eq!(after, 40);
    let events = store
        .workbench_request("events.list", r#"{"limit":200}"#)
        .unwrap();
    assert!(events.len() <= 2 * 1024 * 1024);
    assert_eq!(number(&events, "$.has_more"), 1);
}

#[test]
fn invalid_unicode_is_a_validation_error_and_valid_pairs_round_trip() {
    let f = Fixture::new();
    f.repo("r1");
    for field in [
        r#""title":"\ud800""#,
        r#""title":"ok","description":"\udc00""#,
        r#""title":"ok","labels":["\ud800"]"#,
        r#""title":"ok","\ud800":"key""#,
    ] {
        f.refuse("items.put",&format!(r#"{{"request_id":"bad-unicode","id":"t1","expected_revision":0,"repository_ids":["r1"],"primary_repository_id":"r1",{field}}}"#),"invalid_input");
    }
    let result=f.call("items.put",r#"{"request_id":"bad-unicode","id":"t1","expected_revision":0,"title":"Fix \ud83d\ude80 launch","repository_ids":["r1"],"primary_repository_id":"r1"}"#);
    assert_eq!(value(&result, "$.item.title"), "Fix 🚀 launch");
    assert_eq!(number(&f.call("events.list", "{}"), "$.total"), 2);
}
