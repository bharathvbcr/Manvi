import copy


class PatchError(Exception):
    pass


def apply_patch(doc, ops):
    """Apply a JSON Patch. See SPEC.md."""
    if not isinstance(ops, list):
        raise PatchError("ops must be a list")
    current = copy.deepcopy(doc)
    for op in ops:
        if not isinstance(op, dict):
            raise PatchError("operation must be an object")
        current = _apply(current, op)
    return current


def _apply(doc, op):
    kind = op.get("op")
    if kind == "add":
        return _add(doc, _pointer(op, "path"), _required(op, "value"))
    if kind == "remove":
        _remove(doc, _pointer(op, "path"))
        return doc
    if kind == "replace":
        path = _pointer(op, "path")
        value = _required(op, "value")
        if path == "":
            return copy.deepcopy(value)
        _remove(doc, path)
        return _add(doc, path, value)
    if kind == "test":
        path = _pointer(op, "path")
        if _get(doc, _tokens(path)) != _required(op, "value"):
            raise PatchError("test failed")
        return doc
    if kind == "copy":
        src = _pointer(op, "from")
        dst = _pointer(op, "path")
        return _add(doc, dst, copy.deepcopy(_get(doc, _tokens(src))))
    if kind == "move":
        src = _pointer(op, "from")
        dst = _pointer(op, "path")
        if src == dst:
            return doc
        if dst.startswith(src + "/"):
            raise PatchError("cannot move a value into its own descendant")
        return _add(doc, dst, _remove(doc, src))
    raise PatchError("unknown op")


def _pointer(op, field):
    if field not in op or not isinstance(op[field], str):
        raise PatchError(f"missing {field}")
    return op[field]


def _required(op, field):
    if field not in op:
        raise PatchError(f"missing {field}")
    return op[field]


def _tokens(path):
    if path == "":
        return []
    if not path.startswith("/"):
        raise PatchError("pointer must be empty or start with /")
    return [raw.replace("~1", "/").replace("~0", "~") for raw in path.split("/")[1:]]


def _get(doc, tokens):
    current = doc
    for token in tokens:
        current = _child(current, token)
    return current


def _child(current, token):
    if isinstance(current, dict):
        if token not in current:
            raise PatchError("missing key")
        return current[token]
    if isinstance(current, list):
        return current[_index(token, len(current), for_add=False)]
    raise PatchError("cannot descend into a scalar")


def _index(token, length, *, for_add):
    if token == "-":
        if not for_add:
            raise PatchError("'-' is only valid when adding")
        return length
    if token == "" or not token.isdigit() or (len(token) > 1 and token[0] == "0"):
        raise PatchError("array index must be a canonical non-negative integer")
    index = int(token)
    if for_add:
        if index > length:
            raise PatchError("array index out of range")
    elif index >= length:
        raise PatchError("array index out of range")
    return index


def _add(doc, path, value):
    value = copy.deepcopy(value)
    tokens = _tokens(path)
    if not tokens:
        return value
    parent = _get(doc, tokens[:-1])
    last = tokens[-1]
    if isinstance(parent, dict):
        parent[last] = value
        return doc
    if isinstance(parent, list):
        parent.insert(_index(last, len(parent), for_add=True), value)
        return doc
    raise PatchError("cannot add under a scalar")


def _remove(doc, path):
    tokens = _tokens(path)
    if not tokens:
        raise PatchError("cannot remove the whole document")
    parent = _get(doc, tokens[:-1])
    last = tokens[-1]
    if isinstance(parent, dict):
        if last not in parent:
            raise PatchError("missing key")
        return parent.pop(last)
    if isinstance(parent, list):
        return parent.pop(_index(last, len(parent), for_add=False))
    raise PatchError("cannot remove under a scalar")
