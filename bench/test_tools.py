"""Sandbox tools: dispatch, the output cap, shell capture, and file-open safety."""
import os
import random
import subprocess
import sys
import tempfile
import threading
import time
import tracemalloc

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import mh.tools as toolmod
from mh.tools import Sandbox, ToolError, TOOL_NAMES, cap_output

PASS, FAIL = [], []


def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print(f"  {'ok  ' if cond else 'FAIL'} {name}" + (f"  {detail}" if not cond and detail else ""))


def tmpdir():
    return os.path.realpath(tempfile.mkdtemp(prefix="mh-tools-"))


# L4. First on purpose: nothing before it has allocated much, and tracemalloc
# measures this process's Python allocations, not a lifetime RSS high-water mark
# that an earlier test could already have raised.
print("run_shell holds a bounded amount of output in memory (L4)")
root = tmpdir()
sb = Sandbox(root)
N = 64_000_000
tracemalloc.start()
t0 = time.time()
res = sb.run_shell(cmd=f"yes | head -c {N}")
elapsed = time.time() - t0
_, peak = tracemalloc.get_traced_memory()
tracemalloc.stop()
check("a 64 MB producer is capped", "bytes elided by the harness" in res, res[:120])
check("and the harness never held it: peak Python allocation < 4 MB",
      peak < 4_000_000, f"peak={peak:,} bytes over {N:,} produced")
check("the banner counts every byte the command produced",
      f"[{N - 2 * (toolmod.MAX_OUTPUT_BYTES // 2)} bytes elided" in res, res[15000:15200])
stats = getattr(sb, "last_shell_stats", None) or {}
check("the total bytes seen are recorded on the sandbox",
      stats.get("stdout_bytes") == N and stats.get("stderr_bytes") == 0, str(stats))
check("and it stays quick", elapsed < 10, f"{elapsed:.1f}s")


print("sandbox handlers")
handlers = sb.handlers()
check("finish is a named handler, not a getattr", handlers["finish"] == sb.finish)
check("every advertised tool has a handler", set(handlers) == set(TOOL_NAMES))
check("finish returns the summary", sb.finish(summary="done") == "done")


# L5. The retained payload -- head plus tail, banner excluded -- never exceeds
# the limit, and a limit of 1 keeps nothing rather than the whole input.
print("cap_output degenerate limits (L5)")


def payload(body):
    """(head, tail) around the banner of a capped body."""
    head, rest = body.split("\n\n... [", 1)
    tail = rest.split("] ...\n\n", 1)[1]
    return head, tail


body, trunc = cap_output("abcdef", 1)
h, t = payload(body)
check("limit=1 keeps no head and no tail", trunc and h == "" and t == "", repr(body))
check("limit=1 does not return the whole input as the tail", "abcdef" not in body,
      repr(body))
rng = random.Random(1234)
alphabet = "ab\n漢é😀"
worst = []
for _ in range(2000):
    text = "".join(rng.choice(alphabet) for _ in range(rng.randint(0, 60)))
    limit = rng.randint(1, 70)
    body, trunc = cap_output(text, limit)
    raw = text.encode("utf-8")
    if not trunc:
        if body != text or len(raw) > limit:
            worst.append((text, limit, "uncapped"))
        continue
    h, t = payload(body)
    if len(h.encode()) + len(t.encode()) > limit:
        worst.append((text, limit, "payload over limit"))
    if not raw.startswith(h.encode()) or not raw.endswith(t.encode()):
        worst.append((text, limit, "not a head/tail of the input"))
    keep = limit // 2
    if f"[{len(raw) - 2 * keep} bytes elided" not in body:
        worst.append((text, limit, "banner count"))
check("retained head+tail never exceeds the limit, for any limit >= 1",
      not worst, str(worst[:3]))
check("an input of exactly the limit is returned whole",
      cap_output("x" * 30_000, 30_000) == ("x" * 30_000, False))
check("one byte over is capped", cap_output("x" * 30_001, 30_000)[1])


# L4 equivalence. The streamed capture must equal what the old code produced:
# the whole output decoded (errors=replace, universal newlines), stderr joined
# on, then cap_output. The reference below is that old path, written out.
print("streamed capture equals cap_output of the full output (L4)")
ENC = "utf-8" if sys.flags.utf8_mode else __import__("locale").getencoding()


def old_decode(b):
    return b.decode(ENC, "replace").replace("\r\n", "\n").replace("\r", "\n")


def old_body(out_b, err_b, limit):
    out, err = old_decode(out_b), old_decode(err_b)
    body = out
    if err.strip():
        body += ("\n" if body and not body.endswith("\n") else "") + "[stderr]\n" + err
    body, _ = cap_output(body, limit)
    return body


PIECES = [b"a", b"\n", b"\r", b"\r\n", "漢".encode(), "😀".encode(), b"\xff",
          b"\xe6\xbc", b" ", b"\t", "é".encode()]


def rand_bytes(r, n):
    out = bytearray()
    while len(out) < n:
        out += r.choice(PIECES)
    return bytes(out[:n])


