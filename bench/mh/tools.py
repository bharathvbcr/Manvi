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
import atexit
import codecs
import errno
import io
import locale
import os
import re
import selectors
import signal
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
import time
import weakref

from . import procscan

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


def _elided(head_raw, tail_raw, total, limit):
    """The capped rendering of a `total`-byte text, from its two ends.

    `head_raw` must start with the text's first `limit // 2` bytes and `tail_raw`
    must end with its last `limit // 2` bytes; nothing else of the text is needed.
    That is what lets a streamed capture that never held the middle render exactly
    what cap_output renders from the whole string: both call this.
    """
    keep = limit // 2
    # A cut can land mid-codepoint; drop the partial character rather than emit
    # a replacement char the model would have to reason about.
    head = head_raw[:keep].decode("utf-8", "ignore")
    # Not tail_raw[-keep:]: with keep == 0 that is tail_raw[0:], the WHOLE tail,
    # which is how cap_output(text, 1) used to return its entire input.
    tail = tail_raw[len(tail_raw) - keep:].decode("utf-8", "ignore") if keep else ""
    dropped = total - 2 * keep
    return (f"{head}\n\n... [{dropped} bytes elided by the harness; "
            f"{keep} head + {keep} tail bytes shown] ...\n\n{tail}")


def cap_output(text, limit=MAX_OUTPUT_BYTES):
    """Head+tail truncation, measured in UTF-8 bytes.

    Never drop the middle silently: the tail of a build log holds the error and the
    head holds the command, and a model shown a quietly-truncated log will confidently
    reason about output it never saw.

    The cap is a *byte* cap, as the name and the banner have always said. It used to
    count characters, so 40k CJK characters passed a 30,000 "byte" cap as 90,084
    bytes -- 3x the budget the context accounting assumes.

    Exact semantics, which the paper's "head-and-tail cap of 30,000 bytes" means:

    * Sizes are of the text's UTF-8 encoding. For run_shell that text is the
      command's stdout, then (only if stderr is not blank) a "[stderr]" line and
      stderr, each decoded with errors="replace" and universal newlines.
    * A text of at most `limit` bytes is returned unchanged.
    * A longer text is replaced by its first `limit // 2` bytes, an elision
      banner, and its last `limit // 2` bytes. The limit bounds the RETAINED
      payload; the banner is not counted against it. The banner is
      "\\n\\n... [D bytes elided by the harness; K head + K tail bytes shown]
      ...\\n\\n": 69 bytes plus the decimal digits of D once and of K twice.
      At limit=30,000 that is 80-88 bytes, so a capped result is 30,080 bytes
      (D=1) to 30,084 (100 KB of output) to 30,088 (D of nine digits). An odd
      limit retains limit - 1 bytes.
    * A cut that lands inside a multi-byte character drops that partial
      character, so up to 3 bytes fewer than K may be shown on each side; the
      banner still states K.
    * limit <= 0 disables the cap (the no-outcap ablation passes 10**9 instead).
    """
    if limit <= 0:
        return text, False
    raw = text.encode("utf-8", "replace")
    if len(raw) <= limit:
        return text, False
    keep = limit // 2
    return _elided(raw[:keep], raw[len(raw) - keep:], len(raw), limit), True


def _text_encoding():
    """The encoding subprocess's text mode decodes with; what run_bounded used."""
    return "utf-8" if sys.flags.utf8_mode else locale.getencoding()


class StreamCapture:
    """One pipe's output, held as its first and last `limit` bytes only.

    Decodes exactly as Popen(text=True, errors="replace") did -- the process's
    text encoding, replacement for bad bytes, universal newlines -- but a chunk
    at a time, so a split multi-byte character or a split "\\r\\n" decodes the
    same as it would whole. The decoded text is re-encoded as UTF-8, because
    that is the unit cap_output measures in, and only its two ends are kept.

    run_shell used to hold the whole of a command's output before capping it:
    `yes` for 120 s is gigabytes of harness RSS for a 30,000-byte answer. Now
    memory is O(limit) per stream, whatever the command produces.

    limit <= 0 keeps everything (run_bounded's contract, and the no-outcap
    ablation's), which is the old behaviour.
    """

    def __init__(self, limit):
        self.limit = limit if limit and limit > 0 else 0
        self._dec = io.IncrementalNewlineDecoder(
            codecs.getincrementaldecoder(_text_encoding())(errors="replace"),
            translate=True)
        self._head = bytearray()
        self._tail = bytearray()
        self.total = 0          # UTF-8 bytes of the decoded text
        self.raw_bytes = 0      # bytes read from the pipe
        self.nonblank = False   # whether str.strip() of the whole text is non-empty
        self._closed = False

    def feed(self, data):
        self.raw_bytes += len(data)
        self._add(self._dec.decode(data))

    def close(self):
        if not self._closed:
            self._closed = True
            self._add(self._dec.decode(b"", final=True))

    def _add(self, text):
        if not text:
            return
        if not self.nonblank and text.strip():
            self.nonblank = True
        b = text.encode("utf-8", "replace")
        self.total += len(b)
        if not self.limit:
            self._head += b
            return
        if len(self._head) < self.limit:
            self._head += b[:self.limit - len(self._head)]
        self._tail += b
        if len(self._tail) > 2 * self.limit:
            del self._tail[:len(self._tail) - self.limit]

    def head(self):
        """At least the first min(total, limit) bytes."""
        return bytes(self._head)

    def tail(self):
        """Exactly the last min(total, limit) bytes (everything when unbounded)."""
        if not self.limit:
            return bytes(self._head)
        return bytes(self._tail[len(self._tail) - min(self.total, self.limit):])

    def complete(self):
        """True when the whole text is held."""
        return not self.limit or self.total <= self.limit

    def text(self):
        if not self.complete():
            raise ValueError("this capture holds only the two ends of its text")
        return self._head.decode("utf-8")

    def held_bytes(self):
        return len(self._head) + len(self._tail)


_STDERR_MARK = "[stderr]\n"


def shell_body(out, err, limit):
    """run_shell's body from two captures: cap_output(stdout [+ stderr]).

    Byte-identical to building the whole string and calling cap_output on it,
    which is the old path and the property test's reference. `out` and `err`
    must be StreamCaptures made with this same `limit`.
    """
    joined = err.nonblank
    sep = ""
    if joined and out.total and out.tail()[-1:] != b"\n":
        sep = "\n"
    mid = (sep + _STDERR_MARK).encode() if joined else b""
    err_total = err.total if joined else 0
    total = out.total + len(mid) + err_total
    if limit <= 0 or total <= limit:
        body = out.text()
        if joined:
            body += sep + _STDERR_MARK + err.text()
        return body
    head = out.head() + mid + (err.head() if joined else b"")
    tail = out.tail() + mid + (err.tail() if joined else b"")
    keep = limit // 2
    return _elided(head[:keep], tail[len(tail) - keep:], total, limit)


# --- OS containment ----------------------------------------------------------


