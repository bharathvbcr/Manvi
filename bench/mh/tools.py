"""Tool surface for the harness.

Five tools, deliberately. Small models degrade as the tool count grows, and every
tool here earns its place by removing a class of failure: read/write/edit exist so
the model never has to get shell quoting right to touch a file.

Everything is confined to one sandbox directory. The agent is a 27-35B model running
real shell on a real machine, so containment is enforced on our side -- by resolved
path for the file tools, and by an OS sandbox for run_shell. Until the hardening pass
that added `contained_argv`, only the file tools were contained: run_shell was a plain
`/bin/bash -lc` in the sandbox, from which `../../tasks/<name>/hidden_test.py` was
both readable and writable. That is now refused by the kernel, not by convention.

NOTE: this file is the measuring instrument. The frozen grid under bench/results/ was
collected and analysed with the pre-hardening version and is NOT to be re-run or
re-scored; everything here applies to future runs only.
"""
import os
import signal
import shlex
import shutil
import subprocess
import sys
import tempfile

MAX_OUTPUT_BYTES = 30_000       # Terminus-KIRA's cap, inherited by the paper's harness
SHELL_TIMEOUT_S = 120
DIR_LIST_LIMIT = 200            # entries shown for a directory listing
MAX_READ_BYTES = 8_000_000      # hard ceiling on what read_file pulls into memory

# The benchmark installation: tasks/, results/, paper/ and the harness itself. A
# shell command run by the agent must not be able to read or write any of it --
# reading tasks/<name>/hidden_test.py is a complete cheat, and writing it poisons
# every later episode for that task.
BENCH_ROOT = os.path.dirname(os.path.dirname(os.path.realpath(__file__)))

# Escape hatch for platforms where no containment backend is implemented. Setting
# it does not make the run contained; it makes an uncontained run possible and
# loud. Harness records it on every episode so no result can silently claim
# containment it did not have.
UNCONTAINED_ENV = "MH_UNCONTAINED_SHELL"


class ToolError(Exception):
    """Recoverable: the message goes back to the model as the tool result."""


class ContainmentUnavailable(RuntimeError):
    """No OS containment backend on this platform, and no explicit opt-out."""


def cap_output(text, limit=MAX_OUTPUT_BYTES):
    """Head+tail truncation, measured in UTF-8 bytes.

    Never drop the middle silently: the tail of a build log holds the error and the
    head holds the command, and a model shown a quietly-truncated log will confidently
    reason about output it never saw.

    The cap is a *byte* cap, as the name and the banner have always said. It used to
    count characters, so 40k CJK characters passed a 30,000 "byte" cap as 90,084
    bytes -- 3x the budget the context accounting assumes.
    """
    if limit <= 0:
        return text, False
    raw = text.encode("utf-8", "replace")
    if len(raw) <= limit:
        return text, False
    keep = limit // 2
    # A cut can land mid-codepoint; drop the partial character rather than emit
    # a replacement char the model would have to reason about.
    head = raw[:keep].decode("utf-8", "ignore")
    tail = raw[-keep:].decode("utf-8", "ignore")
    dropped = len(raw) - 2 * keep
    return (f"{head}\n\n... [{dropped} bytes elided by the harness; "
            f"{keep} head + {keep} tail bytes shown] ...\n\n{tail}"), True


# --- OS containment ----------------------------------------------------------

def _sbpl(path):
    """Quote a path for a Seatbelt profile literal."""
    return path.replace("\\", "\\\\").replace('"', '\\"')


def _writable_system_roots():
    """Places a build legitimately writes to: /dev and the temp directories.

    A compiler that cannot write its own temp files is not containment, it is a
    broken toolchain, and every task would fail for the wrong reason.
    """
    roots = {"/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp", "/dev"}
    for p in (tempfile.gettempdir(), os.environ.get("TMPDIR") or ""):
        if p:
            roots.add(os.path.realpath(p))
    return sorted(roots)


