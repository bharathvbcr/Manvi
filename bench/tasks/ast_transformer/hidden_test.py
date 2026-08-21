import ast
import asyncio
import os
import sys
from rewrite import rewrite

bad = 0


def eq(label, got, want):
    global bad
    if got != want:
        print("FAIL", label, "got", repr(got), "want", repr(want))
        bad += 1


def no_sugar(tree):
    for n in ast.walk(tree):
        if isinstance(n, (ast.AsyncFor, ast.AsyncWith)):
            return False
    return True


def banned_imports():
    path = os.path.join(os.environ.get("PYTHONPATH", "."), "rewrite.py")
    # sandbox cwd is the sandbox; PYTHONPATH is the sandbox
    if not os.path.isfile(path):
        path = "rewrite.py"
    src = open(path).read()
    tree = ast.parse(src)
    bad_mods = []
    for n in ast.walk(tree):
        if isinstance(n, ast.Import):
            for a in n.names:
                if a.name.split(".")[0] in ("re", "regex"):
                    bad_mods.append(a.name)
        if isinstance(n, ast.ImportFrom) and (n.module or "").split(".")[0] in ("re", "regex"):
            bad_mods.append(n.module)
    return bad_mods


class Tracer:
    def __init__(self, items, log, name="t"):
        self.items = list(items)
        self.log = log
        self.name = name
        self.i = 0

    def __aiter__(self):
        self.log.append(self.name + ".aiter")
        return self

    async def __anext__(self):
        self.log.append(self.name + ".anext")
        if self.i >= len(self.items):
            raise StopAsyncIteration
        v = self.items[self.i]
        self.i += 1
        if isinstance(v, BaseException):
            raise v
        return v

    async def aclose(self):
        self.log.append(self.name + ".aclose")


class CM:
    def __init__(self, log, name, value=1, suppress=False, enter_exc=None):
        self.log = log
        self.name = name
        self.value = value
        self.suppress = suppress
        self.enter_exc = enter_exc

    async def __aenter__(self):
        self.log.append(self.name + ".enter")
        if self.enter_exc is not None:
            raise self.enter_exc
        return self.value

    async def __aexit__(self, et, ev, tb):
        self.log.append((self.name + ".exit", None if et is None else et.__name__))
        return self.suppress


def run_src(src, extra=None):
    out = rewrite(src)
    tree = ast.parse(out)
    if not no_sugar(tree):
        raise AssertionError("AsyncFor/AsyncWith remain in:\n" + out)
    ns = {}
    if extra:
        ns.update(extra)
    exec(compile(tree, "<rewritten>", "exec"), ns)
    return ns, out


eq("no re/regex", banned_imports(), [])

# 1. simple async for
log = []
src = '''
async def run(t):
    out = []
    async for x in t:
        out.append(x)
    return out
'''
ns, _ = run_src(src)
got = asyncio.run(ns["run"](Tracer([1, 2], log)))
eq("simple values", got, [1, 2])
eq("simple log", log, ["t.aiter", "t.anext", "t.anext", "t.anext", "t.aclose"])

# 2. break still acloses, else does not run
log = []
src = '''
async def run(t):
    hit_else = False
    seen = []
    async for x in t:
        seen.append(x)
        break
    else:
        hit_else = True
    return seen, hit_else
'''
ns, _ = run_src(src)
got = asyncio.run(ns["run"](Tracer([1, 2, 3], log)))
eq("break values", got, ([1], False))
eq("break aclose", "t.aclose" in log, True)

# 3. else runs on exhaustion
log = []
src = '''
async def run(t):
    hit_else = False
    async for x in t:
        pass
    else:
        hit_else = True
    return hit_else
'''
ns, _ = run_src(src)
eq("else on exhaust", asyncio.run(ns["run"](Tracer([1], log))), True)
eq("else aclose", "t.aclose" in log, True)

