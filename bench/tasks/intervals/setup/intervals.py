def merge(intervals):
    """Merge a list of half-open [start, end) intervals. See SPEC.md."""
    out = []
    for s, e in intervals:
        if out and s < out[-1][1]:
            out[-1] = (out[-1][0], max(out[-1][1], e))
        else:
            out.append((s, e))
    return out


def intersect(a, b):
    """Intersection of two interval lists."""
    out = []
    for s1, e1 in a:
        for s2, e2 in b:
            s, e = max(s1, s2), min(e1, e2)
            if s <= e:
                out.append((s, e))
    return out


def subtract(a, b):
    """Everything in a that is not in b."""
    out = []
    for s, e in a:
        for bs, be in b:
            if bs > s:
                out.append((s, bs))
            s = be
        out.append((s, e))
    return out
