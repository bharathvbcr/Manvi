def _parse(pattern):
    p = _P(pattern)
    tree = p.expr()
    if p.i != len(pattern):
        raise ValueError("trailing")
    return tree


class _P:
    def __init__(self, s):
        self.s = s
        self.i = 0

    def peek(self):
        return self.s[self.i] if self.i < len(self.s) else None

    def get(self):
        c = self.peek()
        if c is None:
            raise ValueError("eof")
        self.i += 1
        return c

    def expr(self):
        t = self.term()
        while self.peek() == "|":
            self.get()
            t = ("alt", t, self.term())
        return t

    def term(self):
        if self.peek() is None or self.peek() in "|)":
            return ("eps",)
        t = self.factor()
        while self.peek() is not None and self.peek() not in "|)":
            t = ("cat", t, self.factor())
        return t

    def factor(self):
        a = self.atom()
        q = self.peek()
        if q is not None and q in "*+?":
            self.get()
            if q == "*":
                a = ("star", a)
            elif q == "+":
                a = ("plus", a)
            else:
                a = ("opt", a)
            nxt = self.peek()
            if nxt is not None and nxt in "*+?":
                raise ValueError("double quant")
        return a

    def atom(self):
        c = self.peek()
        if c is None or c in "*+?|)":
            raise ValueError("atom")
        if c == ".":
            self.get()
            return ("any",)
        if c == "\\":
            self.get()
            n = self.peek()
            if n is None:
                raise ValueError("backslash")
            self.get()
            return ("lit", n)
        if c == "(":
            self.get()
            inner = self.expr()
            if self.get() != ")":
                raise ValueError("paren")
            return inner
        self.get()
        return ("lit", c)


def _ends(node, s, i, memo):
    key = (node, i)
    if key in memo:
        return memo[key]
    tag = node[0]
    out = set()
    if tag == "eps":
        out.add(i)
    elif tag == "lit":
        if i < len(s) and s[i] == node[1]:
            out.add(i + 1)
    elif tag == "any":
        if i < len(s) and s[i] != "\n":
            out.add(i + 1)
    elif tag == "cat":
        for j in _ends(node[1], s, i, memo):
            out |= _ends(node[2], s, j, memo)
    elif tag == "alt":
        out |= _ends(node[1], s, i, memo)
        out |= _ends(node[2], s, i, memo)
    elif tag == "star":
        out.add(i)
        frontier = {i}
        visited = set()
        while frontier:
            p = frontier.pop()
            if p in visited:
                continue
            visited.add(p)
            for q in _ends(node[1], s, p, memo):
                if q not in out:
                    out.add(q)
                    frontier.add(q)
    elif tag == "plus":
        for j in _ends(node[1], s, i, memo):
            out |= _ends(("star", node[1]), s, j, memo)
    elif tag == "opt":
        out.add(i)
        out |= _ends(node[1], s, i, memo)
    else:
        raise ValueError("tag")
    memo[key] = out
    return out


def fullmatch(pattern, text):
    if not isinstance(pattern, str) or not isinstance(text, str):
        raise TypeError
    tree = _parse(pattern)
    return len(text) in _ends(tree, text, 0, {})