# 4. continue
log = []
src = '''
async def run(t):
    out = []
    async for x in t:
        if x == 2:
            continue
        out.append(x)
    return out
'''
ns, _ = run_src(src)
eq("continue", asyncio.run(ns["run"](Tracer([1, 2, 3], log))), [1, 3])

# 5. unpack target
log = []
src = '''
async def run(t):
    out = []
    async for a, b in t:
        out.append(a + b)
    return out
'''
ns, _ = run_src(src)
eq("unpack", asyncio.run(ns["run"](Tracer([(1, 2), (3, 4)], log))), [3, 7])

# 6. aiter failure does not aclose
class Boom:
    def __aiter__(self):
        raise RuntimeError("nope")


src = '''
async def run(t):
    async for x in t:
        return x
'''
ns, _ = run_src(src)
try:
    asyncio.run(ns["run"](Boom()))
    eq("aiter boom", "no-raise", "RuntimeError")
except RuntimeError:
    pass

# 7. async with happy path
log = []
src = '''
async def run(m):
    async with m as v:
        return v
'''
ns, _ = run_src(src)
eq("with value", asyncio.run(ns["run"](CM(log, "m", value=9))), 9)
eq("with log", log, ["m.enter", ("m.exit", None)])

# 8. async with no as-binding
log = []
src = '''
async def run(m):
    async with m:
        return 4
'''
ns, _ = run_src(src)
eq("with no as", asyncio.run(ns["run"](CM(log, "m"))), 4)
eq("with no as exit", log[-1], ("m.exit", None))

# 9. exception, no suppress -> re-raise, aexit saw the type
log = []
src = '''
async def run(m):
    async with m:
        raise ValueError("x")
'''
ns, _ = run_src(src)
try:
    asyncio.run(ns["run"](CM(log, "m", suppress=False)))
    eq("with raise", "no-raise", "ValueError")
except ValueError:
    pass
eq("aexit saw ValueError", log, ["m.enter", ("m.exit", "ValueError")])

# 10. suppress
log = []
src = '''
async def run(m):
    async with m:
        raise ValueError("x")
    return "ok"
'''
ns, _ = run_src(src)
eq("suppress", asyncio.run(ns["run"](CM(log, "m", suppress=True))), "ok")

# 11. nested with: enter A, enter B, body, exit B, exit A
log = []
src = '''
async def run(a, b):
    async with a as x, b as y:
        return x + y
'''
ns, _ = run_src(src)
eq("nested values", asyncio.run(ns["run"](CM(log, "A", value=1), CM(log, "B", value=2))), 3)
eq("nested order", [e if isinstance(e, str) else e[0] for e in log],
   ["A.enter", "B.enter", "B.exit", "A.exit"])

# 12. inner with raises, outer still exits
log = []
src = '''
async def run(a, b):
    async with a:
        async with b:
            raise KeyError("k")
'''
ns, _ = run_src(src)
try:
    asyncio.run(ns["run"](CM(log, "A"), CM(log, "B")))
except KeyError:
    pass
eq("nested raise exits", [e if isinstance(e, str) else e[0] for e in log],
   ["A.enter", "B.enter", "B.exit", "A.exit"])

# 13. collision: source already uses likely temp names
log = []
src = '''
async def run(t, _it0, _ex0):
    out = []
    async for x in t:
        out.append(x + _it0 + _ex0)
    return out
'''
ns, _ = run_src(src)
eq("name collision", asyncio.run(ns["run"](Tracer([1], log), 10, 100)), [111])
eq("collision aclose", "t.aclose" in log, True)

# 14. for inside with
log = []
src = '''
async def run(m, t):
    out = []
    async with m:
        async for x in t:
            out.append(x)
    return out
'''
ns, _ = run_src(src)
eq("for in with", asyncio.run(ns["run"](CM(log, "m"), Tracer([5], log))), [5])
eq("for in with aclose", "t.aclose" in log, True)

sys.exit(1 if bad else 0)
