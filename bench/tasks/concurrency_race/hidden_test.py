"""Contention, FIFO, capacity, and close — things the visible test never hits."""
import sys
import threading
import time
from bqueue import BoundedQueue, QueueClosed

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
    # global FIFO: the sequence of successful gets equals the sequence of
    # successful puts. Producers interleave, but each put is visible to get
    # in the order put acquired the slot.
    # We cannot recover put-order from `sent` without a lock around put+append.
    # Check the weaker per-producer order instead: values from each residue
    # class must appear in increasing order.
    for r in range(N_PROD):
        seq = [x for x in got if x % N_PROD == r]
        eq(f"per-producer order r={r}", seq, list(range(r, N_ITEMS, N_PROD)))


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


# --- close unblocks waiters; leftover items are still delivered
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
# leftover items still come out
rest = []
while True:
    try:
        rest.append(q.get())
    except QueueClosed:
        break
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


sys.exit(1 if bad else 0)
