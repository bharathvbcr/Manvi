"""Contention, FIFO, capacity, and close - things the visible test never hits.

TASK.md states two contracts the old version of this file left unchecked: the
queue is a FIFO ("the n-th successful `get` returns the n-th successful `put`"),
and the stdlib `queue` module is off limits. Both are asserted here. The FIFO
checks use values whose natural sort order is not their insertion order, and a
gate around `put` so the global put order is recoverable, because a queue that
returns items in some *other* well-defined order satisfies every other check in
this file.
"""
import sys
import os

FORBIDDEN = ("queue", "importlib", "imp", "runpy")
MAX_FILES = 200
MAX_BYTES = 1 << 20


def die(msg):
    print("FAIL:", msg)
    raise SystemExit(1)


# --- the stdlib `queue` ban, enforced rather than merely stated -------------
if not getattr(os, "__file__", ""):
    die("the `os` module has no __file__; the interpreter is not intact")

_UNTRUSTED = set()
for _p in (os.environ.get("PYTHONPATH") or "").split(os.pathsep):
    if _p:
        _UNTRUSTED.add(os.path.abspath(_p))
SANDBOX = os.path.abspath(os.getcwd())
_UNTRUSTED.add(SANDBOX)


def _under_untrusted(path):
    if not path:
        return False
    d = os.path.abspath(os.path.dirname(path))
    return any(d == u or d.startswith(u + os.sep) for u in _UNTRUSTED)


for _name in ("sitecustomize.py", "usercustomize.py"):
    if os.path.exists(os.path.join(SANDBOX, _name)):
        die(f"{_name} in the sandbox: the fix belongs in bqueue.py, "
            f"not in interpreter startup")

_saved_path = list(sys.path)
sys.path = [p for p in sys.path if os.path.abspath(p or ".") not in _UNTRUSTED]
for _mod_name, _mod in list(sys.modules.items()):
    if _under_untrusted(getattr(_mod, "__file__", None)):
        del sys.modules[_mod_name]

import ast          # noqa: E402
import threading    # noqa: E402
import time         # noqa: E402

for _m in (ast, threading, time):
    if _under_untrusted(getattr(_m, "__file__", None)):
        die(f"stdlib module {_m.__name__!r} is shadowed by a sandbox file "
            f"({_m.__file__}); the checks cannot be trusted")

sys.path = _saved_path
sys.dont_write_bytecode = True
sys.pycache_prefix = os.path.join(
    os.path.abspath(os.sep), "nonexistent-mh-pycache", str(os.getpid()))

sources = []
for root, dirs, files in os.walk(SANDBOX):
    dirs[:] = [d for d in dirs if d not in ("__pycache__", ".git")]
    for f in sorted(files):
        if f.endswith(".py"):
            sources.append(os.path.join(root, f))
sources.sort()
if len(sources) > MAX_FILES:
    die(f"{len(sources)} Python files in the sandbox exceeds the {MAX_FILES} "
        f"the constraint check will read; refusing to report a partial scan")

offenders = []
for path in sources:
    rel = os.path.relpath(path, SANDBOX)
    if os.path.getsize(path) > MAX_BYTES:
        die(f"{rel} is larger than {MAX_BYTES} bytes; refusing to report a "
            f"partial scan")
    try:
        tree = ast.parse(open(path, encoding="utf-8", errors="replace").read(),
                         filename=rel)
    except SyntaxError as e:
        die(f"{rel} is not parseable Python ({e}); the constraint check "
            f"cannot run over it")
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for a in node.names:
                if a.name.split(".")[0] in FORBIDDEN:
                    offenders.append(f"{rel}: import {a.name}")
        elif isinstance(node, ast.ImportFrom):
            top = (node.module or "").split(".")[0]
            if top in FORBIDDEN:
                offenders.append(f"{rel}: from {node.module} import ...")
        elif isinstance(node, ast.Call) and isinstance(node.func, ast.Name) \
                and node.func.id == "__import__":
            offenders.append(f"{rel}: __import__(...)")
        elif isinstance(node, ast.Attribute) and node.attr in (
                "import_module", "find_spec", "load_module"):
            offenders.append(f"{rel}: {node.attr}(...)")
if offenders:
    die("TASK.md forbids the stdlib queue module: " + "; ".join(sorted(set(offenders))))

for _mod_name in list(sys.modules):
    if _mod_name.partition(".")[0] in FORBIDDEN:
        del sys.modules[_mod_name]


