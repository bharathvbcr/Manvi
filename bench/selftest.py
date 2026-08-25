"""Validate the task suite before spending GPU time on it.

Every task must (a) fail out of the box, (b) pass with the reference solution,
(c) fail if the visible test is tampered with, (d) fail if the sandbox contains a
file the interpreter would load on its own, and (e) still pass when it is
verified a second time over a sandbox that already holds the artefacts the first
verification built. A task that does not satisfy all five is not measuring
anything.

(d) is the suite-wide form of the sitecustomize hole: a two-line
`sitecustomize.py` in the sandbox used to pass 18 of these 19 tasks with no task
file touched. (e) is its counterweight -- the additions check must not fail an
honest solution whose build leaves a binary behind.

NOTE: this validates the instrument. The frozen grid under bench/results/ was
collected with the pre-hardening instrument and is not re-run or re-scored.
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

        sb = t.materialise(os.path.join(tmp, t.name + "-hook"))
        overlay(os.path.join(t.dir, "reference"), sb)
        with open(os.path.join(sb, "sitecustomize.py"), "w") as f:
            f.write("import os\nos._exit(0)\n")
        ok3, out3 = t.verify(sb)
        row.append("hook-rejected=" + ("yes" if not ok3 else "NO"))
        if ok3:
            failures.append(f"{t.name}: a sitecustomize.py in the sandbox still passed")

        # Verify twice over one sandbox: the second run sees whatever the first
        # one built (envbuild's `stats`, any bytecode cache) as agent additions.
        sb = t.materialise(os.path.join(tmp, t.name + "-again"))
        overlay(os.path.join(t.dir, "reference"), sb)
        okA, _ = t.verify(sb)
        okB, outB = t.verify(sb)
        row.append("re-verify=" + ("yes" if (okA and okB) else "NO"))
        if okA and not okB:
            failures.append(f"{t.name}: verification is not repeatable; the second "
                            f"run refused what the first one built:\n{outB[:900]}")

        print("  ".join(row), flush=True)

    shutil.rmtree(tmp, ignore_errors=True)
    print()
    if failures:
        print(f"SUITE INVALID -- {len(failures)} problem(s):\n")
        for f in failures:
            print(" -", f)
        return 1
    print(f"suite valid: {len(tasks)} tasks start broken, accept their reference "
          f"solution, reject test tampering and interpreter start-up hooks, and "
          f"still accept what an honest solution builds")
    return 0


sys.exit(main())
