"""Sandbox tools: dispatch, the output cap, shell capture, and file-open safety."""
import os
import random
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import tracemalloc

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import mh.tools as toolmod
from mh.tools import Sandbox, ToolError, TOOL_NAMES, cap_output

from mh.checks import FAIL, PASS, check  # noqa: E402


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

# G1. git_guard_roots fails closed. It used to answer an unreadable pointer
# with the start itself, which the final filter then dropped as "the bench's own
# checkout" -- so one unreadable sibling worktrees/<n>/gitdir silently unguarded
# the main checkout and every other worktree, and the hidden tests in them.
print("git_guard_roots refuses a git pointer it cannot follow (G1)")


def make_repo(base, worktrees, nested=()):
    """A hand-built main checkout with linked worktrees, as git lays them out.

    `worktrees` live beside main; `nested` under main/tmp (the layout a lane
    worktree uses). Returns (main, {name: worktree dir}).
    """
    main = os.path.join(base, "main")
    os.makedirs(os.path.join(main, ".git", "worktrees"))
    made = {}
    for name in list(worktrees) + list(nested):
        wt = (os.path.join(main, "tmp", name) if name in nested
              else os.path.join(base, name))
        meta = os.path.join(main, ".git", "worktrees", name)
        os.makedirs(os.path.join(wt, "bench"))
        os.makedirs(meta)
        with open(os.path.join(wt, ".git"), "w") as f:
            f.write(f"gitdir: {meta}\n")
        with open(os.path.join(meta, "gitdir"), "w") as f:
            f.write(os.path.join(wt, ".git") + "\n")
        with open(os.path.join(meta, "commondir"), "w") as f:
            f.write("../..\n")
        made[name] = wt
    return main, made


def guard_error(*starts):
    """The GitGuardError git_guard_roots raises for `starts`, else None."""
    try:
        toolmod.git_guard_roots(*starts)
    except toolmod.GitGuardError as e:
        return e
    return None


def rel(base, roots):
    return sorted(os.path.relpath(r, base) for r in roots)


def unreadable(path):
    """Make `path` unreadable; skipped as root, which ignores file modes."""
    os.chmod(path, 0o000)
    return os.geteuid() != 0 and not os.access(path, os.R_OK)


base = tmpdir()
main, wts = make_repo(base, ["wt_a", "wt_b", "wt_c"])
start = os.path.join(wts["wt_a"], "bench")
full = ["main", "main/.git", "main/.git/worktrees/wt_a", "wt_b", "wt_c"]
check("normal layout: the main checkout, git dirs and siblings are guarded",
      rel(base, toolmod.git_guard_roots(start)) == full,
      str(rel(base, toolmod.git_guard_roots(start))))

pruned = os.path.join(main, ".git", "worktrees", "gone")
os.makedirs(pruned)
with open(os.path.join(pruned, "gitdir"), "w") as f:
    f.write(os.path.join(base, "gone_wt", ".git") + "\n")
check("a pruned worktree (readable pointer, missing target) is skipped, the rest intact",
      not os.path.exists(os.path.join(base, "gone_wt"))
      and rel(base, toolmod.git_guard_roots(start)) == full,
      str(rel(base, toolmod.git_guard_roots(start))))
shutil.rmtree(pruned)

# A stray file beside the worktree entries (macOS's .DS_Store) is not a pointer.
open(os.path.join(main, ".git", "worktrees", ".DS_Store"), "w").close()
check("a stray file among the worktree entries is not mistaken for a broken one",
      rel(base, toolmod.git_guard_roots(start)) == full)
os.remove(os.path.join(main, ".git", "worktrees", ".DS_Store"))

sib = os.path.join(main, ".git", "worktrees", "wt_a", "gitdir")
if unreadable(sib):
    err = guard_error(start)
    check("an unreadable sibling gitdir raises, naming the file (the reported leak)",
          err is not None and sib in str(err) and err.path == sib, str(err))
os.chmod(sib, 0o644)

own = os.path.join(wts["wt_a"], ".git")
if unreadable(own):
    err = guard_error(start)
    check("an unreadable own .git pointer raises, naming the file",
          err is not None and own in str(err), str(err))
os.chmod(own, 0o644)

cdir = os.path.join(main, ".git", "worktrees", "wt_a", "commondir")
if unreadable(cdir):
    err = guard_error(start)
    check("an unreadable commondir raises, naming the file",
          err is not None and cdir in str(err), str(err))
os.chmod(cdir, 0o644)

wtmeta = os.path.join(main, ".git", "worktrees", "wt_b")
os.chmod(wtmeta, 0o000)
if os.geteuid() != 0:
    err = guard_error(start)
    check("an unsearchable worktree entry raises instead of looking absent",
          err is not None and wtmeta in str(err), str(err))
