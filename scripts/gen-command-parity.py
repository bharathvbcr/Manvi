#!/usr/bin/env python3
"""Regenerate testdata/command-parity.tsv from DevCouncil's own policy engine.

The Go command gate is a port, and the failure mode of a port is silent
divergence — a command the incumbent denies that the port allows, or the
reverse, with nothing failing. This drives the real `TaskPolicyEngine` over a
corpus covering every rung of the ladder and every normalisation quirk, and
records what it decided.

Three sections, because the Python engine exposes the halves separately:

    NORM      normalize_allowlist_command(cmd)          -> string
    ALLOW     evaluate_command(cmd, task_or_none)       -> allow|warn|deny
    GIT       evaluate_hook_command(cmd)                -> allow|warn|deny

Usage, from a checkout with DevCouncil importable:

    DEVCOUNCIL_SRC=../DevCouncil/src python3 scripts/gen-command-parity.py \
        > testdata/command-parity.tsv
"""

import os
import sys
from pathlib import Path

DEFAULT_SRC = Path(__file__).resolve().parents[2] / "DevCouncil" / "src"
sys.path.insert(0, os.environ.get("DEVCOUNCIL_SRC", str(DEFAULT_SRC)))

from devcouncil.domain.task import Task  # noqa: E402
from devcouncil.execution.policy_engine import (  # noqa: E402
    TaskPolicyEngine,
    normalize_allowlist_command,
)

# Commands chosen to exercise each rung: bootstrap allowlist, lease lifecycle,
# cd chaining, path-prefixed and uv-wrapped dev binaries, redirect stripping,
# the task allowlist, and every git-safety rule including the refspec form of a
# force push that carries no --force flag.
COMMANDS = [
    "",
    "   ",
    "dev status",
    "dev status --json",
    "dev tasks",
    "dev next-task",
    "dev checkout TASK-001",
    "dev approve",
    "git status",
    "git diff",
    "git diff --stat",
    "echo hello",
    "true",
    ":",
    "dev map",
    "dev map query foo",
    "dev graph query foo",
    "dev doctor",
    "dev release TASK-001",
    "dev scope update",
    "dev run-cmd pytest",
    "pytest tests/",
    "python -m pytest tests/ -q",
    "uv run pytest tests/",
    "uv run dev map",
    "uv run --project . dev map",
    "uv run --directory /repo dev status",
    "uv run -p /repo dev map query x",
    ".venv/bin/dev map",
    "/abs/path/.venv/bin/dev status",
    "./scripts/dev map",
    "DevCouncil/dev map",
    "devcouncil status",
    "dev map > /tmp/out.json",
    "dev map >> /tmp/out.json",
    "dev map 2> /tmp/err",
    "dev map 2>&1",
    "cd repo",
    "cd repo && dev map",
    "pushd /tmp",
    "popd",
    "cdx something",
    "rm -rf /",
    "npm install",
    "make build",
    "cargo test",
    "go test ./...",
    "git commit -m 'x'",
    "git commit --no-verify -m 'x'",
    "git commit --no-gpg-sign -m 'x'",
    "git reset --hard main",
    "git reset --hard origin/main",
    "git reset --hard origin/master",
    "git reset --hard HEAD~1",
    "git push origin --force",
    "git push origin --force-with-lease",
    "git push -f origin feature",
    "git push origin +HEAD:master",
    "git push origin +refs/heads/x:refs/heads/y",
    "git push origin main",
    "git push origin HEAD:main",
    "git push origin master:refs/heads/master",
    "git push origin feature",
    "GIT PUSH ORIGIN --FORCE",
]

TASK_ALLOWED = ["pytest *", "go test *", "make build", ".venv/bin/dev *"]


def main() -> None:
    root = Path.cwd()
    engine = TaskPolicyEngine(root, global_allowed_commands=["npm install"])
    task = Task(
        id="TASK-001",
        title="parity",
        description="parity corpus",
        allowed_commands=list(TASK_ALLOWED),
    )

    print(
        "# section\tcommand\tresult  — generated from DevCouncil's TaskPolicyEngine; "
        "regenerate with scripts/gen-command-parity.py"
    )
    print(f"# task allowed_commands: {' | '.join(TASK_ALLOWED)}")
    print("# global allowed_commands: npm install")

    for cmd in COMMANDS:
        print(f"NORM\t{cmd}\t{normalize_allowlist_command(cmd)}")
    for cmd in COMMANDS:
        print(f"ALLOW_TASK\t{cmd}\t{engine.evaluate_command(cmd, task).action}")
    for cmd in COMMANDS:
        print(f"ALLOW_NOTASK\t{cmd}\t{engine.evaluate_command(cmd, None).action}")
    for cmd in COMMANDS:
        print(f"GIT\t{cmd}\t{engine.evaluate_hook_command(cmd).action}")


if __name__ == "__main__":
    main()