class _Blocked:
    """Refuse every import of a forbidden module, however it is spelled."""

    @staticmethod
    def find_spec(name, path=None, target=None):
        if name.partition(".")[0] in FORBIDDEN:
            raise ImportError(f"TASK.md forbids importing {name!r}")
        return None

    @staticmethod
    def find_module(name, path=None):       # pragma: no cover - py<3.12 only
        _Blocked.find_spec(name, path)
        return None


sys.meta_path.insert(0, _Blocked)

# Execute the candidate from the source text that was just scanned, so a
# bytecode cache beside it cannot stand in for the file that was inspected.
_path = os.path.join(SANDBOX, "bqueue.py")
if not os.path.isfile(_path):
    die("bqueue.py is missing from the sandbox")
_module = type(sys)("bqueue")
_module.__file__ = _path
sys.modules["bqueue"] = _module
try:
    exec(compile(open(_path, encoding="utf-8").read(), _path, "exec"),
         _module.__dict__)
except BaseException as e:          # noqa: BLE001 - report, never propagate
    die(f"importing bqueue.py raised {type(e).__name__}: {e}")
try:
    BoundedQueue = _module.BoundedQueue
    QueueClosed = _module.QueueClosed
except AttributeError as e:
    die(f"bqueue.py does not export the public names: {e}")

bad = 0


def eq(label, got, want):
    global bad
    if got != want:
        print("FAIL", label, "got", repr(got), "want", repr(want))
        bad += 1


def join_all(threads, timeout):
    deadline = time.monotonic() + timeout
    for t in threads:
        remaining = deadline - time.monotonic()
        t.join(timeout=max(0.01, remaining))
    alive = [t.name for t in threads if t.is_alive()]
    if alive:
        print("FAIL deadlock: still running", alive)
        return False
    return True


# --- sequential contract the broken code almost satisfies
q = BoundedQueue(2)
q.put(1)
q.put(2)
eq("fifo1", q.get(), 1)
eq("fifo2", q.get(), 2)

# --- FIFO, not "some other order". These values are deliberately not in
# sorted order, so a heap or any other reordering container shows up here.
q = BoundedQueue(3)
for item in ("zebra", "apple", "mango"):
    q.put(item)
eq("fifo is insertion order", [q.get() for _ in range(3)],
   ["zebra", "apple", "mango"])
q = BoundedQueue(5)
for item in (30, 10, 50, 20, 40):
    q.put(item)
eq("fifo is not sorted order", [q.get() for _ in range(5)],
   [30, 10, 50, 20, 40])
eq("empty after fifo drain", q.qsize(), 0)

# --- capacity=1, many producers and consumers. notify() + if-wait deadlocks.
N_ITEMS = 200
N_PROD = 8
N_CONS = 8
q = BoundedQueue(1)
got = []
got_lock = threading.Lock()
sent = []
errors = []


def prod(start, step):
    try:
        for i in range(start, N_ITEMS, step):
            q.put(i)
            sent.append(i)
    except Exception as e:
        errors.append(("prod", type(e).__name__, str(e)))


def cons():
    try:
        while True:
            try:
                item = q.get()
            except QueueClosed:
                return
            with got_lock:
                got.append(item)
                if len(got) >= N_ITEMS:
                    return
    except Exception as e:
        errors.append(("cons", type(e).__name__, str(e)))


producers = []
consumers = []
for i in range(N_PROD):
    t = threading.Thread(target=prod, args=(i, N_PROD), name=f"p{i}", daemon=True)
    producers.append(t)
for i in range(N_CONS):
    t = threading.Thread(target=cons, name=f"c{i}", daemon=True)
    consumers.append(t)
for t in producers + consumers:
    t.start()

if not join_all(producers, 8.0):
    bad += 1
q.close()
if not join_all(consumers, 8.0):
    bad += 1
else:
    eq("errors", errors, [])
    eq("got count", len(got), N_ITEMS)
    eq("got set", sorted(got), list(range(N_ITEMS)))
    # With many consumers the order in which `got` is appended is not the order
    # in which items left the queue, so only the per-producer order is provable
    # here: values from each residue class must appear in increasing order.
    # The global FIFO contract is checked in the next block instead.
    for r in range(N_PROD):
        seq = [x for x in got if x % N_PROD == r]
        eq(f"per-producer order r={r}", seq, list(range(r, N_ITEMS, N_PROD)))


# --- global FIFO: the n-th successful get returns the n-th successful put.
# A gate around put makes the global put order observable; a single consumer
# makes the get order observable. The two lists must be identical.
FIFO_N = 240
FIFO_PROD = 4
q = BoundedQueue(4)
put_gate = threading.Lock()
fifo_sent = []
fifo_got = []
fifo_errors = []