class GitGuardError(ContainmentUnavailable):
    """A git pointer in the walk could not be read or parsed.

    Guarding the checkouts around the benchmark means following every `.git`
    file, `commondir` and registered worktree's `gitdir`. One that cannot be
    followed leaves a checkout that tracks hidden tests and reference solutions
    unguarded, and an unguarded checkout is indistinguishable from "there was
    nothing to guard" -- so the guard refuses instead of guessing. It is a
    ContainmentUnavailable: containment that cannot be set up is the same
    refusal whichever part could not be set up. `path` is the offending file.
    """

    def __init__(self, path, reason):
        self.path = path
        super().__init__(
            f"cannot work out which git checkouts to guard: {path}: {reason}. "
            f"Refusing to run: an unguarded checkout would leave hidden tests "
            f"and reference solutions readable to the model. Repair or remove "
            f"that path.")


def _read_git_pointer(path, *, dotgit=False, optional=False, must_exist=False):
    """The directory a `.git` file or `gitdir`/`commondir` file points at.

    Parsed the way git parses it (checked against git 2.54: each variant below
    makes `git status` fail with "invalid gitfile format" or "not a git
    repository"). A `.git` file is exactly `gitdir: <path>` -- the literal
    prefix with its space, the rest of the file taken as the path with only the
    trailing newline dropped, so an extra line becomes part of the path. A UTF-8
    byte-order mark, leading whitespace and more than one line are refused in
    every pointer: git cannot follow them, and a pointer git cannot follow names
    a checkout this guard cannot name.

    Raises GitGuardError when the file cannot be read, is not UTF-8, or names
    nothing. With `optional`, only a file that does not exist (or whose parent
    is not a directory) returns None: "no such file" is a fact about the
    repository, whereas an unreadable one is not. With `must_exist`, a target
    that is not a directory raises too: the own `.git` file and `commondir`
    point at the repository this guard is protecting, and a target that is not
    there leaves the whole guard set empty. A sibling worktree's `gitdir` does
    not set it, because its target is the sibling's checkout, which is
    legitimately gone once removed.
    """
    try:
        with open(path, encoding="utf-8", newline="") as f:
            text = f.read()
    except (FileNotFoundError, NotADirectoryError) as e:
        if optional:
            return None
        raise GitGuardError(path, f"{type(e).__name__}: {e.strerror}") from e
    except (OSError, UnicodeDecodeError) as e:
        raise GitGuardError(path, f"{type(e).__name__}: {e}") from e
    if text.startswith("\ufeff"):
        raise GitGuardError(path, "it begins with a UTF-8 byte-order mark, "
                                  "which git does not accept")
    if dotgit:
        if not text.startswith("gitdir: "):
            raise GitGuardError(path, "a .git file must start with 'gitdir: '")
        line = text[len("gitdir: "):].rstrip("\r\n")
    else:
        line = text.rstrip()
    if not line:
        raise GitGuardError(path, "the pointer names no directory")
    if line != line.lstrip():
        raise GitGuardError(path, "the pointer starts with whitespace")
    if "\n" in line or "\r" in line:
        raise GitGuardError(path, "the pointer spans more than one line")
    target = os.path.realpath(os.path.join(os.path.dirname(path), line))
    if must_exist and not os.path.isdir(target):
        raise GitGuardError(path, f"it points at {target}, which is not a "
                                  f"directory")
    return target


def git_guard_roots(*starts):
    """Every git object store and sibling checkout reachable from `starts`.

    The benchmark tree normally sits inside a git checkout, and that checkout
    tracks tasks/*/hidden_test.py and tasks/*/reference/. Denying the tree
    itself does nothing about `git show HEAD:bench/tasks/x/hidden_test.py`,
    which reads the same bytes out of <repo>/.git/objects, nor about another
    worktree or the main checkout of this repository. So each enclosing git
    dir, its common dir, and every other worktree registered in that common
    dir are protected too. Only the checkout the benchmark actually runs in
    (the first work tree reached walking up from each start) is skipped:
    masking it would mask the benchmark's own toolchain. Every OTHER registered
    checkout is guarded even when it geometrically contains a start. That is
    the nested layout -- a worktree under the main checkout's gitignored tmp/
    -- where the main checkout's work tree tracks the same hidden tests and
    used to be dropped from the guard for "containing" the bench. Only paths
    that exist are returned (a bwrap tmpfs mask needs a mount point), so a
    pruned worktree whose pointer is readable but whose target is gone is
    simply absent.

    Fails closed: a `.git` pointer, `commondir`, or any registered worktree's
    `gitdir` that cannot be read or parsed raises GitGuardError naming it, and
    so does a worktree admin directory with no `gitdir` (its checkout is live)
    and an own `.git` or `commondir` that leads nowhere. Only a sibling's
    well-formed `gitdir` naming a checkout that is gone is let through. It
    must not be skipped or answered with the start itself -- the start is the
    checkout the benchmark runs in and is filtered out, so that would silently
    drop every sibling. Callers that need this to fail once, at start-up, call
    it before anything runs (run.py:require_containment does).
    """
    starts = [os.path.realpath(p) for p in starts if p]

    roots = set()
    own = set()
    for start in starts:
        cur = start
        owned = False
        while True:
            dotgit = os.path.join(cur, ".git")
            if os.path.lexists(dotgit):
                if not owned:
                    own.add(cur)
                    owned = True
                if os.path.isdir(dotgit):
                    gitdir = common = os.path.realpath(dotgit)
                else:
                    gitdir = common = _read_git_pointer(dotgit, dotgit=True,
                                                        must_exist=True)
                    cdir = _read_git_pointer(os.path.join(gitdir, "commondir"),
                                             optional=True, must_exist=True)
                    if cdir is not None:
                        common = cdir
                roots.update((gitdir, common))
                wtroot = os.path.join(common, "worktrees")
                try:
                    names = os.listdir(wtroot)
                except (FileNotFoundError, NotADirectoryError):
                    names = []      # a repository with no linked worktrees
                except OSError as e:
                    raise GitGuardError(wtroot, f"{type(e).__name__}: {e}") from e
                for name in names:
                    entry = os.path.join(wtroot, name)
                    try:
                        is_entry = stat.S_ISDIR(os.stat(entry).st_mode)
                    except FileNotFoundError:
                        continue    # removed while we were listing
                    except OSError as e:
                        raise GitGuardError(entry, f"{type(e).__name__}: {e}") from e
                    if not is_entry:
                        continue    # a stray file (.DS_Store), not an admin dir
                    # An admin directory is a live checkout until git prunes
                    # the whole directory; one with no readable `gitdir` is a
                    # checkout this guard cannot name, not an absent one.
                    roots.add(os.path.dirname(_read_git_pointer(
                        os.path.join(entry, "gitdir"))))
                if os.path.basename(common) == ".git":
                    roots.add(os.path.dirname(common))
            parent = os.path.dirname(cur)
            if parent == cur:
                break
            cur = parent
    return tuple(sorted(r for r in roots
                        if os.path.exists(r) and r not in own))


# `.env*` guards, bounded.
ENV_GUARD_PRUNE = frozenset({".git", "node_modules", "target", "__pycache__",
                              ".venv", "venv"})
