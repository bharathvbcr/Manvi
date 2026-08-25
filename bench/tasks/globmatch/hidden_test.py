"""Check the candidate against CPython's own fnmatch, on the pattern/name grid
this repository already uses for Go/Rust parity, plus extra adversarial pairs.

The ban on `fnmatch`/`glob`/`re`/`regex`/`pathlib` is the point of the task, so
it is enforced three ways, because each one alone is bypassable:

  1. statically, over *every* Python source in the sandbox -- a helper module
     the candidate wrote is still the candidate's code;
  2. at runtime, for the whole duration of the grid, so a lazy import inside a
     function fails when it is reached rather than sliding past the parser;
  3. with this checker's own stdlib imports resolved from outside the sandbox,
     so dropping an `ast.py` next to `globmatch.py` cannot neuter step 1.

The candidate is executed from the source text step 1 parsed, so a bytecode
cache beside it cannot stand in for the file that was inspected. The oracle is
held by direct reference, taken before the ban goes live.
"""
import sys
import os

FORBIDDEN = ("fnmatch", "glob", "re", "regex", "pathlib",
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
        die(f"{_name} in the sandbox: the fix belongs in globmatch.py, "
            f"not in interpreter startup")

# --- 2. import this checker's dependencies from outside the sandbox --------
_saved_path = list(sys.path)
sys.path = [p for p in sys.path if os.path.abspath(p or ".") not in _UNTRUSTED]
for _mod_name, _mod in list(sys.modules.items()):
    if _under_untrusted(getattr(_mod, "__file__", None)):
        del sys.modules[_mod_name]

import ast                    # noqa: E402
import fnmatch as _fnmatch    # noqa: E402  -- the authority, held by reference

for _m in (ast, _fnmatch):
    if _under_untrusted(getattr(_m, "__file__", None)):
        die(f"stdlib module {_m.__name__!r} is shadowed by a sandbox file "
            f"({_m.__file__}); the constraint check cannot be trusted")

sys.path = _saved_path
# Ignore any bytecode cache beside the candidate's sources.
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

# --- 4. decide every expected answer while fnmatch is still usable -------
PATTERNS = ["*.py", "**/.env", ".env", ".env.*", "src/*", "**/credentials/**",
            "**/*.pem", ".claude/*", ".claude/**", ".git/*", ".devcouncil/*",
            ".github/workflows/*.yml", "package.json", "**/id_rsa",
            "src/legacy/**", "a[bc]d", "a[!bc]d", "x?z", "[]]a",
            "**/secrets/**", "uv.lock", "*",
            # adversarial extras
            "[a-c]x", "[!a-c]x", "[-a]x", "[a-]x", "[]]", "[!]]", "a[", "a[b",
            "", "?", "??", "*a*", "a*b*c", "[abc", "x[a-c]y", "**", "a\\b",
            "[A-Z]bc", "[!A-Z]bc", "*.*", "[0-9]", "]", "[]"]
NAMES = ["src/foo.py", "foo.py", "a/b/.env", ".env", ".env.local", "src/a/b/c.py",
         "x/credentials/y", "credentials/y", "x/credentials/y/z", "a.pem",
         "deep/a.pem", ".claude/settings.json", ".claude/a/b", ".git/config",
         ".devcouncil/state.sqlite", ".github/workflows/ci.yml", "package.json",
         "sub/package.json", "home/id_rsa", "src/legacy/old.go",
         "src/legacy/a/b.go", "abd", "acd", "axd", "xyz", "x/z", "]a", "uv.lock",
         "p/secrets/k", "secrets/k", "",
         "ax", "bx", "dx", "-x", "]", "a[", "a[b", "a\\b", "Abc", "abc", "5",
         "a", "ab", "abc", "aXbYc", "x.y", "[", "a-x"]

ORACLE = [(p, n, _fnmatch.fnmatchcase(n, p))
          for p in PATTERNS for n in NAMES]

# --- 5. runtime block, live for the whole of the grid ----------------------
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
_path = os.path.join(SANDBOX, "globmatch.py")
if not os.path.isfile(_path):
    die("globmatch.py is missing from the sandbox")
_module = type(sys)("globmatch")
_module.__file__ = _path
sys.modules["globmatch"] = _module
try:
    exec(compile(open(_path, encoding="utf-8").read(), _path, "exec"),
         _module.__dict__)
except BaseException as e:          # noqa: BLE001 - report, never propagate
    die(f"importing globmatch.py without {FORBIDDEN} raised "
        f"{type(e).__name__}: {e}")
matches = getattr(_module, "matches", None)
if not callable(matches):
    die("globmatch.py does not define a callable `matches`")

bad = 0
shown = 0
checked = 0
for p, n, want in ORACLE:
    checked += 1
    try:
        got = matches(p, n)
    except Exception as e:
        print(f"EXC pattern={p!r} name={n!r}: {type(e).__name__}: {e}")
        bad += 1
        shown += 1
        if shown > 12:
            sys.exit(1)
        continue
    if bool(got) is not want:
        print(f"FAIL pattern={p!r} name={n!r} got={got} want={want}")
        bad += 1
        shown += 1
        if shown > 12:
            sys.exit(1)

if checked != len(PATTERNS) * len(NAMES):
    print(f"FAIL: only {checked} of {len(PATTERNS) * len(NAMES)} pairs ran")
    sys.exit(1)
sys.meta_path.remove(_Blocked)
print(f"scanned {len(sources)} Python files; "
      f"checked {checked} pairs, {bad} wrong")
sys.exit(1 if bad else 0)
