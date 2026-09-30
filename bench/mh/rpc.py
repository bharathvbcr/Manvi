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
that proxy -- attribute access, call, item access, iteration, len, bool, compare,
context managers, async iteration -- is a typed message to the worker, which runs
the real thing and sends back a value (copied) or a handle (a proxy the other
way, so a test-supplied callback or Clock still works). Candidate code exiting,
killing, or forging inside the worker only kills the worker: the checker sees the
pipe close and fails the verification. A verdict can be *earned* by making the
checks pass; it cannot be *asserted*.

The wire is a restricted, typed JSON encoding. There is no pickle, marshal or
eval anywhere on it. A malformed message, an oversize message, EOF, or a blown
deadline is fatal to the checker and scores fail -- never pass.

This module is shipped to both children as source on their stdin (so nothing has
to be read from the protected benchmark tree, which neither child may read), then
executed; `_entry` dispatches on the role in argv. `bench.Task._verify` is the
only caller.
"""
import base64
import importlib
import importlib.util
import json
import os
import queue
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


class _Fatal(Exception):
    """A transport failure. Whoever raises it has already scheduled the exit."""


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
            return {"$": "by", "b": base64.b64encode(obj).decode("ascii")}
        if t is bytearray:
            return {"$": "ba", "b": base64.b64encode(bytes(obj)).decode("ascii")}
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
            return base64.b64decode(j["b"])
        if tag == "ba":
            return bytearray(base64.b64decode(j["b"]))
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
        return self._ep.request("bool", {"id": self._rid})

    def __eq__(self, other):
        return self._ep.request("eq", {"id": self._rid,
                                       "other": self._ep.codec.encode(other)})

    def __hash__(self):
        return self._ep.request("hash", {"id": self._rid})

    def __str__(self):
        return self._ep.request("str", {"id": self._rid})

    def __repr__(self):
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


class _Lane:
    __slots__ = ("inbox", "started")

    def __init__(self):
        self.inbox = queue.Queue()
        self.started = False


class Endpoint:
    """Symmetric half of the checker/worker link."""

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
        # candidate code has executed in this (worker) process; used to fail
        # closed if an import ban is armed after the fact.
        self.candidate_ran = False
        self._forbidden = ()

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
        data = json.dumps(msg, allow_nan=False).encode("utf-8")
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
            return json.loads(body)
        except ValueError:
            self._fatal("malformed frame")

    def _fatal(self, why):
        # A transport failure must fail the verification, never pass it, and
        # must not be catchable by the hidden test. The checker owns the exit
        # code; the worker just dies and the checker sees the pipe close.
        sys.stderr.write(f"[mh-rpc {self.side}] fatal: {why}\n")
        sys.stderr.flush()
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
                frame = L.inbox.get(timeout=0.2)
            except queue.Empty:
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
        payload = {"id": target_id,
                   "a": [self.codec.encode(x) for x in args],
                   "kw": {k: self.codec.encode(v) for k, v in kwargs.items()}}
        # Objects whose in-place mutation a check might observe: keep them so we
        # can write the worker's post-call state back into the originals.
        self._send({"k": "q", "lane": lane, "rid": rid, "m": "call", "p": payload})
        result = self._pump(lane, rid, wb_args=args)
        return result

    def _pump(self, lane, rid, wb_args=None):
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
                self._raise_remote(frame["x"])
            self._writeback(wb_args, frame.get("wb"))
            return self.codec.decode(frame["v"])

    def _lane_get(self, L):
        while True:
            timeout = 0.5
            if self._deadline is not None:
                timeout = self._deadline - time.monotonic()
                if timeout <= 0:
                    self._fatal("deadline exceeded")
            try:
                return L.inbox.get(timeout=min(timeout, 0.5))
            except queue.Empty:
                if self._deadline is not None and \
                        time.monotonic() >= self._deadline:
                    self._fatal("deadline exceeded")

    def _writeback(self, args, wb):
        if not args or not wb:
            return
        for orig, enc in zip(args, wb):
            if enc is None:
                continue
            new = self.codec.decode(enc)
            if isinstance(orig, list) and isinstance(new, list):
                orig[:] = new
            elif isinstance(orig, bytearray) and isinstance(new, (bytes, bytearray)):
                orig[:] = new
            elif isinstance(orig, dict) and isinstance(new, dict):
                orig.clear()
                orig.update(new)
            elif isinstance(orig, set) and isinstance(new, set):
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
        try:
            args = tuple(self.codec.decode(a) for a in x.get("args", []))
            exc = cls(*args)
        except Exception:
            exc = cls(x.get("str", ""))
        raise exc

    def _execute(self, method, p):
        # Returns (encoded_value, writeback_list_or_None).
        h = getattr(self, "_op_" + method, None)
        if h is None:
            raise _Fatal(f"unknown op {method!r}")
        return h(p)

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
        self.candidate_ran = True
        res = obj(*args, **kwargs)
        wb = self._make_wb(args)
        return self.codec.encode(res), wb

    def _make_wb(self, args):
        wb = None
        for i, a in enumerate(args):
            if isinstance(a, (list, dict, set, bytearray)) and len(a) <= MUT_WB_MAX:
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

    def _op_eq(self, p):
        return self.codec.encode(self._obj(p) == self.codec.decode(p["other"])), None

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

    # ---- worker-only operations ------------------------------------------- #

    def _op_find(self, p):
        name = p["name"]
        top = name.split(".")[0]
        if top in getattr(sys, "stdlib_module_names", frozenset()):
            return self.codec.encode({"ok": False, "pkg": False}), None
        try:
            spec = importlib.util.find_spec(name)
        except (ImportError, ValueError, AttributeError):
            spec = None
        ok = spec is not None
        pkg = bool(spec and spec.submodule_search_locations is not None)
        return self.codec.encode({"ok": ok, "pkg": pkg}), None

    def _op_modattr(self, p):
        self.candidate_ran = True
        mod = importlib.import_module(p["name"])
        return self.codec.encode(getattr(mod, p["attr"])), None

    def _op_forbid(self, p):
        names = tuple(self.codec.decode(p["names"]))
        self._forbidden = names
        if self.candidate_ran:
            # Fail closed: a ban armed after candidate code has already run
            # cannot prove anything, so refuse rather than report a clean scan.
            raise RuntimeError("import ban armed after candidate code ran")
        for name in list(sys.modules):
            if name.partition(".")[0] in names:
                del sys.modules[name]
        sys.meta_path.insert(0, _ImportBan(names))
        return None, None

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

    # ---- lifecycle -------------------------------------------------------- #

    def start_reader(self):
        threading.Thread(target=self._read_loop, daemon=True).start()

    def run_worker(self):
        if self.sandbox not in sys.path:
            sys.path.insert(0, self.sandbox)
        self.start_reader()
        self._send({"k": "hello", "v": PROTO})
        while not self._done:
            time.sleep(0.05)

    def close_from_checker(self, code):
        # os._exit skips buffer flushing, so the hidden test's own output (its
        # FAIL lines, which become the redacted labels the model sees) would be
        # lost on a block-buffered pipe. Flush before exiting.
        for s in (sys.stdout, sys.stderr):
            try:
                s.flush()
            except Exception:
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


class _ImportBan:
    """A meta-path finder that refuses a set of top-level module names."""

    def __init__(self, names):
        self.names = names

    def find_spec(self, name, path=None, target=None):
        if name.partition(".")[0] in self.names:
            raise ImportError(f"import of {name!r} is forbidden by the task")
        return None

    def find_module(self, name, path=None):   # pragma: no cover - py<3.12
        self.find_spec(name, path)
        return None


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
        self.ep = ep
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
        return importlib.util.spec_from_loader(
            name, _ProxyLoader(self.ep, name, info.get("pkg", False)))


class _MH:
    """The `_mh` helper the restructured hidden tests import.

    SANDBOX is the real sandbox path (the checker may read it, so the static
    import scans run here unchanged). exec_source / arun / forbid_imports are
    thin RPCs into the worker, so candidate-generated code and the import ban
    live where candidate code runs, not where the verdict is decided.
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
        self._ep.request("forbid",
                         {"names": self._ep.codec.encode(tuple(names))})