ENV_GUARD_MAX_DIRS = 50_000
_ENV_GUARD_CACHE = {}


def _checkout_top(start):
    """The top of the git work tree holding start, or start itself outside one."""
    cur = start
    while True:
        if os.path.lexists(os.path.join(cur, ".git")):
            return cur
        parent = os.path.dirname(cur)
        if parent == cur:
            return start
        cur = parent


def _model_writable_roots(*starts):
    """Realpaths of the directories a model can write, which the .env* walk leaves out.

    bench/.work is where episodes put their workspaces and is never cleaned: an
    episode can plant `ln -s /opt/homebrew .env.x` there, a mode-000 directory,
    or fifty thousand empty ones. Walking it would turn that into a guard on
    /opt/homebrew for every later shell, or a refusal to start. Nothing a model
    writes there is a provider key the harness holds, so it is not scanned. The
    runner's private temp parents (mh-runner-*) are the other model-writable
    tree; they are pruned by name in the walk, since they can sit anywhere the
    host temp does.
    """
    roots = {os.path.realpath(os.path.join(BENCH_ROOT, ".work"))}
    for start in starts:
        if start:
            roots.add(os.path.realpath(os.path.join(os.path.realpath(start), ".work")))
    roots.update(_RUNNER_PARENTS.values())
    return frozenset(roots)


def _walk_env_files(top, skip=frozenset()):
    """(guard paths, stats) for every `.env*` file, symlink or directory under top.

    Bounded: .git, node_modules, target and virtualenvs are not entered, and at
    most ENV_GUARD_MAX_DIRS directories are visited. Nested checkouts (a
    directory holding its own .git) ARE entered, under the same cap: an .env in
    a vendored or nested checkout is as readable as any other, and
    git_guard_roots names only registered worktrees. A directory named .env* is
    one guard root as a whole (its files are not .env files by name) and is not
    entered. Hitting
    that cap raises GitGuardError: a partial guard set leaves the .env files past
    the cap readable, and an episode run on it would look the same as one run on
    a complete scan. A symlink named .env* is
    included without being followed; the profile resolves it to its target. A
    symlink whose target is an existing directory outside top is the exception:
    it is not an .env file, and guarding it would deny that whole directory
    (/opt/homebrew, /usr) to every shell, so it is left out. A link to a file
    outside top (a real secret kept elsewhere) and a dangling link stay guarded.
    Directories in `skip` (the model's workspace root, see
    _model_writable_roots) and runner parents are neither entered nor counted.
    """
    found, dirs = set(), 0
    top_prefix = top.rstrip(os.sep) + os.sep

    def outside_directory_link(p):
        if not os.path.islink(p):
            return False
        real = os.path.realpath(p)
        return (os.path.isdir(real) and real != top
                and not real.startswith(top_prefix))

    skip_names = {os.path.basename(s) for s in skip}

    def skipped(dp, n):
        if n.startswith(RUNNER_PARENT_PREFIX):
            return True
        # realpath only for a name a skip root can have: one per directory
        # would cost a stat chain for each of up to ENV_GUARD_MAX_DIRS.
        return n in skip_names and os.path.realpath(os.path.join(dp, n)) in skip

    def onerror(e):
        raise GitGuardError(getattr(e, "filename", top) or top,
                            f"{type(e).__name__}: {e}") from e

    for dp, dns, fns in os.walk(top, followlinks=False, onerror=onerror):
        dirs += 1
        if dirs > ENV_GUARD_MAX_DIRS:
            raise GitGuardError(
                top, f"the .env* scan reached its cap of {ENV_GUARD_MAX_DIRS} "
                     f"directories ({dirs} directories visited, more remain), so "
                     f"the .env files past it would be unguarded")
        for n in fns:
            if n.startswith(".env") and not outside_directory_link(os.path.join(dp, n)):
                found.add(os.path.join(dp, n))
        for n in dns:
            if n.startswith(".env") and not outside_directory_link(os.path.join(dp, n)):
                found.add(os.path.join(dp, n))
        dns[:] = [n for n in dns if n not in ENV_GUARD_PRUNE
                  and not n.startswith(".env")
                  and not os.path.islink(os.path.join(dp, n))
                  and not skipped(dp, n)]
    return tuple(sorted(found)), {"dirs": dirs, "cap": ENV_GUARD_MAX_DIRS}


def env_file_guards(*starts):
    """Every `.env*` file in the git work tree around each start.

    A provider key kept in the repository's .env.local, or in a .env of any
    subdirectory, is readable by any shell that can read the checkout, and the
    checkout itself is deliberately not guarded (it holds the benchmark's own
    toolchain). So each .env* file anywhere in the work tree is a guard root of
    its own, symlinks included. Tracked ones are guarded too: a file named
    .env* is never something the model needs, and telling tracked from untracked
    would need a git call whose failure would have to mean "guard nothing". An
    unreadable directory raises GitGuardError for the same reason the git guard
    does. Results are cached per work tree for the life of the process.
    """
    found = set()
    for start in starts:
        if not start:
            continue
        top = _checkout_top(os.path.realpath(start))
        if top not in _ENV_GUARD_CACHE:
            _ENV_GUARD_CACHE[top] = _walk_env_files(top, _model_writable_roots(*starts))
        found.update(_ENV_GUARD_CACHE[top][0])
    return tuple(sorted(found))


def env_guard_stats():
    """How far the .env* scans got: directories visited, and the cap they ran under.

    A scan that reached the cap raised instead of being cached, so every scan
    counted here is a complete one.
    """
    runs = [s for _, s in _ENV_GUARD_CACHE.values()]
    return {"dirs": sum(s["dirs"] for s in runs), "cap": ENV_GUARD_MAX_DIRS}


def default_protected_roots():
    """What a shell must not touch when the caller names nothing narrower."""
    return ((BENCH_ROOT,) + git_guard_roots(BENCH_ROOT)
            + env_file_guards(BENCH_ROOT))


class WorkspaceLayoutError(ContainmentUnavailable):
    """The workspace root sits where concurrent episodes can read each other."""


def require_protected_workspace_root(workspace_root, protected_roots=None):
    """Refuse a workspace root that no protected root contains.

    Reads outside the protected roots are left open (a compiler that cannot read
    /usr is not a harness). What keeps one concurrent episode out of another's
    workspace is therefore the deny on the protected root ABOVE both, with each
    sandbox re-allowed only its own directory. A workspace root outside every
    protected root has no such deny over it, so every episode's solution files
    are readable to every other episode's shell. The supported layout is
    bench/.work inside BENCH_ROOT; anything else is refused, not stamped.
    Returns the protected root that contains workspace_root.
    """
    if protected_roots is None:
        protected_roots = default_protected_roots()
    real = os.path.realpath(workspace_root)
    for p in protected_roots:
        if not p:
            continue
        q = os.path.realpath(p)
        if real == q or real.startswith(q + os.sep):
            return q
    raise WorkspaceLayoutError(
        f"workspace root {real} is outside every protected root "
        f"({', '.join(sorted(os.path.realpath(p) for p in protected_roots if p))}). "
        f"Concurrent episodes there can read each other's workspaces. Use the "
        f"supported layout, a directory inside {BENCH_ROOT}.")


