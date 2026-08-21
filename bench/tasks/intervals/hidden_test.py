import random, sys
from intervals import merge, intersect, subtract

bad = 0
def eq(label, got, want):
    global bad
    if got != want:
        print("FAIL", label, "got", got, "want", want); bad += 1

# --- explicit cases
eq("merge overlap", merge([(1, 3), (2, 6)]), [(1, 6)])
eq("merge touching", merge([(0, 5), (5, 9)]), [(0, 9)])
eq("merge unsorted", merge([(5, 9), (0, 5)]), [(0, 9)])
eq("merge disjoint", merge([(5, 9), (0, 2)]), [(0, 2), (5, 9)])
eq("merge empty iv dropped", merge([(3, 3), (0, 2)]), [(0, 2)])
eq("merge reversed iv dropped", merge([(5, 1)]), [])
eq("merge contained", merge([(0, 10), (2, 3)]), [(0, 10)])
eq("merge empty", merge([]), [])
eq("merge negative", merge([(-5, -1), (-1, 2)]), [(-5, 2)])
eq("merge dupes", merge([(1, 2), (1, 2)]), [(1, 2)])

eq("intersect basic", intersect([(0, 10)], [(4, 6)]), [(4, 6)])
eq("intersect none", intersect([(0, 2)], [(5, 9)]), [])
eq("intersect touching is empty", intersect([(0, 5)], [(5, 9)]), [])
eq("intersect multi", intersect([(0, 5), (7, 10)], [(3, 8)]), [(3, 5), (7, 8)])
eq("intersect empty a", intersect([], [(1, 2)]), [])
eq("intersect empty b", intersect([(1, 2)], []), [])
eq("intersect unsorted", intersect([(7, 10), (0, 5)], [(3, 8)]), [(3, 5), (7, 8)])
eq("intersect adjacent output merged", intersect([(0, 10)], [(0, 5), (5, 10)]), [(0, 10)])

eq("subtract middle", subtract([(0, 10)], [(4, 6)]), [(0, 4), (6, 10)])
eq("subtract all", subtract([(0, 10)], [(0, 10)]), [])
eq("subtract none", subtract([(0, 10)], [(20, 30)]), [(0, 10)])
eq("subtract prefix", subtract([(0, 10)], [(0, 4)]), [(4, 10)])
eq("subtract suffix", subtract([(0, 10)], [(6, 10)]), [(0, 6)])
eq("subtract touching noop", subtract([(0, 5)], [(5, 9)]), [(0, 5)])
eq("subtract multi", subtract([(0, 10)], [(2, 3), (5, 6)]), [(0, 2), (3, 5), (6, 10)])
eq("subtract empty b", subtract([(0, 10)], []), [(0, 10)])
eq("subtract empty a", subtract([], [(0, 10)]), [])
eq("subtract negative", subtract([(-10, 0)], [(-5, -4)]), [(-10, -5), (-4, 0)])
eq("subtract unsorted", subtract([(5, 10), (0, 2)], [(1, 6)]), [(0, 1), (6, 10)])

# --- no mutation of arguments
a = [(0, 5), (3, 9)]; b = [(4, 6)]
a_copy, b_copy = list(a), list(b)
merge(a); intersect(a, b); subtract(a, b)
eq("merge/intersect/subtract do not mutate a", a, a_copy)
eq("... nor b", b, b_copy)

# --- randomised against a point-set oracle
def points(ivs, lo=-12, hi=12):
    return {x for x in range(lo, hi) for (s, e) in ivs if s <= x < e}

def from_points(pts, lo=-12, hi=12):
    out = []
    for x in range(lo, hi):
        if x in pts:
            if out and out[-1][1] == x:
                out[-1] = (out[-1][0], x + 1)
            else:
                out.append((x, x + 1))
    return [tuple(t) for t in out]

random.seed(4)
for trial in range(400):
    def rnd():
        return [tuple(sorted((random.randint(-10, 10), random.randint(-10, 10))))
                if random.random() < 0.8
                else (random.randint(-10, 10), random.randint(-10, 10))
                for _ in range(random.randint(0, 5))]
    a, b = rnd(), rnd()
    for name, fn, want_pts in (
            ("merge", lambda: merge(a), points(a)),
            ("intersect", lambda: intersect(a, b), points(a) & points(b)),
            ("subtract", lambda: subtract(a, b), points(a) - points(b))):
        try:
            got = [tuple(t) for t in fn()]
        except Exception as e:
            print(f"EXC {name} a={a} b={b}: {type(e).__name__}: {e}"); bad += 1; break
        want = from_points(want_pts)
        if got != want:
            print(f"FAIL {name} a={a} b={b} got={got} want={want}"); bad += 1; break
    if bad > 4:
        break

sys.exit(1 if bad else 0)
