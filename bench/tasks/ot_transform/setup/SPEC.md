# Character-wise operational transform

A document is a Python `str`. An operation is one of:

- `("ins", pos, ch, site)` — insert the single character `ch` at `pos`
  (`0 <= pos <= len(doc)`). `site` is an `int` site id.
- `("del", pos, site)` — delete the character at `pos`
  (`0 <= pos < len(doc)`).
- `("nop",)` — do nothing.

`apply(doc, op) -> str` returns a new string. It must not mutate `doc`.
Out-of-range `pos` is a `ValueError`. `ch` must be a one-character `str`.

## `transform(a, b) -> (a2, b2)`

`a` and `b` were generated concurrently against the **same** document.
`transform` rewrites them so they commute:

```
apply(apply(doc, a), b2) == apply(apply(doc, b), a2)
```

Neither input op is mutated. Rules:

### insert vs insert

Compare insertion positions.

- If `a.pos < b.pos`, increment `b2.pos` by 1 (a went first, to the left).
- If `a.pos > b.pos`, increment `a2.pos` by 1.
- If `a.pos == b.pos`, the **lower site id** keeps its position (goes
  left); the higher site id's position is incremented. Equal site ids: keep
  both positions (the two `ch` values are then inserted as `a.ch + b.ch`
  when `a` is applied first — `a2` stays, `b2.pos += 1` as a tie-break so
  the pair still converges).

### insert vs delete

Let `I` be the insert and `D` the delete.

- If `I.pos <= D.pos`, increment `D`'s position by 1.
- If `I.pos > D.pos`, decrement `I`'s position by 1.

(The insert that lands **on** the deleted index is treated as inserting
before the original character, so the delete shifts right.)

### delete vs delete

- If `a.pos < b.pos`, decrement `b2.pos` by 1.
- If `a.pos > b.pos`, decrement `a2.pos` by 1.
- If `a.pos == b.pos`, both become `("nop",)` (the character is already
  gone).

### nop

`transform(nop, x) == (nop, x)` and `transform(x, nop) == (x, nop)`.

Site ids on delete/insert are preserved on the rewritten op (except nop).