def run_checker(ep, test_src):
    mh = _MH(ep)
    mh_mod = types.ModuleType("_mh")
    mh_mod.SANDBOX = mh.SANDBOX
    mh_mod.exec_source = mh.exec_source
    mh_mod.arun = mh.arun
    mh_mod.forbid_imports = mh.forbid_imports
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


def _temp_roots():
    import tempfile
    roots = {"/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp", "/dev"}
    for p in (tempfile.gettempdir(), os.environ.get("TMPDIR") or ""):
        if p:
            roots.add(os.path.realpath(p))
    return sorted(roots)


def _seatbelt(write_dirs, read_dirs, guard_roots, no_signal_fork, deny_write_dirs=()):
    lines = ["(version 1)", "(allow default)", '(deny file-write* (subpath "/"))']
    for p in _temp_roots():
        if os.path.exists(p):
            lines.append(f'(allow file-write* (subpath "{_sbpl(os.path.realpath(p))}"))')
    for p in write_dirs:
        if p:
            lines.append(f'(allow file-write* (subpath "{_sbpl(os.path.realpath(p))}"))')
    for p in guard_roots:
        if p:
            q = _sbpl(os.path.realpath(p))
            lines.append(f'(deny file-read* (subpath "{q}"))')
            lines.append(f'(deny file-write* (subpath "{q}"))')
    # Deny writes to the sandbox explicitly, after the temp-root allows: in a
    # test layout the sandbox lives under /tmp, which those allows would
    # otherwise re-open. The worker may read the candidate's source but never
    # write the sandbox, so it can leave nothing behind for the next turn.
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


