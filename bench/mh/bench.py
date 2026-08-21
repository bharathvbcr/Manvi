"""Task loading, sandboxing and verification.

The verifier is the only thing that decides pass/fail. It lives outside the
sandbox, the agent never sees it, and it re-checks the protected files so that
editing the visible test cannot buy a pass. A check that did not run must never
report the same result as a check that ran and passed.
"""
import hashlib
import json
import os
import shutil
import subprocess
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
TASKS_DIR = os.path.join(os.path.dirname(HERE), "tasks")


def _sha(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


class Task:
    def __init__(self, directory):
        self.dir = directory
        with open(os.path.join(directory, "task.json")) as f:
            self.meta = json.load(f)
        self.name = self.meta["name"]
        self.kind = self.meta["kind"]
        self.timeout = self.meta.get("timeout", 120)
        self.blurb = self.meta.get("blurb", "")
        self.protect = self.meta.get("protect", [])
        self.setup_dir = os.path.join(directory, "setup")
        with open(os.path.join(directory, "TASK.md")) as f:
            self.instruction = f.read().strip()
        self.hidden = os.path.join(directory, self.meta["hidden"])
        self._baseline = {p: _sha(os.path.join(self.setup_dir, p))
                          for p in self.protect}

    def materialise(self, dest):
        """Copy a pristine copy of the task into dest."""
        if os.path.exists(dest):
            shutil.rmtree(dest)
        shutil.copytree(self.setup_dir, dest)
        return dest

    def tampered(self, sandbox):
        """Protected files that were modified or deleted by the agent."""
        out = []
        for rel, want in self._baseline.items():
            p = os.path.join(sandbox, rel)
            if not os.path.exists(p):
                out.append(f"{rel} (deleted)")
            elif _sha(p) != want:
                out.append(f"{rel} (modified)")
        return out

    def verify(self, sandbox):
        """Return (passed, output). Never raises."""
        bad = self.tampered(sandbox)
        if bad:
            return False, ("VERIFY FAILED: protected files were changed: "
                           + ", ".join(bad)
                           + "\nThe task requires fixing the code, not the tests.")
        # Run the hidden test from a directory the agent never touched, so the
        # agent cannot shadow it with a file of its own.
        runner = tempfile.mkdtemp(prefix="mhverify-")
        try:
            local = os.path.join(runner, os.path.basename(self.hidden))
            shutil.copy2(self.hidden, local)
            if self.kind == "python":
                cmd = ["python3", local]
                env = dict(os.environ, PYTHONPATH=sandbox, PYTHONDONTWRITEBYTECODE="1")
                cwd = sandbox
            elif self.kind == "shell":
                cmd = ["/bin/sh", local, sandbox]
                env = dict(os.environ)
                cwd = runner
            else:
                return False, f"VERIFY ERROR: unknown task kind {self.kind!r}"
            try:
                proc = subprocess.run(cmd, cwd=cwd, env=env, capture_output=True,
                                      text=True, errors="replace",
                                      timeout=self.timeout)
            except subprocess.TimeoutExpired:
                return False, (f"VERIFY FAILED: hidden checks timed out after "
                               f"{self.timeout}s")
            body = (proc.stdout or "") + (proc.stderr or "")
            return proc.returncode == 0, f"exit={proc.returncode}\n{body.strip()}"
        except Exception as e:
            # An infrastructure failure is not a pass.
            return False, f"VERIFY ERROR: {type(e).__name__}: {e}"
        finally:
            shutil.rmtree(runner, ignore_errors=True)


def load_tasks(names=None, tasks_dir=TASKS_DIR):
    out = []
    for entry in sorted(os.listdir(tasks_dir)):
        d = os.path.join(tasks_dir, entry)
        if not os.path.isfile(os.path.join(d, "task.json")):
            continue
        if names and entry not in names:
            continue
        out.append(Task(d))
    if names:
        found = {t.name for t in out}
        missing = set(names) - found
        if missing:
            raise SystemExit(f"no such task(s): {', '.join(sorted(missing))}")
    return out
