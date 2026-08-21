"""Pratt parser. See SPEC.md."""

PREFIX = {"+": 30, "-": 30}
INFIX = {
    "+": (10, 11),
    "-": (10, 11),
    "*": (20, 21),
    "/": (20, 21),
    "^": (40, 39),
}
POSTFIX = {"!": 50}
BIN_TAG = {"+": "+", "-": "-", "*": "*", "/": "/", "^": "pow"}


class Parser:
    def __init__(self, expr):
        self.toks = _lex(expr)
        self.i = 0

    def peek(self):
        return self.toks[self.i] if self.i < len(self.toks) else None

    def get(self):
        t = self.peek()
        if t is None:
            raise ValueError("eof")
        self.i += 1
        return t

    def parse(self, min_bp=0):
        t = self.get()
        if t[0] == "num":
            left = t[1]
        elif t[0] == "op" and t[1] in PREFIX:
            right = self.parse(PREFIX[t[1]])
            left = ("neg", right) if t[1] == "-" else right
        elif t[0] == "op" and t[1] == "(":
            left = self.parse(0)
            nxt = self.get()
            if nxt != ("op", ")"):
                raise ValueError("paren")
        else:
            raise ValueError("nud")
        while True:
            nxt = self.peek()
            if nxt is None:
                break
            if nxt[0] != "op":
                raise ValueError("junk")
            op = nxt[1]
            if op in POSTFIX and POSTFIX[op] >= min_bp:
                self.get()
                left = ("fact", left)
                continue
            if op in INFIX:
                lbp, rbp = INFIX[op]
                if lbp < min_bp:
                    break
                self.get()
                right = self.parse(rbp)
                left = (BIN_TAG[op], left, right)
                continue
            if op == ")":
                break
            raise ValueError("led")
        return left


def _lex(expr):
    if expr is None:
        raise ValueError("empty")
    s = expr
    i = 0
    out = []
    n = len(s)
    while i < n:
        c = s[i]
        if c in " \t\n\r":
            i += 1
            continue
        if c.isdigit():
            j = i
            while j < n and s[j].isdigit():
                j += 1
            out.append(("num", int(s[i:j])))
            i = j
            continue
        if c in "+-*/^!()":
            out.append(("op", c))
            i += 1
            continue
        raise ValueError("char")
    return out


def parse(expr):
    if not isinstance(expr, str) or expr.strip() == "":
        raise ValueError("empty")
    p = Parser(expr)
    tree = p.parse(0)
    if p.peek() is not None:
        raise ValueError("trailing")
    return tree
