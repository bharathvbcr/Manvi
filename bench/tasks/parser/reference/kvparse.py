def _unquote(value):
    inner = value[1:-1]
    out = []
    i = 0
    while i < len(inner):
        if inner[i] == "\\" and i + 1 < len(inner) and inner[i + 1] in '"\\':
            out.append(inner[i + 1])
            i += 2
        else:
            out.append(inner[i])
            i += 1
    return "".join(out)


def parse_kv(text):
    result = {}
    for line in text.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if "=" not in stripped:
            raise ValueError(f"line has no '=': {line!r}")
        key, _, value = stripped.partition("=")
        key = key.strip()
        value = value.strip()
        if not key:
            raise ValueError(f"empty key: {line!r}")
        if len(value) >= 2 and value.startswith('"') and value.endswith('"'):
            value = _unquote(value)
        result[key] = value
    return result