def _seatbelt_profile(allow_write, protected_roots):
    """Deny writes everywhere, deny the benchmark tree outright, then re-allow.

    Rule order is significant: in SBPL the last matching rule wins, so the
    sandbox's own allow must come after both denies. Temp roots are allowed
    before the protected denies so that a protected root living under a temp
    directory (the layout every test fixture uses) is still protected.
    """
    lines = ["(version 1)", "(allow default)", '(deny file-write* (subpath "/"))']
    for p in _writable_system_roots():
        if os.path.exists(p):
            lines.append(f'(allow file-write* (subpath "{_sbpl(os.path.realpath(p))}"))')
    for p in protected_roots:
        if p:
            q = _sbpl(os.path.realpath(p))
            lines.append(f'(deny file-read* (subpath "{q}"))')
            lines.append(f'(deny file-write* (subpath "{q}"))')
    for p in allow_write:
        if p:
            q = _sbpl(os.path.realpath(p))
            lines.append(f'(allow file-read* file-write* (subpath "{q}"))')
    return "\n".join(lines)


def containment_backend():
    """Which containment mechanism this machine has, or None.

    'off' means the operator explicitly opted out; it is not containment and is
    reported as such everywhere it matters.
    """
    if os.environ.get(UNCONTAINED_ENV) == "1":
        return "off"
    if sys.platform == "darwin" and os.path.exists("/usr/bin/sandbox-exec"):
        return "sandbox-exec"
    if sys.platform.startswith("linux"):
        for exe in ("/usr/bin/bwrap", "/bin/bwrap"):
            if os.path.exists(exe):
                return "bwrap"
    return None


def _bwrap_argv(exe, argv, allow_write, protected_roots):
    """bubblewrap arguments. Order matters: later binds win over earlier ones.

    Everything is readable and nothing is writable by default. The order is
    therefore: the temp root read-write, then the protected roots covered with
    an empty tmpfs so they are neither readable nor writable, then the sandbox
    -- which lives *under* one of those roots -- bound back read-write. That
    sequence is what lets a sandbox inside the benchmark tree stay usable while
    the tree around it disappears, AND keeps a protected root that happens to
    live under TMPDIR masked rather than re-exposed by the temp bind.
    """
    out = [exe, "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc"]
    # The temp root FIRST, because a later bind wins and this one must not be
    # allowed to undo the masks below. It used to come last, which re-exposed
    # everything under TMPDIR read-write -- including any protected root that
    # happened to live there. On a box where the benchmark tree sits outside
    # /tmp that is invisible, and the Linux containment assertions were gated
    # to macOS so nothing ever tried it: the hidden tests were readable and
    # writable from the model's shell.
    tmp = os.environ.get("TMPDIR") or "/tmp"
    out += ["--bind", os.path.realpath(tmp), os.path.realpath(tmp)]
    # Then the masks, so a protected root under TMPDIR is still protected.
    for p in protected_roots:
        if p:
            out += ["--tmpfs", os.path.realpath(p)]
    # Then the sandbox, last, because it lives *under* a masked root and has to
    # win over the tmpfs that just covered its parent.
    for p in allow_write:
        if p:
            r = os.path.realpath(p)
            out += ["--bind", r, r]
    # --unshare-pid makes bwrap PID 1 of a private PID namespace, so when it is
    # killed the kernel takes every descendant with it. Without it, --new-session
    # setsid()s the payload into a process group that is NOT bwrap's, so
    # run_bounded's killpg on a timeout reached the wrapper and left the model's
    # children running: three orphans per timed-out command, measured on the
    # GH200. macOS has no bwrap, so the suite never saw it.
    out += ["--unshare-pid", "--die-with-parent", "--new-session", "--"]
    return out + list(argv)


_PROBE_CACHE = {}


