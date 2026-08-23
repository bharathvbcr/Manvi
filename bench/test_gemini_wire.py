"""Gemini wire serialization. No network, no credential required.

Regression test for the defect that produced a 315-episode gemini arm with zero
`finished` stops: the client sent function_result entries with no preceding
function_call and no thought signature, so the server saw results for calls it
had never been told about and the model lost its own action history every turn.

The expected shapes here are taken from a recorded capture of a working MANVI
run against the same endpoint (results/live-gemini/gemini-wire.log): 1371
function_call items paired 1:1 with 1371 function_result items, every
function_call carrying a non-empty signature.
"""
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import mh.model as M

PASS, FAIL = [], []


def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print(f"  {'ok  ' if cond else 'FAIL'} {name}" + (f"  {detail}" if not cond and detail else ""))


def build(messages, tools=None):
    """Capture the request body the client would send, without sending it."""
    captured = {}

    class _Req:
        def __init__(self, url, data=None, headers=None):
            captured["body"] = json.loads(data.decode())

    class _Boom(Exception):
        pass

    def _urlopen(req, timeout=None):
        raise _Boom("captured")

    real_req, real_open = M.urllib.request.Request, M.urllib.request.urlopen
    real_key = M._get_gemini_api_key
    M._get_gemini_api_key = lambda: "test-key-not-used"
    M.urllib.request.Request = _Req
    M.urllib.request.urlopen = _urlopen
    try:
        c = M.GeminiClient(model="gemini-3.7-flash", timeout=1)
        try:
            c.chat(messages, tools=tools, retries=0)
        except Exception:
            pass
    finally:
        M.urllib.request.Request, M.urllib.request.urlopen = real_req, real_open
        M._get_gemini_api_key = real_key
    return captured.get("body", {})


TOOLS = [{"type": "function", "function": {
    "name": "run_shell", "description": "run",
    "parameters": {"type": "object", "properties": {"cmd": {"type": "string"}}}}}]

# One completed tool round-trip, exactly as mh/harness.py appends it.
MSGS = [
    {"role": "system", "content": "sys prompt"},
    {"role": "user", "content": "do the thing"},
    {"role": "assistant", "content": "thinking out loud",
     "tool_calls": [{"id": "call_7", "signature": "SIG-ABC",
                     "function": {"name": "run_shell", "arguments": {"cmd": "ls"}}}]},
    {"role": "tool", "name": "run_shell", "tool_call_id": "call_7", "content": "a.txt"},
]

body = build(MSGS, TOOLS)
inp = body.get("input") or []
types = [i.get("type") for i in inp]
calls = [i for i in inp if i.get("type") == "function_call"]
results = [i for i in inp if i.get("type") == "function_result"]

print("function_call emission (the regression)")
check("a function_call item is emitted at all", len(calls) == 1, f"types={types}")
check("function_call pairs 1:1 with function_result", len(calls) == len(results),
      f"{len(calls)} calls vs {len(results)} results")
check("function_call precedes its function_result",
      "function_call" in types and "function_result" in types
      and types.index("function_call") < types.index("function_result"), f"types={types}")

if calls:
    fc = calls[0]
    check("function_call carries the thought signature",
          fc.get("signature") == "SIG-ABC", f"got {fc.get('signature')!r}")
    check("function_call.arguments is an object, not a string",
          isinstance(fc.get("arguments"), dict), f"got {type(fc.get('arguments')).__name__}")
    check("function_call id matches the result call_id",
          fc.get("id") == results[0].get("call_id"))
    check("function_call has the keys the working capture used",
          set(fc.keys()) == {"type", "id", "name", "arguments", "signature"}, f"got {sorted(fc.keys())}")

if results:
    check("function_result keeps its documented shape",
          set(results[0].keys()) == {"type", "call_id", "name", "result"}, f"got {sorted(results[0].keys())}")

