# Half-open interval algebra

An interval is a tuple `(start, end)` meaning the half-open range
`start <= x < end`. Numbers may be negative. All three functions:

- accept the two lists in **any order**, possibly overlapping, possibly
  containing empty intervals,
- treat an interval with `start >= end` as **empty**: it contributes nothing and
  never appears in output,
- return a list of tuples sorted ascending by `start`, with no two output
  intervals overlapping **or touching** — `(0, 5)` and `(5, 9)` must come back as
  the single interval `(0, 9)`,
- never mutate their arguments.

## The functions

- `merge(intervals)` — the union, normalised as above.
- `intersect(a, b)` — every point in both `a` and `b`, normalised.
- `subtract(a, b)` — every point in `a` that is not in `b`, normalised.

Given empty input, each returns `[]`.