def _bwrap_argv(exe, argv, write_dirs, read_ro_dirs, guard_roots):
    out = [exe, "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc"]
    tmp = os.environ.get("TMPDIR") or "/tmp"
    out += ["--bind", os.path.realpath(tmp), os.path.realpath(tmp)]
    for p in guard_roots:
        if p:
            out += ["--tmpfs", os.path.realpath(p)]
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


def verify_python(hidden_src, sandbox, guard_roots, timeout, output_cap):
    """Run a Python hidden test in a checker, the candidate in a worker.

    Returns a dict: rc (checker exit code; 0 == pass), stdout/stderr (the
    checker's = the hidden test's own output, capped), worker_output (capped,
    for the record only -- never shown to the model), timed_out, backend.
    Raises ContainmentUnavailable when no backend exists and no opt-out is set.
    """
    import subprocess
    import tempfile
    from .tools import (containment_backend, _kill_group, StreamCapture,
                        ContainmentUnavailable)

    backend = containment_backend()
    if backend is None:
        raise ContainmentUnavailable(
            "no containment backend: refusing to run the candidate uncontained")

    rpc_src = _module_source().encode("utf-8")
    empty = tempfile.mkdtemp(prefix="mh-checker-")
    wpriv = tempfile.mkdtemp(prefix="mh-worker-")
    a, b = socket.socketpair(socket.AF_UNIX, socket.SOCK_STREAM)
    py = sys.executable or "python3"
    base = [py, "-I", "-B", "-c", _BOOT]
    checker_inner = base + ["checker", str(a.fileno()), sandbox, str(timeout)]
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

    caps = {}
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
            worker_argv, cwd=sandbox, stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            pass_fds=[b.fileno()], start_new_session=True)
        cproc = subprocess.Popen(
            checker_argv, cwd=empty, stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            pass_fds=[a.fileno()], start_new_session=True)
        a.close()
        b.close()
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
        return {"rc": rc, "timed_out": timed_out, "backend": backend,
                "stdout": _cap_text(ccap_o), "stderr": _cap_text(ccap_e),
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
        import shutil
        shutil.rmtree(empty, ignore_errors=True)
        shutil.rmtree(wpriv, ignore_errors=True)


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
    fd = int(fd_s)
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM, fileno=fd)
    deadline_s = float(timeout_s)
    if role == "worker":
        ep = Endpoint("w", sock, sandbox, deadline_s, server_side=True)
        ep.run_worker()
    elif role == "checker":
        ep = Endpoint("c", sock, sandbox, deadline_s, server_side=False)
        run_checker(ep, payload.decode("utf-8"))
    else:
        sys.stderr.write(f"mh_rpc: unknown role {role!r}\n")
        os._exit(2)
