import ast
import os
import random
import re
import sys
from nfa import fullmatch

bad = 0


def banned():
    path = os.path.join(os.environ.get("PYTHONPATH", "."), "nfa.py")
    if not os.path.isfile(path):
        path = "nfa.py"
    tree = ast.parse(open(path).read())
    bad_mods = []
    for n in ast.walk(tree):
        if isinstance(n, ast.Import):
            for a in n.names:
                if a.name.split(".")[0] in ("re", "regex", "fnmatch", "pathlib"):
                    bad_mods.append(a.name)
        if isinstance(n, ast.ImportFrom) and (n.module or "").split(".")[0] in (
                "re", "regex", "fnmatch", "pathlib"):
            bad_mods.append(n.module)
    return bad_mods


def eq(label, got, want):
    global bad
    if got != want:
        print("FAIL", label, "got", repr(got), "want", repr(want))
        bad += 1


def raises(label, pat):
    global bad
    try:
        fullmatch(pat, "")
    except ValueError:
        return
    except Exception as e:
        print("FAIL", label, type(e).__name__)
        bad += 1
        return
    print("FAIL", label, "did not raise", repr(pat))
    bad += 1


eq("no re/regex/fnmatch", banned(), [])
eq("lit", fullmatch("abc", "abc"), True)
eq("lit miss", fullmatch("abc", "ab"), False)
eq("dot", fullmatch("a.c", "axc"), True)
eq("dot nl", fullmatch(".", "\n"), False)
eq("star0", fullmatch("a*", ""), True)
eq("star", fullmatch("a*", "aaaa"), True)
eq("star fail", fullmatch("a*", "b"), False)
eq("plus", fullmatch("a+", ""), False)
eq("plus2", fullmatch("a+", "aa"), True)
eq("opt", fullmatch("a?", ""), True)
eq("opt1", fullmatch("a?", "a"), True)
eq("opt2", fullmatch("a?", "aa"), False)
eq("alt", fullmatch("a|bc", "bc"), True)
eq("alt2", fullmatch("a|bc", "a"), True)
eq("alt3", fullmatch("a|bc", "b"), False)
eq("group", fullmatch("(ab)+", "abab"), True)
eq("empty pat", fullmatch("", ""), True)
eq("empty pat2", fullmatch("", "a"), False)
eq("empty alt", fullmatch("a|", ""), True)
eq("empty alt2", fullmatch("a|", "a"), True)
eq("esc", fullmatch(r"a\*b", "a*b"), True)
eq("esc dot", fullmatch(r"\.", "."), True)
eq("esc paren", fullmatch(r"\(", "("), True)
eq("nested", fullmatch("(a|b)*c", "ababac"), True)
eq("not partial", fullmatch("a", "ba"), False)
eq("cat star", fullmatch("a*b*", "aaabbb"), True)

raises("unclosed", "(ab")
raises("extra close", "ab)")
raises("lead star", "*a")
raises("double star", "a**")
raises("trail bs", "abc\\")
raises("bar star", "|*")

# oracle: Python re on the same restricted language (no backslash in random)
random.seed(9)
atoms = list("abc.")
for trial in range(250):
    # tiny random regex
    def rand_expr(depth):
        if depth <= 0:
            return random.choice(atoms)
        k = random.choice(["lit", "dot", "star", "plus", "opt", "alt", "grp", "cat"])
        if k == "lit":
            return random.choice("abc")
        if k == "dot":
            return "."
        inner = rand_expr(depth - 1)
        if k == "star":
            return "(" + inner + ")*"
        if k == "plus":
            return "(" + inner + ")+"
        if k == "opt":
            return "(" + inner + ")?"
        if k == "alt":
            return "(" + rand_expr(depth - 1) + "|" + rand_expr(depth - 1) + ")"
        if k == "grp":
            return "(" + inner + ")"
        return inner + rand_expr(depth - 1)

    pat = rand_expr(3)
    text = "".join(random.choice("abc") for _ in range(random.randint(0, 6)))
    try:
        want = re.fullmatch(pat, text) is not None
    except re.error:
        continue
    try:
        got = fullmatch(pat, text)
    except Exception as e:
        print("FAIL oracle exc", pat, text, type(e).__name__, e)
        bad += 1
        break
    if got != want:
        print("FAIL oracle", "pat", pat, "text", text, "got", got, "want", want)
        bad += 1
        break

sys.exit(1 if bad else 0)
