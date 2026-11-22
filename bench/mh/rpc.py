"""Out-of-process verdict: a CHECKER runs the hidden test, a WORKER runs the
candidate, and nothing the candidate does can decide pass/fail.

Why this exists
---------------
The pre-split verifier imported the candidate module *into* the same interpreter
that ran the hidden test and returned that interpreter's exit code as the
verdict. `import os; os._exit(0)` at import time, `raise SystemExit(0)`, or a
walk up `sys._getframe().f_back` to the launcher's `bad` counter therefore bought
a pass on every Python task -- the verdict channel was reachable from the code
being judged (AUDIT C1). The same interpreter also held the hidden test's source
in a live frame, so candidate code could copy it into the sandbox and read it
next turn (AUDIT C2).

The split closes both by construction:

  * The CHECKER (`python -I -B`, cwd = a fresh empty dir) runs the hidden test.
    It never executes candidate code. Its exit code is the verdict again, but now
    nothing the candidate controls runs in it.
  * The WORKER (`python -I -B`, cwd = the sandbox) imports and runs the candidate.
    It never holds the hidden test's source and cannot write the sandbox, so it
    has nothing to exfiltrate and nowhere to put it.

When the hidden test imports a sandbox module it gets a PROXY. Every operation on
that proxy -- attribute access, call, item access, iteration, len, bool,
context managers, async iteration -- is a typed message to the worker, which runs
the real thing and sends back a value (copied) or a handle (a proxy the other
way, so a test-supplied callback or Clock still works). Candidate code exiting,
killing, or forging inside the worker only kills the worker: the checker sees the
pipe close and fails the verification. A verdict can be *earned* by making the
checks pass; it cannot be *asserted*.

The link is asymmetric. The worker runs candidate code, so everything it sends
the checker is untrusted: `CheckerEndpoint` serves only CHECKER_OPS (call, getattr
and setattr on objects the hidden test exported as callbacks, coroutine driving,
release), refuses any other op by exiting the checker with a failing code, and
never exposes a private or dunder name. And a Proxy never lets candidate code
decide `==`/`!=`: a Proxy equals itself, and otherwise the checker compares a
plain copy of a builtin subclass's contents (a namedtuple, Counter, IntEnum)
that it asked the worker for, never sending the other operand. Any other
worker object is unequal to everything but itself, and `!=` against one raises
(it cannot be answered, so it is never answered "true").

What the candidate can undo, and what it cannot
-----------------------------------------------
Everything inside the worker process is the candidate's to rewrite: the
harness's classes, its globals, `sys.meta_path`, `sys.modules`, builtins. So no
check that decides a verdict is *computed* there:

  * The import ban is an interpreter audit hook (`sys.addaudithook`), installed
    by the worker before the candidate's first line runs. CPython has no way to
    remove an audit hook, and the hook is built from a source template whose
    ban list is a code constant, closing over nothing and reading no module
    global, so there is no cell, global or attribute for candidate code to
    flip. It refuses the `import` event of a banned name however the import is
    spelled (statement, `__import__`, importlib, `builtins.__dict__`), the
    reading of a banned module's source file, `ctypes` and the sub-interpreter
    modules (the ways round a hook), and `gc.get_objects` / `get_referrers` /
    `get_referents` and `sys._current_frames` (the ways to the harness's own
    objects). Four loader calls raise no `import` event at all
    (`_imp.create_builtin`, `init_frozen`, `get_frozen_object`,
    `find_frozen`), so the worker replaces them with stubs that ask the hook
    to do the load (event `mh.loader`) and refuse the banned names and the
    interpreter's own import machinery (a fresh `_imp` would be a pristine
    `create_builtin`). That does not make the ban airtight: the real functions
    sit in the hook's frame while a load runs, and a finalizer the garbage
    collector starts inside the load can read them. Every record of a task with
    a ban says so (`LOADER_RESIDUAL` in `unenforced`). The worker harness
    itself loads none of `re`, `queue`, `json`, `base64` or `importlib`, so there is
    no already-imported copy to reach through `sys.modules["__main__"]._g` or
    `json.decoder.re`; the `forbid` reply (sent before any candidate code has
    run, so it is the one worker statement that is trusted) lists anything that
    was loaded anyway, and the verification record names it as unenforced.
  * The static audit (`audit_sources`) runs in the checker, which reads the
    sandbox but executes nothing in it. It is a second, independent layer, not
    the enforcement: a spelling it cannot see still meets the hook.
  * Argument writeback (did the candidate mutate its list/dict/set arguments)
    cannot be made unforgeable in CPython: the only copy that was mutated lives
    in the worker, and any report of it is the worker's word. The checker keeps
    the encoding it sent, demands a report for every container argument,
    compares the report against what it sent, and fails the run when the report
    is withheld. A worker that reports a pristine copy of arguments it mutated
    is not detected, so every run that relied on a writeback report names
    `writeback` in the record's `unenforced` list, and the record's
    `arg_mutations` is the checker's own count.

The wire is a restricted, typed JSON encoding. There is no pickle, marshal or
eval anywhere on it. A malformed message, an oversize message, EOF, or a blown
deadline is fatal to the checker and scores fail -- never pass. The worker
reads and writes it with `wire_dumps` / `wire_loads` below, which import
nothing, so the worker never loads `json` (and with it `re`); the checker uses
the stdlib's `json`, which it can afford because it runs no candidate code.

This module is shipped to both children as source on their stdin (so nothing has
to be read from the protected benchmark tree, which neither child may read), then
executed; `_entry` dispatches on the role in argv. `bench.Task._verify` is the
only caller.
"""
import binascii
import collections
import os
import socket
import struct
import sys
import threading
import time
import types

PROTO = 3
MAX_FRAME = 256 * 1024 * 1024      # a bigger message is treated as an attack
MAX_DEPTH = 200                    # encode/decode recursion ceiling
MAX_LANES = 512                    # concurrent causal chains; more is fatal
MUT_WB_MAX = 4096                  # writeback only containers at most this long
FATAL_EXIT = 70                    # checker exit code for any transport failure
SEP = b"\n#==MH-RPC-SRC-END==#\n"  # separates shipped source from the payload

# Types transported by value; everything else crosses as a handle.
_BYVAL = (type(None), bool, int, float, str, bytes, bytearray,
          list, tuple, dict, set, frozenset)

# Container arguments whose post-call state the worker must attest (and the
# checker must receive): exact builtin types only, because a subclass crosses
# as a handle, never as a copy.
_WB_TYPES = (list, dict, set, bytearray)


def _wb_wants(a):
    return type(a) in _WB_TYPES and len(a) <= MUT_WB_MAX


def _wb_holds(x, depth=0):
    """True when x is, or holds within a few tuple/frozenset levels, a mutable container."""
    if type(x) in _WB_TYPES:
        return True
    if type(x) in (tuple, frozenset) and depth < 4:
        return any(_wb_holds(e, depth + 1) for e in x)
    return False


def _jeq(a, b, depth=0):
    """Structural equality of two wire trees (exact types; sets as multisets)."""
    ta = type(a)
    if ta is not type(b) or depth > MAX_DEPTH:
        return False
    if ta is list:
        return len(a) == len(b) and all(_jeq(x, y, depth + 1) for x, y in zip(a, b))
    if ta is dict:
        if a.get("$") in ("s", "fs") and a.get("$") == b.get("$"):
            xs, ys = a.get("v"), b.get("v")
            return (type(xs) is list and type(ys) is list
                    and sorted(map(repr, xs)) == sorted(map(repr, ys)))
        return a.keys() == b.keys() and all(_jeq(a[k], b[k], depth + 1) for k in a)
    return a == b


# --------------------------------------------------------------------------- #
# Wire JSON for the worker.  The worker must not import `json` (it imports `re`)
# or `base64` (also `re`): a banned module the harness itself loaded is a module
# candidate code can reach by attribute.  This is the whole of JSON the typed
# encoding needs; the checker, which runs no candidate code, uses the stdlib's.
# --------------------------------------------------------------------------- #

_ESC = {0x22: '\\"', 0x5c: "\\\\", 0x0a: "\\n", 0x0d: "\\r", 0x09: "\\t",
        0x08: "\\b", 0x0c: "\\f"}
_UNESC = {'"': '"', "\\": "\\", "/": "/", "n": "\n", "r": "\r", "t": "\t",
          "b": "\b", "f": "\f"}
_HEX = frozenset("0123456789abcdefABCDEF")
_WS = " \t\n\r"
_PARSE_DEPTH = 900


def _quote(s):
    if s.isascii() and s.isprintable() and '"' not in s and "\\" not in s:
        return '"' + s + '"'
    out = ['"']
    for ch in s:
        c = ord(ch)
        e = _ESC.get(c)
        if e is not None:
            out.append(e)
        elif 0x20 <= c < 0x7f:
            out.append(ch)
        elif c < 0x10000:
            out.append("\\u%04x" % c)
        else:
            c -= 0x10000
            out.append("\\u%04x\\u%04x" % (0xd800 | (c >> 10), 0xdc00 | (c & 0x3ff)))
    out.append('"')
    return "".join(out)


def _emit(o, out, depth):
    if depth > _PARSE_DEPTH:
        raise ValueError("nesting too deep")
    t = type(o)
    if o is None:
        out.append("null")
    elif t is bool:
        out.append("true" if o else "false")
    elif t is int:
        out.append(int.__repr__(o))
    elif t is float:
        if o != o or o in (float("inf"), float("-inf")):
            raise ValueError("non-finite float on the wire")
        out.append(float.__repr__(o))
    elif t is str:
        out.append(_quote(o))
    elif t is list:
        out.append("[")
        for i, x in enumerate(o):
            if i:
                out.append(",")
            _emit(x, out, depth + 1)
        out.append("]")
    elif t is dict:
        out.append("{")
        for i, (k, v) in enumerate(o.items()):
            if i:
                out.append(",")
            if type(k) is not str:
                raise ValueError("object key is not a string")
            out.append(_quote(k))
            out.append(":")
            _emit(v, out, depth + 1)
        out.append("}")
    else:
        raise ValueError(f"not JSON: {t.__name__}")


def wire_dumps(obj):
    """JSON text of a tree of None/bool/int/float/str/list/dict, as ASCII bytes."""
    out = []
    _emit(obj, out, 0)
    return "".join(out).encode("ascii")


def _skip(s, i):
    n = len(s)
    while i < n and s[i] in _WS:
        i += 1
    return i


def _read_str(s, i):
    out = []
    while True:
        q = s.find('"', i)
        if q < 0:
            raise ValueError("unterminated string")
        b = s.find("\\", i, q)
        if b < 0:
            out.append(s[i:q])
            return "".join(out), q + 1
        out.append(s[i:b])
        e = s[b + 1:b + 2]
        if e == "u":
            h = s[b + 2:b + 6]
            if len(h) != 4 or not _HEX.issuperset(h):
                raise ValueError("bad \\u escape")
            cp = int(h, 16)
            i = b + 6
            if 0xd800 <= cp < 0xdc00 and s.startswith("\\u", i):
                h2 = s[i + 2:i + 6]
                if len(h2) == 4 and _HEX.issuperset(h2) and 0xdc00 <= int(h2, 16) < 0xe000:
                    cp = 0x10000 + ((cp - 0xd800) << 10) + (int(h2, 16) - 0xdc00)
                    i += 6
            out.append(chr(cp))
        else:
            m = _UNESC.get(e)
            if m is None:
                raise ValueError("bad escape")
            out.append(m)
            i = b + 2
        # the closing quote search restarts from i, so an escaped quote is skipped


