def apply(doc, op):
    if not isinstance(doc, str):
        raise TypeError("doc")
    kind = op[0]
    if kind == "nop":
        return doc
    if kind == "ins":
        _, pos, ch, _site = op
        if not isinstance(ch, str) or len(ch) != 1:
            raise ValueError("ch")
        if pos < 0 or pos > len(doc):
            raise ValueError("pos")
        return doc[:pos] + ch + doc[pos:]
    if kind == "del":
        _, pos, _site = op
        if pos < 0 or pos >= len(doc):
            raise ValueError("pos")
        return doc[:pos] + doc[pos + 1:]
    raise ValueError("op")


def transform(a, b):
    if a[0] == "nop":
        return ("nop",), b
    if b[0] == "nop":
        return a, ("nop",)
    if a[0] == "ins" and b[0] == "ins":
        pa, pb = a[1], b[1]
        if pa < pb:
            return a, ("ins", pb + 1, b[2], b[3])
        if pa > pb:
            return ("ins", pa + 1, a[2], a[3]), b
        if a[3] < b[3]:
            return a, ("ins", pb + 1, b[2], b[3])
        if a[3] > b[3]:
            return ("ins", pa + 1, a[2], a[3]), b
        return a, ("ins", pb + 1, b[2], b[3])
    if a[0] == "ins" and b[0] == "del":
        if a[1] <= b[1]:
            return a, ("del", b[1] + 1, b[2])
        return ("ins", a[1] - 1, a[2], a[3]), b
    if a[0] == "del" and b[0] == "ins":
        if b[1] <= a[1]:
            return ("del", a[1] + 1, a[2]), b
        return a, ("ins", b[1] - 1, b[2], b[3])
    if a[0] == "del" and b[0] == "del":
        if a[1] < b[1]:
            return a, ("del", b[1] - 1, b[2])
        if a[1] > b[1]:
            return ("del", a[1] - 1, a[2]), b
        return ("nop",), ("nop",)
    raise ValueError("op")
