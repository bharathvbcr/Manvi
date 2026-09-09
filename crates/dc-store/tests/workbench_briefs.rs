use dc_store::Store;

fn call(store: &Store, method: &str, input: &str) -> String {
    store
        .workbench_request(method, input)
        .unwrap_or_else(|e| panic!("{method}: {e}"))
}

fn fixture() -> Store {
    let store = Store::open_in_memory().unwrap();
    for id in ["r1", "r2"] {
        call(
            &store,
            "repositories.put",
            &format!(
                r#"{{"id":"{id}","request_id":"{id}","expected_revision":0,"name":"Repository {id}","identity_key":"local:{id}","remote_url":"https://credential@example.test/repo"}}"#
            ),
        );
    }
    call(
        &store,
        "workspaces.put",
        r#"{"id":"w","request_id":"w","expected_revision":0,"name":"My workspace","repository_ids":["r2"]}"#,
    );
    call(
        &store,
        "items.put",
        r#"{"id":"t","request_id":"t","expected_revision":0,"title":"Preserve E42","description":"Exact text: `$(touch nope)`\nDo not change public APIs. 🧪","kind":"bug","priority":0,"severity":"high","owner":"Pat","due_at":12345,"labels":["backend","unicode"],"acceptance_criteria":["Keep E42 verbatim","Pass the API checks"],"repository_ids":["r2","r1"],"primary_repository_id":"r1","home_workspace_id":"w","locked_fields":["title"]}"#,
    );
    store
}

fn field(store: &Store, raw: &str, path: &str) -> String {
    store
        .connection()
        .query_row("SELECT json_extract(?1,?2)", [raw, path], |r| r.get(0))
        .unwrap()
}