acc_cls = getattr(toolmod, "StreamCapture", None)
check("a streaming capture exists", acc_cls is not None)
if acc_cls is not None:
    bad = []
    r = random.Random(99)
    for case in range(3000):
        limit = r.choice([0, 1, 2, 3, 7, 8, 31, 64, 65, r.randint(1, 400)])
        sizes = [0, 1, max(0, limit - 1), limit, limit + 1, 2 * limit,
                 r.randint(0, 3 * limit + 10)]
        out_b = rand_bytes(r, r.choice(sizes))
        err_b = rand_bytes(r, r.choice(sizes))
        if case % 7 == 0:
            err_b = b" \n\t" * r.randint(0, 5)   # whitespace-only stderr
        cap_o, cap_e = acc_cls(limit), acc_cls(limit)
        for data, acc in ((out_b, cap_o), (err_b, cap_e)):
            i = 0
            while i < len(data):
                step = r.randint(1, 9)
                acc.feed(data[i:i + step])
                i += step
            acc.close()
        got = toolmod.shell_body(cap_o, cap_e, limit)
        want = old_body(out_b, err_b, limit)
        if got != want:
            bad.append((out_b[:40], err_b[:40], limit))
        if cap_o.raw_bytes != len(out_b) or cap_e.raw_bytes != len(err_b):
            bad.append(("raw count", len(out_b), cap_o.raw_bytes))
    check("3,000 random chunkings: streamed body == cap_output(full body)",
          not bad, str(bad[:3]))

    # Memory is bounded by the limit, not by the input.
    acc = acc_cls(1000)
    for _ in range(2000):
        acc.feed(b"y\n" * 5000)
    acc.close()
    check("a capture holds O(limit) bytes whatever it is fed",
          acc.held_bytes() <= 4 * 1000 + 8192, str(acc.held_bytes()))