def gated_prod(pid):
    # Values are chosen so that ascending-by-value is not ascending-by-time:
    # producer 0 emits the largest values, producer 3 the smallest.
    try:
        for i in range(FIFO_N // FIFO_PROD):
            with put_gate:
                q.put((FIFO_PROD - 1 - pid) * 1000 + i)
                fifo_sent.append((FIFO_PROD - 1 - pid) * 1000 + i)
    except Exception as e:
        fifo_errors.append(("prod", type(e).__name__, str(e)))


def solo_cons():
    try:
        while len(fifo_got) < FIFO_N:
            fifo_got.append(q.get())
    except Exception as e:
        fifo_errors.append(("cons", type(e).__name__, str(e)))


fifo_producers = [threading.Thread(target=gated_prod, args=(p,),
                                   name=f"gp{p}", daemon=True)
                  for p in range(FIFO_PROD)]
fifo_consumer = threading.Thread(target=solo_cons, name="gc", daemon=True)
for t in fifo_producers:
    t.start()
fifo_consumer.start()
if not join_all(fifo_producers, 8.0) or not join_all([fifo_consumer], 8.0):
    bad += 1
    q.close()
    join_all([fifo_consumer], 2.0)
else:
    eq("global fifo errors", fifo_errors, [])
    eq("global fifo count", len(fifo_got), FIFO_N)
    eq("global fifo order", fifo_got, fifo_sent)


# --- qsize never exceeds capacity under churn
q = BoundedQueue(3)
overflow = []
stop = threading.Event()


def watcher():
    while not stop.is_set():
        n = q.qsize()
        if n < 0 or n > 3:
            overflow.append(n)
        time.sleep(0)


def churn_put():
    for i in range(80):
        q.put(i)


def churn_get():
    for _ in range(80):
        q.get()


wt = threading.Thread(target=watcher, daemon=True)
pt = threading.Thread(target=churn_put, daemon=True)
gt = threading.Thread(target=churn_get, daemon=True)
wt.start()
pt.start()
gt.start()
ok = join_all([pt, gt], 8.0)
stop.set()
wt.join(timeout=1)
if not ok:
    bad += 1
eq("no overflow samples", overflow, [])
eq("empty after churn", q.qsize(), 0)


# --- close unblocks waiters; leftover items are still delivered, in order
q = BoundedQueue(4)
q.put("keep")
released = []


def blocked_get():
    try:
        released.append(("got", q.get()))
    except QueueClosed as e:
        released.append(("closed", type(e).__name__))
    except Exception as e:
        released.append(("err", type(e).__name__))


def blocked_put():
    try:
        q.put("late")
        released.append(("put", "ok"))
    except QueueClosed as e:
        released.append(("put-closed", type(e).__name__))
    except Exception as e:
        released.append(("put-err", type(e).__name__))


# fill the rest so a put will block, then close
q.put("a")
q.put("b")
q.put("c")  # now full (keep, a, b, c)
tg = threading.Thread(target=blocked_get, daemon=True)
tp = threading.Thread(target=blocked_put, daemon=True)
# one extra getter will block on empty after the four items are drained
# first: close while a putter is blocked on full
tp.start()
time.sleep(0.05)
q.close()
tg.start()
if not join_all([tg, tp], 4.0):
    bad += 1
# `tg` is joined above, so it took exactly one item and nothing else has run:
# under FIFO that item is the head, "keep", and the drain sees the rest in order.
eq("blocked get took the head", released.count(("got", "keep")), 1)
rest = []
while True:
    try:
        rest.append(q.get())
    except QueueClosed:
        break
eq("leftovers drain in order", rest, ["a", "b", "c"])
eq("put after close raised", any(x[0] == "put-closed" for x in released), True)
eq("close is idempotent", q.close(), None)
try:
    q.put("nope")
    eq("put on closed", "no-raise", "QueueClosed")
except QueueClosed:
    pass


# --- get on an empty closed queue raises, does not hang
q = BoundedQueue(1)
q.close()
raised = []


def empty_get():
    try:
        q.get()
        raised.append("returned")
    except QueueClosed:
        raised.append("closed")
    except Exception as e:
        raised.append(type(e).__name__)


t = threading.Thread(target=empty_get, daemon=True)
t.start()
if not join_all([t], 2.0):
    bad += 1
eq("empty closed get", raised, ["closed"])


# --- constructor rejects capacity < 1
try:
    BoundedQueue(0)
    eq("cap 0", "no-raise", "ValueError")
except ValueError:
    pass


sys.meta_path.remove(_Blocked)
sys.exit(1 if bad else 0)
