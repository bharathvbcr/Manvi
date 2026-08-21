#!/usr/bin/env python3
"""Regenerate testdata/fnmatch-parity.tsv from CPython's own fnmatch.

The Go and Rust glob matchers must agree with each other *and* with the Python
engine whose path rules they port. One fixture, generated from the authority,
read by both implementations — so a divergence fails a test instead of quietly
widening or narrowing the write gate in one language only.

    python3 scripts/gen-fnmatch-parity.py > testdata/fnmatch-parity.tsv
"""

import fnmatch

# Every glob shape DevCouncil's policy engine actually uses, plus the character
# class and edge cases the matchers have to get right.
PATTERNS = [
    "*.py",
    "**/.env",
    ".env",
    ".env.*",
    "src/*",
    "**/credentials/**",
    "**/*.pem",
    ".claude/*",
    ".claude/**",
    ".git/*",
    ".devcouncil/*",
    ".github/workflows/*.yml",
    "package.json",
    "**/id_rsa",
    "src/legacy/**",
    "a[bc]d",
    "a[!bc]d",
    "x?z",
    "[]]a",
    "**/secrets/**",
    "uv.lock",
    "*",
]

NAMES = [
    "src/foo.py",
    "foo.py",
    "a/b/.env",
    ".env",
    ".env.local",
    "src/a/b/c.py",
    "x/credentials/y",
    "credentials/y",
    "x/credentials/y/z",
    "a.pem",
    "deep/a.pem",
    ".claude/settings.json",
    ".claude/a/b",
    ".git/config",
    ".devcouncil/state.sqlite",
    ".github/workflows/ci.yml",
    "package.json",
    "sub/package.json",
    "home/id_rsa",
    "src/legacy/old.go",
    "src/legacy/a/b.go",
    "abd",
    "acd",
    "axd",
    "xyz",
    "x/z",
    "]a",
    "uv.lock",
    "p/secrets/k",
    "secrets/k",
    "",
]


def main() -> None:
    print(
        "# pattern\tname\texpected  — generated from CPython fnmatch; "
        "regenerate with scripts/gen-fnmatch-parity.py"
    )
    for pattern in PATTERNS:
        for name in NAMES:
            print(f"{pattern}\t{name}\t{str(fnmatch.fnmatch(name, pattern)).lower()}")


if __name__ == "__main__":
    main()
