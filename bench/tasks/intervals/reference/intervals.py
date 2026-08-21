def _norm(intervals):
    items = sorted((s, e) for s, e in intervals if s < e)
    out = []
    for s, e in items:
        if out and s <= out[-1][1]:
            out[-1] = (out[-1][0], max(out[-1][1], e))
        else:
            out.append((s, e))
    return out


def merge(intervals):
    return _norm(intervals)


def intersect(a, b):
    A, B = _norm(a), _norm(b)
    out = []
    i = j = 0
    while i < len(A) and j < len(B):
        s = max(A[i][0], B[j][0])
        e = min(A[i][1], B[j][1])
        if s < e:
            out.append((s, e))
        if A[i][1] < B[j][1]:
            i += 1
        else:
            j += 1
    return _norm(out)


def subtract(a, b):
    A, B = _norm(a), _norm(b)
    out = []
    for s, e in A:
        cur = s
        for bs, be in B:
            if be <= cur or bs >= e:
                continue
            if bs > cur:
                out.append((cur, bs))
            cur = max(cur, be)
            if cur >= e:
                break
        if cur < e:
            out.append((cur, e))
    return _norm(out)