os.chmod(wtmeta, 0o755)

wlist = os.path.join(main, ".git", "worktrees")
os.chmod(wlist, 0o000)
if os.geteuid() != 0:
    err = guard_error(os.path.join(main, "tmp"))
    check("an unlistable worktrees directory raises",
          err is not None and wlist in str(err), str(err))
os.chmod(wlist, 0o755)

with open(sib, "wb") as f:
    f.write(b"\xff\xfe\x80 not utf-8\n")
err = guard_error(start)
check("a non-UTF-8 pointer raises, naming the file",
      err is not None and sib in str(err) and "UnicodeDecodeError" in str(err), str(err))
with open(sib, "w") as f:
    f.write(os.path.join(wts["wt_a"], ".git") + "\n")

for label, text in (("an empty .git pointer", ""), ("a .git file with no gitdir line", "garbage\n")):
    with open(own, "w") as f:
        f.write(text)
    err = guard_error(start)
    check(f"{label} raises", err is not None and own in str(err), str(err))
with open(own, "w") as f:
    f.write(f"gitdir: {os.path.join(main, '.git', 'worktrees', 'wt_a')}\n")
check("the repaired layout guards the full set again",
      rel(base, toolmod.git_guard_roots(start)) == full)
check("GitGuardError is a ContainmentUnavailable, so every existing refusal path handles it",
      issubclass(toolmod.GitGuardError, toolmod.ContainmentUnavailable))

# G2. Review findings: a pointer that parses but names the wrong thing is as
# unguarded as one that does not parse.
print("git_guard_roots validates what a pointer names (G2)")


def put(path, data):
    with open(path, "wb") as f:
        f.write(data)


def variant(label, path, data, who=None):
    """Write `data` to `path`, expect GitGuardError naming `who` (default: path)."""
    saved = open(path, "rb").read()
    try:
        put(path, data)
        err = guard_error(start)
    finally:
        put(path, saved)
    check(label, err is not None and (who or path) in str(err), str(err))


base = tmpdir()
main, wts = make_repo(base, ["wt_a", "wt_b", "wt_c"])
start = os.path.join(wts["wt_a"], "bench")
full = ["main", "main/.git", "main/.git/worktrees/wt_a", "wt_b", "wt_c"]
admin = os.path.join(main, ".git", "worktrees")
own = os.path.join(wts["wt_a"], ".git")
own_text = open(own, "rb").read()
cdir = os.path.join(admin, "wt_a", "commondir")
sib = os.path.join(admin, "wt_b", "gitdir")
sib_text = open(sib, "rb").read()

# B1: the admin directory is there, so the checkout is live; its `gitdir` is
# what names it. Only a stray non-directory entry is skippable.
saved = open(sib, "rb").read()
os.remove(sib)
err = guard_error(start)
check("a worktree admin directory with no gitdir file raises (live checkout, unnameable)",
      err is not None and sib in str(err), str(err))
put(sib, saved)
# A directory entry that is not a worktree at all still has to be named.
os.makedirs(os.path.join(admin, "empty_entry"))
err = guard_error(start)
check("an empty directory entry under worktrees/ raises too",
      err is not None and os.path.join(admin, "empty_entry") in str(err), str(err))
os.rmdir(os.path.join(admin, "empty_entry"))
check("with the pointer restored the full guard set is back",
      rel(base, toolmod.git_guard_roots(start)) == full)

# B2(a): the own `.git` file must lead to a directory that exists.
variant("own .git -> a nonexistent admin directory raises",
        own, b"gitdir: /nonexistent/admin\n")
saved_dir = os.path.join(base, "saved_admin_a")
shutil.move(os.path.join(admin, "wt_a"), saved_dir)
err = guard_error(start)
check("own admin directory removed while the checkout survives raises",
      err is not None and own in str(err), str(err))
shutil.move(saved_dir, os.path.join(admin, "wt_a"))
# B2(b): a present commondir must lead to a directory that exists.
variant("a commondir naming a nonexistent directory raises",
        cdir, b"../../nonexistent\n")
open(os.path.join(main, ".git", "HEAD"), "w").close()
variant("a commondir naming a file raises", cdir, b"../../HEAD\n")
# B2(c): a `.git` file is `gitdir: <rest of file>`, as git reads it (git 2.54
# fails "not a git repository" on all of these), so an extra line is part of
# the path and the result does not exist.
variant("a .git file with a trailing extra line raises", own, own_text + b"extra\n")
variant("a .git file with trailing spaces raises", own, own_text.rstrip(b"\n") + b"   \n")
variant("a .git file with two spaces after the colon raises",
        own, own_text.replace(b"gitdir: ", b"gitdir:  "))
