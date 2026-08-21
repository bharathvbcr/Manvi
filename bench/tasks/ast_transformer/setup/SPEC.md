# Async control-flow desugaring

`rewrite(source: str) -> str` parses `source` as a Python module, rewrites
every `async for` and every `async with` according to the rules below, and
returns syntactically valid Python. After rewriting, a walk of the returned
tree must contain **no** `ast.AsyncFor` and **no** `ast.AsyncWith` nodes.

Rewriting is recursive: nested `async for` / `async with` (including those
inside the body of another) are rewritten too. Other statement kinds are
left alone, including ordinary `for`, `with`, `async def`, and `yield`.

Fresh names used by the desugaring must not collide with any name already
present in the source (as `ast.Name`, `ast.arg`, or `ast.alias`). Pick names
that do not appear; a monotonic suffix is fine.

You may use the `ast` module. You may not import `re` or `regex`.

## `async for`

```
async for TARGET in ITER:
    BODY
else:
    ELSE
```

is equivalent to:

```
_it = None
try:
    _it = aiter(ITER)
    _ex = False
    while not _ex:
        try:
            TARGET = await anext(_it)
        except StopAsyncIteration:
            _ex = True
            continue
        BODY
    if _ex:
        ELSE
finally:
    if _it is not None:
        _ac = getattr(_it, "aclose", None)
        if _ac is not None:
            await _ac()
```

Consequences, which the rewrite must preserve by using this shape (or
another that has the same control-flow and the same calls):

- `break` inside `BODY` leaves the `while`, skips `ELSE`, and still runs
  `aclose` in `finally`.
- `continue` inside `BODY` continues the `while` (so the next `anext`).
- `ELSE` runs only when the iterator was exhausted (`StopAsyncIteration`),
  not when `BODY` broke.
- If `ITER` raises before `aiter` succeeds, `aclose` is not called.
- `TARGET` may be any assignment target (`x`, `x.y`, `(a, b)`, ...).
- If there is no `else` clause, omit the `if _ex: ELSE` statement.

`aiter` and `anext` are the builtins.

## `async with`

A single item

```
async with EXPR as VAR:
    BODY
```

is equivalent to:

```
_mgr = EXPR
_exit = type(_mgr).__aexit__
_entered = await type(_mgr).__aenter__(_mgr)
_ok = True
try:
    VAR = _entered
    BODY
except BaseException:
    _ok = False
    if not await _exit(_mgr, *sys.exc_info()):
        raise
finally:
    if _ok:
        await _exit(_mgr, None, None, None)
```

- If there is no `as VAR`, skip the `VAR = _entered` assignment but still
  enter, run `BODY`, and exit.
- `__aexit__` returning a true value suppresses the exception (do not
  re-raise).
- The rewritten module must `import sys` if it does not already, because
  `sys.exc_info` is required on the exception path.

Multiple items desugar left-to-right by nesting:

```
async with A as a, B as b:
    BODY
```

is `async with A as a:` wrapping `async with B as b: BODY`.