def _read_value(s, i, depth):
    if depth > _PARSE_DEPTH:
        raise ValueError("nesting too deep")
    if i >= len(s):
        raise ValueError("unexpected end of input")
    c = s[i]
    if c == '"':
        return _read_str(s, i + 1)
    if c == "{":
        obj = {}
        i = _skip(s, i + 1)
        if s[i:i + 1] == "}":
            return obj, i + 1
        while True:
            if s[i:i + 1] != '"':
                raise ValueError("object key must be a string")
            k, i = _read_str(s, i + 1)
            i = _skip(s, i)
            if s[i:i + 1] != ":":
                raise ValueError("expected ':'")
            v, i = _read_value(s, _skip(s, i + 1), depth + 1)
            obj[k] = v
            i = _skip(s, i)
            c = s[i:i + 1]
            if c == ",":
                i = _skip(s, i + 1)
            elif c == "}":
                return obj, i + 1
            else:
                raise ValueError("expected ',' or '}'")
    if c == "[":
        arr = []
        i = _skip(s, i + 1)
        if s[i:i + 1] == "]":
            return arr, i + 1
        while True:
            v, i = _read_value(s, i, depth + 1)
            arr.append(v)
            i = _skip(s, i)
            c = s[i:i + 1]
            if c == ",":
                i = _skip(s, i + 1)
            elif c == "]":
                return arr, i + 1
            else:
                raise ValueError("expected ',' or ']'")
    for lit, val in (("true", True), ("false", False), ("null", None)):
        if s.startswith(lit, i):
            return val, i + len(lit)
    j = i
    while j < len(s) and s[j] in "+-.eE0123456789":
        j += 1
    tok = s[i:j]
    if not tok:
        raise ValueError("not JSON")
    if "." in tok or "e" in tok or "E" in tok:
        return float(tok), j
    return int(tok), j


def wire_loads(data):
    """The tree wire_dumps (or the stdlib's json.dumps) wrote. ValueError otherwise."""
    s = data.decode("utf-8") if isinstance(data, (bytes, bytearray)) else data
    val, i = _read_value(s, _skip(s, 0), 0)
    if _skip(s, i) != len(s):
        raise ValueError("trailing data after the JSON value")
    return val


# Set by _entry to the endpoint's _fatal once a real checker or worker process is up.
# In-process uses of the codec (tests) leave it empty and just see the exception.
_EXIT_HOOK = []


class _Fatal(Exception):
    """A transport failure. Raising it ends the process (see _EXIT_HOOK), so no
    except clause in candidate or test code can swallow it: construction is the exit."""

    def __init__(self, *args):
        super().__init__(*args)
        if _EXIT_HOOK:
            _EXIT_HOOK[0](str(self))


class RemoteFatal(Exception):
    """Surfaced in the checker when the worker died or raised BaseException.

    Deliberately an ``Exception``, not the remote's real ``SystemExit`` /
    ``KeyboardInterrupt`` / ``GeneratorExit``: re-raising those in the checker
    would let candidate code end the checker with a chosen exit code, which is
    exactly the hole the split closes. A test's own ``except Exception`` may
    catch this and count it as a failed check -- which it is.
    """


# --------------------------------------------------------------------------- #
# Typed JSON codec.  Objects: {"$": tag, ...}.  Bare arrays are lists; bare
# scalars are themselves.  Handles and classes carry the id of the side that
# owns the real object, so a handle sent back to its owner decodes to the
# original rather than to a proxy-of-a-proxy.
# --------------------------------------------------------------------------- #

class _Codec:
    def __init__(self, ep):
        self.ep = ep

    def encode(self, obj, depth=0):
        if depth > MAX_DEPTH:
            raise _Fatal("encode too deep")
        t = type(obj)
        if obj is None or t is bool or t is int or t is str:
            return obj
        if t is float:
            if obj != obj:
                return {"$": "fl", "v": "nan"}
            if obj == float("inf"):
                return {"$": "fl", "v": "inf"}
            if obj == float("-inf"):
                return {"$": "fl", "v": "-inf"}
            return obj
        if t is bytes:
            return {"$": "by", "b": binascii.b2a_base64(obj, newline=False).decode("ascii")}
        if t is bytearray:
            return {"$": "ba", "b": binascii.b2a_base64(bytes(obj), newline=False).decode("ascii")}
        if t is list:
            return [self.encode(x, depth + 1) for x in obj]
        if t is tuple:
            return {"$": "t", "v": [self.encode(x, depth + 1) for x in obj]}
        if t is set:
            return {"$": "s", "v": [self.encode(x, depth + 1) for x in obj]}
        if t is frozenset:
            return {"$": "fs", "v": [self.encode(x, depth + 1) for x in obj]}
        if t is dict:
            return {"$": "d", "v": [[self.encode(k, depth + 1),
                                     self.encode(v, depth + 1)]
                                    for k, v in obj.items()]}
        if t is Proxy and obj._ep is self.ep:
            # A proxy of the peer's object goes back as the peer's own handle, so
            # it decodes there to the original rather than to a proxy-of-a-proxy.
            return {"$": "h", "o": "w" if self.ep.side == "c" else "c", "id": obj._rid}
        if isinstance(obj, types.CoroutineType):
            # A checker coroutine (e.g. an async method called through a plain
            # proxy call, like the desugar's `aclose()`): crosses as an
            # awaitable the worker drives back over the pipe when it awaits.
            return {"$": "co", "o": self.ep.side, "id": self.ep.remember(obj)}
        if isinstance(obj, type) and issubclass(obj, BaseException):
            # Exception classes are rebuilt on the far side so `except RealName`
            # and the raised-exception's class are the same object. Every other
            # class crosses as a handle, so calling it constructs in the worker
            # rather than building an empty look-alike here.
            return self.encode_class(obj)
        # Anything else -- an instance, a non-exception class, a function, a
        # module, a method, an iterator -- stays on this side and crosses as a
        # handle the peer drives through this endpoint.
        return self.ep.make_handle(obj)

    def encode_class(self, cls):
        mro = [[getattr(c, "__module__", "builtins"),
                getattr(c, "__qualname__", getattr(c, "__name__", "?"))]
               for c in cls.__mro__ if c is not object]
        return {"$": "c", "o": self.ep.side, "id": self.ep.remember(cls),
                "mro": mro, "name": getattr(cls, "__qualname__", cls.__name__)}

    @staticmethod
    def _b64(text):
        try:
            return binascii.a2b_base64(text, strict_mode=True)
        except (binascii.Error, ValueError, TypeError) as e:
            raise _Fatal(f"malformed base64 on the wire: {e}") from None

    def decode(self, j, depth=0):
        if depth > MAX_DEPTH:
            raise _Fatal("decode too deep")
        t = type(j)
        if j is None or t is bool or t is int or t is str or t is float:
            return j
        if t is list:
            return [self.decode(x, depth + 1) for x in j]
        if t is not dict or "$" not in j:
            raise _Fatal(f"malformed wire node: {t}")
        tag = j["$"]
        if tag == "fl":
            return float(j["v"])
        if tag == "by":
            return self._b64(j["b"])
        if tag == "ba":
            return bytearray(self._b64(j["b"]))
        if tag == "t":
            return tuple(self.decode(x, depth + 1) for x in j["v"])
        if tag == "s":
            return {self.decode(x, depth + 1) for x in j["v"]}
        if tag == "fs":
            return frozenset(self.decode(x, depth + 1) for x in j["v"])
        if tag == "d":
            return {self.decode(k, depth + 1): self.decode(v, depth + 1)
                    for k, v in j["v"]}
        if tag == "co":
            if j["o"] == self.ep.side:
                return self.ep._objs[j["id"]]
            return _AwaitableProxy(self.ep, j["id"])
        if tag == "h":
            return self.ep.resolve_handle(j["o"], j["id"])
        if tag == "c":
            return self.ep.resolve_class(j)
        raise _Fatal(f"unknown wire tag {tag!r}")


# --------------------------------------------------------------------------- #
# Proxies.  One proxy stands for a remote object.  Every dunder the hidden
# tests exercise is defined explicitly, because Python resolves dunders on the
# type, not through __getattr__.
# --------------------------------------------------------------------------- #

def _opaque(x, depth=0):
    """True when x is, or holds, a Proxy: its data is not all in hand."""
    if isinstance(x, Proxy):
        return True
    if depth > 50:
        return True
    t = type(x)
    if t in (list, tuple, set, frozenset):
        return any(_opaque(e, depth + 1) for e in x)
    if t is dict or t is collections.OrderedDict:
        return any(_opaque(k, depth + 1) or _opaque(v, depth + 1) for k, v in x.items())
    return False


