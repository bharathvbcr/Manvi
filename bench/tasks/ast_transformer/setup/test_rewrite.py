import ast
import asyncio
from rewrite import rewrite

def _no_sugar(tree):
    for n in ast.walk(tree):
        if isinstance(n, (ast.AsyncFor, ast.AsyncWith)):
            return False
    return True

def main():
    src = '''
async def agen():
    yield 1
    yield 2

async def run():
    out = []
    async for x in agen():
        out.append(x)
    return out
'''
    out = rewrite(src)
    tree = ast.parse(out)
    assert _no_sugar(tree), "async for/with must be desugared"
    ns = {}
    exec(compile(tree, "<rewritten>", "exec"), ns)
    assert asyncio.run(ns["run"]()) == [1, 2]

    src2 = '''
class M:
    async def __aenter__(self):
        return 7
    async def __aexit__(self, *a):
        return False

async def run():
    async with M() as x:
        return x
'''
    out2 = rewrite(src2)
    tree2 = ast.parse(out2)
    assert _no_sugar(tree2)
    ns = {}
    exec(compile(tree2, "<rewritten>", "exec"), ns)
    assert asyncio.run(ns["run"]()) == 7
    print("ok")

main()
