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

/// Where DevCouncil is checked out, or None.
///
/// This counted exactly three parents and re-entered by name, which is right
/// only when this crate sits at `<something>/Manvi/crates/dc-store` with
/// DevCouncil beside `Manvi`. Run from a git worktree — `Manvi/.claude/
/// worktrees/<branch>/crates/dc-store` — the same three steps land in
/// `.claude/worktrees/`, and `DevCouncil` is not there. The tests then took
/// the "no venv" path and cargo reported **ok** for three checks that never
/// ran, which is the precise failure this file's header says must not happen.
/// It was doing it on every worktree run.
///
/// So the search is now a search: walk up from the manifest and take the first
/// ancestor with a `DevCouncil` beside it that actually looks like the
/// repository, rather than betting on a fixed depth. `DEVCOUNCIL_ROOT`
/// overrides it for a checkout that lives somewhere else entirely.
fn devcouncil_root() -> Option<PathBuf> {
    if let Some(explicit) = std::env::var_os("DEVCOUNCIL_ROOT") {
        let root = PathBuf::from(explicit);
        return looks_like_devcouncil(&root).then_some(root);
    }
    let mut dir: Option<&Path> = Some(Path::new(env!("CARGO_MANIFEST_DIR")));
    while let Some(current) = dir {
        let candidate = current.join("DevCouncil");
        if looks_like_devcouncil(&candidate) {
            return Some(candidate);
        }
        dir = current.parent();
    }
    None
}

/// The two things this test actually needs from a checkout, so a directory
/// that merely has the right name is not mistaken for the repository.
fn looks_like_devcouncil(root: &Path) -> bool {
    root.join("src/devcouncil").is_dir() && root.join(".venv/bin/python").is_file()
}

/// Locates a Python interpreter with DevCouncil importable, or None.
fn devcouncil_python() -> Option<(PathBuf, PathBuf)> {
    let repo = devcouncil_root()?;
    let python = repo.join(".venv/bin/python");
    let src = repo.join("src");
    Some((python, src))
}

/// Reports that interop could not be exercised, and decides whether that is
/// tolerable.
///
/// `eprintln!` + `return` is what let this rot: cargo prints `ok`, and stderr
/// from a passing test is hidden without `--nocapture`, so three unrun checks
/// were invisible in every summary. `DC_STORE_REQUIRE_INTEROP=1` turns the
/// skip into a failure so CI can demand the evidence; unset, a contributor
/// without a DevCouncil checkout still gets a message rather than a broken
/// build.
fn skip_or_fail(test: &str) {
    let message = format!(
        "interop unverified: no DevCouncil checkout with .venv/bin/python found above {} \
         (set DEVCOUNCIL_ROOT) — the Python side was never driven, so schema and \
         timestamp agreement is UNPROVEN for {test}",
        env!("CARGO_MANIFEST_DIR"),
    );
    assert!(
        std::env::var_os("DC_STORE_REQUIRE_INTEROP").is_none(),
        "{message} (DC_STORE_REQUIRE_INTEROP is set, so this is a failure)"
    );
    eprintln!("SKIP: {message}");
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
        skip_or_fail("python_reads_a_lease_the_rust_store_wrote");
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
        skip_or_fail("the_rust_store_reads_a_lease_python_wrote_and_refuses_to_double_book_it");
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
        skip_or_fail("both_sides_agree_on_when_a_lease_expires");
        return;
    };
    let db = temp_db("expiry");

    let store = Store::open(&db).unwrap();
    // A lease whose one-second life has passed. (A negative TTL would be the
    // obvious way to get here, but the store now refuses those: minting a
    // lease that is born expired while reporting success was a defect, not a
    // feature.)
    let lease = store
        .acquire(&AcquireRequest {
            task_id: "TASK-EXPIRED".into(),
            owner: "rust-builder".into(),
            ttl_seconds: Some(1),
            ..Default::default()
        })
        .unwrap();
    std::thread::sleep(std::time::Duration::from_millis(1100));

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