class Proxy:
    __slots__ = ("_ep", "_rid", "__weakref__")

    def __init__(self, ep, rid):
        object.__setattr__(self, "_ep", ep)
        object.__setattr__(self, "_rid", rid)

    def __getattr__(self, name):
        if name in ("_ep", "_rid"):
            raise AttributeError(name)
        return self._ep.request("getattr", {"id": self._rid, "attr": name})

    def __setattr__(self, name, value):
        self._ep.request("setattr", {"id": self._rid, "attr": name,
                                      "val": self._ep.codec.encode(value)})

    def __call__(self, *args, **kwargs):
        return self._ep.request_call(self._rid, args, kwargs)

    def __getitem__(self, key):
        return self._ep.request("getitem", {"id": self._rid,
                                             "key": self._ep.codec.encode(key)})

    def __setitem__(self, key, value):
        self._ep.request("setitem", {"id": self._rid,
                                     "key": self._ep.codec.encode(key),
                                     "val": self._ep.codec.encode(value)})

    def __len__(self):
        return self._ep.request("len", {"id": self._rid})

    def __iter__(self):
        return self._ep.request("iter", {"id": self._rid})

    def __next__(self):
        return self._ep.request("next", {"id": self._rid})

    def __bool__(self):
        # The worker holds proxies of the checker's callbacks, and the checker
        # serves no `bool`/`hash`/`str`/`repr`: those are answered here, so an
        # honest `clock or time.monotonic` or `{cb}` still works.
        if self._ep.side == "w":
            return True
        return self._ep.request("bool", {"id": self._rid})

    def _data(self):
        """The object's builtin contents as a 1-tuple, or None when it has none.

        The worker is asked for DATA, never for a verdict, and its answer is
        taken as data only when it has exactly the shape of one: `[True, copy]`
        with a by-value builtin, or `[True, pairs, "od"]` for an OrderedDict,
        whose order is part of its equality.
        """
        got = self._ep.request("value", {"id": self._rid})
        if type(got) is not list or len(got) not in (2, 3) or got[0] is not True:
            return None
        data = got[1]
        if len(got) == 3:
            if type(got[2]) is not str or got[2] != "od" or type(data) is not list:
                return None
            if any(type(p) is not list or len(p) != 2 for p in data):
                return None
            try:
                return (collections.OrderedDict(data),)
            except TypeError:
                return None
        if type(data) not in _BYVAL:
            return None
        return (data,)

    def _decide(self, other):
        """True: equal. False: unequal, decided from data. None: cannot be decided.

        Candidate code never decides a comparison. A Proxy equals itself
        (proxies are one per remote id, so `is` is identity of the real
        object). Otherwise the checker asks the worker for the object's DATA and
        compares that copy itself; the other operand is never sent. An object
        with no builtin data (anything but a builtin or a subclass of one) has
        nothing to compare: it is unequal to everything but itself, and `!=`
        against it is undecidable (None), not "true". Two returned objects
        compared with each other are compared by data the worker supplied for
        both: the same trust as two values it returned by value, i.e. the
        candidate's answer, never a verdict.
        """
        if other is self:
            return True
        if self._ep.side == "w":
            return False
        mine = self._data()
        if mine is None:
            return None
        if isinstance(other, Proxy):
            theirs = other._data()
            if theirs is None:
                return None
            other = theirs[0]
        equal = (mine[0] == other) is True
        if not equal and (_opaque(mine[0]) or _opaque(other)):
            return None
        return equal

    def __eq__(self, other):
        return self._decide(other) is True

    def __ne__(self, other):
        decided = self._decide(other)
        if decided is None and self._ep.side == "c":
            # `got != want` used as a negative check ("must differ") would pass
            # for any impostor object if this answered True. It cannot be
            # answered, so the hidden test fails at this line.
            raise RemoteFatal("!= on a worker object with no builtin data cannot "
                              "be decided; compare its type and data explicitly")
        return not decided

    def __hash__(self):
        if self._ep.side == "w":
            return hash(("mh-checker-object", self._rid))
        return self._ep.request("hash", {"id": self._rid})

    def __str__(self):
        if self._ep.side == "w":
            return f"<checker object {self._rid}>"
        return self._ep.request("str", {"id": self._rid})

    def __repr__(self):
        if self._ep.side == "w":
            return f"<checker object {self._rid}>"
        return self._ep.request("repr", {"id": self._rid})

    def __aiter__(self):
        return self._ep.request("aiter", {"id": self._rid})

    async def __anext__(self):
        return self._ep.request("anext", {"id": self._rid})

    async def __aenter__(self):
        return self._ep.request("aenter", {"id": self._rid})

    async def __aexit__(self, et, ev, tb):
        return self._ep.request("aexit", {"id": self._rid,
                                          "et": self._ep.codec.encode(et),
                                          "ev": self._ep.codec.encode(ev),
                                          "tb": self._ep.codec.encode(tb)})

    def __del__(self):
        try:
            self._ep.release(self._rid)
        except Exception:
            pass


class _AwaitableProxy:
    """A worker-side stand-in for a checker coroutine.

    Awaiting it drives the remote coroutine one step (the test's async methods
    complete in a single step) and yields its result. It never runs candidate
    code; the coroutine lives and runs in the checker.
    """
    __slots__ = ("_ep", "_rid")

    def __init__(self, ep, rid):
        self._ep = ep
        self._rid = rid

    def __await__(self):
        result = self._ep.request("codrive", {"id": self._rid})
        return result
        yield  # unreachable; makes __await__ a generator so `await` accepts it


class _Empty(Exception):
    """An inbox get that timed out."""


class _Inbox:
    """A FIFO with a timed get. Not `queue.Queue`: the worker harness must not
    load `queue` (a task bans it, and a loaded module is a reachable one)."""
    __slots__ = ("_q", "_cv")

    def __init__(self):
        self._q = collections.deque()
        self._cv = threading.Condition(threading.Lock())

    def put(self, item):
        with self._cv:
            self._q.append(item)
            self._cv.notify()

    def get(self, timeout):
        with self._cv:
            if not self._q:
                self._cv.wait(timeout)
                if not self._q:
                    raise _Empty
            return self._q.popleft()


class _Lane:
    __slots__ = ("inbox", "started")

    def __init__(self):
        self.inbox = _Inbox()
        self.started = False


# What the CHECKER will do when the worker asks. The worker runs candidate code,
# so everything it sends is untrusted: it may call, read and write the objects
# the hidden test exported as callbacks (a Clock, a Tracer), drive the test's
# coroutines, and release handles -- nothing else. Any other op (exec_source,
# modattr, forbid, find, getitem, len, ...) is a worker-side operation; a worker
# that sends one to the checker is trying to make the checker run its code, and
# the verification fails on the spot.
CHECKER_OPS = frozenset({"call", "getattr", "setattr", "codrive", "aiter",
                         "anext", "aenter", "aexit", "release"})

# What the WORKER will do when the checker asks: everything the proxies can say,
# plus the worker-only ops that import and execute candidate code.
WORKER_OPS = CHECKER_OPS | frozenset({
    "getitem", "setitem", "len", "bool", "hash", "str", "repr", "iter",
    "next", "value", "find", "modattr", "forbid", "exec_source", "arun"})


_FULL = slice(None)


def _builtin_data(obj):
    """The builtin contents of an instance of a builtin subclass, as an exact
    builtin, or None when obj is not one.

    The base is found from type(obj) (an instance cannot fake its type the way
    it can fake `__class__` for isinstance), and the copy is read through the
    base type's own unbound methods, so an override of `__iter__`, `__eq__`,
    `items`, `__index__`, `__str__` or `__buffer__` on the subclass is never
    called.
    """
    t = type(obj)
    if t in _BYVAL:
        return None
    if issubclass(t, int):
        return int.__index__(obj)
    if issubclass(t, float):
        return float.__float__(obj)
    if issubclass(t, str):
        return str.__str__(obj)
    if issubclass(t, bytes):
        # Not bytes(memoryview(obj)): on 3.12+ memoryview() calls a subclass's
        # __buffer__, so the subclass would choose the bytes that get compared.
        return bytes.__getitem__(obj, _FULL)
    if issubclass(t, bytearray):
        return bytearray.copy(obj)
    if issubclass(t, tuple):
        return tuple.__getitem__(obj, _FULL)
    if issubclass(t, list):
        return list.copy(obj)
    if issubclass(t, collections.OrderedDict):
        # Its order is part of its equality, and dict.items() is the underlying
        # dict's order, which move_to_end / popitem(last=False) do not touch.
        return collections.OrderedDict(collections.OrderedDict.items(obj))
    if issubclass(t, dict):
        return dict(dict.items(obj))
    if issubclass(t, frozenset):
        return frozenset(frozenset.__iter__(obj))
    if issubclass(t, set):
        return set(set.__iter__(obj))
    return None


