Implement `rewrite(source)` in `rewrite.py` exactly as `SPEC.md` describes.

`python3 test_rewrite.py` covers two happy-path programs. The hidden check
runs the rewritten code against async iterators that record `aiter` / `anext`
/ `aclose` / `__aenter__` / `__aexit__` calls, including `break`, `continue`,
the `else` clause, exceptions inside `async with`, and nested `async with`.

You may use the `ast` module. You may not use `re`, `regex`, or string
substitution to perform the rewrite. Do not modify the test or the spec.