def containment_proves_itself(sandbox=None):
    """Run a probe under the real backend and check it is actually confining.

    A containment backend is a claim. This checks it: under the wrapper, try to
    read a file inside the protected tree and to write outside the sandbox. If
    either succeeds, the backend is not containing anything and we must refuse
    rather than report those episodes as contained.

    This exists because the macOS profile is exercised on every developer run
    while the Linux one is not, and an untested confinement that silently does
    nothing is worse than an honest refusal. The probe makes the platform prove
    itself instead of being trusted.

    Returns (ok, backend, detail). Cached per (backend, sandbox).
    """
    backend = containment_backend()
    if backend in (None, "off"):
        # "ok" means reads and writes are actually confined. An explicit
        # opt-out is a declared state, not a confined one, and must not read
        # the same as a working backend.
        return False, backend, ("containment explicitly disabled via "
                                + UNCONTAINED_ENV if backend == "off"
                                else "no containment backend on this platform")
    key = (backend, sandbox)
    if key in _PROBE_CACHE:
        return _PROBE_CACHE[key]

    work = sandbox or tempfile.mkdtemp(prefix="mh-probe-")
    made = sandbox is None
    canary = os.path.join(BENCH_ROOT, "mh", "tools.py")
    # Not TMPDIR: the profile deliberately re-allows temp roots, because a
    # toolchain that cannot write a temp file is not a harness. The write that
    # must be refused is one into the benchmark tree, where the hidden tests
    # live. (The first version of this probe used TMPDIR and reported the real
    # macOS backend as broken -- the probe was wrong, not the backend.)
    outside = os.path.join(BENCH_ROOT, "mh-probe-escape.txt")
    script = (
        f'if cat {shlex.quote(canary)} >/dev/null 2>&1; then echo READ_OK; fi; '
        f'if echo x > {shlex.quote(outside)} 2>/dev/null; then echo WRITE_OK; fi; '
        f'echo PROBE_RAN')
    escaped = False
    try:
        argv, _ = contained_argv(["/bin/sh", "-c", script],
                                allow_write=[work])
        r = subprocess.run(argv, capture_output=True, text=True, timeout=30)
        out = (r.stdout or "") + (r.stderr or "")
        # The invariant is that the benchmark tree did not change, NOT that the
        # write syscall returned an error. The two backends refuse differently:
        # macOS sandbox-exec denies the write outright, while bwrap covers the
        # tree with a tmpfs, so the write lands in an ephemeral overlay and
        # succeeds. Asking "did the write succeed?" therefore reported a
        # perfectly confined Linux host as broken -- and the tempting way to get
        # past that is MH_UNCONTAINED, which removes the protection for the
        # entire grid. Checked before the cleanup below deletes the evidence.
        escaped = os.path.exists(outside)
    except Exception as e:  # a backend that cannot even start is not containment
        res = (False, backend, f"probe failed to run: {type(e).__name__}: {e}")
        _PROBE_CACHE[key] = res
        return res
    finally:
        if made:
            shutil.rmtree(work, ignore_errors=True)
        try:
            os.remove(outside)
        except OSError:
            pass

    if "PROBE_RAN" not in out:
        res = (False, backend, f"probe did not run under {backend}: {out.strip()[:200]}")
    elif "READ_OK" in out:
        res = (False, backend, f"{backend} did not stop a read of {canary}")
    elif escaped:
        res = (False, backend,
               f"{backend} let a write reach {outside} on the host filesystem")
    elif "WRITE_OK" in out:
        res = (True, backend,
               f"{backend} confined reads; the write succeeded into a masked "
               f"overlay and did not reach {outside} on the host")
    else:
        res = (True, backend, f"{backend} confined reads and writes")
    _PROBE_CACHE[key] = res
    return res


def contained_argv(argv, allow_write=(), protected_roots=(BENCH_ROOT,)):
    """Wrap argv so the process is confined for the two things that matter.

    Writes: only inside allow_write (plus /dev and the temp directories a
    toolchain needs). Reads and writes of protected_roots -- the benchmark
    installation, tasks and results included: refused outright. Reads elsewhere
    are left alone, because a compiler that cannot read /usr is not a harness.

    Returns (argv, backend). Raises ContainmentUnavailable when this platform has
    no backend and the operator has not opted out: a subprocess that could not be
    contained must never look like one that was.
    """
    backend = containment_backend()
    if backend == "sandbox-exec":
        profile = _seatbelt_profile(list(allow_write), list(protected_roots))
        return ["/usr/bin/sandbox-exec", "-p", profile] + list(argv), backend
    if backend == "bwrap":
        exe = "/usr/bin/bwrap" if os.path.exists("/usr/bin/bwrap") else "/bin/bwrap"
        return _bwrap_argv(exe, argv, list(allow_write),
                           list(protected_roots)), backend
    if backend == "off":
        return list(argv), "off"
    raise ContainmentUnavailable(
        f"no shell containment backend on {sys.platform!r}: macOS needs "
        f"/usr/bin/sandbox-exec, Linux needs bubblewrap (bwrap). Refusing to "
        f"run an uncontained shell, "
        f"because an uncontained shell can read and overwrite the hidden tests "
        f"under {BENCH_ROOT}. Set {UNCONTAINED_ENV}=1 to run anyway; every "
        f"episode is then stamped uncontained and must not be reported as "
        f"contained.")