class Endpoint:
    """One half of the checker/worker link: framing, lanes, codec, dispatch.

    Not instantiated directly. `CheckerEndpoint` and `WorkerEndpoint` fix the
    side and the set of operations the peer may ask this side to perform; the
    link is deliberately asymmetric.
    """

    _OPS = frozenset()
    # The worker's wire codec imports nothing; CheckerEndpoint swaps in the stdlib's.
    _dumps = staticmethod(wire_dumps)
    _loads = staticmethod(wire_loads)

    def __init__(self, side, sock, sandbox, deadline_s, server_side):
        self.side = side                 # "c" or "w"
        self.sock = sock
        self.sandbox = sandbox
        self.server_side = server_side   # spawn a thread per inbound lane
        self.codec = _Codec(self)
        self._deadline = time.monotonic() + deadline_s if deadline_s else None
        self._lock = threading.Lock()
        self._local = threading.local()  # carries the lane being served
        self._objs = {}                  # id -> local object we handed out
        self._obj_ids = {}               # id(object) -> our handle id
        self._next_oid = 1
        self._proxies = {}               # remote id -> Proxy (stable identity)
        self._classes = {}              # remote id -> reconstructed class
        self._exc_by_name = {}           # (module, qualname) -> class
        self._pending = {}
        self._next_rid = 1
        self._lanes = {}
        self._wlock = threading.Lock()
        self._closing = False
        self._done = False
        self.wb_attested = 0             # container args whose post-state the peer reported
        self.wb_oversized = 0            # container args too long to be attested at all
        self.wb_kw = 0                   # container keyword arguments: never attested
        self.wb_nested = 0               # containers inside a tuple/frozenset argument: never attested
        self.wb_raised = 0               # container args of a call that raised: no report comes back
        self.mutations = 0               # attested args whose post-state differs from what we sent

    # ---- object tables ---------------------------------------------------- #

    def remember(self, obj):
        key = id(obj)
        with self._lock:
            hid = self._obj_ids.get(key)
            if hid is None:
                hid = self._next_oid
                self._next_oid += 1
                self._objs[hid] = obj
                self._obj_ids[key] = hid
            return hid

    def make_handle(self, obj):
        return {"$": "h", "o": self.side, "id": self.remember(obj)}

    def resolve_handle(self, owner, rid):
        if owner == self.side:
            return self._objs[rid]
        p = self._proxies.get(rid)
        if p is None:
            p = Proxy(self, rid)
            self._proxies[rid] = p
        return p

    def resolve_class(self, j):
        if j["o"] == self.side:
            return self._objs[j["id"]]
        cached = self._classes.get(j["id"])
        if cached is not None:
            return cached
        cls = self._build_class(j["mro"], j["name"])
        self._classes[j["id"]] = cls
        return cls

    def _build_class(self, mro, name):
        """Reconstruct a class from its MRO so `except RealName` still matches.

        Builtins (and their exception hierarchy) map to the real builtin, so
        `except ValueError` works. A sandbox class is rebuilt once, keyed by
        (module, qualname), with its reconstructed bases -- so a candidate's
        `CRCError(ProtocolError)` is caught by `except ProtocolError`.
        """
        import builtins
        if not mro:
            return Exception
        module, qual = mro[0]
        if module == "builtins":
            got = getattr(builtins, qual, None)
            if isinstance(got, type):
                return got
        key = (module, qual)
        have = self._exc_by_name.get(key)
        if have is not None:
            return have
        bases = tuple(self._build_class(mro[i:], mro[i][1])
                      for i in range(1, len(mro))) or (Exception,)
        # Collapse to a single concrete base to avoid MRO conflicts; the first
        # reconstructable ancestor is enough for isinstance/except.
        base = bases[0] if isinstance(bases[0], type) else Exception
        cls = type(qual, (base,), {"__module__": module})
        self._exc_by_name[key] = cls
        return cls

    def register_exc_class(self, cls):
        """Make a proxy-imported class the one the error path will raise."""
        key = (getattr(cls, "__module__", "?"),
               getattr(cls, "__qualname__", getattr(cls, "__name__", "?")))
        self._exc_by_name.setdefault(key, cls)

    def release(self, rid):
        if self._done:
            return
        self.request("release", {"id": rid}, oneway=True)

    # ---- framing ---------------------------------------------------------- #

    def _send(self, msg):
        data = self._dumps(msg)
        if len(data) > MAX_FRAME:
            self._fatal("outgoing frame too large")
        with self._wlock:
            try:
                self.sock.sendall(struct.pack(">I", len(data)) + data)
            except OSError:
                self._fatal("send failed")

    def _recv_exact(self, n):
        buf = bytearray()
        while len(buf) < n:
            try:
                chunk = self.sock.recv(n - len(buf))
            except OSError:
                return None
            if not chunk:
                return None
            buf += chunk
        return bytes(buf)

    def _read_frame(self):
        hdr = self._recv_exact(4)
        if hdr is None:
            return None
        (n,) = struct.unpack(">I", hdr)
        if n > MAX_FRAME:
            self._fatal("incoming frame too large")
        body = self._recv_exact(n)
        if body is None:
            return None
        try:
            return self._loads(body)
        except (ValueError, RecursionError):
            # RecursionError: a frame nested deeper than the parser's stack
            # would otherwise end this reader thread silently.
            self._fatal("malformed frame")

    def _fatal(self, why):
        # A transport failure must fail the verification, never pass it, and
        # must not be catchable by the hidden test. The checker owns the exit
        # code; the worker just dies and the checker sees the pipe close.
        sys.stderr.write(f"[mh-rpc {self.side}] fatal: {why}\n")
        # os._exit skips the buffered-stdout flush a normal exit does. On a pipe
        # stdout is block-buffered, so a worker whose reply races the checker's
        # close (the verdict already in) would lose everything the candidate
        # printed, and the worker report the record carries would read empty.
        # Flushing changes no verdict: the checker's exit code alone decides it.
        # Only the worker's stdout is flushed: the checker's is the hidden
        # test's own output, which a fatal exit must keep discarding.
        streams = (sys.stdout, sys.stderr) if self.side == "w" else (sys.stderr,)
        for s in streams:
            try:
                s.flush()
            except Exception:
                pass
        os._exit(FATAL_EXIT if self.side == "c" else 1)

    # ---- reader / lane dispatch ------------------------------------------- #

    def _read_loop(self):
        while True:
            frame = self._read_frame()
            if frame is None:
                # The worker is reactive: once the checker closes the pipe the
                # verdict is already decided, so the worker just exits. Only the
                # checker treats an unexpected close as a transport failure.
                if self.side == "c" and not self._closing:
                    self._fatal("peer closed the connection")
                self._done = True
                return
            # A frame is a dict with a hashable lane. Anything else would raise
            # in this thread and end it silently, leaving the verification to
            # time out; fail it now instead.
            if type(frame) is not dict or not isinstance(frame.get("lane", 0), int):
                self._fatal("malformed frame")
            k = frame.get("k")
            if k == "hello":
                continue
            lane = frame.get("lane", 0)
            L = self._get_lane(lane)
            L.inbox.put(frame)
            if self.server_side and k == "q" and not L.started:
                L.started = True
                threading.Thread(target=self._lane_loop, args=(lane, L),
                                 daemon=True).start()

    def _get_lane(self, lane):
        with self._lock:
            L = self._lanes.get(lane)
            if L is None:
                if len(self._lanes) >= MAX_LANES:
                    self._fatal("too many concurrent lanes")
                L = _Lane()
                self._lanes[lane] = L
            return L

    def _lane_loop(self, lane, L):
        while not self._done:
            try:
                frame = L.inbox.get(0.2)
            except _Empty:
                continue
            if frame.get("k") == "q":
                try:
                    self._serve(lane, frame)
                except _Fatal as e:
                    # A transport failure while serving must kill this side now,
                    # so the peer sees the pipe close and fails fast, rather than
                    # the lane thread dying quietly and the peer deadlining.
                    self._fatal(str(e))

    # ---- issuing a request (this thread waits, pumping its own lane) ------ #

    def _lane_for(self):
        # A callback issued while serving a request must stay on the lane that
        # originated the whole chain, so the one thread blocked on that lane
        # (the checker's origin thread, or the worker's lane thread) services
        # it. A fresh top-level call uses this thread's identity as a new lane.
        return getattr(self._local, "lane", None) or threading.get_ident()

    def request(self, method, payload, oneway=False):
        lane = self._lane_for()
        with self._lock:
            rid = self._next_rid
            self._next_rid += 1
        self._send({"k": "q", "lane": lane, "rid": rid,
                    "m": method, "p": payload})
        if oneway:
            return None
        return self._pump(lane, rid)

    def request_call(self, target_id, args, kwargs):
        lane = self._lane_for()
        with self._lock:
            rid = self._next_rid
            self._next_rid += 1
        self._note_wb_gaps(args, kwargs)
        sent = [self.codec.encode(x) for x in args]
        payload = {"id": target_id, "a": sent,
                   "kw": {k: self.codec.encode(v) for k, v in kwargs.items()}}
        # `sent` is the checker's own record of what it handed over. The reply
        # must attest the post-call state of every container argument; it is
        # compared against `sent`, and the originals are brought up to date.
        self._send({"k": "q", "lane": lane, "rid": rid, "m": "call", "p": payload})
        result = self._pump(lane, rid, wb_args=args, wb_sent=sent)
        return result

    def _pump(self, lane, rid, wb_args=None, wb_sent=None):
        L = self._get_lane(lane)
        while True:
            frame = self._lane_get(L)
            k = frame.get("k")
            if k == "q":
                self._serve(lane, frame)
                continue
            if frame.get("rid") != rid:
                # Strict ping-pong per lane means the only response that can
                # arrive while we wait is ours. Anything else is a protocol bug.
                self._fatal("out-of-order response")
            if k == "e":
                self.wb_raised += sum(1 for a in (wb_args or ()) if _wb_wants(a))
                self._raise_remote(frame["x"])
            self._writeback(wb_args, frame.get("wb"), wb_sent)
            return self.codec.decode(frame["v"])

    def _lane_get(self, L):
        while True:
            timeout = 0.5
            if self._deadline is not None:
                timeout = self._deadline - time.monotonic()
                if timeout <= 0:
                    self._fatal("deadline exceeded")
            try:
                return L.inbox.get(min(timeout, 0.5))
            except _Empty:
                if self._deadline is not None and \
                        time.monotonic() >= self._deadline:
                    self._fatal("deadline exceeded")

    def _note_wb_gaps(self, args, kwargs):
        """Count the container arguments whose mutation no report will cover."""
        self.wb_kw += sum(1 for v in kwargs.values() if _wb_holds(v))
        self.wb_nested += sum(1 for a in args if type(a) in (tuple, frozenset) and _wb_holds(a))

    def _writeback(self, args, wb, sent):
        """Bring the container arguments we sent up to date with the worker's report.

        The report is the worker's word, so it is held to what the checker can
        check: it must exist for every container argument (a withheld report is
        a transport failure, never "unchanged"), it must have the shape of one,
        and it is compared against the encoding the checker itself sent, so the
        checker -- not the worker -- decides whether the call mutated anything.
        What it cannot check is whether a well-formed report is true; that is
        recorded as unenforced (see `report`).
        """
        if not args:
            return
        if wb is not None and (type(wb) is not list or len(wb) != len(args)):
            self._fatal("malformed writeback report")
        for i, orig in enumerate(args):
            if type(orig) in _WB_TYPES and not _wb_wants(orig):
                self.wb_oversized += 1
                continue
            if not _wb_wants(orig):
                continue
            enc = None if wb is None else wb[i]
            if enc is None:
                self._fatal(f"the worker withheld the post-call state of argument {i}")
            self.wb_attested += 1
            if _jeq(enc, sent[i]):
                continue
            self.mutations += 1
            new = self.codec.decode(enc)
            if type(new) is not type(orig) and not (
                    type(orig) is bytearray and type(new) is bytes):
                self._fatal(f"the worker reported argument {i} as another type")
            if type(orig) is list:
                orig[:] = new
            elif type(orig) is bytearray:
                orig[:] = new
            elif type(orig) is dict:
                orig.clear()
                orig.update(new)
            else:
                orig.clear()
                orig.update(new)

    # ---- serving a request from the peer ---------------------------------- #

    def _serve(self, lane, frame):
        rid = frame["rid"]
        prev = getattr(self._local, "lane", None)
        self._local.lane = lane
        try:
            v, wb = self._execute(frame["m"], frame.get("p", {}))
            self._send({"k": "r", "lane": lane, "rid": rid, "v": v, "wb": wb})
        except _Fatal:
            raise
        except BaseException as e:  # noqa: BLE001 - report, never propagate
            self._send({"k": "e", "lane": lane, "rid": rid,
                        "x": self._encode_exc(e)})
        finally:
            self._local.lane = prev

    def _encode_exc(self, e):
        if isinstance(e, (SystemExit, KeyboardInterrupt, GeneratorExit)):
            return {"fatal_kind": type(e).__name__, "str": str(e)}
        cls = type(e)
        mro = [[getattr(c, "__module__", "builtins"),
                getattr(c, "__qualname__", getattr(c, "__name__", "?"))]
               for c in cls.__mro__ if c is not object]
        try:
            args = [self.codec.encode(a) for a in e.args]
        except Exception:
            args = []
        return {"mro": mro, "args": args, "str": str(e)}

    def _raise_remote(self, x):
        if "fatal_kind" in x:
            raise RemoteFatal(f"worker raised {x['fatal_kind']}: {x.get('str', '')}")
        cls = self._build_class(x["mro"], x["mro"][0][1] if x["mro"] else "Exception")
        if not issubclass(cls, Exception):
            # `fatal_kind` is what an honest endpoint sends for SystemExit and
            # friends. A reply that names one through `mro` instead is a forgery
            # of that kind: re-raising it here would let candidate code end the
            # checker with the exit code of its choice (SystemExit(0) -> pass).
            raise RemoteFatal(f"worker raised {cls.__name__}: {x.get('str', '')}")
        try:
            args = tuple(self.codec.decode(a) for a in x.get("args", []))
            exc = cls(*args)
        except Exception:
            exc = cls(x.get("str", ""))
        raise exc

    def _execute(self, method, p):
        # Returns (encoded_value, writeback_list_or_None). The allow-list is the
        # whole gate: an op outside it never reaches a handler. `_fatal` exits
        # the process now rather than raising, because a raised exception could
        # be swallowed by the hidden test's own `except Exception` and the
        # refusal would then score nothing.
        if method not in self._OPS:
            self._fatal(f"refused op {method!r}: not served by this side")
        return getattr(self, "_op_" + method)(p)

    # ---- operations ------------------------------------------------------- #

    def _obj(self, p):
        return self._objs[p["id"]]

    def _op_getattr(self, p):
        return self.codec.encode(getattr(self._obj(p), p["attr"])), None

    def _op_setattr(self, p):
        setattr(self._obj(p), p["attr"], self.codec.decode(p["val"]))
        return None, None

    def _op_call(self, p):
        obj = self._obj(p)
        args = [self.codec.decode(a) for a in p["a"]]
        kwargs = {k: self.codec.decode(v) for k, v in p.get("kw", {}).items()}
        res = obj(*args, **kwargs)
        wb = self._make_wb(args)
        return self.codec.encode(res), wb

    def _make_wb(self, args):
        wb = None
        for i, a in enumerate(args):
            if _wb_wants(a):
                if wb is None:
                    wb = [None] * len(args)
                wb[i] = self.codec.encode(a)
        return wb

    def _op_getitem(self, p):
        return self.codec.encode(self._obj(p)[self.codec.decode(p["key"])]), None

    def _op_setitem(self, p):
        self._obj(p)[self.codec.decode(p["key"])] = self.codec.decode(p["val"])
        return None, None

    def _op_len(self, p):
        return self.codec.encode(len(self._obj(p))), None

    def _op_bool(self, p):
        return self.codec.encode(bool(self._obj(p))), None

    def _op_hash(self, p):
        return self.codec.encode(hash(self._obj(p))), None

    def _op_str(self, p):
        return self.codec.encode(str(self._obj(p))), None

    def _op_repr(self, p):
        return self.codec.encode(repr(self._obj(p))), None

    def _op_iter(self, p):
        return self.make_handle(iter(self._obj(p))), None

    def _op_next(self, p):
        return self.codec.encode(next(self._obj(p))), None

    def _op_release(self, p):
        with self._lock:
            obj = self._objs.pop(p["id"], None)
            if obj is not None:
                self._obj_ids.pop(id(obj), None)
        return None, None

    # async: drive a checker coroutine one step (test callbacks never really
    # await), convert its completion into a value or exception.
    def _drive(self, coro):
        try:
            coro.send(None)
        except StopIteration as e:
            return e.value
        raise _Fatal("callback coroutine did not complete in one step")

    def _op_codrive(self, p):
        return self.codec.encode(self._drive(self._obj(p))), None

    def _op_aiter(self, p):
        return self.codec.encode(self._obj(p).__aiter__()), None

    def _op_anext(self, p):
        return self.codec.encode(self._drive(self._obj(p).__anext__())), None

    def _op_aenter(self, p):
        return self.codec.encode(self._drive(self._obj(p).__aenter__())), None

    def _op_aexit(self, p):
        et = self.codec.decode(p["et"])
        ev = self.codec.decode(p["ev"])
        tb = self.codec.decode(p["tb"])
        return self.codec.encode(self._drive(self._obj(p).__aexit__(et, ev, tb))), None

    # ---- lifecycle -------------------------------------------------------- #

    def start_reader(self):
        threading.Thread(target=self._read_loop, daemon=True).start()