#[test]
fn a_brief_contains_the_exact_task_and_every_linked_repository_in_order() {
    let store = fixture();
    let before = call(&store, "events.list", r#"{"limit":1}"#);
    let task = call(&store, "items.get", r#"{"id":"t"}"#);
    let result = call(
        &store,
        "items.brief.get",
        r#"{"id":"t","expected_revision":1}"#,
    );
    assert_eq!(
        field(&store, &result, "$.item.task"),
        field(&store, &task, "$.item")
    );
    assert_eq!(field(&store, &result, "$.item.repositories[0].id"), "r2");
    assert_eq!(field(&store, &result, "$.item.repositories[1].id"), "r1");
    assert_eq!(
        field(&store, &result, "$.item.workspace.name"),
        "My workspace"
    );
    let markdown = field(&store, &result, "$.item.markdown");
    for required in [
        "Task brief v1",
        "Preserve E42",
        "revision 1",
        "Repository r2",
        "Repository r1",
        "Exact text: `$(touch nope)`\nDo not change public APIs. 🧪",
        "Keep E42 verbatim",
        "Pass the API checks",
        "Pat",
        "12345",
        "backend",
        "high",
    ] {
        assert!(
            markdown.contains(required),
            "brief omitted {required}: {markdown}"
        );
    }
    assert!(
        !result.contains("credential@example.test"),
        "unneeded remote credentials entered the exported brief"
    );
    assert_eq!(
        call(&store, "events.list", r#"{"limit":1}"#),
        before,
        "a brief read wrote an event"
    );
    assert_eq!(
        call(
            &store,
            "items.brief.get",
            r#"{"id":"t","expected_revision":1}"#
        ),
        result,
        "unchanged source produced a different brief"
    );
}

#[test]
fn brief_reads_reject_stale_deleted_and_ambiguous_requests() {
    let store = fixture();
    for input in [
        r#"{"id":"t"}"#,
        r#"{"id":"t","expected_revision":0}"#,
        r#"{"id":"t","expected_revision":1,"skip_permissions":true}"#,
        r#"{"id":"t","id":"other","expected_revision":1}"#,
    ] {
        assert_eq!(
            store
                .workbench_request("items.brief.get", input)
                .unwrap_err()
                .code,
            "invalid_input"
        );
    }
    assert_eq!(
        store
            .workbench_request("items.brief.get", r#"{"id":"t","expected_revision":2}"#)
            .unwrap_err()
            .code,
        "revision_conflict"
    );
    call(
        &store,
        "items.delete",
        r#"{"id":"t","request_id":"delete","expected_revision":1}"#,
    );
    assert_eq!(
        store
            .workbench_request("items.brief.get", r#"{"id":"t","expected_revision":2}"#)
            .unwrap_err()
            .code,
        "not_found"
    );
}

#[test]
fn repository_changes_are_explicit_in_the_snapshot_without_changing_task_revision() {
    let store = fixture();
    let before = call(
        &store,
        "items.brief.get",
        r#"{"id":"t","expected_revision":1}"#,
    );
    call(
        &store,
        "repositories.put",
        r#"{"id":"r2","request_id":"rename","expected_revision":1,"name":"Renamed repository","identity_key":"local:r2"}"#,
    );
    let after = call(
        &store,
        "items.brief.get",
        r#"{"id":"t","expected_revision":1}"#,
    );
    assert_ne!(
        field(&store, &before, "$.item.repositories"),
        field(&store, &after, "$.item.repositories")
    );
    assert_eq!(
        field(&store, &before, "$.item.task"),
        field(&store, &after, "$.item.task")
    );
    assert!(field(&store, &after, "$.item.markdown").contains("Renamed repository"));
}

#[test]
fn a_large_brief_preserves_unicode_and_every_criterion_without_truncation() {
    let store = fixture();
    let description = "🧪".repeat(16_384);
    let criterion = "\\\"".repeat(1000);
    let criteria = vec![criterion; 40];
    let criteria_json: String = store
        .connection()
        .query_row(
            "SELECT json_group_array(value) FROM json_each(?1)",
            [format!(
                "[{}]",
                criteria
                    .iter()
                    .map(|s| store
                        .connection()
                        .query_row::<String, _, _>("SELECT json_quote(?1)", [s], |r| r.get(0))
                        .unwrap())
                    .collect::<Vec<_>>()
                    .join(",")
            )],
            |r| r.get(0),
        )
        .unwrap();
    let input: String = store.connection().query_row(
        "SELECT json_object('id','large','request_id','large','expected_revision',0,'title','Large task','description',?1,'acceptance_criteria',json(?2),'repository_ids',json('[\"r1\"]'),'primary_repository_id','r1')",
        [&description,&criteria_json],|r|r.get(0),
    ).unwrap();
    call(&store, "items.put", &input);
    let result = call(
        &store,
        "items.brief.get",
        r#"{"id":"large","expected_revision":1}"#,
    );
    assert!(result.len() > 256 * 1024);
    assert!(result.len() < 2 * 1024 * 1024);
    assert_eq!(
        field(&store, &result, "$.item.task.description"),
        description
    );
    assert_eq!(
        field(&store, &result, "$.item.task.acceptance_criteria"),
        criteria_json
    );
    let markdown = field(&store, &result, "$.item.markdown");
    assert!(markdown.contains(&description));
    assert_eq!(markdown.matches("- [ ] ").count(), 40);
    assert!(markdown.ends_with(&format!("- [ ] {}\n", criteria[39])));
}

#[test]
fn an_empty_description_and_criteria_do_not_invent_instructions() {
    let store = fixture();
    call(
        &store,
        "items.put",
        r#"{"id":"minimal","request_id":"minimal","expected_revision":0,"title":"Investigate E42","repository_ids":["r1"],"primary_repository_id":"r1"}"#,
    );
    let result = call(
        &store,
        "items.brief.get",
        r#"{"id":"minimal","expected_revision":1}"#,
    );
    let markdown = field(&store, &result, "$.item.markdown");
    assert!(markdown.contains("Home workspace: None\n"));
    assert!(markdown.contains("Due (Unix seconds): None\n"));
    assert!(markdown.contains("\n## Description\n\n"));
    assert!(markdown.ends_with("No acceptance criteria recorded.\n"));
}

#[test]
fn missing_reordered_or_oversized_storage_cannot_be_exported_as_complete() {
    for sql in [
        "DELETE FROM work_item_repositories WHERE item_id='t' AND repository_id='r1'",
        "UPDATE work_item_repositories SET position=2-position WHERE item_id='t'",
        "UPDATE work_items SET body=json_set(body,'$.id','other') WHERE id='t'",
        "UPDATE work_workspaces SET deleted=1 WHERE id='w'",
    ] {
        let store = fixture();
        store.connection().execute(sql, []).unwrap();
        let error = store
            .workbench_request("items.brief.get", r#"{"id":"t","expected_revision":1}"#)
            .unwrap_err();
        assert_eq!(error.code, "store_error", "{sql}: {error}");
    }
    let store = fixture();
    store
        .connection()
        .execute(
            "UPDATE work_items SET body=json_set(body,'$.oversized',?1) WHERE id='t'",
            ["x".repeat(2 * 1024 * 1024)],
        )
        .unwrap();
    assert_eq!(
        store
            .workbench_request("items.brief.get", r#"{"id":"t","expected_revision":1}"#)
            .unwrap_err()
            .code,
        "response_too_large"
    );
}

#[test]
fn deleting_a_home_workspace_requires_a_fresh_task_revision() {
    let store = fixture();
    call(
        &store,
        "workspaces.delete",
        r#"{"id":"w","request_id":"remove-workspace","expected_revision":1}"#,
    );
    assert_eq!(
        store
            .workbench_request("items.brief.get", r#"{"id":"t","expected_revision":1}"#)
            .unwrap_err()
            .code,
        "revision_conflict"
    );
    let result = call(
        &store,
        "items.brief.get",
        r#"{"id":"t","expected_revision":2}"#,
    );
    assert!(field(&store, &result, "$.item.markdown").contains("Home workspace: None\n"));
    assert_eq!(field(&store, &result, "$.item.repositories[1].id"), "r1");
}