# End to end through a real shell, including the exact-limit boundary.
root2 = tmpdir()
e2e_bad = []
r = random.Random(7)
for limit in (1, 2, 64, 65, 1000):
    for n_out, n_err in ((limit, 0), (limit + 1, 0), (0, limit), (limit // 2, limit),
                         (3 * limit, 5), (r.randint(0, 2 * limit), r.randint(0, 2 * limit))):
        out_b, err_b = rand_bytes(r, n_out), rand_bytes(r, n_err)
        with open(os.path.join(root2, "o.bin"), "wb") as f:
            f.write(out_b)
        with open(os.path.join(root2, "e.bin"), "wb") as f:
            f.write(err_b)
        s = Sandbox(root2, output_cap=limit)
        got = s.run_shell(cmd="cat o.bin; cat e.bin >&2")
        body = old_body(out_b, err_b, limit)
        want = f"exit=0\n{body}" if body.strip() else "exit=0\n(no output)"
        if got != want:
            e2e_bad.append((limit, n_out, n_err))
check("real run_shell output equals the old capture, byte for byte",
      not e2e_bad, str(e2e_bad[:5]))


# L7. A backgrounded process must not outlive the command that started it.
print("run_shell leaves nothing running behind it (L7)")


def alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def gone_within(pid, seconds=3.0):
    deadline = time.time() + seconds
    while time.time() < deadline:
        if not alive(pid):
            return True
        time.sleep(0.05)
    return False


root3 = tmpdir()
sb3 = Sandbox(root3)
out = sb3.run_shell(cmd="nohup sleep 300 >/dev/null 2>&1 & echo $!")
pid = int(out.split("\n")[1].strip())
ok = gone_within(pid)
check("nohup sleep 300 & is killed when the command returns", ok, f"pid {pid}")
if not ok:
    os.kill(pid, 9)
out = sb3.run_shell(cmd="(sleep 300; echo late) >/dev/null 2>&1 & echo $!")
pid = int(out.split("\n")[1].strip())
ok = gone_within(pid)
check("a backgrounded subshell is killed too", ok, f"pid {pid}")
if not ok:
    os.kill(pid, 9)
rc, o, e = toolmod.run_bounded(["/bin/sh", "-c", "sleep 300 >/dev/null 2>&1 & echo $!"],
                               timeout=30)
pid = int(o.strip())
ok = gone_within(pid)
check("run_bounded (the verifier's path) reaps the group too", ok, f"pid {pid}")
if not ok:
    os.kill(pid, 9)
rc, o, e = toolmod.run_bounded(["/bin/sh", "-c", "printf 'a\\r\\nb'; printf 'x' >&2; exit 3"])
check("run_bounded keeps its (rc, out, err) contract", (rc, o, e) == (3, "a\nb", "x"),
      repr((rc, o, e)))
rc, o, e = toolmod.run_bounded(["/bin/cat"], input_text="é" * 200_000, timeout=30)
check("run_bounded still feeds large stdin without deadlock",
      rc == 0 and o == "é" * 200_000, f"rc={rc} len={len(o)}")
try:
    toolmod.run_bounded(["/bin/sh", "-c", "sleep 30"], timeout=0.5)
    timed_out = False
except subprocess.TimeoutExpired:
    timed_out = True
check("run_bounded still raises TimeoutExpired", timed_out)


# L7 TOCTOU. resolve() checks a path and the open happens afterwards; a path
# component swapped for a symlink in between must be refused by the open itself.
print("file tools refuse a path swapped after it was checked (L7)")


class SwapSandbox(Sandbox):
    """Swaps a path component for a symlink right after resolve() approves it."""

    def __init__(self, root, swap):
        super().__init__(root)
        self.swap = swap

    def resolve(self, path):
        real = super().resolve(path)
        if self.swap:
            self.swap()
            self.swap = None
        return real


outside = tmpdir()
with open(os.path.join(outside, "f.txt"), "w") as f:
    f.write("SECRET mine\n")


def fresh(target_dir):
    base = tmpdir()
    os.makedirs(os.path.join(base, "d"))
    with open(os.path.join(base, "d", "f.txt"), "w") as f:
        f.write("mine\n")
    inside = os.path.join(base, "elsewhere")
    os.makedirs(inside)
    with open(os.path.join(inside, "f.txt"), "w") as f:
        f.write("INSIDE mine\n")

    def swap_dir():
        os.rename(os.path.join(base, "d"), os.path.join(base, "d.orig"))
        os.symlink(target_dir or inside, os.path.join(base, "d"))

    def swap_file():
        os.remove(os.path.join(base, "d", "f.txt"))
        os.symlink(os.path.join(target_dir or inside, "f.txt"),
                   os.path.join(base, "d", "f.txt"))
    return base, swap_dir, swap_file


def refused(fn):
    try:
        fn()
    except ToolError:
        return True
    return False


for label, target in (("outside the sandbox", outside), ("inside the sandbox", None)):
    for which in ("dir", "file"):
        base, swap_dir, swap_file = fresh(target)
        swap = swap_dir if which == "dir" else swap_file
        s = SwapSandbox(base, swap)
        check(f"read_file: {which} swapped to a symlink {label} is refused",
              refused(lambda: s.read_file(path="d/f.txt")))
        base, swap_dir, swap_file = fresh(target)
        swap = swap_dir if which == "dir" else swap_file
        s = SwapSandbox(base, swap)
        check(f"write_file: {which} swapped to a symlink {label} is refused",
              refused(lambda: s.write_file(path="d/f.txt", content="PWNED\n")))
        base, swap_dir, swap_file = fresh(target)
        swap = swap_dir if which == "dir" else swap_file
        s = SwapSandbox(base, swap)
        check(f"edit_file: {which} swapped to a symlink {label} is refused",
              refused(lambda: s.edit_file(path="d/f.txt", old="mine", new="PWNED")))
with open(os.path.join(outside, "f.txt")) as f:
    check("the file outside the sandbox was never written", f.read() == "SECRET mine\n")

# Symlinks that resolve inside the sandbox still work: resolve() follows them,
# and the open walks the resolved, symlink-free path.
base = tmpdir()
os.makedirs(os.path.join(base, "src"))
with open(os.path.join(base, "src", "m.py"), "w") as f:
    f.write("x = 1\n")
os.symlink("src", os.path.join(base, "lib"))
s = Sandbox(base)
check("a legitimate in-sandbox symlink can still be read",
      "x = 1" in s.read_file(path="lib/m.py"))
s.write_file(path="lib/m.py", content="x = 2\n")
check("and written", open(os.path.join(base, "src", "m.py")).read() == "x = 2\n")
s.edit_file(path="lib/m.py", old="x = 2", new="x = 3")
check("and edited", open(os.path.join(base, "src", "m.py")).read() == "x = 3\n")
s.write_file(path="new/deep/n.txt", content="hi")
check("write_file still creates missing directories",
      open(os.path.join(base, "new", "deep", "n.txt")).read() == "hi")
check("a directory still lists", "is a directory" in s.read_file(path="src"))
check("a directory is refused for write",
      refused(lambda: s.write_file(path="src", content="x")))
check("a missing file is still a clear refusal",
      refused(lambda: s.read_file(path="nope.txt")))

# A FIFO made by the model used to hang the harness forever in open().
fifo = os.path.join(base, "p")
os.mkfifo(fifo)
result = {}


def _read_fifo():
    try:
        s.read_file(path="p")
        result["r"] = "returned"
    except ToolError as e:
        result["r"] = f"refused: {e}"


th = threading.Thread(target=_read_fifo, daemon=True)
th.start()
th.join(3)
hung = th.is_alive()
if hung:
    fd = os.open(fifo, os.O_WRONLY | os.O_NONBLOCK)   # unblock the reader
    os.close(fd)
    th.join(3)
check("read_file on a FIFO is refused instead of hanging the harness",
      not hung and result.get("r", "").startswith("refused"), str(result))

print()
print(f"{len(PASS)} passed, {len(FAIL)} failed")
if FAIL:
    sys.exit(1)