# Objects whose public attributes lead out of the sandboxed callback surface into
# the checker's frames, code, globals or builtins.
_INTERNALS = (types.FrameType, types.CodeType, types.TracebackType,
              types.ModuleType, types.GeneratorType, types.CoroutineType,
              types.AsyncGeneratorType)


# What the loader gateway (see _LOADER_WRAPPERS) cannot close in CPython, named on
# every record of a task with an import ban. The ban refuses the import statement,
# __import__, importlib and every call into _imp / the frozen importer, but the
# real _imp functions are still held by the audit hook's frame while a load runs,
# and CPython offers no way to hide a live frame from the thread that is running
# in it: a __del__ finalizer that the garbage collector starts inside the load can
# read it with sys._getframe and so call the real create_builtin.
LOADER_RESIDUAL = (
    "import-ban: loader: a candidate that runs a finalizer (__del__ via the garbage "
    "collector) inside a loader call can read the real _imp.create_builtin from the "
    "gateway's live frame and load a banned builtin module such as _sre; every "
    "other spelling, including direct _imp calls, is refused")


# The source-read refusal stats the path the audit event names and the open happens
# afterwards; a candidate thread that re-points a symlink in between reads a banned
# file the hook judged harmless. The audit event carries no file descriptor to
# judge instead, so this cannot be closed from a hook.
SOURCE_RACE_RESIDUAL = (
    "import-ban: source read: the hook judges the path an open event names, then the "
    "open happens; a symlink re-pointed by another thread in between reads a banned "
    "module's source")


class CheckerEndpoint(Endpoint):
    """The checker side: serves only the callback ops of `CHECKER_OPS`."""

    _OPS = CHECKER_OPS

    def __init__(self, sock, sandbox, deadline_s, report_fd=None):
        super().__init__("c", sock, sandbox, deadline_s, server_side=False)
        # The checker runs no candidate code, so it can use the stdlib's json
        # (which the worker must not load, because json loads re).
        import json
        self._dumps = lambda msg: json.dumps(msg, allow_nan=False).encode("utf-8")
        self._loads = json.loads
        self.report_fd = report_fd       # a pipe to the parent; the worker has no such fd
        self.bans = []                   # what each forbid_imports reply said
        self.static_only = []            # bans held by the static audit alone

    def report(self):
        """What this run relied on that the checker cannot itself verify.

        Written to the parent over `report_fd` when the checker exits, so it
        reaches the verification record by a channel the candidate has no end of.
        """
        unenforced = []
        if self.wb_attested:
            unenforced.append(
                f"writeback: the post-call state of {self.wb_attested} container "
                f"argument(s) is attested by the worker, which runs the candidate; "
                f"a report of pristine state for an argument it mutated is not detectable")
        if self.wb_oversized:
            unenforced.append(
                f"writeback: {self.wb_oversized} container argument(s) longer than "
                f"{MUT_WB_MAX} are not attested at all")
        for n, what in ((self.wb_kw, "container keyword argument(s)"),
                        (self.wb_nested, "container(s) inside a tuple or frozenset argument"),
                        (self.wb_raised, "container argument(s) of a call that raised")):
            if n:
                unenforced.append(
                    f"writeback: {n} {what}: a mutation of them is not attested at all "
                    f"(the report covers only the exact-builtin positional containers of a call that returned)")
        for names in self.static_only:
            unenforced.append(
                "import-ban: runtime: " + ", ".join(names) + " are banned by the static "
                "audit only; the worker itself needs them, so no audit hook refuses a "
                "spelling the audit cannot see")
        if self.bans:
            unenforced.append(LOADER_RESIDUAL)
            unenforced.append(SOURCE_RACE_RESIDUAL)
        for b in self.bans:
            if b["preloaded"]:
                unenforced.append(
                    "import-ban: " + ", ".join(b["preloaded"]) + " were already loaded "
                    "when the ban was armed and stay reachable through module attributes")
        return {"v": 1, "unenforced": unenforced, "bans": self.bans,
                "arg_mutations": self.mutations, "wb_attested": self.wb_attested}

    def _public(self, p):
        # The worker may reach the public surface of an exported callback and
        # nothing behind it: no dunder, no private name. A `__class__ ->
        # __init__ -> __globals__ -> __builtins__` walk would otherwise hand the
        # candidate the checker's builtins as callable handles. The refusal is an
        # AttributeError (the candidate sees "no such attribute"), not a fatal
        # exit, so an honest `hasattr(cb, "__name__")` probe still works.
        name = p["attr"]
        if not isinstance(name, str) or name.startswith("_"):
            raise AttributeError(f"{name!r} is not reachable from the worker")
        obj = self._obj(p)
        if isinstance(obj, _INTERNALS):
            raise AttributeError(f"{name!r}: interpreter internals are not reachable")
        return obj

    def _op_getattr(self, p):
        # Public names are not enough: `cr_frame`, `f_builtins`, `f_globals`,
        # `gi_code`, `tb_frame` carry no underscore, and a checker coroutine or
        # module is exactly what a callback can hand out. Neither the object
        # asked nor the value found may be an interpreter internal.
        val = getattr(self._public(p), p["attr"])
        if isinstance(val, _INTERNALS):
            raise AttributeError(f"{p['attr']!r}: interpreter internals are not reachable")
        return self.codec.encode(val), None

    def _op_setattr(self, p):
        setattr(self._public(p), p["attr"], self.codec.decode(p["val"]))
        return None, None

    def close_from_checker(self, code):
        # os._exit skips buffer flushing, so the hidden test's own output (its
        # FAIL lines, which become the redacted labels the model sees) would be
        # lost on a block-buffered pipe. Flush before exiting.
        for s in (sys.stdout, sys.stderr):
            try:
                s.flush()
            except Exception:
                pass
        if self.report_fd is not None:
            try:
                os.write(self.report_fd, wire_dumps(self.report()))
                os.close(self.report_fd)
            except (OSError, ValueError):
                pass
        self._closing = True
        self._done = True
        try:
            self.sock.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        try:
            self.sock.close()
        except OSError:
            pass
        os._exit(code if isinstance(code, int) else 1)


# Names whose import is refused from the first candidate line on, whatever a task
# bans: the ways round an audit hook (a native module can overwrite the hook's
# own memory; a sub-interpreter has its own hooks).
_ESCAPE_MODULES = ("ctypes", "_ctypes", "_interpreters", "_xxsubinterpreters",
                   "_interpqueues", "_interpchannels", "concurrent.interpreters",
                   "_testcapi", "_testinternalcapi", "_cffi_backend")
# Audit events refused from then on: the heap walks that reach the harness's own
# classes and the checker link.
# sys._current_frames hands a thread the live frames of every other thread, which is
# where the loader gateway's real functions sit while a load runs.
# os.exec* replaces the worker with a fresh interpreter that has no hook (the macOS
# profile denies fork but not exec, and the Linux profile allows fork), so every way
# to start another process is refused too; the names are from the CPython audit
# events table (os.exec, os.fork, os.forkpty, os.posix_spawn, os.spawn, os.system,
# subprocess.Popen) plus the _posixsubprocess.fork_exec that Popen reaches.
_DENIED_EVENTS = ("gc.get_objects", "gc.get_referrers", "gc.get_referents",
                  "sys._current_frames", "sys._current_exceptions",
                  "os.exec", "os.fork", "os.forkpty", "os.posix_spawn", "os.spawn",
                  "os.system", "subprocess.Popen", "_posixsubprocess.fork_exec")
AUDIT_IDS_MAX = 4000      # files whose identity the hook can hold; more is refused, never truncated
# The C half of a ban is banned with the pure-Python half: a candidate that may
# not use queue may not use _queue.SimpleQueue either.
_BAN_ALIASES = {
    "queue": ("_queue",),
    "re": ("_sre", "sre_compile", "sre_parse", "sre_constants"),
    "fnmatch": ("_fnmatch",),
    "pathlib": ("_pathlib", "pathlib._local", "pathlib._abc"),
    "glob": ("_glob",),
}

