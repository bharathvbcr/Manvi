"""Validate the task suite before spending GPU time on it.

Every task must (a) fail out of the box, (b) pass with the reference solution,
and (c) fail if the visible test is tampered with. A task that does not satisfy
all three is not measuring anything.
"""
import os, shutil, sys, tempfile
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mh.bench import load_tasks


def overlay(src, dest):
    for root, _, files in os.walk(src):
        rel = os.path.relpath(root, src)
        target = os.path.join(dest, rel) if rel != "." else dest
        os.makedirs(target, exist_ok=True)
        for f in files:
            shutil.copy2(os.path.join(root, f), os.path.join(target, f))


def main():
    tasks = load_tasks(sys.argv[1:] or None)
    tmp = tempfile.mkdtemp(prefix="mhselftest-")
    failures = []
    for t in tasks:
        row = [f"{t.name:24s}"]

        sb = t.materialise(os.path.join(tmp, t.name + "-broken"))
        ok0, out0 = t.verify(sb)
        row.append("starts-broken=" + ("FAIL(passes already!)" if ok0 else "yes"))
        if ok0:
            failures.append(f"{t.name}: passes without any fix")

        sb = t.materialise(os.path.join(tmp, t.name + "-ref"))
        overlay(os.path.join(t.dir, "reference"), sb)
        ok1, out1 = t.verify(sb)
        row.append("reference-passes=" + ("yes" if ok1 else "NO"))
        if not ok1:
            failures.append(f"{t.name}: reference solution does NOT pass:\n{out1[:900]}")

        sb = t.materialise(os.path.join(tmp, t.name + "-tamper"))
        overlay(os.path.join(t.dir, "reference"), sb)
        victim = os.path.join(sb, t.protect[0])
        with open(victim, "a") as f:
            f.write("\n# tampered\n")
        ok2, out2 = t.verify(sb)
        row.append("tamper-caught=" + ("yes" if not ok2 else "NO"))
        if ok2:
            failures.append(f"{t.name}: tampering with {t.protect[0]} still passed")

        print("  ".join(row), flush=True)

    shutil.rmtree(tmp, ignore_errors=True)
    print()
    if failures:
        print(f"SUITE INVALID -- {len(failures)} problem(s):\n")
        for f in failures:
            print(" -", f)
        return 1
    print(f"suite valid: {len(tasks)} tasks start broken, accept their reference "
          f"solution, and reject test tampering")
    return 0


sys.exit(main())
