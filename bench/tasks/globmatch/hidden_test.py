"""Check the candidate against CPython's own fnmatch, on the pattern/name grid
this repository already uses for Go/Rust parity, plus extra adversarial pairs.

The ban on `fnmatch`/`glob`/`re`/`regex`/`pathlib` is the point of the task, and
it is enforced by one call, `_mh.enforce_bans`, that does two independent things:

  1. a static audit over *every* Python source in the sandbox -- a helper
     module the candidate wrote is still the candidate's code. It runs in the
     checker, which may read the sandbox but never executes anything in it, and
     names the dynamic spellings (`__import__`, `builtins.__dict__[...]`,
     importlib) as well as direct imports.
  2. an audit hook in the WORKER, for the whole time the candidate runs. It
     refuses the import event of every banned module that an import statement,
     `__import__` or importlib spells, gates the loader calls that raise no such
     event, and candidate code cannot remove it. One loader route stays open and
     is named in every record's `unenforced` list (see mh/rpc.py).

Verifier-split note (this file was adapted when the grader was split into a
checker and a worker): the candidate is imported by name into the worker, whose
interpreter is source-only (`-B`, no writable pycache) and started fresh, so the
old third defence -- resolving this checker's own stdlib imports from outside the
sandbox, and exec'ing the candidate from the scanned source text -- is now
structural: the checker's `sys.path` does not contain the sandbox at all, so an
`ast.py` or a stale `.pyc` dropped next to `globmatch.py` cannot reach or stand
in for anything the checker imports. The oracle (`fnmatch`) is held in the
checker, which never runs candidate code, so it cannot be shadowed. The worker
harness loads none of the banned modules, so there is no already-imported copy
(the old `json.decoder.re` leak) to reach; the verification record says so
(`bans[].preloaded == []`) or names what was reachable.
"""
import fnmatch as _fnmatch     # the authority; the checker never runs candidate code
import os
import sys

import _mh

FORBIDDEN = ("fnmatch", "glob", "re", "regex", "pathlib",
             "importlib", "imp", "runpy", "sre_compile", "sre_parse")


def die(msg):
    print("FAIL:", msg)
    raise SystemExit(1)


# --- 1. the directory the candidate controls ------------------------------
if not getattr(os, "__file__", ""):
    die("the `os` module has no __file__; the interpreter is not intact")

SANDBOX = _mh.SANDBOX

for _name in ("sitecustomize.py", "usercustomize.py"):
    if os.path.exists(os.path.join(SANDBOX, _name)):
        die(f"{_name} in the sandbox: the fix belongs in globmatch.py, "
            f"not in interpreter startup")

# --- 3. decide every expected answer while fnmatch is still usable -------
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

# --- 4. arm the runtime ban in the worker, then import the candidate -------
_mh.enforce_bans(FORBIDDEN)
from globmatch import matches  # noqa: E402  -- imported into the worker, under the ban

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
print(f"checked {checked} pairs, {bad} wrong")
sys.exit(1 if bad else 0)