# The hook is built from this template so that nothing it consults is state a
# candidate can reach: the ban list and the denied events are constants of the
# code object (a code object cannot be edited), every builtin it uses is bound as
# a default argument of a function nothing refers to, and it reads no module
# global. A traceback that mentions it leaves a finished frame whose locals
# cannot alter the next call. It runs no method of any object the candidate
# supplied: an event argument is used only after a type check against the exact
# builtin type.
_HOOK_TEMPLATE = '''
def hook(event, args, _t=type, _s=str, _b=bytes, _i=int, _l=list, _E=ImportError, _R=RuntimeError,
         _N=NSPEC, _CB=LCB, _GF=LGF, _FF=LFF, _stat=STAT):
    if _t(event) is not _s:
        return
    if event == "import":
        n = args[0]
        if _t(n) is _s:
            for m in MODS:
                if n == m or n.startswith(m + "."):
                    raise _E("import of " + n + " is forbidden here")
    elif event == "open":
        if FILES:
            p = args[0]
            t = _t(p)
            if t is _b:
                p = p.decode("latin-1")
            elif t is not _s:
                if t is _i or p is None:
                    return
                raise _R("open of a non-path object is refused while an import ban is armed")
            for f in FILES:
                if ("/" + f + ".") in p or ("/" + f + "/") in p:
                    raise _R("reading the source of a banned module is refused")
            try:
                st = _stat(p)
            except BaseException:
                return
            if (st.st_dev, st.st_ino) in IDS:
                raise _R("reading the source of a banned module is refused")
    elif event in EVENTS:
        raise _R(event + " is not available to candidate code")
    elif event == "mh.loader":
        k = args[0]
        n = args[1]
        out = args[-1]
        if _t(k) is not _s or _t(n) is not _s or _t(out) is not _l:
            raise _E("malformed loader request")
        for m in LOADER_NAMES:
            if n == m or n.startswith(m + "."):
                raise _E("import of " + n + " is forbidden here")
        if GATEWAY:
            try:
                if k == "create_builtin":
                    out.append(_CB(_N(name=n)))
                elif k == "init_frozen":
                    raise _E("init_frozen is not available to candidate code")
                elif k == "get_frozen_object":
                    if args[2] is None:
                        out.append(_GF(n))
                    elif _t(args[2]) is _b:
                        out.append(_GF(n, args[2]))
                    else:
                        raise _E("malformed loader request")
                elif k == "find_frozen":
                    out.append(_FF(n, withdata=args[2] is True))
                else:
                    raise _E("malformed loader request")
            except BaseException:
                # A traceback keeps this frame: drop the loader functions from it.
                _CB = _GF = _FF = None
                raise
'''


# Names the loader gateway refuses whatever a task bans: the interpreter's own
# import machinery. A fresh copy of _imp (create_builtin of "_imp" returns one)
# is a pristine create_builtin, so none of these may be handed out.
_LOADER_PRIVATE = ("_imp", "_frozen_importlib", "_frozen_importlib_external",
                   "importlib._bootstrap", "importlib._bootstrap_external")

# The loader entry points that raise no "import" audit event: _imp.create_builtin
# (reached by BuiltinImporter.create_module and _gcd_import alike), get_frozen_object
# and find_frozen(withdata=True) (the frozen module's code), and init_frozen (refused
# outright: it would run the frozen module's code under the hook's own builtins).
# The replacements below hold nothing: each one asks the audit hook for the load
# by firing "mh.loader" and takes the answer from a list it passes in. The real
# functions live only in the hook's own defaults, which no object a candidate
# can name refers to, so a candidate that calls _imp.create_builtin (however it
# got to _imp) gets the same refusal the import statement gives.
_LOADER_WRAPPERS = '''
def create_builtin(spec):
    out = list()
    _a("mh.loader", "create_builtin", spec.name, out)
    return out[0]

def init_frozen(name):
    out = list()
    _a("mh.loader", "init_frozen", name, out)
    return out[0]

def get_frozen_object(name, data=None, /):
    out = list()
    _a("mh.loader", "get_frozen_object", name, data, out)
    return out[0]

def find_frozen(name, /, *, withdata=False):
    out = list()
    _a("mh.loader", "find_frozen", name, withdata, out)
    return out[0]
'''
_LOADER_GATE = []       # set once the _imp functions are replaced; a second ban must not wrap the wrappers
_LOADER_NAMES = ("create_builtin", "init_frozen", "get_frozen_object", "find_frozen")
assert all(n in _LOADER_WRAPPERS for n in _LOADER_NAMES)


def _banned_file_ids(names):
    """(st_dev, st_ino) of every source, compiled and extension file of the banned
    top-level modules, found without importing any of them.

    A path test is spelling; an inode is the file. A relative path, a case variant
    on a case-insensitive disk, a symlink and a hard link all stat to the same pair.
    A package contributes every file under its directory. More than AUDIT_IDS_MAX
    files raises: a partial set would read as a complete one.
    """
    ids = set()
    for top in sorted({n.partition(".")[0] for n in names}):
        try:
            spec = _find_spec(top)
        except (ImportError, ValueError, AttributeError):
            continue
        if spec is None:
            continue
        paths = [getattr(spec, "origin", None), getattr(spec, "cached", None)]
        dirs = list(spec.submodule_search_locations or ())
        if not dirs and isinstance(paths[0], str) and os.path.isfile(paths[0]):
            cache = os.path.join(os.path.dirname(paths[0]), "__pycache__")
            if os.path.isdir(cache):
                paths += [os.path.join(cache, f) for f in os.listdir(cache)
                          if f.partition(".")[0] == top]
        for d in dirs:
            for root, _dirs, files in os.walk(d):
                paths += [os.path.join(root, f) for f in files]
        for p in paths:
            if not isinstance(p, str) or p in ("built-in", "frozen"):
                continue
            try:
                st = os.stat(p)
            except OSError:
                continue
            ids.add((st.st_dev, st.st_ino))
            if len(ids) > AUDIT_IDS_MAX:
                raise RuntimeError(f"more than {AUDIT_IDS_MAX} banned-module files; refusing a partial set")
    return ids


def _set_literal(pairs):
    """Source for a set of (dev, ino) pairs the hook's builtin-less namespace can read."""
    return "{" + ", ".join(repr(p) for p in sorted(pairs)) + "}" if pairs else "()"


def _install_audit_hook(banned):
    """Add an audit hook refusing ESCAPE modules, the heap walks, and `banned`.

    Hooks cannot be removed, so this only ever tightens; a second call adds a
    second hook. Nothing is returned and nothing keeps a reference the Python
    side can reach. The first call also puts the loader gateway (see
    _LOADER_WRAPPERS) in front of _imp; every later hook only refuses names.
    """
    import _imp
    mods = []
    for n in tuple(_ESCAPE_MODULES) + tuple(banned):
        if (type(n) is not str or not n
                or not all(c.isalnum() or c in "_." for c in n)):
            raise ValueError(f"not a module name: {n!r}")
        mods.append(n)
    files = sorted({n.partition(".")[0] for n in banned})
    gateway = not _LOADER_GATE
    subst = {"MODS": repr(tuple(mods)), "FILES": repr(tuple(files)),
             "EVENTS": repr(_DENIED_EVENTS),
             "LOADER_NAMES": repr(tuple(mods) + _LOADER_PRIVATE),
             "GATEWAY": repr(gateway), "IDS": _set_literal(_banned_file_ids(banned))}
    src = _HOOK_TEMPLATE
    for token, text in subst.items():
        src = src.replace(token, text)
    ns = {"__builtins__": {"type": type, "str": str, "bytes": bytes, "int": int, "list": list,
                           "BaseException": BaseException, "ImportError": ImportError, "RuntimeError": RuntimeError},
          "NSPEC": types.SimpleNamespace, "STAT": os.stat}
    orig = {n: getattr(_imp, n) for n in _LOADER_NAMES} if gateway else {}
    ns.update(LCB=orig.get("create_builtin"), LGF=orig.get("get_frozen_object"),
              LFF=orig.get("find_frozen"))
    exec(compile(src, "<mh audit hook>", "exec"), ns)  # noqa: S102
    sys.addaudithook(ns["hook"])
    if gateway:
        wns = {"__builtins__": {"list": list}, "_a": sys.audit}
        exec(compile(_LOADER_WRAPPERS, "<mh loader gateway>", "exec"), wns)  # noqa: S102
        for n in _LOADER_NAMES:
            setattr(_imp, n, wns[n])
        _LOADER_GATE.append(True)


def _find_spec(name):
    """importlib.util.find_spec for an absolute name, without loading importlib
    (a task bans it, and a loaded module is a reachable one)."""
    mod = sys.modules.get(name)
    if mod is not None:
        return getattr(mod, "__spec__", None)
    parent = name.rpartition(".")[0]
    path = None
    if parent:
        pm = __import__(parent, fromlist=["__path__"])
        path = getattr(pm, "__path__", None)
        if path is None:
            raise ModuleNotFoundError(f"{parent!r} is not a package", name=name)
    return sys.modules["_frozen_importlib"]._find_spec(name, path)


class WorkerEndpoint(Endpoint):
    """The worker side: runs candidate code, so it alone serves `WORKER_OPS`."""

    _OPS = WORKER_OPS

    def __init__(self, sock, sandbox, deadline_s):
        super().__init__("w", sock, sandbox, deadline_s, server_side=True)
        # candidate code has executed in this process; used to fail closed if an
        # import ban is armed after the fact.
        self.candidate_ran = False

    def _op_call(self, p):
        self.candidate_ran = True
        return super()._op_call(p)

    def _op_value(self, p):
        # The data a Proxy compares with, never a comparison: [True, copy] for
        # a builtin subclass, [False, None] for anything else. Elements of the
        # copy cross by the usual rule (exact builtins by value, the rest as
        # handles), so a nested namedtuple is compared the same way in turn. An
        # OrderedDict crosses as its ordered pairs, flagged "od".
        data = _builtin_data(self._obj(p))
        if data is None:
            return self.codec.encode([False, None]), None
        if type(data) is collections.OrderedDict:
            pairs = [[k, v] for k, v in data.items()]
            return self.codec.encode([True, pairs, "od"]), None
        return self.codec.encode([True, data]), None

    def _op_find(self, p):
        name = p["name"]
        top = name.split(".")[0]
        if top in getattr(sys, "stdlib_module_names", frozenset()):
            return self.codec.encode({"ok": False, "pkg": False}), None
        if "." in name:
            # Resolving a dotted name imports its parent package: candidate code.
            self.candidate_ran = True
        try:
            spec = _find_spec(name)
        except (ImportError, ValueError, AttributeError):
            spec = None
        ok = spec is not None
        pkg = bool(spec and spec.submodule_search_locations is not None)
        return self.codec.encode({"ok": ok, "pkg": pkg}), None

    def _op_modattr(self, p):
        self.candidate_ran = True
        mod = sys.modules["_frozen_importlib"]._gcd_import(p["name"])
        return self.codec.encode(getattr(mod, p["attr"])), None

    def _op_forbid(self, p):
        names = tuple(self.codec.decode(p["names"]))
        if self.candidate_ran:
            # Fail closed: a ban armed after candidate code has already run
            # cannot prove anything, so refuse rather than report a clean scan.
            raise RuntimeError("import ban armed after candidate code ran")
        banned = []
        for n in names:
            if type(n) is not str or not n:
                raise ValueError(f"not a module name: {n!r}")
            for m in (n,) + _BAN_ALIASES.get(n, ()):
                if m not in banned:
                    banned.append(m)
        tops = {m.partition(".")[0] for m in banned}
        # Before any candidate code has run this process is still trusted, so
        # this reply is the one worker statement the checker believes: which
        # banned modules the harness had already loaded (and so left reachable
        # through some other module's attributes). The frozen import machinery
        # is part of the interpreter, not of the harness.
        preloaded = sorted(
            n for n, m in list(sys.modules.items())
            if n.partition(".")[0] in tops and getattr(
                getattr(m, "__spec__", None), "origin", None) not in ("frozen", "built-in"))
        for n in list(sys.modules):
            if n.partition(".")[0] in tops:
                del sys.modules[n]
        _install_audit_hook(banned)
        return self.codec.encode({"banned": banned, "preloaded": preloaded,
                                  "mechanism": "audit-hook"}), None

    def _op_exec_source(self, p):
        # Execute candidate-generated source in the worker, never the checker.
        # A namespace handle goes back; the checker drives it via proxies.
        self.candidate_ran = True
        src = self.codec.decode(p["src"])
        ns = {"__name__": "<mh candidate exec>", "__builtins__": __builtins__}
        exec(compile(src, "<mh candidate exec>", "exec"), ns)  # noqa: S102
        return self.make_handle(ns), None

    def _op_arun(self, p):
        import asyncio
        self.candidate_ran = True
        fn = self._objs[p["fn"]]
        args = [self.codec.decode(a) for a in p["a"]]
        return self.codec.encode(asyncio.run(fn(*args))), None

    def run_worker(self):
        # Armed before the reader starts, so before the first candidate line.
        _install_audit_hook(())
        if self.sandbox not in sys.path:
            sys.path.insert(0, self.sandbox)
        self.start_reader()
        self._send({"k": "hello", "v": PROTO})
        while not self._done:
            time.sleep(0.05)


