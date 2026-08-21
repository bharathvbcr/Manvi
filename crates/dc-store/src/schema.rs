//! The DevCouncil schema this crate reads and writes.
//!
//! Transcribed from a live `.devcouncil/state.sqlite` rather than from the
//! SQLModel class definitions, because what the harness has to interoperate
//! with is the file on disk, not the ORM's intent. Every statement is
//! `IF NOT EXISTS`, so opening an existing DevCouncil store leaves it exactly
//! as it was.
//!
//! `ux_task_leases_active` is the important line. It is the partial unique
//! index that makes two builders racing for one task resolve to exactly one
//! winner, and it must exist before the store is used concurrently — the
//! check-then-insert in `acquire` is a friendlier error message, not the
//! safety mechanism.

/// Idempotent DDL for the tables the harness touches.
pub const SCHEMA: &str = r#"
CREATE TABLE IF NOT EXISTS tasks (
    id VARCHAR NOT NULL,
    title VARCHAR NOT NULL,
    description VARCHAR NOT NULL,
    requirement_ids_json VARCHAR NOT NULL DEFAULT '[]',
    acceptance_criterion_ids_json VARCHAR NOT NULL DEFAULT '[]',
    planned_files_json VARCHAR NOT NULL DEFAULT '[]',
    expected_tests_json VARCHAR NOT NULL DEFAULT '[]',
    allowed_commands_json VARCHAR NOT NULL DEFAULT '[]',
    forbidden_changes_json VARCHAR NOT NULL DEFAULT '[]',
    status VARCHAR NOT NULL DEFAULT 'planned',
    difficulty VARCHAR,
    agent_appended_expected_tests_json VARCHAR NOT NULL DEFAULT '[]',
    agent_appended_allowed_commands_json VARCHAR NOT NULL DEFAULT '[]',
    priority VARCHAR,
    agent_appended_planned_files_json VARCHAR NOT NULL DEFAULT '[]',
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS task_leases (
    id VARCHAR NOT NULL,
    task_id VARCHAR NOT NULL,
    owner VARCHAR NOT NULL,
    agent VARCHAR,
    client_id VARCHAR,
    run_id VARCHAR,
    branch VARCHAR,
    lease_token VARCHAR NOT NULL,
    status VARCHAR NOT NULL,
    created_at VARCHAR NOT NULL,
    expires_at VARCHAR,
    released_at VARCHAR,
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS ix_task_leases_task_id ON task_leases (task_id);
CREATE INDEX IF NOT EXISTS ix_task_leases_status ON task_leases (status);

-- The mutual-exclusion primitive. Without this, two concurrent acquires both
-- pass their pre-check and both insert.
CREATE UNIQUE INDEX IF NOT EXISTS ux_task_leases_active
    ON task_leases (task_id) WHERE status = 'active';
"#;
