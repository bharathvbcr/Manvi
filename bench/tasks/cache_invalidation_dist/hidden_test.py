import random
import sys
from distcache import DistCache

bad = 0


def eq(label, got, want):
    # Exact types first: what crosses from the worker is data, and an object
    # that is not the wanted builtin is not the wanted value however it compares.
    global bad
    if type(got) is not type(want) or not (got == want):
        print("FAIL", label, "got", repr(got), "want", repr(want))
        bad += 1


def ne(label, got, want):
    """got must be a value of want's type that differs from it."""
    global bad
    if type(got) is not type(want) or got == want:
        print("FAIL", label, "got", repr(got), "must differ from", repr(want))
        bad += 1


def views(c, key):
    return [c.get(i, key) for i in range(c.n_nodes)]


# --- happy path: put replicates, invalidate replicates (no partition)
c = DistCache(3)
c.put(0, "a", "1")
eq("replicate put", views(c, "a"), ["1", "1", "1"])
c.invalidate(2, "a")
eq("replicate inv", views(c, "a"), [None, None, None])
eq("missing", c.get(0, "nope"), None)

# --- split hides writes
c = DistCache(3)
c.split([[0], [1, 2]])
c.put(0, "k", "A")
c.put(1, "k", "B")
eq("isolated 0", c.get(0, "k"), "A")
eq("group 1-2 share B", (c.get(1, "k"), c.get(2, "k")), ("B", "B"))
ne("0 does not see B", c.get(0, "k"), "B")
ne("2 does not see A", c.get(2, "k"), "A")

# --- heal: (clock, origin) — both sides wrote once from 0, so clocks are 1
# (1, 1) > (1, 0) so B wins
c.heal()
eq("heal tie-break nid", views(c, "k"), ["B", "B", "B"])

# --- the same tie, written in the other order. (clock, origin) does not
# depend on wall-clock order, so a per-node Lamport clock gives B either way.
# A single counter shared across the partition -- which no partitioned system
# can have -- gives A here, because A was simply written second.
c = DistCache(3)
c.split([[0], [1, 2]])
c.put(1, "k", "B")
c.put(0, "k", "A")
c.heal()
eq("heal tie-break nid, reversed", views(c, "k"), ["B", "B", "B"])

# --- clocks are per node: writes on one side of the partition must not
# advance the clock on the other side. Three writes on the isolated node 0
# outrank one write on node 1, whose own clock only ever reached 1.
c = DistCache(2)
c.split([[0], [1]])
c.put(0, "k", "A0")             # clock0 = 1
c.put(0, "k", "A1")             # clock0 = 2
c.put(0, "k", "A2")             # clock0 = 3 -> (3, 0)
c.put(1, "k", "B")              # clock1 = 1 -> (1, 1)
c.heal()
eq("isolated writes keep their own clock", views(c, "k"), ["A2", "A2"])

# --- and the mirror image: one write on node 0 must lose to three on node 1
c = DistCache(2)
c.split([[0], [1]])
c.put(1, "k", "B0")
c.put(1, "k", "B1")
c.put(1, "k", "B2")             # clock1 = 3 -> (3, 1)
c.put(0, "k", "A")              # clock0 = 1 -> (1, 0)
c.heal()
eq("isolated writes keep their own clock, mirrored",
   views(c, "k"), ["B2", "B2"])

# --- receiving a record raises the receiver's clock (step 4), so node 0 can
# out-rank node 1 despite node 1 holding the higher origin id.
c = DistCache(2)
c.put(1, "seed", "s")           # clock1 = 1, and clock0 = max(0, 1) = 1
c.split([[0], [1]])
c.put(0, "k", "A")              # clock0 = 2
c.put(0, "k", "A2")             # clock0 = 3 -> (3, 0)
c.put(1, "k", "B")              # clock1 = 2 -> (2, 1)
c.heal()
eq("apply raises the receiver clock", views(c, "k"), ["A2", "A2"])

# --- heal levels every clock to the maximum, so the next write on a node
# that was behind still outranks what the leader wrote before the heal.
c = DistCache(2)
c.split([[0], [1]])
c.put(1, "seed", "s")           # clock1 = 1
c.put(1, "seed", "s2")          # clock1 = 2
c.heal()                        # clocks become 2 everywhere
c.split([[0], [1]])
c.put(0, "k", "A")              # clock0 = 3
c.put(0, "k", "A2")             # clock0 = 4 -> (4, 0)
c.put(1, "k", "B")              # clock1 = 3 -> (3, 1)
c.heal()
eq("heal levels the clocks", views(c, "k"), ["A2", "A2"])

# --- a concurrent write must not beat a tombstone stamped at the same clock
# by a higher node id, whichever of the two happened first in real time.
for order in ("tomb-first", "write-first"):
    c = DistCache(3)
    c.put(0, "d", "live")       # clocks = 1 everywhere
    c.split([[0], [1, 2]])
    if order == "tomb-first":
        c.invalidate(1, "d")    # (tomb, 2, 1)
        c.put(0, "d", "again")  # (again, 2, 0)
    else:
        c.put(0, "d", "again")  # (again, 2, 0)
        c.invalidate(1, "d")    # (tomb, 2, 1)
    c.heal()
    eq(f"tombstone wins the version tie ({order})", views(c, "d"),
       [None, None, None])

# --- later write on a higher clock wins even from a lower nid
c = DistCache(3)
c.split([[0, 1], [2]])
c.put(2, "x", "from2")          # clock2=1
c.put(0, "x", "from0")          # clock0=1
c.put(0, "x", "from0b")         # clock0=2  -> (2,0) > (1,2)
c.heal()
eq("higher clock wins", views(c, "x"), ["from0b", "from0b", "from0b"])

