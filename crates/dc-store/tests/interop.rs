//! Interoperability with the incumbent, on one file.
//!
//! This is Phase 2's real gate. Every other test in this crate proves the Rust
//! store is self-consistent, which is necessary and nowhere near sufficient:
//! during the migration `dev tasks` and the harness read and write the same
//! `.devcouncil/state.sqlite`, and a schema or timestamp divergence would let
//! both sides believe they hold the same task.
//!
//! So the test drives both. Rust acquires, Python reads it back through
//! DevCouncil's own `TaskLeaseRepository`, and the reverse. If DevCouncil is
//! not importable the test skips loudly rather than passing quietly — a skipped
//! interop check must never look like a passed one.

use std::path::{Path, PathBuf};
use std::process::Command;

use dc_store::{AcquireRequest, LeaseCode, Store};

/// Locates a Python interpreter with DevCouncil importable, or None.
fn devcouncil_python() -> Option<(PathBuf, PathBuf)> {
    let repo = Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()?
        .parent()?
        .parent()?
        .join("DevCouncil");
    let python = repo.join(".venv/bin/python");
    let src = repo.join("src");
    if python.is_file() && src.is_dir() {
        Some((python, src))
    } else {
        None
    }
}

/// Runs a snippet with DevCouncil on the path, returning stdout.
fn run_python(python: &Path, src: &Path, code: &str) -> String {
    let output = Command::new(python)
        .arg("-c")
        .arg(code)
        .env("PYTHONPATH", src)
        .output()
        .expect("spawn python");
    if !output.status.success() {
        panic!(
            "python failed:\n--- stdout ---\n{}\n--- stderr ---\n{}",
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        );
    }
    String::from_utf8_lossy(&output.stdout).trim().to_string()
}

fn temp_db(name: &str) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("dc-store-interop-{}-{name}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    let path = dir.join("state.sqlite");
    let _ = std::fs::remove_file(&path);
    path
}

#[test]
fn python_reads_a_lease_the_rust_store_wrote() {
    let Some((python, src)) = devcouncil_python() else {
        eprintln!("SKIP: DevCouncil venv not found; interop unverified in this environment");
        return;
    };
    let db = temp_db("rust-writes");

    let store = Store::open(&db).unwrap();
    let lease = store
        .acquire(&AcquireRequest {
            task_id: "TASK-INTEROP".into(),
            owner: "rust-builder".into(),
            client_id: Some("harness".into()),
            ttl_seconds: Some(900),
            ..Default::default()
        })
        .unwrap();

    // DevCouncil's own repository, against the file Rust just wrote.
    let code = format!(
        r#"
from sqlmodel import create_engine, Session
from devcouncil.storage.native import TaskLeaseRepository
engine = create_engine("sqlite:///{db}")
with Session(engine) as s:
    repo = TaskLeaseRepository(s)
    active = repo.active_for_task("TASK-INTEROP")
    assert active is not None, "Python saw no active lease"
    print(active.owner)
    print(active.lease_token)
    print(repo.validate("TASK-INTEROP", "{token}"))
    print(repo.validate("TASK-INTEROP", "wrong-token"))
"#,
        db = db.display(),
        token = lease.token
    );
    let out = run_python(&python, &src, &code);
    let lines: Vec<&str> = out.lines().collect();

    assert_eq!(
        lines[0], "rust-builder",
        "owner did not survive the boundary"
    );
    assert_eq!(lines[1], lease.token, "token did not survive the boundary");
    assert_eq!(lines[2], "True", "Python rejected a token Rust issued");
    assert_eq!(lines[3], "False", "Python accepted a token nobody issued");

    let _ = std::fs::remove_dir_all(db.parent().unwrap());
}

#[test]
fn the_rust_store_reads_a_lease_python_wrote_and_refuses_to_double_book_it() {
    let Some((python, src)) = devcouncil_python() else {
        eprintln!("SKIP: DevCouncil venv not found; interop unverified in this environment");
        return;
    };
    let db = temp_db("python-writes");

    // Create the schema from the Rust side, then let Python acquire through it.
    Store::open(&db).unwrap();
    let code = format!(
        r#"
from sqlmodel import create_engine, Session
from devcouncil.storage.native import TaskLeaseRepository
engine = create_engine("sqlite:///{db}")
with Session(engine) as s:
    lease = TaskLeaseRepository(s).acquire("TASK-INTEROP", "python-builder", ttl_seconds=900)
    print(lease.lease_token)
"#,
        db = db.display()
    );
    let token = run_python(&python, &src, &code);

    let store = Store::open(&db).unwrap();
    let active = store
        .active_lease("TASK-INTEROP")
        .unwrap()
        .expect("Rust saw no lease Python wrote");
    assert_eq!(active.owner, "python-builder");
    assert_eq!(active.token, token);

    // The token Python issued validates on the Rust side.
    assert_eq!(
        store.diagnose("TASK-INTEROP", &token).unwrap(),
        LeaseCode::Valid
    );

    // And the mutual exclusion holds across the boundary: the harness must not
    // be able to take a task the Python side is already building.
    let conflict = store.acquire(&AcquireRequest {
        task_id: "TASK-INTEROP".into(),
        owner: "rust-builder".into(),
        ttl_seconds: Some(900),
        ..Default::default()
    });
    assert!(
        conflict.is_err(),
        "the harness double-booked a task Python holds"
    );

    let _ = std::fs::remove_dir_all(db.parent().unwrap());
}

/// The expiry written by Rust must be interpreted the same way by Python.
/// A timestamp both sides can store but read differently is the subtlest way
/// this migration could go wrong: nothing errors, and the two disagree about
/// when a lease died.
#[test]
fn both_sides_agree_on_when_a_lease_expires() {
    let Some((python, src)) = devcouncil_python() else {
        eprintln!("SKIP: DevCouncil venv not found; interop unverified in this environment");
        return;
    };
    let db = temp_db("expiry");

    let store = Store::open(&db).unwrap();
    // A lease that expired an hour ago.
    let lease = store
        .acquire(&AcquireRequest {
            task_id: "TASK-EXPIRED".into(),
            owner: "rust-builder".into(),
            ttl_seconds: Some(-3600),
            ..Default::default()
        })
        .unwrap();

    let code = format!(
        r#"
from sqlmodel import create_engine, Session
from devcouncil.storage.native import TaskLeaseRepository
engine = create_engine("sqlite:///{db}")
with Session(engine) as s:
    print(TaskLeaseRepository(s).active_for_task("TASK-EXPIRED") is None)
"#,
        db = db.display()
    );
    assert_eq!(
        run_python(&python, &src, &code),
        "True",
        "Python still considered an expired lease live"
    );

    // Rust reaches the same conclusion, and the token reads as recoverable.
    assert!(store.active_lease("TASK-EXPIRED").unwrap().is_none());
    assert_eq!(
        store.diagnose("TASK-EXPIRED", &lease.token).unwrap(),
        LeaseCode::Expired
    );

    let _ = std::fs::remove_dir_all(db.parent().unwrap());
}
