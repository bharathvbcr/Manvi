import copy


class PatchError(Exception):
    pass


def _unescape(tok):
    return tok.replace("~1", "/").replace("~0", "~")


def _tokens(ptr):
    if ptr == "":
        return []
    if not isinstance(ptr, str) or not ptr.startswith("/"):
        raise PatchError("pointer")
    return [_unescape(t) for t in ptr.split("/")[1:]]


def _is_index(tok):
    if tok == "0":
        return True
    return bool(tok) and tok[0] != "0" and tok.isdigit()


def _get(doc, tokens):
    cur = doc
    for t in tokens:
        if isinstance(cur, dict):
            if t not in cur:
                raise PatchError("missing")
            cur = cur[t]
        elif isinstance(cur, list):
            if not _is_index(t):
                raise PatchError("index")
            i = int(t)
            if i < 0 or i >= len(cur):
                raise PatchError("range")
            cur = cur[i]
        else:
            raise PatchError("walk")
    return cur


def _parent(doc, tokens):
    if not tokens:
        raise PatchError("parent")
    return _get(doc, tokens[:-1]), tokens[-1]


def _add(doc, tokens, value):
    if not tokens:
        return value
    parent, last = _parent(doc, tokens)
    if isinstance(parent, dict):
        parent[last] = value
        return doc
    if isinstance(parent, list):
        if last == "-":
            i = len(parent)
        elif _is_index(last):
            i = int(last)
            if i < 0 or i > len(parent):
                raise PatchError("range")
        else:
            raise PatchError("index")
        parent.insert(i, value)
        return doc
    raise PatchError("parent type")


def _remove(doc, tokens):
    if not tokens:
        raise PatchError("remove root")
    parent, last = _parent(doc, tokens)
    if isinstance(parent, dict):
        if last not in parent:
            raise PatchError("missing")
        del parent[last]
        return doc
    if isinstance(parent, list):
        if not _is_index(last):
            raise PatchError("index")
        i = int(last)
        if i < 0 or i >= len(parent):
            raise PatchError("range")
        parent.pop(i)
        return doc
    raise PatchError("parent type")


def _replace(doc, tokens, value):
    if not tokens:
        return value
    parent, last = _parent(doc, tokens)
    if isinstance(parent, dict):
        if last not in parent:
            raise PatchError("missing")
        parent[last] = value
        return doc
    if isinstance(parent, list):
        if not _is_index(last):
            raise PatchError("index")
        i = int(last)
        if i < 0 or i >= len(parent):
            raise PatchError("range")
        parent[i] = value
        return doc
    raise PatchError("parent type")


def apply_patch(doc, ops):
    cur = copy.deepcopy(doc)
    if ops is None:
        raise PatchError("ops")
    for op in ops:
        if not isinstance(op, dict) or "op" not in op or "path" not in op:
            raise PatchError("op")
        kind = op["op"]
        tokens = _tokens(op["path"])
        if kind == "add":
            if "value" not in op:
                raise PatchError("value")
            cur = _add(cur, tokens, copy.deepcopy(op["value"]))
        elif kind == "remove":
            cur = _remove(cur, tokens)
        elif kind == "replace":
            if "value" not in op:
                raise PatchError("value")
            cur = _replace(cur, tokens, copy.deepcopy(op["value"]))
        elif kind == "test":
            if "value" not in op:
                raise PatchError("value")
            if _get(cur, tokens) != op["value"]:
                raise PatchError("test")
        elif kind == "copy":
            if "from" not in op:
                raise PatchError("from")
            val = copy.deepcopy(_get(cur, _tokens(op["from"])))
            cur = _add(cur, tokens, val)
        elif kind == "move":
            if "from" not in op:
                raise PatchError("from")
            src = op["from"]
            dst = op["path"]
            if src == dst:
                continue
            if dst.startswith(src + "/"):
                raise PatchError("descendant")
            val = copy.deepcopy(_get(cur, _tokens(src)))
            cur = _remove(cur, _tokens(src))
            cur = _add(cur, tokens, val)
        else:
            raise PatchError("unknown op")
    return cur
