//! The `dcstore` process boundary, exercised as a caller sees it.
//!
//! Everything here runs the real binary and reads its stdout, because the
//! failures being pinned are properties of the boundary rather than of the
//! library: a flag value the parser silently dropped, a reply whose JSON was
//! assembled by hand out of a stored column, and a health check that answered
//! for a database it had just invented.

use std::path::{Path, PathBuf};
use std::process::Command;

const DCSTORE: &str = env!("CARGO_BIN_EXE_dcstore");

/// A directory of this test's own, named for the process and the case, so
/// concurrent tests never share a database.
fn temp_dir(name: &str) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("dc-store-boundary-{}-{name}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).expect("create the test directory");
    dir
}

struct Reply {
    stdout: String,
    code: i32,
}

fn dcstore(db: &Path, args: &[&str]) -> Reply {
    let out = Command::new(DCSTORE)
        .arg("--db")
        .arg(db)
        .args(args)
        .output()
        .expect("run dcstore");
    Reply {
        stdout: String::from_utf8_lossy(&out.stdout).trim().to_string(),
        code: out.status.code().unwrap_or(-1),
    }
}

/// A store the other cases can talk to, created the way any writing command
/// creates one.
fn seeded(name: &str) -> PathBuf {
    let db = temp_dir(name).join("state.sqlite");
    let reply = dcstore(&db, &["ready"]);
    assert_eq!(reply.code, 0, "seeding the store: {}", reply.stdout);
    db
}

/// A mistyped `--db` used to be answered by a database this call created:
/// `{"ok":true,...,"active_leases":0}` from a private, empty file nobody else
/// was using. Two harnesses configured with two spellings of one path shared no
/// exclusion at all while both reported healthy.
#[test]
fn health_refuses_a_database_that_does_not_exist_rather_than_creating_one() {
    let dir = temp_dir("missing-db");
    let real = dir.join("state.sqlite");
    let typo = dir.join("staet.sqlite");

    let seeded = dcstore(&real, &["ready"]);
    assert_eq!(seeded.code, 0, "{}", seeded.stdout);
    assert!(dcstore(&real, &["health"]).stdout.contains("\"ok\":true"));

    let reply = dcstore(&typo, &["health"]);
    assert_eq!(
        reply.code, 2,
        "a health check on a store that does not exist reported success: {}",
        reply.stdout
    );
    assert!(
        reply.stdout.contains("\"ok\":false"),
        "unexpected reply: {}",
        reply.stdout
    );
    assert!(
        !typo.exists(),
        "the health check created the database it was asked about"
    );
}

/// health is the only command that refuses to create; the commands that do work
/// still bootstrap a store, because that is how every cold start begins.
#[test]
fn a_writing_command_still_creates_the_store_it_needs() {
    let db = temp_dir("cold-start").join("state.sqlite");
    let reply = dcstore(
        &db,
        &[
            "acquire",
            "--task",
            "TASK-1",
            "--owner",
            "builder-1",
            "--ttl-seconds",
            "300",
        ],
    );
    assert_eq!(reply.code, 0, "{}", reply.stdout);
    assert!(db.exists(), "a cold-start acquire did not create the store");
    assert!(dcstore(&db, &["health"]).stdout.contains("\"ok\":true"));
}

/// `--force 1` was silently false. It failed *safe* — no steal — but "safe" and
/// "what the caller asked for" are different answers, and only one of them was
/// reported. This is the class `KNOWN_FLAGS` exists to stop, one level down.
#[test]
fn a_force_value_the_parser_does_not_understand_is_refused() {
    let db = seeded("force-parse");
    let held = dcstore(
        &db,
        &[
            "acquire",
            "--task",
            "TASK-1",
            "--owner",
            "agentA",
            "--ttl-seconds",
            "300",
        ],
    );
    assert!(held.stdout.contains("\"ok\":true"), "{}", held.stdout);

    for value in ["1", "yes", "TRUE", ""] {
        let reply = dcstore(
            &db,
            &[
                "acquire",
                "--task",
                "TASK-1",
                "--owner",
                "agentB",
                "--ttl-seconds",
                "300",
                "--force",
                value,
            ],
        );
        assert_eq!(
            reply.code, 2,
            "--force {value:?} was interpreted rather than refused: {}",
            reply.stdout
        );
        assert!(
            reply.stdout.contains("neither"),
            "--force {value:?}: {}",
            reply.stdout
        );
    }

    // The values the parser does understand still work, in both directions.
    assert!(
        dcstore(
            &db,
            &[
                "acquire",
                "--task",
                "TASK-1",
                "--owner",
                "agentB",
                "--ttl-seconds",
                "300",
                "--force",
                "false",
            ],
        )
        .stdout
        .contains("lease_held_by_other")
    );
    assert!(
        dcstore(
            &db,
            &[
                "acquire",
                "--task",
                "TASK-1",
                "--owner",
                "agentB",
                "--ttl-seconds",
                "300",
                "--force",
                "true",
            ],
        )
        .stdout
        .contains("\"owner\":\"agentB\"")
    );
}

