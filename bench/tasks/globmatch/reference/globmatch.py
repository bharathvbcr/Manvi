def _class_end(pattern, i):
    """Index just past the closing ] of the class starting at pattern[i] == '[',
    or None if the class is never closed."""
    j = i + 1
    if j < len(pattern) and pattern[j] == "!":
        j += 1
    if j < len(pattern) and pattern[j] == "]":
        j += 1
    while j < len(pattern) and pattern[j] != "]":
        j += 1
    return j + 1 if j < len(pattern) else None


def _class_matches(body, ch):
    negated = False
    if body.startswith("!"):
        negated = True
        body = body[1:]
    hit = False
    i = 0
    while i < len(body):
        if (i + 2 < len(body) and body[i + 1] == "-" and body[i + 2] != ""):
            lo, hi = body[i], body[i + 2]
            if lo <= ch <= hi:
                hit = True
            i += 3
        else:
            if body[i] == ch:
                hit = True
            i += 1
    return hit != negated


def matches(pattern, name):
    memo = {}

    def go(pi, ni):
        key = (pi, ni)
        if key in memo:
            return memo[key]
        if pi == len(pattern):
            res = ni == len(name)
        else:
            c = pattern[pi]
            if c == "*":
                res = go(pi + 1, ni) or (ni < len(name) and go(pi, ni + 1))
            elif c == "?":
                res = ni < len(name) and go(pi + 1, ni + 1)
            elif c == "[":
                end = _class_end(pattern, pi)
                if end is None:
                    res = ni < len(name) and name[ni] == "[" and go(pi + 1, ni + 1)
                else:
                    body = pattern[pi + 1:end - 1]
                    res = (ni < len(name) and _class_matches(body, name[ni])
                           and go(end, ni + 1))
            else:
                res = ni < len(name) and name[ni] == c and go(pi + 1, ni + 1)
        memo[key] = res
        return res

    return go(0, 0)
