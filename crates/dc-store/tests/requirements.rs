//! The two columns the store creates and never reads.
//!
//! `schema.rs` declares `requirement_ids_json` and
//! `acceptance_criterion_ids_json` because they are in the DevCouncil schema
//! this store was transcribed from, and DevCouncil's planner writes them: they
//! are the link from a task back to the requirement it exists to satisfy and
//! to the acceptance criteria it is accountable for proving.
//!
//! `Store::task` selected neither. Every consumer on this side of the boundary
//! therefore saw a task with no requirements and no acceptance criteria — not
//! *empty*, but absent, which is the more dangerous shape: a requirement
//! coverage gate reading this store would find nothing to check and report a
//! task that satisfies no requirement exactly as it reports one that satisfies
//! all of them. That is the non-cheating invariant broken at the read layer,
//! and it is why this is fixed before anything is built on top of it.
//!
//! The columns are read verbatim and never merged. `planned_files` merges its
//! `agent_appended_*` sibling because an executor may widen its own file scope;
//! there is deliberately no such sibling here. A task may not decide for itself
//! which requirements it satisfies — that is the planner's judgement, and a
//! task that could append to it could discharge a requirement by claiming it.

use dc_store::Store;

mod support;
use support::{dcstore, seeded};

/// Plants a task carrying both links, exactly as DevCouncil's planner writes
/// them, and returns the database path.
fn task_with_links(name: &str) -> std::path::PathBuf {
    let db = seeded(name);
    let store = Store::open(&db).expect("open store");
    store
        .connection()
        .execute(
            "INSERT INTO tasks (id, title, description, status, \
             requirement_ids_json, acceptance_criterion_ids_json) \
             VALUES ('TASK-1', 'planted', '', 'ready', \
             '[\"REQ-1\",\"REQ-2\"]', '[\"AC-1\",\"AC-2\",\"AC-3\"]')",
            [],
        )
        .expect("plant task");
    db
}

#[test]
fn a_tasks_requirements_survive_the_read() {
    let db = task_with_links("requirements-read");
    let store = Store::open(&db).expect("open store");
    let task = store.task("TASK-1").expect("read task").expect("task exists");

    assert_eq!(
        task.requirement_ids_json, "[\"REQ-1\",\"REQ-2\"]",
        "the store dropped the requirements the planner linked; a coverage gate \
         reading this would see a task accountable to nothing"
    );
    assert_eq!(
        task.acceptance_criterion_ids_json, "[\"AC-1\",\"AC-2\",\"AC-3\"]",
        "the store dropped the acceptance criteria the task is meant to prove"
    );
}

#[test]
fn the_links_reach_the_boundary_reply() {
    let db = task_with_links("requirements-boundary");
    let reply = dcstore(&db, &["task", "--task", "TASK-1"]);

    assert_eq!(reply.code, 0, "task read failed: {}", reply.stdout);
    assert!(
        reply.stdout.contains("\"requirement_ids\":[\"REQ-1\",\"REQ-2\"]"),
        "requirement_ids never reached the Go plane: {}",
        reply.stdout
    );
    assert!(
        reply
            .stdout
            .contains("\"acceptance_criterion_ids\":[\"AC-1\",\"AC-2\",\"AC-3\"]"),
        "acceptance_criterion_ids never reached the Go plane: {}",
        reply.stdout
    );
}

/// A task with no links reads as an empty array, not as a missing key.
///
/// The distinction is the whole point of the fix. A consumer that gets no key
/// cannot tell "this task satisfies no requirement" from "this store does not
/// report requirements", and the schema's own `DEFAULT '[]'` says the first of
/// those is a real, representable state.
#[test]
fn a_task_with_no_links_reads_as_empty_rather_than_absent() {
    let db = seeded("requirements-empty");
    let store = Store::open(&db).expect("open store");
    store
        .connection()
        .execute(
            "INSERT INTO tasks (id, title, description, status) \
             VALUES ('TASK-1', 'planted', '', 'ready')",
            [],
        )
        .expect("plant task");
    drop(store);

    let store = Store::open(&db).expect("reopen store");
    let task = store.task("TASK-1").expect("read task").expect("task exists");
    assert_eq!(task.requirement_ids_json, "[]");
    assert_eq!(task.acceptance_criterion_ids_json, "[]");

    let reply = dcstore(&db, &["task", "--task", "TASK-1"]);
    assert!(
        reply.stdout.contains("\"requirement_ids\":[]"),
        "an unlinked task must report an empty list, not omit the key: {}",
        reply.stdout
    );
}

/// The same fail-closed rule the other scope columns get.
///
/// These values are embedded into the reply as raw JSON text, so a column that
/// is not a well-formed array is either a broken document or an injection. The
/// store shares its file with DevCouncil and with anything else holding the
/// path, so the read asserts it rather than trusting every writer.
#[test]
fn a_malformed_requirement_column_is_an_error_not_a_broken_reply() {
    let db = seeded("requirements-malformed");
    let store = Store::open(&db).expect("open store");
    store
        .connection()
        .execute(
            "INSERT INTO tasks (id, title, description, status, requirement_ids_json) \
             VALUES ('TASK-1', 'planted', '', 'ready', '\"REQ-1\"')",
            [],
        )
        .expect("plant task");
    drop(store);

    let store = Store::open(&db).expect("reopen store");
    let err = store
        .task("TASK-1")
        .expect_err("a non-array requirement column must not read as a task");
    let message = err.to_string();
    assert!(
        message.contains("requirement_ids_json"),
        "the error must name the column that is wrong: {message}"
    );
}
