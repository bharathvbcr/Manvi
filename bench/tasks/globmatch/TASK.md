Implement `matches(pattern, name)` in `globmatch.py`, exactly as `SPEC.md`
describes.

`python3 test_glob.py` covers only six cases. Your implementation will be checked
against several hundred pattern/name pairs, including character classes, negation,
ranges, unterminated `[`, and empty strings.

You may not use the `fnmatch`, `glob`, `re`, `regex` or `pathlib` modules; this is
enforced. Do not modify the test or the spec.
