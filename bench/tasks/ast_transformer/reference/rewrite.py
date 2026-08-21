"""Desugar async for / async with. See SPEC.md."""
import ast


def _collected_names(tree):
    names = set()
    for n in ast.walk(tree):
        if isinstance(n, ast.Name):
            names.add(n.id)
        elif isinstance(n, ast.arg):
            names.add(n.arg)
        elif isinstance(n, ast.alias):
            names.add(n.asname or n.name.split(".")[0])
        elif isinstance(n, ast.FunctionDef) or isinstance(n, ast.AsyncFunctionDef):
            names.add(n.name)
        elif isinstance(n, ast.ClassDef):
            names.add(n.name)
        elif isinstance(n, ast.ExceptHandler) and n.name:
            names.add(n.name)
    return names


class _Fresh:
    def __init__(self, used):
        self.used = set(used)
        self.i = 0

    def __call__(self, prefix):
        while True:
            name = "%s%d" % (prefix, self.i)
            self.i += 1
            if name not in self.used:
                self.used.add(name)
                return name


def _name(id, ctx):
    return ast.Name(id=id, ctx=ctx)


def _call(func, args):
    return ast.Call(func=func, args=args, keywords=[])


def _assign(target, value):
    if not isinstance(target, list):
        target = [target]
    return ast.Assign(targets=target, value=value)


def _const(v):
    return ast.Constant(v)


class Desugar(ast.NodeTransformer):
    def __init__(self, fresh):
        self.fresh = fresh

    def visit_AsyncFor(self, node):
        self.generic_visit(node)
        it = self.fresh("_it")
        ex = self.fresh("_ex")
        ac = self.fresh("_ac")
        assign_target = _assign(node.target, ast.Await(
            _call(_name("anext", ast.Load()), [_name(it, ast.Load())])))
        try_anext = ast.Try(
            body=[assign_target],
            handlers=[ast.ExceptHandler(
                type=_name("StopAsyncIteration", ast.Load()),
                name=None,
                body=[
                    _assign(_name(ex, ast.Store()), _const(True)),
                    ast.Continue(),
                ],
            )],
            orelse=[],
            finalbody=[],
        )
        loop = ast.While(
            test=ast.UnaryOp(op=ast.Not(), operand=_name(ex, ast.Load())),
            body=[try_anext] + list(node.body),
            orelse=[],
        )
        try_body = [
            _assign(_name(it, ast.Store()), _call(
                _name("aiter", ast.Load()), [node.iter])),
            _assign(_name(ex, ast.Store()), _const(False)),
            loop,
        ]
        if node.orelse:
            try_body.append(ast.If(
                test=_name(ex, ast.Load()),
                body=list(node.orelse),
                orelse=[]))
        finally_body = [
            ast.If(
                test=ast.Compare(
                    left=_name(it, ast.Load()),
                    ops=[ast.IsNot()],
                    comparators=[_const(None)]),
                body=[
                    _assign(_name(ac, ast.Store()), _call(
                        _name("getattr", ast.Load()),
                        [_name(it, ast.Load()), _const("aclose"), _const(None)])),
                    ast.If(
                        test=ast.Compare(
                            left=_name(ac, ast.Load()),
                            ops=[ast.IsNot()],
                            comparators=[_const(None)]),
                        body=[ast.Expr(value=ast.Await(
                            _call(_name(ac, ast.Load()), [])))],
                        orelse=[]),
                ],
                orelse=[]),
        ]
        return ast.Try(
            body=[_assign(_name(it, ast.Store()), _const(None))] + try_body,
            handlers=[],
            orelse=[],
            finalbody=finally_body,
        )

    def visit_AsyncWith(self, node):
        self.generic_visit(node)
        body = list(node.body)
        for item in reversed(node.items):
            body = self._one_with(item, body)
        return body

    def _one_with(self, item, body):
        mgr = self.fresh("_mg")
        ext = self.fresh("_ax")
        entered = self.fresh("_en")
        ok = self.fresh("_ok")
        type_mgr = _call(_name("type", ast.Load()), [_name(mgr, ast.Load())])
        aenter = ast.Attribute(value=type_mgr, attr="__aenter__", ctx=ast.Load())
        setup = [
            _assign(_name(mgr, ast.Store()), item.context_expr),
            _assign(_name(ext, ast.Store()),
                    ast.Attribute(value=_call(_name("type", ast.Load()),
                                              [_name(mgr, ast.Load())]),
                                  attr="__aexit__", ctx=ast.Load())),
            _assign(_name(entered, ast.Store()),
                    ast.Await(_call(aenter, [_name(mgr, ast.Load())]))),
        ]
        inner = list(body)
        if item.optional_vars is not None:
            inner = [_assign(item.optional_vars, _name(entered, ast.Load()))] + inner
        exc_info = ast.Starred(
            value=_call(ast.Attribute(value=_name("sys", ast.Load()),
                                      attr="exc_info", ctx=ast.Load()), []),
            ctx=ast.Load())
        handler_body = [
            _assign(_name(ok, ast.Store()), _const(False)),
            ast.If(
                test=ast.UnaryOp(
                    op=ast.Not(),
                    operand=ast.Await(_call(_name(ext, ast.Load()),
                                            [_name(mgr, ast.Load()), exc_info]))),
                body=[ast.Raise(exc=None, cause=None)],
                orelse=[]),
        ]
        try_stmt = ast.Try(
            body=inner,
            handlers=[ast.ExceptHandler(
                type=_name("BaseException", ast.Load()),
                name=None,
                body=handler_body)],
            orelse=[],
            finalbody=[
                ast.If(
                    test=_name(ok, ast.Load()),
                    body=[ast.Expr(value=ast.Await(_call(
                        _name(ext, ast.Load()),
                        [_name(mgr, ast.Load()), _const(None),
                         _const(None), _const(None)])))],
                    orelse=[]),
            ],
        )
        return setup + [_assign(_name(ok, ast.Store()), _const(True)), try_stmt]


def _has_sys_import(tree):
    for n in tree.body:
        if isinstance(n, ast.Import):
            for a in n.names:
                if a.name == "sys" or a.name.startswith("sys."):
                    return True
        if isinstance(n, ast.ImportFrom) and n.module == "sys":
            return True
    return False


def rewrite(source):
    tree = ast.parse(source)
    fresh = _Fresh(_collected_names(tree) | {"sys", "aiter", "anext",
                                             "StopAsyncIteration", "BaseException",
                                             "getattr", "type"})
    tree = Desugar(fresh).visit(tree)
    if not _has_sys_import(tree):
        tree.body.insert(0, ast.Import(names=[ast.alias(name="sys", asname=None)]))
    ast.fix_missing_locations(tree)
    return ast.unparse(tree)