def _kill_group(proc):
    """Kill the whole process group, not just the shell we spawned.

    A bare proc.kill() leaves `sleep 600 &` and every other backgrounded child
    running after the episode ends; three orphans per timed-out command was the
    measured behaviour.
    """
    try:
        os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
    except (ProcessLookupError, PermissionError, OSError):
        try:
            proc.kill()
        except OSError:
            pass


def run_bounded(argv, cwd=None, env=None, input_text=None, timeout=None):
    """subprocess.run, except a timeout takes the whole process group with it.

    One owner for "run something with a deadline": the shell tool and the
    verifier both go through here, so neither can leave orphans behind while the
    other cleans up. Raises subprocess.TimeoutExpired, like subprocess.run does.
    """
    proc = subprocess.Popen(
        argv, cwd=cwd, env=env,
        stdin=subprocess.PIPE if input_text is not None else subprocess.DEVNULL,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        text=True, errors="replace", start_new_session=True)
    try:
        out, err = proc.communicate(input_text, timeout=timeout)
    except subprocess.TimeoutExpired:
        _kill_group(proc)
        try:
            proc.communicate(timeout=10)
        except subprocess.TimeoutExpired:
            pass
        raise
    return proc.returncode, out or "", err or ""


class Sandbox:
    def __init__(self, root, output_cap=MAX_OUTPUT_BYTES,
                 protected_roots=(BENCH_ROOT,)):
        self.root = os.path.realpath(root)
        self.output_cap = output_cap
        self.protected_roots = tuple(p for p in protected_roots if p)
        self.containment = None      # set on first run_shell
        self._containment_checked = False

    def resolve(self, path):
        """Resolve a model-supplied path inside the sandbox, or refuse."""
        if not isinstance(path, str) or not path.strip():
            raise ToolError("path must be a non-empty string")
        p = path.strip()
        full = p if os.path.isabs(p) else os.path.join(self.root, p)
        real = os.path.realpath(full)
        if real != self.root and not real.startswith(self.root + os.sep):
            raise ToolError(
                f"path {path!r} resolves outside the task directory and was refused. "
                f"Work only inside {self.root}.")
        return real

    # ---- tool implementations ------------------------------------------------

    def _wrap(self, argv):
        try:
            return contained_argv(argv, allow_write=[self.root],
                                  protected_roots=self.protected_roots)
        except ContainmentUnavailable as e:
            raise ToolError(str(e))

    def _shell_argv(self, cmd):
        """Containment-wrapped argv for one shell command, verified once."""
        if not self._containment_checked:
            # A profile that fails to compile would otherwise turn every command
            # into a mysterious exit=1. Prove the backend works on a command with
            # no side effects before trusting it with the model's.
            probe_argv, backend = self._wrap(["/usr/bin/true"])
            if backend == "sandbox-exec":
                try:
                    r = subprocess.run(probe_argv, cwd=self.root, stdin=subprocess.DEVNULL,
                                       capture_output=True, text=True,
                                       errors="replace", timeout=30)
                except (OSError, subprocess.SubprocessError) as e:
                    raise ToolError(f"shell containment backend failed to start: {e}")
                if r.returncode != 0:
                    raise ToolError(
                        "shell containment backend rejected its profile "
                        f"({(r.stderr or '').strip()[:300]}); refusing to run an "
                        "uncontained shell.")
            self._containment_checked = True
            self.containment = backend
        argv, _ = self._wrap(["/bin/bash", "-lc", cmd])
        return argv

    def run_shell(self, cmd=None, **_):
        if not isinstance(cmd, str) or not cmd.strip():
            raise ToolError("run_shell requires a non-empty 'cmd' string")
        argv = self._shell_argv(cmd)
        try:
            rc, out, err = run_bounded(argv, cwd=self.root, timeout=SHELL_TIMEOUT_S)
        except subprocess.TimeoutExpired:
            raise ToolError(
                f"command timed out after {SHELL_TIMEOUT_S}s; the command and every "
                f"process it started were killed. "
                f"Long-running or interactive commands will not work here.")
        except OSError as e:
            raise ToolError(f"could not start the shell: {e}")
        body = out
        if err.strip():
            body += ("\n" if body and not body.endswith("\n") else "") + "[stderr]\n" + err
        body, _ = cap_output(body, self.output_cap)
        return f"exit={rc}\n{body}" if body.strip() else f"exit={rc}\n(no output)"

    def _list_dir(self, path, real):
        entries = sorted(os.listdir(real))
        shown = entries[:DIR_LIST_LIMIT]
        head = f"{path} is a directory containing {len(entries)} entries"
        if len(entries) > len(shown):
            # Carry both numbers. A capped sample presented as complete coverage
            # is the exact failure cap_output exists to prevent, and this listing
            # used to truncate at 200 with no marker at all.
            head += (f"; showing the first {len(shown)}, "
                     f"{len(entries) - len(shown)} not listed")
        body, _ = cap_output(head + ":\n" + "\n".join(shown), self.output_cap)
        return body

    def read_file(self, path=None, **_):
        real = self.resolve(path)
        if not os.path.exists(real):
            raise ToolError(f"{path!r} does not exist. List the directory before reading.")
        if os.path.isdir(real):
            return self._list_dir(path, real)
        try:
            size = os.path.getsize(real)
        except OSError as e:
            raise ToolError(f"could not read {path!r}: {e}")
        # Bound the read *before* building per-line strings. Reading a 200 MB file
        # and then splitting it into numbered lines grew RSS by 857 MB; the budget
        # now bounds the bytes we touch, not just the bytes we return.
        cap = self.output_cap if self.output_cap > 0 else MAX_READ_BYTES
        budget = max(1024, min(cap, MAX_READ_BYTES))
        try:
            return self._numbered(real, size, budget)
        except OSError as e:
            raise ToolError(f"could not read {path!r}: {e}")

    def _numbered(self, real, size, budget):
        """The file with line numbers, built a line at a time under a byte budget.

        The old version read the whole file and then built one string per line
        before capping anything, so a 200 MB file grew RSS by 857 MB. Nothing
        here holds more than `budget` bytes, whatever the file's size or shape.
        """
        big = size > budget
        limit = budget // 2 if big else budget
        parts, total, lines, overran = [], 0, 0, False
        with open(real, "r", errors="replace") as f:
            for i, line in enumerate(f, 1):
                piece = f"{i:>5}\t{line.rstrip(chr(10))}"
                n = len(piece.encode("utf-8", "replace")) + 1
                if total + n > limit:
                    overran = True
                    break
                parts.append(piece)
                total += n
                lines = i
        head = "\n".join(parts)
        if not (big or overran):
            body, _ = cap_output(head, self.output_cap)
            return body if body.strip() else "(empty file)"
        # Keep the tail: the end of a file is where the thing you are looking for
        # usually is, and a silently head-only read invites confident nonsense.
        with open(real, "rb") as f:
            f.seek(max(total, size - budget // 2))
            tail = f.read(budget // 2).decode("utf-8", "replace")
        tail = tail.split("\n", 1)[1] if "\n" in tail else tail
        return (head +
                f"\n\n... [the harness read only part of this {size}-byte file: "
                f"the first {lines} lines ({total} bytes) with line numbers, and the "
                f"last {len(tail.encode('utf-8', 'replace'))} bytes without them. "
                f"The middle was never read. Use run_shell with sed -n to see a "
                f"specific line range.] ...\n\n" + tail)

    def write_file(self, path=None, content=None, **_):
        real = self.resolve(path)
        if content is None:
            raise ToolError("write_file requires 'content'")
        if os.path.isdir(real):
            raise ToolError(f"{path!r} is a directory, not a file; pick a path inside it.")
        if not isinstance(content, str):
            content = str(content)
        os.makedirs(os.path.dirname(real) or self.root, exist_ok=True)
        try:
            with open(real, "w") as f:
                f.write(content)
        except OSError as e:
            raise ToolError(f"could not write {path!r}: {e}")
        return f"wrote {len(content)} bytes to {path}"

    def edit_file(self, path=None, old=None, new=None, **_):
        real = self.resolve(path)
        if not os.path.exists(real):
            raise ToolError(f"{path!r} does not exist; use write_file to create it.")
        if os.path.isdir(real):
            raise ToolError(f"{path!r} is a directory, not a file; edit a file inside it.")
        if not isinstance(old, str) or old == "":
            raise ToolError("edit_file requires a non-empty 'old' string to replace")
        if new is None:
            new = ""
        try:
            with open(real, "r", errors="replace") as f:
                text = f.read()
        except OSError as e:
            raise ToolError(f"could not read {path!r}: {e}")
        count = text.count(old)
        if count == 0:
            raise ToolError(
                f"the 'old' text was not found in {path!r}. Read the file again and "
                f"copy the exact text, including indentation.")
        if count > 1:
            raise ToolError(
                f"the 'old' text appears {count} times in {path!r}; it must match "
                f"exactly once. Include more surrounding context to disambiguate.")
        try:
            with open(real, "w") as f:
                f.write(text.replace(old, new, 1))
        except OSError as e:
            raise ToolError(f"could not write {path!r}: {e}")
        return f"replaced 1 occurrence in {path}"

    def finish(self, summary=None, **_):
        return summary or ""


SCHEMAS = [
    {"type": "function", "function": {
        "name": "run_shell",
        "description": "Run a bash command in the task directory and return its exit "
                       "code and combined output. Use this to explore, build and test.",
        "parameters": {"type": "object", "properties": {
            "cmd": {"type": "string", "description": "The bash command to run."}},
            "required": ["cmd"]}}},
    {"type": "function", "function": {
        "name": "read_file",
        "description": "Read a file and return it with line numbers. If given a "
                       "directory, lists its entries.",
        "parameters": {"type": "object", "properties": {
            "path": {"type": "string", "description": "Path relative to the task directory."}},
            "required": ["path"]}}},
    {"type": "function", "function": {
        "name": "write_file",
        "description": "Create or overwrite a file with the exact content given.",
        "parameters": {"type": "object", "properties": {
            "path": {"type": "string", "description": "Path relative to the task directory."},
            "content": {"type": "string", "description": "Full new content of the file."}},
            "required": ["path", "content"]}}},
    {"type": "function", "function": {
        "name": "edit_file",
        "description": "Replace one exact unique occurrence of 'old' with 'new' in a "
                       "file. Preferred over write_file for small changes.",
        "parameters": {"type": "object", "properties": {
            "path": {"type": "string", "description": "Path relative to the task directory."},
            "old": {"type": "string", "description": "Exact text to replace; must occur exactly once."},
            "new": {"type": "string", "description": "Replacement text."}},
            "required": ["path", "old", "new"]}}},
    {"type": "function", "function": {
        "name": "finish",
        "description": "Declare the task complete. Only call this after you have run "
                       "the tests and seen them pass.",
        "parameters": {"type": "object", "properties": {
            "summary": {"type": "string", "description": "What you changed and how you verified it."}},
            "required": ["summary"]}}},
]

TOOL_NAMES = [s["function"]["name"] for s in SCHEMAS]
FILE_TOOLS = ("read_file", "write_file", "edit_file")


def schemas_for(nativetools=True):
    """Full five-tool surface, or shell+finish only.

    Native file tools exist so a small model never has to quote a pipeline to
    touch a file. Turning them off is the ablation: the model must go through
    run_shell. The flag was a no-op until this function was the single caller.
    """
    if nativetools:
        return SCHEMAS
    return [s for s in SCHEMAS if s["function"]["name"] not in FILE_TOOLS]
