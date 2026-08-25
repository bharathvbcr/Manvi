"""Hidden checks for nfa_match.

The task's defining constraint is "implement the matcher yourself": no `re`,
`regex`, `fnmatch` or `pathlib`, and no dynamic-import machinery used to reach
them.  Enforcing that needs three things, because any one of them alone is
bypassable:

  1. a *static* scan of every Python source file in the sandbox, not just
     `nfa.py` -- a helper module is still the candidate's own code;
  2. a *runtime* block, so a lazy or dynamically-spelled import fails at the
     moment it is attempted rather than sliding past the parser;
  3. stdlib modules that this checker itself depends on (`ast` above all)
     imported from a path the candidate cannot write to, so that dropping an
     `ast.py` into the sandbox cannot neuter check 1.

The candidate module is executed from its *source text*, which is the same text
check 1 parsed, so a stale or forged bytecode cache beside it cannot stand in
for the file that was inspected.
"""
import sys
import os

FORBIDDEN = ("re", "regex", "fnmatch", "pathlib",
             "importlib", "imp", "runpy", "sre_compile", "sre_parse")
MAX_FILES = 200
MAX_BYTES = 1 << 20


def die(msg):
    print("FAIL:", msg)
    raise SystemExit(1)


# --- 1. locate the directories the candidate controls ----------------------
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
        die(f"{_name} in the sandbox: the fix belongs in nfa.py, "
            f"not in interpreter startup")

# --- 2. import this checker's own dependencies from outside the sandbox -----
_saved_path = list(sys.path)
sys.path = [p for p in sys.path if os.path.abspath(p or ".") not in _UNTRUSTED]
for _mod_name, _mod in list(sys.modules.items()):
    if _under_untrusted(getattr(_mod, "__file__", None)):
        del sys.modules[_mod_name]

import ast          # noqa: E402
import random       # noqa: E402
import re as _re    # noqa: E402  -- the oracle, held by reference from here on

for _m in (ast, random, _re):
    if _under_untrusted(getattr(_m, "__file__", None)):
        die(f"stdlib module {_m.__name__!r} is shadowed by a sandbox file "
            f"({_m.__file__}); the constraint check cannot be trusted")

sys.path = _saved_path
# Ignore any bytecode cache sitting next to the candidate's sources: point the
# cache lookup at a directory that does not exist and is never written.
sys.dont_write_bytecode = True
sys.pycache_prefix = os.path.join(
    os.path.abspath(os.sep), "nonexistent-mh-pycache", str(os.getpid()))

# --- 3. static scan of every Python source in the sandbox ------------------
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

# --- 4. precompute the oracle while `re` is still importable ---------------
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

# --- 5. runtime block, live for the whole of the candidate's execution ------
for _mod_name in list(sys.modules):
    if _mod_name.partition(".")[0] in FORBIDDEN:
        del sys.modules[_mod_name]


class _Blocked:
    """Refuse every import of a forbidden module, however it is spelled."""

    @staticmethod
    def find_spec(name, path=None, target=None):
        if name.partition(".")[0] in FORBIDDEN:
            raise ImportError(f"SPEC.md forbids importing {name!r}")
        return None

    @staticmethod
    def find_module(name, path=None):       # pragma: no cover - py<3.12 only
        _Blocked.find_spec(name, path)
        return None


sys.meta_path.insert(0, _Blocked)

# --- 6. execute the candidate from the source text that was scanned --------
_nfa_path = os.path.join(SANDBOX, "nfa.py")
if not os.path.isfile(_nfa_path):
    die("nfa.py is missing from the sandbox")
_module = type(sys)("nfa")
_module.__file__ = _nfa_path
sys.modules["nfa"] = _module
try:
    exec(compile(open(_nfa_path, encoding="utf-8").read(), _nfa_path, "exec"),
         _module.__dict__)
except BaseException as e:          # noqa: BLE001 - report, never propagate
    die(f"importing nfa.py raised {type(e).__name__}: {e}")
fullmatch = getattr(_module, "fullmatch", None)
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

sys.meta_path.remove(_Blocked)
sys.exit(1 if bad else 0)