# --- stale replica must not resurrect a delete
c = DistCache(3)
c.put(0, "d", "live")           # all have live, clocks=1
c.split([[0], [1, 2]])
c.invalidate(1, "d")            # {1,2} tombstone clock=2; node 0 still "live" clock=1
eq("stale still live", c.get(0, "d"), "live")
eq("majority deleted", (c.get(1, "d"), c.get(2, "d")), (None, None))
c.heal()
eq("delete wins heal", views(c, "d"), [None, None, None])

# --- put after invalidate on the same component
c = DistCache(2)
c.put(0, "z", "1")
c.invalidate(0, "z")
c.put(1, "z", "2")
eq("put after inv", views(c, "z"), ["2", "2"])

# --- split does not mutate the argument
groups = [[0, 1], [2]]
c = DistCache(3)
c.split(groups)
groups.append([9])
c.put(0, "m", "yes")
eq("split arg not reused", c.get(2, "m"), None)

# --- heal is idempotent and converges every key
c = DistCache(3)
c.split([[0], [1], [2]])
c.put(0, "p", "P")
c.put(1, "q", "Q")
c.put(2, "p", "R")              # (1,2) vs (1,0) -> R wins on p
c.heal()
c.heal()
eq("multi-key p", views(c, "p"), ["R", "R", "R"])
eq("multi-key q", views(c, "q"), ["Q", "Q", "Q"])

# --- get never returns the tombstone marker
c = DistCache(1)
c.put(0, "t", "v")
c.invalidate(0, "t")
eq("no tomb leak", c.get(0, "t"), None)

# --- fuzzer: a model of the spec, run in the checker, is the want. The candidate
# is compared with it after every get and every heal, never with itself: a cache
# that notices the fuzzer and answers one thing everywhere agrees with itself.
class _Model:
    TOMB = "__tomb__"

    def __init__(self, n):
        self.n = n
        self.store = [{} for _ in range(n)]
        self.clock = [0] * n
        self.group = [0] * n

    def _apply(self, j, key, rec):
        cur = self.store[j].get(key)
        if cur is None or (rec[1], rec[2]) > (cur[1], cur[2]):
            self.store[j][key] = rec
        if rec[1] > self.clock[j]:
            self.clock[j] = rec[1]

    def _replicate(self, node, key, tag):
        self.clock[node] += 1
        rec = (tag, self.clock[node], node)
        for j in range(self.n):
            if self.group[node] == self.group[j]:
                self._apply(j, key, rec)

    def put(self, node, key, value):
        self._replicate(node, key, value)

    def invalidate(self, node, key):
        self._replicate(node, key, self.TOMB)

    def get(self, node, key):
        rec = self.store[node].get(key)
        return None if rec is None or rec[0] == self.TOMB else rec[0]

    def split(self, groups):
        gmap = [None] * self.n
        for gi, g in enumerate(groups):
            for nid in g:
                gmap[nid] = gi
        self.group = gmap

    def heal(self):
        self.group = [0] * self.n
        keys = set()
        for s in self.store:
            keys.update(s)
        for k in keys:
            best = None
            for s in self.store:
                rec = s.get(k)
                if rec is not None and (best is None or (rec[1], rec[2]) > (best[1], best[2])):
                    best = rec
            if best is not None:
                for s in self.store:
                    s[k] = best
        self.clock = [max(self.clock)] * self.n

    def views(self, key):
        return [self.get(i, key) for i in range(self.n)]


class _FuzzStop(Exception):
    pass


random.seed(11)
trials_done = 0
for trial in range(40):
    n = 3
    c = DistCache(n)
    m = _Model(n)
    keys = ["a", "b", "c"]
    try:
        for _ in range(25):
            op = random.choice(["put", "put", "inv", "get", "split", "heal"])
            node = random.randrange(n)
            key = random.choice(keys)
            if op == "put":
                val = random.choice(["x", "y", "z"])
                c.put(node, key, val)
                m.put(node, key, val)
            elif op == "inv":
                c.invalidate(node, key)
                m.invalidate(node, key)
            elif op == "get":
                v, w = c.get(node, key), m.get(node, key)
                if type(v) is not type(w) or not (v == w):
                    print("FAIL get differs from the model", trial, node, key,
                          repr(v), "want", repr(w))
                    bad += 1
                    raise _FuzzStop
            elif op == "split":
                # random partition of 3 nodes
                kind = random.choice(["iso", "2-1", "3"])
                if kind == "iso":
                    k = random.randrange(n)
                    groups = [[k], [i for i in range(n) if i != k]]
                elif kind == "2-1":
                    groups = [[0, 1], [2]]
                else:
                    groups = [[0], [1], [2]]
                c.split(groups)
                m.split(groups)
            else:
                c.heal()
                m.heal()
                for key in keys:
                    vs, ws = views(c, key), m.views(key)
                    if len(vs) != len(ws) or any(
                            type(x) is not type(y) or not (x == y) for x, y in zip(vs, ws)):
                        print("FAIL heal diverge", trial, key, vs, "want", ws)
                        bad += 1
                        raise _FuzzStop
    except _FuzzStop:
        break
    except Exception as e:
        # Whatever the candidate raised -- including StopIteration -- is a failed
        # trial, never a reason to skip the remaining ones.
        print("FAIL fuzz raised", trial, type(e).__name__, e)
        bad += 1
        break
    trials_done += 1
eq("fuzz trials", trials_done, 40)

sys.exit(1 if bad else 0)