# --- one owner for a child's environment, temp directory and write roots -----
#
# Four kinds of child run in an episode: the model's shell, the shell-kind
# verifier and the two RPC children of the Python verifier. Each used to build
# its own env and its own write roots: three inherited the runner's whole
# environment (provider keys included) and all could write the host's shared
# temp directories, so a verifier worker could leave its hidden-test arguments in
# /tmp for the next run_shell to read back. Everything below is the only place
# that decides those three things.

# What a child may inherit. Nothing else gets through, so a provider key (any
# *_API_KEY, CEREBRAS_*, OPENAI_*, ANTHROPIC_*, ...) is excluded by construction
# rather than by a deny-list that has to name the next provider. Every entry is
# an exact name: a prefix such as LC_ would pass LC_SECRET or LC_ALL_KEY. The
# toolchain homes are read-only locations a rustup/cargo, Go or JVM install
# needs to be found; HOME, TMPDIR/TMP/TEMP are set by the scope, never
# inherited.
CHILD_ENV_ALLOW = ("PATH", "LANG", "LANGUAGE", "TERM", "TZ", "USER", "LOGNAME",
                   "SHELL", "RUSTUP_HOME", "CARGO_HOME", "GOROOT", "JAVA_HOME",
                   "LC_ALL", "LC_CTYPE", "LC_COLLATE", "LC_MESSAGES",
                   "LC_MONETARY", "LC_NUMERIC", "LC_TIME")
CHILD_TEMP_VARS = ("TMPDIR", "TMP", "TEMP")


def scrubbed_env(private_dir, environ=None):
    """The environment of a child whose home and temp directory are `private_dir`."""
    src = os.environ if environ is None else environ
    env = {k: v for k, v in src.items() if k in CHILD_ENV_ALLOW}
    env["HOME"] = private_dir
    for k in CHILD_TEMP_VARS:
        env[k] = private_dir
    return env


# --- scope placement, removal and reaping -------------------------------------
#
# Every ChildScope lives under ONE runner-private parent, mh-runner-<pid>-<rand>,
# mode 0700, created under the host temp. Sandboxed children are denied that
# parent outright and re-allowed only their own scope, so one episode's children
# cannot read another's temp, or an in-flight verification's, even though reads
# elsewhere stay open. The pid in the name lets the next runner reap the parent a
# SIGKILLed runner left behind.

RUNNER_PARENT_PREFIX = "mh-runner-"
_RUNNER_PARENT_RE = re.compile(r"^mh-runner-(\d+)-")
_RUNNER_PARENTS = {}


def _pid_alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except OSError:
        return True         # EPERM: it exists, and is not ours
    return True


def _open_up(path):
    """Make every directory under path traversable and writable by its owner."""
    stack = [path]
    while stack:
        p = stack.pop()
        try:
            if not stat.S_ISDIR(os.lstat(p).st_mode):
                continue
            os.chmod(p, 0o700)
            with os.scandir(p) as it:
                stack.extend(e.path for e in it if e.is_dir(follow_symlinks=False))
        except OSError:
            continue


def remove_tree(path):
    """Remove a tree a child may have locked; True when it is gone.

    A worker that leaves a directory without its write bit (or without read and
    search) defeats shutil.rmtree(ignore_errors=True), which then silently
    leaves its files behind for the next episode. So directories are chmodded
    open first, and a tree that still cannot be removed is reported on stderr
    with its path rather than swallowed.
    """
    last = None
    for _ in range(2):
        _open_up(path)
        try:
            shutil.rmtree(path)
        except FileNotFoundError:
            return True
        except OSError as e:
            last = e
        if not os.path.lexists(path):
            return True
    print(f"[mh] could not remove private temp {path}: {last}", file=sys.stderr,
          flush=True)
    return False


def reap_stale_scopes(bases=None):
    """Remove runner parents whose runner is dead; returns the paths removed.

    Called when a runner starts. A parent whose pid is alive is left alone, and
    so is this process's own.
    """
    bases = list(bases) if bases else [os.path.realpath(tempfile.gettempdir())]
    reaped = []
    for base in bases:
        try:
            # Lazily, and only the matching names kept: a host temp directory
            # can hold tens of thousands of entries.
            with os.scandir(base) as it:
                names = [e.name for e in it if e.name.startswith(RUNNER_PARENT_PREFIX)]
        except OSError:
            continue
        for name in names:
            m = _RUNNER_PARENT_RE.match(name)
            if not m or int(m.group(1)) == os.getpid() or _pid_alive(int(m.group(1))):
                continue
            full = os.path.join(base, name)
            if remove_tree(full):
                reaped.append(full)
    return reaped


def runner_parent():
    """This process's private parent for every scope, created on first use.

    A host temp whose path holds ':' or a newline would corrupt the PATH entry
    the mktemp shim needs, so such a base is not used: the parent goes under
    /tmp instead (it is 0700 and denied to children, so that costs nothing).
    """
    base = os.path.realpath(tempfile.gettempdir())
    if os.pathsep in base or "\n" in base:
        base = os.path.realpath("/tmp")
    key = (os.getpid(), base)
    if key not in _RUNNER_PARENTS:
        reap_stale_scopes([base])
        path = os.path.realpath(tempfile.mkdtemp(
            prefix=f"{RUNNER_PARENT_PREFIX}{os.getpid()}-", dir=base))
        os.chmod(path, 0o700)
        _RUNNER_PARENTS[key] = path
        atexit.register(remove_tree, path)
    return _RUNNER_PARENTS[key]


_DEFAULT_PATH = "/usr/bin:/bin:/usr/sbin:/sbin"

_MKTEMP_SHIM = r"""#!/bin/sh
real=/usr/bin/mktemp
[ -n "${TMPDIR:-}" ] || exec "$real" "$@"
for a in "$@"; do [ "$a" = -p ] && exec "$real" "$@"; done
n=$#; mode=; pfx=; tmpl=
while [ "$n" -gt 0 ]; do
  a=$1; shift; n=$((n - 1))
  if [ "$mode" = t ]; then pfx=$a; mode=; continue; fi
  case "$a" in
    -t) mode=t ;;
    -*) set -- "$@" "$a" ;;
    *) tmpl=$a; set -- "$@" "$a" ;;
  esac
done
[ -n "$tmpl" ] && exec "$real" "$@"
exec "$real" "$@" "$TMPDIR/${pfx:-tmp}.XXXXXXXXXX"
"""


