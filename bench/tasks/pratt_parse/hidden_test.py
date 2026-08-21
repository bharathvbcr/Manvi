import sys
from parse import parse

bad = 0


def eq(label, got, want):
    global bad
    if got != want:
        print("FAIL", label, "got", repr(got), "want", repr(want))
        bad += 1


def raises(label, s):
    global bad
    try:
        parse(s)
    except ValueError:
        return
    except Exception as e:
        print("FAIL", label, "raised", type(e).__name__)
        bad += 1
        return
    print("FAIL", label, "did not raise", repr(s))
    bad += 1


def ev(tree):
    if isinstance(tree, int):
        return tree
    tag = tree[0]
    if tag == "neg":
        return -ev(tree[1])
    if tag == "fact":
        n = ev(tree[1])
        if n < 0:
            raise ValueError("fact")
        r = 1
        for i in range(2, n + 1):
            r *= i
        return r
    a, b = ev(tree[1]), ev(tree[2])
    if tag == "+":
        return a + b
    if tag == "-":
        return a - b
    if tag == "*":
        return a * b
    if tag == "/":
        if b == 0:
            raise ValueError("div0")
        # toward zero
        s = -1 if (a < 0) ^ (b < 0) else 1
        return s * (abs(a) // abs(b))
    if tag == "pow":
        if b < 0:
            raise ValueError("negexp")
        return a ** b
    raise ValueError(tag)


eq("prec", parse("1+2*3"), ("+", 1, ("*", 2, 3)))
eq("paren", parse("(1+2)*3"), ("*", ("+", 1, 2), 3))
eq("left +", parse("1+2+3"), ("+", ("+", 1, 2), 3))
eq("left *", parse("2*3*4"), ("*", ("*", 2, 3), 4))
eq("right ^", parse("2^3^2"), ("pow", 2, ("pow", 3, 2)))
eq("unary vs ^", parse("-2^2"), ("neg", ("pow", 2, 2)))
eq("unary val", ev(parse("-2^2")), -4)
eq("double neg", parse("--2"), ("neg", ("neg", 2)))
eq("fact", parse("3!"), ("fact", 3))
eq("fact val", ev(parse("3!")), 6)
eq("fact fact", parse("3!!"), ("fact", ("fact", 3)))
eq("fact vs *", parse("2*3!"), ("*", 2, ("fact", 3)))
eq("ws", parse(" 1 + 2 "), ("+", 1, 2))
eq("unary plus", parse("+2"), 2)
eq("sub", parse("5-3-1"), ("-", ("-", 5, 3), 1))
eq("div toward 0", ev(parse("(-7)/2")), -3)
eq("nested paren", parse("((2))"), 2)
eq("pow unary right", parse("2^-1"), ("pow", 2, ("neg", 1)))
eq("mix", ev(parse("(1+2)*3^2")), 27)

raises("empty", "")
raises("ws only", "   ")
raises("trail +", "1+")
raises("lead *", "*1")
raises("bang prefix", "!3")
raises("empty paren", "()")
raises("unclosed", "(1")
raises("extra close", "1)")
raises("juxtapose", "1 2")
raises("letter", "1+a")
raises("double bin", "1++*2")

# leftover after a complete expr inside unused close already covered
raises("two exprs", "1+2 3")

sys.exit(1 if bad else 0)
