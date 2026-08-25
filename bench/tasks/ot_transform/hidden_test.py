"""Hidden checks for ot_transform.

Convergence alone does not pin the transform down: an implementation that drops
the site-id tie-break entirely still converges, it just converges on the wrong
document. So the checks below assert the *resulting string* for every tie the
spec names, in both argument orders, and the fuzzer checks the property that
the tie-break exists to provide -- transform(a, b) and transform(b, a) must land
on the same document whenever the two sites differ.
"""
import random
import sys
from ot import apply, transform

bad = 0


def eq(label, got, want):
    global bad
    if got != want:
        print("FAIL", label, "got", repr(got), "want", repr(want))
        bad += 1


def settle(doc, a, b):
    """Converged document reached by applying a then b2. None on divergence."""
    global bad
    before = (repr(a), repr(b))
    try:
        a2, b2 = transform(a, b)
    except Exception as e:
        print("FAIL transform exc", doc, a, b, type(e).__name__, e)
        bad += 1
        return None
    if (repr(a), repr(b)) != before:
        print("FAIL transform mutated its inputs", before, "->", (repr(a), repr(b)))
        bad += 1
        return None
    try:
        left = apply(apply(doc, a), b2)
        right = apply(apply(doc, b), a2)
    except Exception as e:
        print("FAIL conv exc", doc, a, b, type(e).__name__, e)
        bad += 1
        return None
    if left != right:
        print("FAIL conv", "doc", repr(doc), "a", a, "b", b,
              "a2", a2, "b2", b2, "L", repr(left), "R", repr(right))
        bad += 1
        return None
    return left


def conv(doc, a, b):
    return settle(doc, a, b) is not None


def both_orders(label, doc, a, b, want):
    """The pair must converge each way round, and on the same document.

    Site ids decide who goes left, so swapping the arguments must not change
    the answer -- that is exactly the property a missing tie-break destroys.
    """
    eq(label + " (a,b)", settle(doc, a, b), want)
    eq(label + " (b,a)", settle(doc, b, a), want)


doc = "ac"
a = ("ins", 1, "b", 1)
b = ("ins", 2, "d", 2)
eq("visible-like", conv(doc, a, b), True)
eq("abcd", apply(apply(doc, a), transform(a, b)[1]), "abcd")
both_orders("disjoint inserts", doc, a, b, "abcd")

# same-position insert: the LOWER site id keeps its position, whichever
# argument slot it arrives in.
doc = "xy"
both_orders("tie lower site is a", doc,
            ("ins", 1, "A", 1), ("ins", 1, "B", 2), "xABy")
both_orders("tie lower site is b", doc,
            ("ins", 1, "A", 2), ("ins", 1, "B", 1), "xBAy")
both_orders("tie lower site is b, wide gap", doc,
            ("ins", 1, "A", 5), ("ins", 1, "B", 2), "xBAy")
both_orders("tie lower site is a, wide gap", doc,
            ("ins", 1, "A", 2), ("ins", 1, "B", 5), "xABy")
both_orders("tie at position 0", "xy",
            ("ins", 0, "A", 9), ("ins", 0, "B", 3), "BAxy")
both_orders("tie at end", "xy",
            ("ins", 2, "A", 3), ("ins", 2, "B", 9), "xyAB")

# equal site ids: the spec keeps both positions and breaks the tie with
# b2.pos += 1, so the pair converges on a.ch + b.ch when a is applied first.
a = ("ins", 1, "A", 7)
b = ("ins", 1, "B", 7)
eq("equal site conv", conv(doc, a, b), True)
eq("equal site order", settle(doc, a, b), "xABy")
eq("equal site order swapped", settle(doc, b, a), "xBAy")

# insert vs delete
doc = "abcd"
eq("ins del after", settle(doc, ("ins", 1, "X", 1), ("del", 2, 2)), "aXbd")
eq("ins del before", settle(doc, ("ins", 3, "X", 1), ("del", 0, 2)), "bcXd")
eq("ins on del pos", settle(doc, ("ins", 2, "X", 1), ("del", 2, 2)), "abXd")
eq("del vs ins swap", settle(doc, ("del", 2, 2), ("ins", 1, "X", 1)), "aXbd")
both_orders("ins on del pos both ways", doc,
            ("ins", 2, "X", 1), ("del", 2, 2), "abXd")
both_orders("ins after del both ways", doc,
            ("ins", 3, "X", 1), ("del", 1, 2), "acXd")

# delete vs delete
eq("del del", settle(doc, ("del", 1, 1), ("del", 3, 2)), "ac")
eq("del same", settle(doc, ("del", 2, 1), ("del", 2, 2)), "abd")
eq("del same result", apply(apply(doc, ("del", 2, 1)),
                            transform(("del", 2, 1), ("del", 2, 2))[1]), "abd")
both_orders("del del both ways", doc, ("del", 1, 1), ("del", 3, 2), "ac")
both_orders("del same both ways", doc, ("del", 2, 1), ("del", 2, 2), "abd")

# nop
eq("nop L", settle(doc, ("nop",), ("ins", 0, "Z", 1)), "Zabcd")
eq("nop R", settle(doc, ("del", 0, 1), ("nop",)), "bcd")
eq("nop passthrough L", transform(("nop",), ("ins", 0, "Z", 1)),
   (("nop",), ("ins", 0, "Z", 1)))
eq("nop passthrough R", transform(("del", 0, 1), ("nop",)),
   (("del", 0, 1), ("nop",)))

# site ids survive the rewrite
_a2, _b2 = transform(("ins", 1, "A", 4), ("ins", 3, "B", 6))
eq("site kept on a2", _a2[3], 4)
eq("site kept on b2", _b2[3], 6)
_a2, _b2 = transform(("del", 1, 4), ("del", 3, 6))
eq("del site kept on a2", _a2[2], 4)
eq("del site kept on b2", _b2[2], 6)

# apply does not mutate
s = "hi"
apply(s, ("ins", 0, "x", 0))
eq("no mutate", s, "hi")

# range errors
try:
    apply("ab", ("ins", 3, "x", 0))
    eq("ins oob", "no", "ValueError")
except ValueError:
    pass
try:
    apply("ab", ("del", 2, 0))
    eq("del oob", "no", "ValueError")
except ValueError:
    pass

# fuzzer: converge, and converge on the SAME document either way round.
# Sites are always 1 and 2, so the site-id rule -- not argument order --
# has to decide every tie.
random.seed(3)
alphabet = "abc"
checked = 0
for trial in range(200):
    n = random.randint(0, 5)
    doc = "".join(random.choice(alphabet) for _ in range(n))
    ops = []
    for site in (1, 2):
        if doc and random.random() < 0.5:
            ops.append(("del", random.randrange(len(doc)), site))
        else:
            ops.append(("ins", random.randint(0, len(doc)),
                        random.choice("XYZ"), site))
    forward = settle(doc, ops[0], ops[1])
    backward = settle(doc, ops[1], ops[0])
    if forward is None or backward is None:
        bad += 1
        break
    if forward != backward:
        print("FAIL order-dependent", "doc", repr(doc), "a", ops[0],
              "b", ops[1], "a-first", repr(forward), "b-first", repr(backward))
        bad += 1
        break
    checked += 1
else:
    eq("fuzz trials", checked, 200)

sys.exit(1 if bad else 0)