/// The steal used to be written before the TTL was validated, so this sequence
/// left the task unleased: agentA revoked, agentB refused, and nothing but an
/// error message about a TTL to say so. The Go client reaches it by truncating
/// any sub-second TTL to zero.
#[test]
fn a_forced_acquire_with_an_unusable_ttl_leaves_the_incumbent_holding() {
    let db = seeded("force-ttl");
    let held = dcstore(
        &db,
        &[
            "acquire",
            "--task",
            "TASK-1",
            "--owner",
            "agentA",
            "--ttl-seconds",
            "300",
        ],
    );
    assert!(held.stdout.contains("\"ok\":true"), "{}", held.stdout);

    let refused = dcstore(
        &db,
        &[
            "acquire",
            "--task",
            "TASK-1",
            "--owner",
            "agentB",
            "--ttl-seconds",
            "0",
            "--force",
            "true",
        ],
    );
    assert_eq!(refused.code, 2, "{}", refused.stdout);
    assert!(
        refused.stdout.contains("unusable ttl"),
        "{}",
        refused.stdout
    );

    let active = dcstore(&db, &["active", "--task", "TASK-1"]);
    assert!(
        active.stdout.contains("\"owner\":\"agentA\""),
        "the incumbent lost its lease to a request that was refused: {}",
        active.stdout
    );
}

/// The injection, end to end at the boundary where it mattered.
///
/// The payload satisfied the old shape check, was stored verbatim, and was
/// spliced unescaped into the `task` reply — closing `planned_files` early and
/// adding a second one. A decoder that takes the last duplicate key then read
/// the executor's own `**` as scope the planner had authorised.
#[test]
fn scope_append_cannot_inject_a_second_key_into_the_task_reply() {
    let db = seeded("scope-injection");
    // The task row is planted directly: planning is DevCouncil's job, and this
    // test is about what the store will hold, not about how it got there.
    let store = dc_store::Store::open(&db).expect("open store");
    store
        .connection()
        .execute(
            "INSERT INTO tasks (id, title, description, planned_files_json, status)
             VALUES ('TASK-1', 'planted', '', '[{\"path\":\"src/a.go\"}]', 'ready')",
            [],
        )
        .expect("plant task");
    drop(store);

    let acquired = dcstore(
        &db,
        &[
            "acquire",
            "--task",
            "TASK-1",
            "--owner",
            "builder-1",
            "--ttl-seconds",
            "300",
        ],
    );
    let token = acquired
        .stdout
        .split("\"token\":\"")
        .nth(1)
        .and_then(|rest| rest.split('"').next())
        .expect("a token in the acquire reply")
        .to_string();

    let injected = concat!(
        r#"[{"path":"benign.go","allowed_change":"modify"}],"#,
        r#""planned_files":[{"path":"**","allowed_change":"modify"}],"junk":[1]"#
    );
    let reply = dcstore(
        &db,
        &[
            "scope-append",
            "--task",
            "TASK-1",
            "--token",
            &token,
            "--expected",
            "[]",
            "--appended",
            injected,
        ],
    );
    assert_eq!(reply.code, 2, "the injection was written: {}", reply.stdout);

    // And the reply a consumer reads still carries exactly one planned_files.
    let task = dcstore(&db, &["task", "--task", "TASK-1"]);
    assert_eq!(
        task.stdout.matches("\"planned_files\":").count(),
        1,
        "the task reply carries a duplicated key: {}",
        task.stdout
    );
    assert!(
        !task.stdout.contains("\"**\""),
        "a path nobody accepted reached the reply: {}",
        task.stdout
    );
}