print("history preservation")
check("system prompt goes to system_instruction", body.get("system_instruction") == "sys prompt")
check("user turn survives", any(i.get("type") == "user_input" for i in inp))
check("store is false, as in the working capture", body.get("store") is False)

# A talk-only assistant turn (the harness's no-tool-call nudge path) must survive.
MSGS2 = MSGS + [
    {"role": "assistant", "content": "I am just talking"},
    {"role": "user", "content": "make a tool call"},
]
inp2 = build(MSGS2, TOOLS).get("input") or []
check("talk-only assistant turn is preserved once tool results exist",
      any(i.get("type") == "model_output" for i in inp2),
      f"types={[i.get('type') for i in inp2]}")

# Arguments arriving as a JSON string (the harness coerces, but be defensive).
MSGS3 = [
    {"role": "user", "content": "go"},
    {"role": "assistant", "content": "",
     "tool_calls": [{"id": "call_1", "signature": "S",
                     "function": {"name": "run_shell", "arguments": '{"cmd": "ls"}'}}]},
    {"role": "tool", "name": "run_shell", "tool_call_id": "call_1", "content": "out"},
]
c3 = [i for i in (build(MSGS3, TOOLS).get("input") or []) if i.get("type") == "function_call"]
check("string arguments are coerced to an object",
      bool(c3) and c3[0].get("arguments") == {"cmd": "ls"},
      f"got {c3[0].get('arguments') if c3 else None!r}")

# A call with no signature must still be emitted (signature omitted, not null).
MSGS4 = [
    {"role": "user", "content": "go"},
    {"role": "assistant", "content": "",
     "tool_calls": [{"id": "call_2", "function": {"name": "run_shell", "arguments": {}}}]},
    {"role": "tool", "name": "run_shell", "tool_call_id": "call_2", "content": "out"},
]
c4 = [i for i in (build(MSGS4, TOOLS).get("input") or []) if i.get("type") == "function_call"]
check("unsigned call is still emitted", len(c4) == 1)
check("absent signature is omitted, not sent as null",
      bool(c4) and "signature" not in c4[0], f"got {c4[0] if c4 else None}")


print("generation_config (measured against the live endpoint)")
gc = body.get("generation_config") or {}
check("generation_config carries thinking_level", "thinking_level" in gc, f"got {gc}")
check("generation_config carries NOTHING else (anything more returns HTTP 500)",
      set(gc.keys()) == {"thinking_level"}, f"got {sorted(gc.keys())}")
check("think=True maps to high", gc.get("thinking_level") == "high", f"got {gc.get('thinking_level')}")

body_lo = build(MSGS, TOOLS)
import mh.model as _M
_orig = _M.GeminiClient.__init__
gc_lo = {}
def _b(msgs, tools, think):
    captured = {}
    class _Req:
        def __init__(self, url, data=None, headers=None):
            captured["body"] = json.loads(data.decode())
    def _open(req, timeout=None):
        raise RuntimeError("captured")
    rq, op, rk = _M.urllib.request.Request, _M.urllib.request.urlopen, _M._get_gemini_api_key
    _M._get_gemini_api_key = lambda: "k"
    _M.urllib.request.Request, _M.urllib.request.urlopen = _Req, _open
    try:
        c = _M.GeminiClient(model="gemini-3.7-flash", think=think, timeout=1)
        try: c.chat(msgs, tools=tools, retries=0)
        except Exception: pass
    finally:
        _M.urllib.request.Request, _M.urllib.request.urlopen, _M._get_gemini_api_key = rq, op, rk
    return captured.get("body", {})
check("think=False maps to low",
      (_b(MSGS, TOOLS, False).get("generation_config") or {}).get("thinking_level") == "low")

print(f"\n{'ok' if not FAIL else 'FAILED'} {len(PASS)} gemini wire tests"
      + (f", {len(FAIL)} failed: {FAIL}" if FAIL else ""))
sys.exit(1 if FAIL else 0)