class ChildScope:
    """A private directory tree for one episode's, or one verification's, children.

    `root` is a fresh mkdtemp directory under the runner-private parent. A
    child gets a subdirectory of it (`private_dir`), which is its HOME and its
    TMPDIR, and which is the only place besides the workspace the child may
    write; `root` itself is what contained_argv re-allows to that child after
    denying the parent. `close()` removes the whole tree (see remove_tree); a
    scope that is dropped without being closed is removed by a finalizer.

    The shell's scope and each verification's scope are separate objects on
    purpose: a verifier child that wrote into the shell's temp would hand the
    model whatever it wrote.
    """

    def __init__(self, parent=None, prefix="mh-scope-"):
        self.root = os.path.realpath(tempfile.mkdtemp(
            prefix=prefix, dir=parent or runner_parent()))
        self._final = weakref.finalize(self, remove_tree, self.root)
        # Unique per scope and carried in every child's environment, so close()
        # can find what the children left behind (see mh.procscan).
        self.marker = os.urandom(16).hex()
        self._marker_issued = False
        self._reaped = False
        self.started = time.time()  # processes older than this are not this scope's
        self.survivors = []       # pids still alive after close() tried to kill them
        self.reap_error = None    # why the scan could not run, when it could not
        self.unverified = []      # processes that cannot be attributed (see procscan)

    def private_dir(self, name):
        path = os.path.join(self.root, name)
        os.makedirs(path, mode=0o700, exist_ok=True)
        return path

    def env(self, private_dir):
        """The scrubbed environment for a child whose HOME/TMPDIR is `private_dir`."""
        env = scrubbed_env(private_dir)
        env[procscan.MARKER_VAR] = self.marker
        self._marker_issued = True
        if sys.platform == "darwin":
            # macOS mktemp ignores TMPDIR for a bare `mktemp -d` and for -t, and
            # writes under the per-user temp directory, which a child may not
            # write. A task whose own test calls it (envbuild) would fail for the
            # wrong reason. A first-on-PATH mktemp that honours TMPDIR keeps
            # every toolchain working without re-opening the shared temp.
            bindir = self.private_dir("bin")
            shim = os.path.join(bindir, "mktemp")
            if not os.path.exists(shim):
                with open(shim, "w", encoding="utf-8") as f:
                    f.write(_MKTEMP_SHIM)
                os.chmod(shim, 0o700)
            env["PATH"] = bindir + os.pathsep + env.get("PATH", _DEFAULT_PATH)
            # A login shell's /etc/profile rebuilds PATH and would drop the shim
            # behind /usr/bin; its own profile puts it back in front.
            with open(os.path.join(private_dir, ".bash_profile"), "w",
                      encoding="utf-8") as f:
                f.write(f'PATH={shlex.quote(bindir)}:"$PATH"\nexport PATH\n')
        return env

    def reap(self):
        """Kill every process the scope's children started, whatever they did.

        The process-group kill after each command cannot reach a child that
        setsid()ed away, so those are found by the environment marker and killed
        here, before the temp they may be using is removed. Survivors, and a scan
        that could not run, are recorded and printed, never swallowed.
        """
        if self._reaped or not self._marker_issued:
            return
        self._reaped = True
        try:
            self.survivors = procscan.reap_marked(self.marker)
            # What the marker cannot find (a hardened system binary hides its
            # environment) is listed, not killed: nobody can say whose it is.
            if containment_backend() == "sandbox-exec":
                self.unverified = procscan.unverified_since(self.started)
        except OSError as e:
            self.reap_error = f"{type(e).__name__}: {e}"
            print(f"[mh] WARNING: could not scan for processes left by {self.root}: "
                  f"{self.reap_error}", file=sys.stderr, flush=True)
            return
        if self.survivors:
            print(f"[mh] WARNING: processes {self.survivors} started under "
                  f"{self.root} survived SIGKILL for {procscan.REAP_DEADLINE_S}s "
                  f"after the episode ended", file=sys.stderr, flush=True)
        if self.unverified:
            listing = ", ".join(f"{u['pid']} ({u['path'] or '?'})" for u in self.unverified)
            print(f"[mh] WARNING: {len(self.unverified)} process(es) started during the episode "
                  f"under {self.root} could not be attributed to it and were NOT killed: "
                  f"{listing}; {procscan.UNVERIFIED_REASON}", file=sys.stderr, flush=True)

    @property
    def clean_close(self):
        """True only when the reap ran, found nothing left, and nothing is unverified."""
        return (self._reaped and not self.survivors and self.reap_error is None
                and not self.unverified)

    def close(self):
        try:
            self.reap()
        finally:
            self._final()

    def stamp(self):
        """The provenance record of what this scope enforces."""
        return {"shell_env": "scrubbed", "env_allow": list(CHILD_ENV_ALLOW),
                "temp_root": self.root, "env_guard": env_guard_stats()}


def _sbpl(path):
    """Quote a path for a Seatbelt profile literal."""
    return path.replace("\\", "\\\\").replace('"', '\\"')


def _writable_system_roots():
    """The one place outside the caller's roots a child may write: /dev.

    The host's shared temp directories are NOT here. A toolchain that needs a
    temp file writes to the TMPDIR its ChildScope gave it, which is inside
    `allow_write`; leaving /tmp, /var/folders and the host TMPDIR writable is
    what let one episode's leftovers reach the next.
    """
    return ["/dev"]


def sbpl_metadata_chain(allowed, protected):
    """SBPL rules letting a child stat the directories that lead to its roots.

    A root the child is re-allowed (its own scope, a workspace inside the
    benchmark tree) can sit under a protected root, whose deny covers the
    metadata of every directory on the way down. Without it a plain chdir or
    getcwd inside the child fails, so the compiler or shell dies for the wrong
    reason. Metadata only, and only the literal directories on that chain:
    listing them, or reading a sibling, stays refused.
    """
    prot = [os.path.realpath(p) for p in protected if p]
    seen, lines = set(), []
    for a in allowed:
        if not a:
            continue
        r = os.path.realpath(a)
        for q in prot:
            if r == q or not r.startswith(q + os.sep):
                continue
            cur = os.path.dirname(r)
            while len(cur) >= len(q):
                if cur not in seen:
                    seen.add(cur)
                    lines.append(f'(allow file-read-metadata (literal "{_sbpl(cur)}"))')
                if cur == q:
                    break
                cur = os.path.dirname(cur)
    return lines


# Mach services through which a shell could leave data for a LATER episode
# without touching the filesystem the profile governs: cfprefsd holds
# `defaults write` values in its own memory and flushes them for any process of
# the same user, and the pasteboard server keeps the clipboard. `(allow default)`
# leaves mach-lookup open, so both are denied by name. The names are the ones
# `launchctl print gui/<uid>` lists on the host.
# notifyd (com.apple.system.notification_center) keeps notify_set_state values for
# any same-user process, and distnoted (com.apple.distributed_notifications@<variant>,
# matched by prefix because the suffix names the session type) delivers
# NSDistributedNotificationCenter posts to listeners outside the episode.
DENIED_MACH_SERVICES = ("com.apple.cfprefsd.agent", "com.apple.cfprefsd.daemon",
                        "com.apple.pasteboard.1", "com.apple.system.notification_center")
DENIED_MACH_SERVICE_PREFIXES = ("com.apple.distributed_notifications",)