class _ProxyLoader:
    """Loader that turns a sandbox module into a proxy to the worker's copy."""

    def __init__(self, ep, name, pkg):
        self.ep = ep
        self.name = name
        self.pkg = pkg

    def create_module(self, spec):
        m = types.ModuleType(spec.name)
        ep, name = self.ep, self.name
        if self.pkg:
            m.__path__ = []

        def __getattr__(attr, _ep=ep, _n=name):
            val = _ep.request("modattr", {"name": _n, "attr": attr})
            if isinstance(val, type) and issubclass(val, BaseException):
                _ep.register_exc_class(val)
            return val

        m.__getattr__ = __getattr__
        return m

    def exec_module(self, module):
        pass


class _CheckerFinder:
    """Claims any non-stdlib name the worker can import from the sandbox."""

    def __init__(self, ep):
        import importlib.util
        self.ep = ep
        self._util = importlib.util
        self._cache = {}

    def find_spec(self, name, path=None, target=None):
        top = name.split(".")[0]
        if top in getattr(sys, "stdlib_module_names", frozenset()):
            return None
        if name in ("_mh", "mh_rpc"):
            return None
        info = self._cache.get(name)
        if info is None:
            info = self.ep.request("find", {"name": name})
            self._cache[name] = info
        if not info.get("ok"):
            return None
        return self._util.spec_from_loader(
            name, _ProxyLoader(self.ep, name, info.get("pkg", False)))


# The static audit. It runs in the checker, over source the checker reads and
# never executes. It is the second, independent layer behind the audit hook: it
# turns the spellings it can see into a named refusal before the candidate runs,
# and a spelling it cannot see (a string built at run time) still meets the hook.
_DYN_NAMES = frozenset({"__import__", "__builtins__"})
_DYN_ATTRS = frozenset({"import_module", "find_spec", "load_module", "exec_module",
                        "module_from_spec", "spec_from_file_location",
                        "spec_from_loader", "SourceFileLoader", "__import__"})
_DYN_MODULES = frozenset({"importlib", "imp", "runpy", "builtins", "_imp",
                          "_frozen_importlib", "_frozen_importlib_external",
                          "zipimport", "pkgutil", "ctypes", "_ctypes"})
# A compiled or native module is code the AST audit cannot read, and a pyc under
# __pycache__ can stand in for a source the audit did read, so each is an offender.
_BINARY_SUFFIXES = (".pyc", ".pyo", ".so", ".pyd", ".dylib", ".dll")
AUDIT_MAX_FILES = 200
AUDIT_MAX_BYTES = 1 << 20


def _folded(node, ast):
    """The str an expression is made of when it is constants joined with +, else None."""
    if isinstance(node, ast.Constant) and type(node.value) is str:
        return node.value
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Add):
        left, right = _folded(node.left, ast), _folded(node.right, ast)
        if left is not None and right is not None:
            return left + right
    return None


def audit_sources(sandbox, banned):
    """Static audit of every Python source under sandbox: (offenders, scanned).

    An offender is an import of a banned module, an import of (or a name,
    attribute or constant string that spells) the dynamic-import machinery, or a
    file the audit cannot read in full. A scan that could not cover every file
    (too many, too large, unparseable) is reported as an offender, never as a
    clean partial scan.
    """
    import ast
    banned = tuple(banned)
    machinery = _DYN_MODULES | _DYN_NAMES | _DYN_ATTRS
    sources = []
    binaries = []
    for root, dirs, files in os.walk(sandbox):
        dirs[:] = [d for d in dirs if d != ".git"]
        for f in sorted(files):
            if f.endswith(".py"):
                sources.append(os.path.join(root, f))
            elif f.endswith(_BINARY_SUFFIXES):
                binaries.append(os.path.relpath(os.path.join(root, f), sandbox))
    sources.sort()
    if len(sources) > AUDIT_MAX_FILES:
        return [f"{len(sources)} Python files in the sandbox exceeds the "
                f"{AUDIT_MAX_FILES} the audit will read; refusing a partial scan"], 0

    def banned_name(n):
        return any(n == b or n.startswith(b + ".") for b in banned)

    offenders = [f"{rel}: compiled or native module in the sandbox; the audit cannot read it"
                 for rel in binaries]
    scanned = 0
    for path in sources:
        rel = os.path.relpath(path, sandbox)
        try:
            if os.path.getsize(path) > AUDIT_MAX_BYTES:
                offenders.append(f"{rel}: larger than {AUDIT_MAX_BYTES} bytes; refusing a partial scan")
                continue
            with open(path, encoding="utf-8", errors="replace") as fh:
                tree = ast.parse(fh.read(), filename=rel)
        except (OSError, SyntaxError, ValueError) as e:
            offenders.append(f"{rel}: not readable as Python ({type(e).__name__}); the audit cannot cover it")
            continue
        scanned += 1
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                for a in node.names:
                    if banned_name(a.name):
                        offenders.append(f"{rel}: import {a.name}")
                    elif a.name.partition(".")[0] in _DYN_MODULES:
                        offenders.append(f"{rel}: import {a.name} (dynamic-import machinery)")
            elif isinstance(node, ast.ImportFrom):
                mod = node.module or ""
                if banned_name(mod):
                    offenders.append(f"{rel}: from {mod} import ...")
                elif mod.partition(".")[0] in _DYN_MODULES:
                    offenders.append(f"{rel}: from {mod} import ... (dynamic-import machinery)")
                else:
                    for a in node.names:
                        if banned_name((mod + "." + a.name) if mod else a.name):
                            offenders.append(f"{rel}: from {mod} import {a.name}")
            elif isinstance(node, ast.Name) and node.id in _DYN_NAMES:
                offenders.append(f"{rel}: {node.id}")
            elif isinstance(node, ast.Attribute) and node.attr in _DYN_ATTRS:
                offenders.append(f"{rel}: .{node.attr}")
            else:
                s = _folded(node, ast)
                if s is not None and s in machinery:
                    offenders.append(f"{rel}: the string {s!r} (a spelled-out name of the import machinery)")
    return sorted(set(offenders)), scanned


class _MH:
    """The `_mh` helper the restructured hidden tests import.

    SANDBOX is the real sandbox path (the checker may read it, so the static
    audit runs here). exec_source / arun / forbid_imports are thin RPCs into the
    worker, so candidate-generated code runs where candidate code runs, not
    where the verdict is decided. `enforce_bans` is the one call a task makes to
    state a ban: audit the sources, then arm the worker's audit hook.
    """

    def __init__(self, ep):
        self._ep = ep
        self.SANDBOX = ep.sandbox

    def exec_source(self, src):
        return self._ep.request("exec_source", {"src": self._ep.codec.encode(src)})

    def arun(self, fn, *args):
        return self._ep.request(
            "arun", {"fn": fn._rid, "a": [self._ep.codec.encode(a) for a in args]})

    def forbid_imports(self, names):
        """Arm the worker's audit hook. The reply is recorded, and a reply that
        is not the shape of one fails the verification."""
        rep = self._ep.request("forbid", {"names": self._ep.codec.encode(tuple(names))})
        if (type(rep) is not dict or rep.get("mechanism") != "audit-hook"
                or type(rep.get("banned")) is not list
                or type(rep.get("preloaded")) is not list
                or not all(type(x) is str for x in rep["banned"] + rep["preloaded"])):
            self._ep._fatal("malformed reply to forbid")
        self._ep.bans.append({"names": list(names), "banned": rep["banned"],
                              "preloaded": rep["preloaded"], "mechanism": "audit-hook"})
        return rep

    def enforce_bans(self, names, runtime=True):
        """State a task's import ban: static audit here, audit hook in the worker.

        Call it before importing the candidate. A source the audit refuses ends
        the hidden test with a FAIL line and exit 1; nothing is imported. A task
        whose own worker needs a banned module (asyncio imports re) passes
        runtime=False: only the static audit holds, and the record says so.
        """
        names = tuple(names)
        offenders, scanned = audit_sources(self.SANDBOX, names)
        if offenders:
            print("FAIL: forbidden module use: " + "; ".join(offenders))
            raise SystemExit(1)
        print(f"scanned {scanned}/{scanned} Python files in the sandbox")
        if not runtime:
            self._ep.static_only.append(list(names))
            return None
        return self.forbid_imports(names)


def run_checker(ep, test_src):
    mh = _MH(ep)
    mh_mod = types.ModuleType("_mh")
    mh_mod.SANDBOX = mh.SANDBOX
    mh_mod.exec_source = mh.exec_source
    mh_mod.arun = mh.arun
    mh_mod.forbid_imports = mh.forbid_imports
    mh_mod.enforce_bans = mh.enforce_bans
    sys.modules["_mh"] = mh_mod
    sys.meta_path.insert(0, _CheckerFinder(ep))
    ep.start_reader()
    ep._send({"k": "hello", "v": PROTO})
    g = {"__name__": "__main__", "__file__": "<hidden test>", "__doc__": None,
         "__package__": None, "__spec__": None, "__loader__": None,
         "__builtins__": __builtins__}
    code = 0
    try:
        exec(compile(test_src, "<hidden test>", "exec"), g)  # noqa: S102
    except SystemExit as e:
        c = e.code
        code = 0 if c is None else (c if isinstance(c, int) else 1)
    except _Fatal:
        code = FATAL_EXIT
    except BaseException:  # noqa: BLE001
        import traceback
        traceback.print_exc()
        code = 1
    ep.close_from_checker(code)


# --------------------------------------------------------------------------- #
# Parent side: spawn a contained checker and a contained worker, wire a socket
# between them, run the hidden test, return the verdict.  Only bench.Task calls
# this; the children never reach it.
# --------------------------------------------------------------------------- #

# The whole of stdin is <this module's source> + SEP + payload. Small enough to
# pass on stdin, so nothing has to be read from the protected tree (which the
# children may not read) and the argv stays well under Linux MAX_ARG_STRLEN.
_BOOT = (
    "import sys\n"
    "_d=sys.stdin.buffer.read()\n"
    "_s=b'" + SEP.decode("latin-1").encode("unicode_escape").decode("ascii") + "'\n"
    "_i=_d.find(_s)\n"
    "_src=_d[:_i].decode('utf-8')\n"
    "_pl=_d[_i+len(_s):]\n"
    "_g={'__name__':'mh_rpc'}\n"
    "exec(compile(_src,'<mh_rpc>','exec'),_g)\n"
    "_g['_entry'](sys.argv[1:],_pl)\n"
)


