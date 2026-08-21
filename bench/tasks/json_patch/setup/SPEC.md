# JSON Pointer + Patch

`apply_patch(doc, ops)` takes a JSON value (`dict` / `list` / `str` /
`int` / `float` / `bool` / `None`) and a list of patch operations, and
returns a **new** document. It must not mutate `doc` or any list/dict
inside it. Raise `PatchError` (subclass of `Exception`) on any failure.

Dict keys are strings. Lists are JSON arrays.

## Pointer (RFC 6901)

A pointer is a string.

- `""` refers to the whole document.
- Otherwise it must start with `/`. Split on `/`, drop the empty first
  segment, then unescape each token: `~1` -> `/`, then `~0` -> `~`
  (in that order).
- Walking: on a dict, the token is a key (missing key is an error except
  as noted for `add`). On a list, the token must be a non-negative
  integer without a leading zero (`0` is fine, `01` is an error), and
  in range. The token `"-"` is **not** a valid index for lookup, only
  for `add` (append).

## Operations

Each op is a `dict` with string keys.

- `{"op":"add","path":P,"value":V}`
  - If `P` is `""`, the result is `V` (replace the whole document).
  - Else the parent of `P` must exist. If the parent is a dict, set the
    last token as a key (overwrite if present). If the parent is a list,
    the last token is an index `0..len` or `"-"` meaning `len`; **insert**
    at that index (shift right). Index `len` is allowed (append). Out of
    range is an error.
- `{"op":"remove","path":P}`
  - `P` must not be `""`. The value must exist. Delete the key / pop the
    index.
- `{"op":"replace","path":P,"value":V}`
  - Equivalent to remove then add at the same path, including `P == ""`
    (whole-document replace). The target must exist unless `P == ""`.
- `{"op":"test","path":P,"value":V}`
  - The value at `P` must exist and be `== V`. On failure raise
    `PatchError`. The document is unchanged.
- `{"op":"copy","from":F,"path":P}`
  - Read the value at `F` (must exist), then `add` a **deep copy** of it
    at `P`.
- `{"op":"move","from":F,"path":P}`
  - If `P` is `F` or `P` starts with `F + "/"` (moving into a descendant),
    raise `PatchError` — except `F == P`, which is a no-op.
  - Otherwise remove at `F` then add that value at `P`.

Unknown `op` is a `PatchError`. Missing required fields too.

Ops apply in order; each sees the document produced by the previous.