def _seatbelt_profile(allow_write, protected_roots):
    """Deny writes everywhere, deny the benchmark tree outright, then re-allow.

    Rule order is significant: in SBPL the last matching rule wins, so the
    sandbox's own allow must come after both denies, which keeps a workspace
    that lives inside a protected root usable.
    """
    lines = ["(version 1)", "(allow default)",
             "(deny mach-lookup "
             + " ".join(f'(global-name "{n}")' for n in DENIED_MACH_SERVICES)
             + " " + " ".join(f'(global-name-prefix "{p}")' for p in DENIED_MACH_SERVICE_PREFIXES) + ")",
             '(deny file-write* (subpath "/"))']
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
    lines += sbpl_metadata_chain(allow_write, protected_roots)
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


def containment_event(backend, scope=None):
    """The provenance record of this episode's containment.

    Only a missing backend and an explicit opt-out are uncontained. Naming one
    platform's mechanism as the sole real containment is how a correctly
    contained episode comes to be stamped as an uncontained one -- the record
    then understates the instrument, and an auditor reading it cannot tell a
    contained run from a leaky one.
    """
    ev = {"t": "containment", "backend": backend}
    if backend in (None, "off"):
        ev["note"] = "shell commands are not OS-contained in this episode"
    if scope is not None:
        ev.update(scope.stamp())
    return ev


