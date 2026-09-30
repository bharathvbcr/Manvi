"""Hidden checks for nfa_match.

The task's defining constraint is "implement the matcher yourself": no `re`,
`regex`, `fnmatch` or `pathlib`, and no dynamic-import machinery used to reach
them. It is enforced two independent ways:

  1. a *static* scan of every Python source file in the sandbox, not just
     `nfa.py` -- a helper module is still the candidate's own code. The scan
     runs in the checker, which may read the sandbox but runs nothing in it.
  2. a *runtime* ban installed in the WORKER (`_mh.forbid_imports`), where the
     candidate runs, so a lazy or dynamically-spelled import fails when reached.

Verifier-split note (adapted when the grader became a checker + a worker): the
candidate is imported by name into the worker, whose interpreter is source-only
(`-B`) and freshly started, so the old defences -- resolving the checker's own
stdlib imports from outside the sandbox and exec'ing the candidate from the
scanned source -- are now structural. The checker's `sys.path` does not contain
the sandbox, so a dropped `ast.py`/`re.py` or a stale `.pyc` cannot reach or
stand in for the checker's imports, and the `re` oracle lives in the checker,
which never runs candidate code. Residual (paper): already-imported references
such as `json.decoder.re` are not revoked; the ban is static + import-hook.
"""
import ast
import os
import random
import re as _re          # the oracle; held in the checker, which runs no candidate code
import sys

import _mh

FORBIDDEN = ("re", "regex", "fnmatch", "pathlib",
             "importlib", "imp", "runpy", "sre_compile", "sre_parse")
MAX_FILES = 200
MAX_BYTES = 1 << 20


def die(msg):
    print("FAIL:", msg)
    raise SystemExit(1)


# --- 1. the directory the candidate controls ------------------------------
if not getattr(os, "__file__", ""):
    die("the `os` module has no __file__; the interpreter is not intact")

SANDBOX = _mh.SANDBOX

for _name in ("sitecustomize.py", "usercustomize.py"):
    if os.path.exists(os.path.join(SANDBOX, _name)):
        die(f"{_name} in the sandbox: the fix belongs in nfa.py, "
            f"not in interpreter startup")

# --- 2. static scan of every Python source in the sandbox ------------------
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
    die("forbidden module use: " + "; ".join(sorted(set(offenders))))

print(f"scanned {len(sources)}/{len(sources)} Python files in the sandbox")

# --- 3. precompute the oracle while `re` is still importable ---------------
random.seed(9)
atoms = list("abc.")


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


ORACLE = []
for _trial in range(250):
    _pat = rand_expr(3)
    _text = "".join(random.choice("abc") for _ in range(random.randint(0, 6)))
    try:
        ORACLE.append((_pat, _text, _re.fullmatch(_pat, _text) is not None))
    except _re.error:
        continue

# --- 4. arm the runtime ban in the worker, then import the candidate -------
_mh.forbid_imports(FORBIDDEN)
from nfa import fullmatch  # noqa: E402  -- imported into the worker, under the ban

if not callable(fullmatch):
    die("nfa.py does not define a callable `fullmatch`")

bad = 0


def eq(label, got, want):
    global bad
    if got != want:
        print("FAIL", label, "got", repr(got), "want", repr(want))
        bad += 1


def call(label, pat, text):
    """Call the candidate; a forbidden import surfaces here as a failure."""
    global bad
    try:
        return fullmatch(pat, text)
    except Exception as e:
        print("FAIL", label, "raised", type(e).__name__, e)
        bad += 1
        return None


def raises(label, pat):
    global bad
    try:
        fullmatch(pat, "")
    except ValueError:
        return
    except Exception as e:
        print("FAIL", label, type(e).__name__, e)
        bad += 1
        return
    print("FAIL", label, "did not raise", repr(pat))
    bad += 1


eq("lit", call("lit", "abc", "abc"), True)
eq("lit miss", call("lit miss", "abc", "ab"), False)
eq("dot", call("dot", "a.c", "axc"), True)
eq("dot nl", call("dot nl", ".", "\n"), False)
eq("star0", call("star0", "a*", ""), True)
eq("star", call("star", "a*", "aaaa"), True)
eq("star fail", call("star fail", "a*", "b"), False)
eq("plus", call("plus", "a+", ""), False)
eq("plus2", call("plus2", "a+", "aa"), True)
eq("opt", call("opt", "a?", ""), True)
eq("opt1", call("opt1", "a?", "a"), True)
eq("opt2", call("opt2", "a?", "aa"), False)
eq("alt", call("alt", "a|bc", "bc"), True)
eq("alt2", call("alt2", "a|bc", "a"), True)
eq("alt3", call("alt3", "a|bc", "b"), False)
eq("group", call("group", "(ab)+", "abab"), True)
eq("empty pat", call("empty pat", "", ""), True)
eq("empty pat2", call("empty pat2", "", "a"), False)
eq("empty alt", call("empty alt", "a|", ""), True)
eq("empty alt2", call("empty alt2", "a|", "a"), True)
eq("esc", call("esc", r"a\*b", "a*b"), True)
eq("esc dot", call("esc dot", r"\.", "."), True)
eq("esc paren", call("esc paren", r"\(", "("), True)
eq("nested", call("nested", "(a|b)*c", "ababac"), True)
eq("not partial", call("not partial", "a", "ba"), False)
eq("cat star", call("cat star", "a*b*", "aaabbb"), True)

raises("unclosed", "(ab")
raises("extra close", "ab)")
raises("lead star", "*a")
raises("double star", "a**")
raises("trail bs", "abc\\")
raises("bar star", "|*")

# oracle: Python re on the same restricted language, decided before the ban
for pat, text, want in ORACLE:
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
else:
    if len(ORACLE) < 200:
        print("FAIL: only", len(ORACLE), "oracle cases were generated")
        bad += 1

sys.exit(1 if bad else 0)
