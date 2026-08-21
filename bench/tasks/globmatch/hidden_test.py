"""Check the candidate against CPython's own fnmatch, on the pattern/name grid
this repository already uses for Go/Rust parity, plus extra adversarial pairs.

The forbidden modules are poisoned before the candidate is imported, so an
implementation that reaches for fnmatch fails at import rather than passing.
"""
import ast, importlib, sys

FORBIDDEN = ("fnmatch", "glob", "re", "regex", "pathlib")

# 1. static check on the candidate's own source
src = open("globmatch.py").read()
used = set()
for node in ast.walk(ast.parse(src)):
    if isinstance(node, ast.Import):
        for a in node.names:
            used.add(a.name.split(".")[0])
    elif isinstance(node, ast.ImportFrom):
        if node.module:
            used.add(node.module.split(".")[0])
    elif isinstance(node, ast.Call) and isinstance(node.func, ast.Name) \
            and node.func.id == "__import__":
        used.add("__import__")
banned = sorted(used.intersection(FORBIDDEN))
if banned or "__import__" in used:
    print(f"FAIL: forbidden module use: {banned or '__import__'}")
    sys.exit(1)

# 2. poison them, then import the candidate
saved = {m: sys.modules.get(m) for m in FORBIDDEN}
for m in FORBIDDEN:
    sys.modules[m] = None
try:
    globmatch = importlib.import_module("globmatch")
    matches = globmatch.matches
except Exception as e:
    print(f"FAIL: importing globmatch without {FORBIDDEN} raised {type(e).__name__}: {e}")
    sys.exit(1)
finally:
    for m, v in saved.items():
        if v is None:
            sys.modules.pop(m, None)
        else:
            sys.modules[m] = v

import fnmatch  # the authority, after the candidate is already bound

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

bad = 0
shown = 0
for p in PATTERNS:
    for n in NAMES:
        want = fnmatch.fnmatchcase(n, p)
        try:
            got = matches(p, n)
        except Exception as e:
            print(f"EXC pattern={p!r} name={n!r}: {type(e).__name__}: {e}")
            bad += 1
            shown += 1
            if shown > 12: sys.exit(1)
            continue
        if bool(got) is not want:
            print(f"FAIL pattern={p!r} name={n!r} got={got} want={want}")
            bad += 1
            shown += 1
            if shown > 12: sys.exit(1)

print(f"checked {len(PATTERNS) * len(NAMES)} pairs, {bad} wrong")
sys.exit(1 if bad else 0)