def _bwrap_argv(exe, argv, allow_write, protected_roots):
    """bubblewrap arguments. Order matters: later binds win over earlier ones.

    Everything is readable and nothing is writable by default; the host's
    shared temp directories are not bound read-write. The order is: the
    protected roots covered with an empty tmpfs (a file, such as a `.env`, is
    covered with /dev/null) so they are neither readable nor writable, then
    every `allow_write` root -- the workspace, which lives *under* one of
    those roots, and the episode's private temp -- bound back read-write.
    Implemented, untested on Linux: this host is macOS.
    """
    out = [exe, "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc"]
    for p in protected_roots:
        if p:
            r = os.path.realpath(p)
            out += (["--ro-bind", "/dev/null", r] if os.path.isfile(r)
                    else ["--tmpfs", r])
    # The write roots last, because they live *under* a masked root and have to
    # win over the tmpfs that just covered their parent.
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
    # A write into the host's shared temp directory, which is what carries one
    # episode's leftovers to the next. It must be refused: a backend that cannot
    # give a child a private temp makes every verdict depend on its neighbours.
    shared_tmp = os.path.join(tempfile.gettempdir(),
                              f"mh-probe-shared-{os.getpid()}-{time.monotonic_ns()}")
    # The enclosing repository's git dirs hold every tracked hidden test and
    # reference solution; a shell that can read one has read them all.
    canaries = [canary] + [os.path.join(r, "HEAD") for r in git_guard_roots(BENCH_ROOT)
                           if os.path.isfile(os.path.join(r, "HEAD"))]
    script = "".join(
        f'if cat {shlex.quote(c)} >/dev/null 2>&1; then echo "READ_OK {c}"; fi; '
        for c in canaries) + (
        f'if echo x > {shlex.quote(outside)} 2>/dev/null; then echo WRITE_OK; fi; '
        f'if echo x > {shlex.quote(shared_tmp)} 2>/dev/null; then echo TEMP_OK; fi; '
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
        shared = os.path.exists(shared_tmp)
    except Exception as e:  # a backend that cannot even start is not containment
        res = (False, backend, f"probe failed to run: {type(e).__name__}: {e}")
        _PROBE_CACHE[key] = res
        return res
    finally:
        if made:
            shutil.rmtree(work, ignore_errors=True)
        for leftover in (outside, shared_tmp):
            try:
                os.remove(leftover)
            except OSError:
                pass

    if "PROBE_RAN" not in out:
        res = (False, backend, f"probe did not run under {backend}: {out.strip()[:200]}")
    elif "READ_OK" in out:
        leaked = [ln.split(" ", 1)[1] for ln in out.splitlines()
                  if ln.startswith("READ_OK ")]
        res = (False, backend,
               f"{backend} did not stop a read of {', '.join(leaked) or canary}")
    elif escaped:
        res = (False, backend,
               f"{backend} let a write reach {outside} on the host filesystem")
    elif shared:
        res = (False, backend,
               f"{backend} let a write reach the host's shared temp directory "
               f"{os.path.dirname(shared_tmp)}: episodes would share it. "
               f"Refusing rather than run with a shared temp")
    elif "WRITE_OK" in out:
        res = (True, backend,
               f"{backend} confined reads; the write succeeded into a masked "
               f"overlay and did not reach {outside} on the host")
    else:
        res = (True, backend, f"{backend} confined reads and writes")
    _PROBE_CACHE[key] = res
    return res


def contained_argv(argv, allow_write=(), protected_roots=None):
    """Wrap argv so the process is confined for the two things that matter.

    Writes: only inside allow_write (plus /dev and the temp directories a
    toolchain needs). Reads and writes of protected_roots -- the benchmark
    installation, tasks and results included: refused outright. Reads elsewhere
    are left alone, because a compiler that cannot read /usr is not a harness.

    Returns (argv, backend). Raises ContainmentUnavailable when this platform has
    no backend and the operator has not opted out: a subprocess that could not be
    contained must never look like one that was.
    """
    if protected_roots is None:
        protected_roots = default_protected_roots()
    # Every scope lives under the runner's private parent, which no child may
    # read; a child's own scope is re-allowed through allow_write, which the
    # profile applies after the protected denies.
    protected_roots = list(protected_roots) + [runner_parent()]
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
    # start_new_session=True makes the child a session and group leader, so its
    # pgid IS its pid. os.getpgid(proc.pid) is not used: it raises once the
    # leader has been reaped, which silently degraded this to proc.kill() of a
    # process that was already gone, leaving its group running.
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except (ProcessLookupError, PermissionError, OSError):
        try:
            proc.kill()
        except OSError:
            pass


# After a kill, how long to wait for the pipes to close and the leader to be
# reaped. A `setsid`'d escapee on macOS can hold a pipe open forever; this
# bounds the harness's wait for it.
_KILL_GRACE_S = 10


def _exited_unreaped(pid):
    """True once `pid` has exited, WITHOUT reaping it (so its pid stays reserved)."""
    try:
        return os.waitid(os.P_PID, pid,
                         os.WEXITED | os.WNOHANG | os.WNOWAIT) is not None
    except ChildProcessError:
        return True


def _wait_exit_then_kill_group(proc, deadline):
    """Wait for the leader to exit, kill what is left of its group, then reap.

    The group is killed while the exited leader is still an unreaped zombie:
    its pid, and therefore the group id, cannot be reused by another process
    until it is reaped, so the killpg cannot reach a stranger. Returns False
    when the deadline passed first (nothing killed; the caller times out).
    """
    if hasattr(os, "waitid") and hasattr(os, "WNOWAIT"):
        delay = 0.001
        while not _exited_unreaped(proc.pid):
            if deadline is not None and time.monotonic() >= deadline:
                return False
            time.sleep(delay)
            delay = min(delay * 2, 0.05)
        _kill_group(proc)
        proc.wait()
        return True
    try:
        proc.wait(None if deadline is None
                  else max(0.0, deadline - time.monotonic()))
    except subprocess.TimeoutExpired:
        return False
    _kill_group(proc)
    return True


def run_bounded_capture(argv, cwd=None, env=None, input_text=None, timeout=None,
                        limit=MAX_OUTPUT_BYTES):
    """Run argv; return (returncode, stdout StreamCapture, stderr StreamCapture).

    The one engine behind run_bounded and run_shell. Semantics:

    * Waits, as communicate() did, for both pipes to reach EOF and the process
      to exit, so what is captured is what communicate() would have captured.
    * Then kills the process's whole group, on success as well as on timeout.
      A `nohup sleep 300 &` used to outlive the command and the episode on
      macOS, because the group was killed only on a timeout. (bwrap's
      --unshare-pid already took descendants down with the namespace.) A
      process that setsid()s itself out of the group is not reached here; it is
      found by ChildScope.reap() through its environment marker at episode end.
    * A timeout kills the group and raises subprocess.TimeoutExpired, like
      subprocess.run.
    * Output is held as a StreamCapture of `limit` (<= 0: keep everything).
    """
    enc = _text_encoding()
    proc = subprocess.Popen(
        argv, cwd=cwd, env=env,
        stdin=subprocess.PIPE if input_text is not None else subprocess.DEVNULL,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        start_new_session=True)
    deadline = None if timeout is None else time.monotonic() + timeout
    caps = {proc.stdout.fileno(): StreamCapture(limit),
            proc.stderr.fileno(): StreamCapture(limit)}
    out_cap, err_cap = caps[proc.stdout.fileno()], caps[proc.stderr.fileno()]
    pending = memoryview(input_text.encode(enc, "replace")) if input_text else None
    # Kept as an int: once proc.stdin is closed its fileno() raises.
    stdin_fd = proc.stdin.fileno() if proc.stdin is not None else None
    sel = selectors.DefaultSelector()
    try:
        for fd in caps:
            sel.register(fd, selectors.EVENT_READ)
        if stdin_fd is not None:
            if pending:
                os.set_blocking(stdin_fd, False)
                sel.register(stdin_fd, selectors.EVENT_WRITE)
            else:
                proc.stdin.close()
        while sel.get_map():
            remaining = None
            if deadline is not None:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise subprocess.TimeoutExpired(argv, timeout)
            for key, _ in sel.select(remaining):
                fd = key.fd
                if fd == stdin_fd:
                    try:
                        n = os.write(fd, pending[:65536])
                        pending = pending[n:]
                    except BlockingIOError:
                        continue
                    except (BrokenPipeError, OSError):
                        pending = None
                    if not pending:
                        sel.unregister(fd)
                        proc.stdin.close()
                    continue
                chunk = os.read(fd, 65536)
                if chunk:
                    caps[fd].feed(chunk)
                else:
                    sel.unregister(fd)
                    caps[fd].close()
        if not _wait_exit_then_kill_group(proc, deadline):
            raise subprocess.TimeoutExpired(argv, timeout)
    except subprocess.TimeoutExpired:
        _kill_group(proc)
        try:
            proc.wait(timeout=_KILL_GRACE_S)
        except subprocess.TimeoutExpired:
            pass
        raise
    except BaseException:
        # Never leave the command's group running behind an exception
        # (KeyboardInterrupt included).
        _kill_group(proc)
        raise
    finally:
        sel.close()
        for f in (proc.stdin, proc.stdout, proc.stderr):
            if f is not None:
                try:
                    f.close()
                except OSError:
                    pass
    out_cap.close()
    err_cap.close()
    return proc.returncode, out_cap, err_cap


def run_bounded(argv, cwd=None, env=None, input_text=None, timeout=None):
    """subprocess.run, except the whole process group is killed afterwards.

    One owner for "run something with a deadline": the shell tool and the
    verifier both go through here, so neither can leave orphans behind while the
    other cleans up. Raises subprocess.TimeoutExpired, like subprocess.run does.
    Returns (returncode, stdout, stderr) as text, decoded exactly as
    Popen(text=True, errors="replace") decoded them. Nothing is capped here; see
    run_bounded_capture for the bounded form.
    """
    rc, out, err = run_bounded_capture(argv, cwd=cwd, env=env,
                                       input_text=input_text, timeout=timeout,
                                       limit=0)
    return rc, out.text(), err.text()


class Sandbox:
    def __init__(self, root, output_cap=MAX_OUTPUT_BYTES,
                 protected_roots=None):
        if protected_roots is None:
            protected_roots = default_protected_roots()
        self.root = os.path.realpath(root)
        self.output_cap = output_cap
        self.protected_roots = tuple(p for p in protected_roots if p)
        self.containment = None      # set on first run_shell
        self._containment_checked = False
        self.last_shell_stats = None  # byte counts of the latest run_shell
        self._scope = None           # the shell's ChildScope, made on first use
        self.survivors = []          # set by close(): pids that outlived the episode
        self.reap_error = None       # set by close(): why the process scan could not run
        self.unverified = []         # set by close(): processes that cannot be attributed

    @property
    def scope(self):
        """This episode's private temp tree; the shell's HOME and TMPDIR live in it."""
        if self._scope is None:
            self._scope = ChildScope(prefix="mh-episode-")
        return self._scope

    def shell_env(self):
        """The environment every child of this sandbox's shell starts with."""
        return self.scope.env(self.scope.private_dir("shell"))

    @property
    def clean_close(self):
        """False whenever close() left a survivor, could not scan, or found an unattributed process."""
        return not (self.survivors or self.reap_error or self.unverified)

    def close(self):
        """End of the episode: remove its private temp. Idempotent."""
        if self._scope is not None:
            scope, self._scope = self._scope, None
            try:
                scope.close()
            finally:
                self.survivors = list(scope.survivors)
                self.reap_error = scope.reap_error
                self.unverified = list(scope.unverified)

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
            return contained_argv(
                argv, allow_write=[self.root, self.scope.root],
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
            rc, out, err = run_bounded_capture(argv, cwd=self.root,
                                               env=self.shell_env(),
                                               timeout=SHELL_TIMEOUT_S,
                                               limit=self.output_cap)
        except subprocess.TimeoutExpired:
            raise ToolError(
                f"command timed out after {SHELL_TIMEOUT_S}s; the command and every "
                f"process it started were killed. "
                f"Long-running or interactive commands will not work here.")
        except OSError as e:
            raise ToolError(f"could not start the shell: {e}")
        # What the command produced, which the model is shown only as a count
        # in the elision banner: recorded so a capped result stays auditable.
        self.last_shell_stats = {"stdout_bytes": out.raw_bytes,
                                 "stderr_bytes": err.raw_bytes,
                                 "stdout_text_bytes": out.total,
                                 "stderr_text_bytes": err.total}
        body = shell_body(out, err, self.output_cap)
        return f"exit={rc}\n{body}" if body.strip() else f"exit={rc}\n(no output)"

    # ---- file access that cannot be redirected after resolve() -------------
    #
    # resolve() realpath()s a model-supplied path and checks it is inside the
    # sandbox; the open used to happen afterwards, by name, in the uncontained
    # harness process. Anything able to swap a path component for a symlink in
    # between -- a background process the model left running -- could send the
    # read or the write anywhere the harness can reach. So the open walks the
    # already-resolved (symlink-free) path from a directory fd on the sandbox
    # root, one component at a time, refusing to follow any symlink it meets.

    def _open_in_root(self, real, flags, mode=0o666, make_dirs=False):
        """os.open() of a resolve()d path, walked from the root without symlinks.

        Raises FileNotFoundError for a missing path (callers phrase that), and
        ToolError when a component is, or has become, a symlink or a non-directory.
        """
        rel = os.path.relpath(real, self.root)
        parts = [] if rel == os.curdir else rel.split(os.sep)
        if any(p in ("", os.curdir, os.pardir) for p in parts):
            raise ToolError(f"path {real!r} is not a plain path inside the sandbox")
        dfd = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY)
        try:
            if not parts:
                return os.open(os.curdir, flags | os.O_NOFOLLOW, mode, dir_fd=dfd)
            for comp in parts[:-1]:
                try:
                    nfd = self._open_dir_at(comp, dfd)
                except FileNotFoundError:
                    if not make_dirs:
                        raise
                    try:
                        os.mkdir(comp, 0o777, dir_fd=dfd)
                    except FileExistsError:
                        pass
                    nfd = self._open_dir_at(comp, dfd)
                os.close(dfd)
                dfd = nfd
            try:
                return os.open(parts[-1], flags | os.O_NOFOLLOW, mode, dir_fd=dfd)
            except OSError as e:
                if e.errno in (errno.ELOOP, errno.EMLINK):
                    raise ToolError(
                        f"{rel!r} is a symbolic link that was not there when the "
                        f"path was checked; refused.") from e
                raise
        finally:
            os.close(dfd)

    @staticmethod
    def _open_dir_at(comp, dfd):
        try:
            return os.open(comp, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                           dir_fd=dfd)
        except OSError as e:
            if e.errno in (errno.ELOOP, errno.ENOTDIR, errno.EMLINK):
                raise ToolError(
                    f"path component {comp!r} is not a plain directory (it is, or "
                    f"became, a symbolic link or a file); refused.") from e
            raise

    def _open_checked(self, path, real, flags, mode=0o666, make_dirs=False,
                      verb="read"):
        """_open_in_root plus the fstat every caller needs.

        Opened O_NONBLOCK so a FIFO the model made cannot hang the harness in
        open(), then refused unless it is a regular file or a directory.
        Returns (fd, stat_result).
        """
        try:
            fd = self._open_in_root(real, flags | os.O_NONBLOCK, mode, make_dirs)
        except FileNotFoundError:
            raise ToolError(f"{path!r} does not exist. List the directory before "
                            f"reading.")
        except IsADirectoryError:
            raise ToolError(f"{path!r} is a directory, not a file; pick a path "
                            f"inside it.")
        except OSError as e:
            raise ToolError(f"could not {verb} {path!r}: {e}")
        try:
            st = os.fstat(fd)
            if not (stat.S_ISREG(st.st_mode) or stat.S_ISDIR(st.st_mode)):
                raise ToolError(f"{path!r} is not a regular file (a FIFO, socket or "
                                f"device); refused.")
            os.set_blocking(fd, True)
        except BaseException:
            os.close(fd)
            raise
        return fd, st

    def _list_dir(self, path, entries):
        entries = sorted(entries)
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
        fd, st = self._open_checked(path, real, os.O_RDONLY)
        try:
            if stat.S_ISDIR(st.st_mode):
                try:
                    return self._list_dir(path, os.listdir(fd))
                except OSError as e:
                    raise ToolError(f"could not read {path!r}: {e}")
            # Bound the read *before* building per-line strings. Reading a 200 MB
            # file and then splitting it into numbered lines grew RSS by 857 MB;
            # the budget now bounds the bytes we touch, not just the bytes we
            # return.
            cap = self.output_cap if self.output_cap > 0 else MAX_READ_BYTES
            budget = max(1024, min(cap, MAX_READ_BYTES))
            try:
                return self._numbered(fd, st.st_size, budget)
            except OSError as e:
                raise ToolError(f"could not read {path!r}: {e}")
        finally:
            os.close(fd)

    def _numbered(self, fd, size, budget):
        """The file with line numbers, built a line at a time under a byte budget.

        The old version read the whole file and then built one string per line
        before capping anything, so a 200 MB file grew RSS by 857 MB. Nothing
        here holds more than `budget` bytes, whatever the file's size or shape.
        `fd` is an open regular file; it is read, never closed, here.
        """
        big = size > budget
        limit = budget // 2 if big else budget
        parts, total, lines, overran = [], 0, 0, False
        with os.fdopen(os.dup(fd), "r", errors="replace") as f:
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
        tail = os.pread(fd, budget // 2, max(total, size - budget // 2))
        tail = tail.decode("utf-8", "replace")
        tail = tail.split("\n", 1)[1] if "\n" in tail else tail
        return (head +
                f"\n\n... [the harness read only part of this {size}-byte file: "
                f"the first {lines} lines ({total} bytes) with line numbers, and the "
                f"last {len(tail.encode('utf-8', 'replace'))} bytes without them. "
                f"The middle was never read. Use run_shell with sed -n to see a "
                f"specific line range.] ...\n\n" + tail)

    def _write_text(self, path, real, text, make_dirs=False):
        """Truncate and write `text` to a regular file opened without symlinks."""
        fd, st = self._open_checked(path, real, os.O_WRONLY | os.O_CREAT,
                                    make_dirs=make_dirs, verb="write")
        if stat.S_ISDIR(st.st_mode):
            os.close(fd)
            raise ToolError(f"{path!r} is a directory, not a file; pick a path "
                            f"inside it.")
        try:
            os.ftruncate(fd, 0)
            with os.fdopen(fd, "w") as f:
                fd = None
                f.write(text)
        except OSError as e:
            raise ToolError(f"could not write {path!r}: {e}")
        finally:
            if fd is not None:
                os.close(fd)

    def write_file(self, path=None, content=None, **_):
        real = self.resolve(path)
        if content is None:
            raise ToolError("write_file requires 'content'")
        if os.path.isdir(real):
            raise ToolError(f"{path!r} is a directory, not a file; pick a path inside it.")
        if not isinstance(content, str):
            content = str(content)
        self._write_text(path, real, content, make_dirs=True)
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
        fd, st = self._open_checked(path, real, os.O_RDONLY)
        if stat.S_ISDIR(st.st_mode):
            os.close(fd)
            raise ToolError(f"{path!r} is a directory, not a file; edit a file inside it.")
        try:
            with os.fdopen(fd, "r", errors="replace") as f:
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
        self._write_text(path, real, text.replace(old, new, 1))
        return f"replaced 1 occurrence in {path}"

    def finish(self, summary=None, **_):
        return summary or ""

    def handlers(self):
        """The five tools by name.

        Dispatch used to `getattr` these, which meant `finish` existed, was
        advertised in SCHEMAS, and had no inbound call the graph could see.
        """
        return {
            "run_shell": self.run_shell,
            "read_file": self.read_file,
            "write_file": self.write_file,
            "edit_file": self.edit_file,
            "finish": self.finish,
        }


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