variant("a .git file without the space after the colon raises",
        own, own_text.replace(b"gitdir: ", b"gitdir:"))
variant("a .git file with a leading blank line raises", own, b"\n" + own_text)
variant("a .git file in upper case raises", own, own_text.replace(b"gitdir", b"GITDIR"))
for label, text in (("CRLF", own_text.rstrip(b"\n") + b"\r\n"),
                    ("no trailing newline", own_text.rstrip(b"\n"))):
    put(own, text)
    check(f"a .git file with {label} still parses (git accepts it)",
          rel(base, toolmod.git_guard_roots(start)) == full)
put(own, own_text)

# U1: git rejects a byte-order mark ("invalid gitfile format" on .git, "Invalid
# path" on commondir), so a pointer carrying one is refused, not read leniently.
bom = b"\xef\xbb\xbf"
variant("a BOM in a sibling gitdir raises (it used to drop the sibling silently)",
        sib, bom + sib_text)
variant("a BOM in the own .git raises", own, bom + own_text)
variant("a BOM in commondir raises", cdir, bom + open(cdir, "rb").read())
variant("a sibling gitdir spanning two lines raises", sib, sib_text + b"junk\n")
variant("a sibling gitdir starting with whitespace raises", sib, b" " + sib_text)
check("a sibling gitdir pointing at a removed checkout is still exempt (pruned)",
      (put(sib, os.path.join(base, "removed_wt", ".git").encode() + b"\n"),
       rel(base, toolmod.git_guard_roots(start)))[1]
      == ["main", "main/.git", "main/.git/worktrees/wt_a", "wt_c"])
put(sib, sib_text)

# The same layout as real git writes it: the hand-built one above must not
# drift from what `git worktree add` produces.
if shutil.which("git"):
    def git(*args, cwd):
        return subprocess.run(["git", "-c", "user.name=t", "-c", "user.email=t@t",
                               "-c", "commit.gpgsign=false", *args], cwd=cwd,
                              check=True, capture_output=True, text=True)
    gbase = tmpdir()
    gmain = os.path.join(gbase, "main")
    os.makedirs(gmain)
    git("init", "-q", cwd=gmain)
    open(os.path.join(gmain, "f"), "w").close()
    git("add", "f", cwd=gmain)
    git("commit", "-q", "-m", "i", cwd=gmain)
    for n in ("a", "b"):
        git("worktree", "add", "-q", os.path.join(gbase, n), "-b", n, cwd=gmain)
    gstart = os.path.join(gbase, "a", "bench")
    os.makedirs(gstart)
    check("real git worktrees: main, its git dir, the admin dir and the sibling are guarded",
          rel(gbase, toolmod.git_guard_roots(gstart))
          == ["b", "main", "main/.git", "main/.git/worktrees/a"],
          str(rel(gbase, toolmod.git_guard_roots(gstart))))
    gsib = os.path.join(gmain, ".git", "worktrees", "b", "gitdir")
    os.remove(gsib)
    st = subprocess.run(["git", "status", "-s"], cwd=os.path.join(gbase, "b"),
                        capture_output=True, text=True)
    err = guard_error(gstart)
    check("real git: a sibling whose gitdir file is gone still runs git, and the guard refuses",
          st.returncode == 0 and err is not None and gsib in str(err),
          f"git rc={st.returncode} {err}")
    shutil.rmtree(gbase, ignore_errors=True)
shutil.rmtree(base, ignore_errors=True)

nbase = tmpdir()
nmain, nwts = make_repo(nbase, ["wt_b"], nested=["lane"])
nstart = os.path.join(nwts["lane"], "bench")
nroots = rel(nbase, toolmod.git_guard_roots(nstart))
check("nested worktree layout: outer checkout, git dirs and sibling all guarded, own tree not",
      nroots == ["main", "main/.git", "main/.git/worktrees/lane", "wt_b"], str(nroots))
nsib = os.path.join(nmain, ".git", "worktrees", "wt_b", "gitdir")
if unreadable(nsib):
    err = guard_error(nstart)
    check("nested layout: an unreadable sibling gitdir raises rather than unguarding the outer checkout",
          err is not None and nsib in str(err), str(err))
os.chmod(nsib, 0o644)
shutil.rmtree(base, ignore_errors=True)
shutil.rmtree(nbase, ignore_errors=True)

print()
print(f"{len(PASS)} passed, {len(FAIL)} failed")
if FAIL:
    sys.exit(1)
