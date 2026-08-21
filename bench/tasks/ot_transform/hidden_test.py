import random
import sys
from ot import apply, transform

bad = 0


def eq(label, got, want):
    global bad
    if got != want:
        print("FAIL", label, "got", repr(got), "want", repr(want))
        bad += 1


def conv(doc, a, b):
    a2, b2 = transform(a, b)
    try:
        left = apply(apply(doc, a), b2)
        right = apply(apply(doc, b), a2)
    except Exception as e:
        print("FAIL conv exc", doc, a, b, type(e).__name__, e)
        return False
    if left != right:
        print("FAIL conv", "doc", repr(doc), "a", a, "b", b,
              "a2", a2, "b2", b2, "L", repr(left), "R", repr(right))
        return False
    return True


doc = "ac"
a = ("ins", 1, "b", 1)
b = ("ins", 2, "d", 2)
eq("visible-like", conv(doc, a, b), True)
eq("abcd", apply(apply(doc, a), transform(a, b)[1]), "abcd")

# same-position insert, site tie-break
doc = "xy"
a = ("ins", 1, "A", 1)
b = ("ins", 1, "B", 2)
eq("tie site", conv(doc, a, b), True)
eq("lower site left", apply(apply(doc, a), transform(a, b)[1]), "xABy")

a = ("ins", 1, "A", 5)
b = ("ins", 1, "B", 2)
eq("higher site right", conv(doc, a, b), True)

# equal site ids
a = ("ins", 1, "A", 7)
b = ("ins", 1, "B", 7)
eq("equal site conv", conv(doc, a, b), True)

# insert vs delete
doc = "abcd"
eq("ins del after", conv(doc, ("ins", 1, "X", 1), ("del", 2, 2)), True)
eq("ins del before", conv(doc, ("ins", 3, "X", 1), ("del", 0, 2)), True)
eq("ins on del pos", conv(doc, ("ins", 2, "X", 1), ("del", 2, 2)), True)
eq("del vs ins swap", conv(doc, ("del", 2, 2), ("ins", 1, "X", 1)), True)

# delete vs delete
eq("del del", conv(doc, ("del", 1, 1), ("del", 3, 2)), True)
eq("del same", conv(doc, ("del", 2, 1), ("del", 2, 2)), True)
eq("del same result", apply(apply(doc, ("del", 2, 1)),
                            transform(("del", 2, 1), ("del", 2, 2))[1]), "abd")

# nop
eq("nop L", conv(doc, ("nop",), ("ins", 0, "Z", 1)), True)
eq("nop R", conv(doc, ("del", 0, 1), ("nop",)), True)

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

# fuzzer
random.seed(3)
alphabet = "abc"
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
    if not conv(doc, ops[0], ops[1]):
        bad += 1
        break

sys.exit(1 if bad else 0)