def _sbpl(path):
    return path.replace("\\", "\\\\").replace('"', '\\"')


def _seatbelt(write_dirs, read_dirs, guard_roots, no_signal_fork, deny_write_dirs=()):
    # (deny network*) is the seatbelt counterpart of bwrap's --unshare-net, so a
    # candidate that touches the network behaves the same on both platforms.
    # Already-open descriptors (the checker/worker socketpair) are unaffected.
    lines = ["(version 1)", "(allow default)", "(deny network*)",
             '(deny file-write* (subpath "/"))']
    # Only /dev is writable by default; the host's shared temp directories are
    # not. A child's temp is its ChildScope directory, re-allowed below.
    lines.append('(allow file-write* (subpath "/dev"))')
    for p in guard_roots:
        if p:
            q = _sbpl(os.path.realpath(p))
            lines.append(f'(deny file-read* (subpath "{q}"))')
            lines.append(f'(deny file-write* (subpath "{q}"))')
    # The private scratch dirs are fresh directories of this verification's
    # ChildScope, so they are re-allowed AFTER the guard denies: a guarded work
    # tree that contains the scope (the nested-checkout layout) must not take
    # the worker's own scratch space with it. bwrap already orders its binds so.
    for p in write_dirs:
        if p:
            lines.append(f'(allow file-read* file-write* (subpath "{_sbpl(os.path.realpath(p))}"))')
    # Deny writes to the sandbox explicitly, after the allows above: in a test
    # layout the sandbox can live under a scratch dir that is writable. The
    # worker may read the candidate's source but never write the sandbox, so it
    # can leave nothing behind for the next turn.
    for p in deny_write_dirs:
        if p:
            lines.append(f'(deny file-write* (subpath "{_sbpl(os.path.realpath(p))}"))')
    # Re-allow reads (only) of the sandbox after the guard denies, so the worker
    # can read the candidate's source and the checker can scan it, while neither
    # can write it and the hidden test (in a guarded dir) stays unreadable.
    for p in read_dirs:
        if p:
            lines.append(f'(allow file-read* (subpath "{_sbpl(os.path.realpath(p))}"))')
    if no_signal_fork:
        # Candidate code in the worker may not signal the checker/parent or fork
        # helper processes. (deny signal) is what actually blocks os.kill on this
        # platform; (target others) does NOT. Threads and kill(getpid(),0) are
        # unaffected, so honest concurrency still runs.
        lines.append("(deny signal)")
        lines.append("(allow signal (target self))")
        lines.append("(deny process-fork)")
    return "\n".join(lines)


# Unverified: this profile mirrors tools.py's bwrap profile but has not been run
# on a Linux host. The sandbox-exec profile is the one stress_test.py C6 proves.
def _bwrap_argv(exe, argv, write_dirs, read_ro_dirs, guard_roots):
    out = [exe, "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc"]
    for p in guard_roots:
        if p:
            r = os.path.realpath(p)
            out += (["--ro-bind", "/dev/null", r] if os.path.isfile(r)
                    else ["--tmpfs", r])
    for p in read_ro_dirs:
        if p:
            r = os.path.realpath(p)
            out += ["--ro-bind", r, r]
    for p in write_dirs:
        if p:
            r = os.path.realpath(p)
            out += ["--bind", r, r]
    out += ["--unshare-pid", "--unshare-net", "--die-with-parent", "--new-session", "--"]
    return out + list(argv)


def _wrap(inner, backend, *, write_dirs, read_dirs, guard_roots, no_signal_fork,
          deny_write_dirs=()):
    if backend == "sandbox-exec":
        prof = _seatbelt(write_dirs, read_dirs, guard_roots, no_signal_fork,
                         deny_write_dirs)
        return ["/usr/bin/sandbox-exec", "-p", prof] + inner
    if backend == "bwrap":
        exe = "/usr/bin/bwrap" if os.path.exists("/usr/bin/bwrap") else "/bin/bwrap"
        return _bwrap_argv(exe, inner, write_dirs, read_dirs, guard_roots)
    if backend == "off":
        return inner
    raise RuntimeError(f"no containment backend for {backend!r}")


def _module_source():
    with open(__file__, "r", encoding="utf-8") as f:
        return f.read()


def verify_python(hidden_src, sandbox, guard_roots, timeout, output_cap,
                  scope=None):
    """Run a Python hidden test in a checker, the candidate in a worker.

    Returns a dict: rc (checker exit code; 0 == pass), stdout/stderr (the
    checker's = the hidden test's own output, capped), worker_output (capped,
    for the record only -- never shown to the model), timed_out, backend.
    Raises ContainmentUnavailable when no backend exists and no opt-out is set.

    Both children get their environment, HOME/TMPDIR and write root from
    `scope` (tools.ChildScope): the checker and the worker each a private
    directory of their own, nothing else writable, no inherited secrets. A scope
    made here (none was passed) is removed on return; a caller's is the
    caller's to close.
    """
    import subprocess
    from .tools import (ChildScope, containment_backend, _kill_group,
                        StreamCapture, ContainmentUnavailable)

    backend = containment_backend()
    if backend is None:
        raise ContainmentUnavailable(
            "no containment backend: refusing to run the candidate uncontained")

    rpc_src = _module_source().encode("utf-8")
    own_scope = scope is None
    if own_scope:
        scope = ChildScope(prefix="mh-verify-")
    empty = scope.private_dir("checker")
    wpriv = scope.private_dir("worker")
    checker_env = scope.env(empty)
    worker_env = scope.env(wpriv)
    a, b = socket.socketpair(socket.AF_UNIX, socket.SOCK_STREAM)
    # The checker's report to this process: the write end goes to the checker
    # only (pass_fds), so the worker has no descriptor onto it.
    report_r, report_w = os.pipe()
    py = sys.executable or "python3"
    base = [py, "-I", "-B", "-c", _BOOT]
    checker_inner = base + ["checker", str(a.fileno()), sandbox, str(timeout),
                            str(report_w)]
    worker_inner = base + ["worker", str(b.fileno()), sandbox, str(timeout)]
    checker_argv = _wrap(checker_inner, backend, write_dirs=[empty],
                         read_dirs=[sandbox], guard_roots=guard_roots,
                         no_signal_fork=False)
    worker_argv = _wrap(worker_inner, backend, write_dirs=[wpriv],
                        read_dirs=[sandbox], guard_roots=guard_roots,
                        no_signal_fork=True, deny_write_dirs=[sandbox])
    isrc = hidden_src if isinstance(hidden_src, bytes) else hidden_src.encode("utf-8")
    checker_stdin = rpc_src + SEP + isrc
    worker_stdin = rpc_src + SEP

    threads = []

    def pump(stream, cap):
        try:
            while True:
                chunk = stream.read(65536)
                if not chunk:
                    break
                cap.feed(chunk)
        except OSError:
            pass
        finally:
            cap.close()

    def feed(stream, data):
        try:
            stream.write(data)
            stream.close()
        except OSError:
            pass

    wproc = cproc = None
    timed_out = False
    try:
        wproc = subprocess.Popen(
            worker_argv, cwd=sandbox, env=worker_env, stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            pass_fds=[b.fileno()], start_new_session=True)
        cproc = subprocess.Popen(
            checker_argv, cwd=empty, env=checker_env, stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            pass_fds=[a.fileno(), report_w], start_new_session=True)
        a.close()
        b.close()
        os.close(report_w)
        report_w = -1
        wcap_o, wcap_e = StreamCapture(4000), StreamCapture(4000)
        ccap_o, ccap_e = StreamCapture(output_cap), StreamCapture(output_cap)
        for stream, cap in ((wproc.stdout, wcap_o), (wproc.stderr, wcap_e),
                            (cproc.stdout, ccap_o), (cproc.stderr, ccap_e)):
            t = threading.Thread(target=pump, args=(stream, cap), daemon=True)
            t.start()
            threads.append(t)
        for t in (threading.Thread(target=feed, args=(wproc.stdin, worker_stdin),
                                   daemon=True),
                  threading.Thread(target=feed, args=(cproc.stdin, checker_stdin),
                                   daemon=True)):
            t.start()
            threads.append(t)
        try:
            cproc.wait(timeout=timeout + 5)
        except subprocess.TimeoutExpired:
            timed_out = True
        rc = cproc.returncode if cproc.returncode is not None else 1
        for t in threads:
            t.join(timeout=2)
        report = None if timed_out else _read_report(report_r)
        stderr = _cap_text(ccap_e)
        if rc == 0 and report is None:
            # A pass whose report never arrived is a check that could not run,
            # which must not read the same as one that ran and passed.
            rc = FATAL_EXIT
            stderr += "\n[mh-rpc] the checker exited 0 but sent no valid report: scored fail"
        return {"rc": rc, "timed_out": timed_out, "backend": backend,
                "stdout": _cap_text(ccap_o), "stderr": stderr, "report": report,
                "worker_output": _join_worker(wcap_o, wcap_e)}
    finally:
        for proc in (cproc, wproc):
            if proc is not None:
                try:
                    _kill_group(proc)
                except Exception:
                    pass
        for s in (a, b):
            try:
                s.close()
            except OSError:
                pass
        for fd in (report_r, report_w):
            if fd >= 0:
                try:
                    os.close(fd)
                except OSError:
                    pass
        if own_scope:
            scope.close()


def _read_report(fd, cap=1 << 16):
    """The checker's report, read after it exited: a dict, or None if absent or malformed."""
    buf = b""
    try:
        while len(buf) <= cap:
            chunk = os.read(fd, 65536)
            if not chunk:
                break
            buf += chunk
    except OSError:
        return None
    if not buf or len(buf) > cap:
        return None
    try:
        rep = wire_loads(buf)
    except (ValueError, RecursionError):
        return None
    if (type(rep) is not dict or rep.get("v") != 1 or type(rep.get("unenforced")) is not list
            or type(rep.get("bans")) is not list or type(rep.get("arg_mutations")) is not int):
        return None
    return rep


def _cap_text(cap):
    from .tools import _elided
    if cap.complete():
        return cap.text()
    return _elided(cap.head(), cap.tail(), cap.total, cap.limit)


def _join_worker(out, err):
    o = out.head().decode("utf-8", "replace")
    e = err.head().decode("utf-8", "replace")
    if e.strip():
        return (o + "\n[worker stderr]\n" + e).strip()
    return o.strip()


def _entry(argv, payload):
    role, fd_s, sandbox, timeout_s = argv[0], argv[1], argv[2], argv[3]
    report_fd = int(argv[4]) if len(argv) > 4 else None
    fd = int(fd_s)
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM, fileno=fd)
    deadline_s = float(timeout_s)
    if role == "worker":
        ep = WorkerEndpoint(sock, sandbox, deadline_s)
        _EXIT_HOOK.append(ep._fatal)
        ep.run_worker()
    elif role == "checker":
        ep = CheckerEndpoint(sock, sandbox, deadline_s, report_fd)
        _EXIT_HOOK.append(ep._fatal)
        run_checker(ep, payload.decode("utf-8"))
    else:
        sys.stderr.write(f"mh_rpc: unknown role {role!r}\n")
        os._exit(2)
