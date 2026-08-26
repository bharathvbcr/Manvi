"""Cerebras wire serialization and response parsing. No network, no credential.

The Gemini arm is the precedent for why this file exists: a wire defect there
produced 315 episodes with zero `finished` stops, and nothing in the suite could
see it because a client that talks to no one is never exercised. Every shape
below is checked against Cerebras' documented Chat Completions contract
(https://inference-docs.cerebras.ai/api-reference/chat-completions, read
2026-08-25) rather than against what the client happens to emit.

Four of these are the ones that fail *silently* against a live endpoint:
tool-call arguments must be a JSON string and not the dict the harness holds;
`max_tokens` is not a parameter this endpoint accepts at all; the per-repeat
seed has to be read from `client.options` at request time, because run.py
rewrites it there between repeats; and `top_k` is not in the documented
parameter set, so an arm that thinks it is running at top_k=20 is not.
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


class _Resp:
    """Minimal urlopen result: context manager with .read()."""

    def __init__(self, payload):
        self._payload = json.dumps(payload).encode()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def read(self, n=-1):
        return self._payload


def drive(client, messages, tools=None, responses=None, raiser=None, retries=0):
    """Run one chat() with the network replaced. Returns (bodies, result, error).

    `drive.last_headers` and `drive.last_url` hold what the final request would
    have carried, so the auth header is checked rather than assumed.
    """
    bodies = []
    queue = list(responses or [])

    class _Req:
        def __init__(self, url, data=None, headers=None):
            bodies.append(json.loads(data.decode()))
            self.headers = headers or {}
            drive.last_headers = dict(headers or {})
            drive.last_url = url

    def _urlopen(req, timeout=None):
        if raiser is not None:
            raise raiser(len(bodies))
        if not queue:
            raise AssertionError("client sent more requests than the test queued")
        return _Resp(queue.pop(0))

    real_req, real_open = M.urllib.request.Request, M.urllib.request.urlopen
    M.urllib.request.Request = _Req
    M.urllib.request.urlopen = _urlopen
    result = error = None
    try:
        result = client.chat(messages, tools=tools, retries=retries)
    except Exception as e:  # the client's own error is part of the contract
        error = e
    finally:
        M.urllib.request.Request, M.urllib.request.urlopen = real_req, real_open
    return bodies, result, error


def build(model="cerebras:gpt-oss-120b", **kw):
    real_key = M._get_cerebras_api_key
    M._get_cerebras_api_key = lambda: "test-key-not-used"
    try:
        return M.CerebrasClient(model, **kw)
    finally:
        M._get_cerebras_api_key = real_key


TOOLS = [{"type": "function", "function": {
    "name": "run_shell", "description": "run",
    "parameters": {"type": "object", "properties": {"cmd": {"type": "string"}}}}}]

# One completed tool round-trip, exactly as mh/harness.py appends it -- including
# the Gemini-only `signature` key it puts on every assistant tool call.
MSGS = [
    {"role": "system", "content": "sys prompt"},
    {"role": "user", "content": "do the thing"},
    {"role": "assistant", "content": "thinking out loud",
     "tool_calls": [{"id": "call_7", "signature": "SIG-ABC",
                     "function": {"name": "run_shell", "arguments": {"cmd": "ls"}}}]},
    {"role": "tool", "name": "run_shell", "tool_call_id": "call_7", "content": "a.txt"},
]

OK_RESPONSE = {
    "choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
        "role": "assistant", "content": "", "reasoning": "let me look",
        "tool_calls": [{"id": "call_9", "type": "function", "function": {
            "name": "run_shell", "arguments": '{"cmd": "pytest -q"}'}}]}}],
    "usage": {"prompt_tokens": 1200, "completion_tokens": 340,
              "prompt_tokens_details": {"cached_tokens": 1024},
              "completion_tokens_details": {"reasoning_tokens": 210}},
    "time_info": {"prompt_time": 0.25, "completion_time": 0.5},
    "system_fingerprint": "fp_test",
}


print("request shape")
client = build()
bodies, reply, err = drive(client, MSGS, TOOLS, responses=[OK_RESPONSE])
body = bodies[0] if bodies else {}
msgs = body.get("messages") or []
assistant = next((m for m in msgs if m.get("role") == "assistant"), {})
toolmsg = next((m for m in msgs if m.get("role") == "tool"), {})
call = (assistant.get("tool_calls") or [{}])[0]

check("the routing prefix is stripped before the wire",
      body.get("model") == "gpt-oss-120b", str(body.get("model")))
check("every harness message is carried",
      [m.get("role") for m in msgs] == ["system", "user", "assistant", "tool"],
      str([m.get("role") for m in msgs]))
check("tool-call arguments are a JSON string, not a dict",
      isinstance(call.get("function", {}).get("arguments"), str),
      repr(call.get("function", {}).get("arguments")))
check("those arguments still decode to the original object",
      json.loads(call["function"]["arguments"]) == {"cmd": "ls"},
      repr(call.get("function", {}).get("arguments")))
check("the tool call declares type=function",
      call.get("type") == "function", str(call))
check("the Gemini-only signature key is not sent",
      "signature" not in call and "SIG-ABC" not in json.dumps(body))
check("the tool result is paired by tool_call_id",
      toolmsg.get("tool_call_id") == "call_7", str(toolmsg))
check("the tool result carries no deprecated name key",
      "name" not in toolmsg, str(toolmsg))
check("tools are forwarded unchanged in OpenAI shape",
      body.get("tools") == TOOLS)
check("tool_choice is auto when tools are offered",
      body.get("tool_choice") == "auto", str(body.get("tool_choice")))

print("parameters this endpoint actually accepts")
check("max_completion_tokens carries the output ceiling",
      body.get("max_completion_tokens") == 16384,
      str(body.get("max_completion_tokens")))
check("max_tokens is never sent (unsupported by this endpoint)",
      "max_tokens" not in body, str(sorted(body)))
check("top_k is never sent (absent from the documented parameter set)",
      "top_k" not in body, str(sorted(body)))
check("num_ctx is a harness-side guard, never a wire parameter",
      "num_ctx" not in body, str(sorted(body)))
check("response_format is never sent (gpt-oss rejects it beside tools)",
      "response_format" not in body, str(sorted(body)))
check("streaming is off", body.get("stream") is False)
check("temperature and top_p are sent",
      body.get("temperature") == 0.6 and body.get("top_p") == 0.95, str(body))
check("the request goes to the documented endpoint",
      drive.last_url == "https://api.cerebras.ai/v1/chat/completions",
      str(drive.last_url))
check("the request authenticates with a bearer token",
      drive.last_headers.get("Authorization") == "Bearer test-key-not-used",
      str({k: v for k, v in drive.last_headers.items() if k != "Authorization"}))
# Not cosmetic: urllib's default User-Agent is refused at this endpoint's CDN
# edge with `HTTP 403: error code: 1010`, before the request reaches the API.
# Every episode of the arm fails, and the 403 reads like a credential problem.
check("an explicit User-Agent is sent",
      bool(drive.last_headers.get("User-Agent")),
      str(sorted(drive.last_headers)))
check("it is not urllib's default, which this endpoint's edge bans",
      "python-urllib" not in (drive.last_headers.get("User-Agent") or "").lower(),
      str(drive.last_headers.get("User-Agent")))
check("the body is declared as JSON",
      drive.last_headers.get("Content-Type") == "application/json",
      str(drive.last_headers.get("Content-Type")))

print("seed pinning reaches the wire")
seeded = build(seed=0)
b0, _, _ = drive(seeded, MSGS, TOOLS, responses=[OK_RESPONSE])
check("the constructor seed is sent", b0[0].get("seed") == 0, str(b0[0].get("seed")))
# This is what run.py does between repeats.
seeded.options["seed"] = 4
b1, _, _ = drive(seeded, MSGS, TOOLS, responses=[OK_RESPONSE])
check("a seed rewritten in client.options is read at request time",
      b1[0].get("seed") == 4, str(b1[0].get("seed")))
seeded.options.pop("seed")
b2, _, _ = drive(seeded, MSGS, TOOLS, responses=[OK_RESPONSE])
check("no seed is sent once it is cleared", "seed" not in b2[0], str(sorted(b2[0])))
check("this arm declares that seeds reach the model",
      M.CerebrasClient.ACCEPTS_SEED is True)
check("the Gemini arm declares that they do not",
      M.GeminiClient.ACCEPTS_SEED is False)

print("reasoning effort follows the model spec")
b, _, _ = drive(build("cerebras:gpt-oss-120b", think=True), MSGS, TOOLS,
                responses=[OK_RESPONSE])
check("gpt-oss with --think runs at the vendor default effort",
      b[0].get("reasoning_effort") == "medium", str(b[0].get("reasoning_effort")))
b, _, _ = drive(build("cerebras:gpt-oss-120b", think=False), MSGS, TOOLS,
                responses=[OK_RESPONSE])
check("gpt-oss with --no-think drops to low",
      b[0].get("reasoning_effort") == "low", str(b[0].get("reasoning_effort")))
b, _, _ = drive(build("cerebras:gemma-4-31b", think=False), MSGS, TOOLS,
                responses=[OK_RESPONSE])
check("gemma with --no-think sends nothing and takes its documented default",
      "reasoning_effort" not in b[0], str(sorted(b[0])))
b, _, _ = drive(build("cerebras:gemma-4-31b", think=True), MSGS, TOOLS,
                responses=[OK_RESPONSE])
check("gemma with --think asks for reasoning explicitly",
      b[0].get("reasoning_effort") == "medium", str(b[0].get("reasoning_effort")))
b, _, _ = drive(build("cerebras:gpt-oss-120b", reasoning_effort="high"), MSGS,
                TOOLS, responses=[OK_RESPONSE])
check("an explicit effort overrides the spec mapping",
      b[0].get("reasoning_effort") == "high", str(b[0].get("reasoning_effort")))

print("model spec defaults match the published specification")
gpt = M.model_spec("cerebras:gpt-oss-120b")
gem = M.model_spec("cerebras:gemma-4-31b")
check("both models sit inside the free-tier 65k context",
      gpt["num_ctx"] == 65536 and gem["num_ctx"] == 65536)
check("both output ceilings sit inside the free-tier 32k max output",
      gpt["num_predict"] <= 32768 and gem["num_predict"] <= 32768)
check("gpt-oss gets headroom for reasoning tokens billed to the same ceiling",
      gpt["num_predict"] > M.LOCAL_SPEC["num_predict"])
check("gemma takes the vendor's recommended sampling values",
      (gem["temperature"], gem["top_p"]) == (1.0, 0.95), str(gem))
check("neither sends top_k", gpt["top_k"] is None and gem["top_k"] is None)
check("an unknown model falls back to the suite's local defaults",
      M.model_spec("qwen3.8:27b") == M.LOCAL_SPEC)
check("the local defaults are unchanged from the frozen cells",
      (M.LOCAL_SPEC["num_ctx"], M.LOCAL_SPEC["num_predict"],
       M.LOCAL_SPEC["temperature"], M.LOCAL_SPEC["top_p"],
       M.LOCAL_SPEC["top_k"]) == (32768, 4096, 0.6, 0.95, 20),
      str(M.LOCAL_SPEC))

print("response parsing")
_, reply, err = drive(build(), MSGS, TOOLS, responses=[OK_RESPONSE])
check("a reply is returned", reply is not None and err is None, repr(err))
if reply is not None:
    check("the tool call is extracted", len(reply.tool_calls) == 1, str(reply.tool_calls))
    check("string arguments are coerced back to a dict",
          reply.tool_calls[0]["args"] == {"cmd": "pytest -q"},
          str(reply.tool_calls[0]))
    check("the reasoning channel is read", reply.reasoning == "let me look",
          repr(reply.reasoning))
    check("usage is recorded",
          (reply.prompt_tokens, reply.output_tokens) == (1200, 340),
          f"{reply.prompt_tokens}/{reply.output_tokens}")
    check("the provider's own timings become nanoseconds for tok_s",
          (reply.prompt_eval_duration_ns, reply.eval_duration_ns)
          == (250_000_000, 500_000_000),
          f"{reply.prompt_eval_duration_ns}/{reply.eval_duration_ns}")
    check("reasoning and cached-prompt token counts are kept for the episode",
          reply.raw.get("reasoning_tokens") == 210
          and reply.raw.get("cached_prompt_tokens") == 1024, str(reply.raw))

LENGTH_RESPONSE = {
    "choices": [{"index": 0, "finish_reason": "length", "message": {
        "role": "assistant", "content": "x" * 20}}],
    "usage": {"prompt_tokens": 10, "completion_tokens": 16384},
}
_, reply, _ = drive(build(), MSGS, TOOLS, responses=[LENGTH_RESPONSE])
check("finish_reason=length is surfaced as a truncated turn",
      reply is not None and reply.truncated is True)

print("degenerate and failing responses")
EMPTY = {"choices": [{"index": 0, "finish_reason": "stop",
                      "message": {"role": "assistant", "content": ""}}],
         "usage": {"prompt_tokens": 10, "completion_tokens": 0}}
bodies, reply, err = drive(build(), MSGS, TOOLS,
                           responses=[EMPTY, OK_RESPONSE], retries=1)
check("an empty completion is retried rather than scored as no_tool_call",
      len(bodies) == 2 and reply is not None and reply.tool_calls,
      f"{len(bodies)} requests, err={err!r}")
bodies, reply, err = drive(build(), MSGS, TOOLS, responses=[EMPTY], retries=0)
check("an empty completion with no retries left is an error, not an empty Reply",
      reply is None and isinstance(err, M.ModelError), repr(err))

NO_CHOICES = {"usage": {}}
_, reply, err = drive(build(), MSGS, TOOLS, responses=[NO_CHOICES], retries=0)
check("a response with no choices is refused, not read as silence",
      isinstance(err, M.ModelError) and "no choices" in str(err), repr(err))


def http_raiser(code, headers=None, retryable_after=None):
    hdrs = dict(headers or {})

    class _E(M.urllib.error.HTTPError):
        def __init__(self):
            super().__init__("u", code, "err", hdrs, None)

        def read(self, *a):
            return b"boom"

    def _raise(n):
        return _E()
    return _raise


bodies, _, err = drive(build(), MSGS, TOOLS,
                       raiser=http_raiser(400), retries=3)
check("a 400 is raised at once, not retried into the budget",
      len(bodies) == 1 and isinstance(err, M.ModelError), f"{len(bodies)} {err!r}")
bodies, _, err = drive(build(), MSGS, TOOLS,
                       raiser=http_raiser(429, {"Retry-After": "600"}), retries=3)
check("a Retry-After above the cap is refused instead of sleeping through "
      "the episode's wall clock",
      isinstance(err, M.ModelError) and "cap" in str(err), repr(err))
# An account refusal is not a request failure: it is the same for every later
# episode, so it must be distinguishable and must not be retried. Without this,
# a quota exhaustion mid-grid wrote 314 zero-token rows that read exactly like
# the ablations destroying the model.
for code in (401, 402, 403):
    bodies, _, err = drive(build(), MSGS, TOOLS,
                           raiser=http_raiser(code), retries=3)
    check(f"HTTP {code} raises AccountRefused, not a plain ModelError",
          isinstance(err, M.AccountRefused), f"{type(err).__name__}: {err}")
    check(f"HTTP {code} is not retried",
          len(bodies) == 1, f"{len(bodies)} requests")
check("an ordinary 500 is NOT an account refusal",
      not isinstance(drive(build(), MSGS, TOOLS, raiser=http_raiser(500),
                           retries=0)[2], M.AccountRefused))
bodies, _, err = drive(build(), MSGS, TOOLS, raiser=http_raiser(500), retries=1)
check("a 5xx is retried within the budget then surfaced",
      len(bodies) == 2 and isinstance(err, M.ModelError), f"{len(bodies)} {err!r}")

print("credentials")
real_key = M._get_cerebras_api_key
real_env = os.environ.pop("CEREBRAS_API_KEY", None)
M._get_cerebras_api_key = lambda: None
try:
    try:
        M.CerebrasClient("cerebras:gpt-oss-120b")
        check("a missing credential is a clear refusal", False, "no error")
    except M.ModelError as e:
        check("a missing credential is a clear refusal",
              "CEREBRAS_API_KEY" in str(e), str(e))
finally:
    M._get_cerebras_api_key = real_key
    if real_env is not None:
        os.environ["CEREBRAS_API_KEY"] = real_env

os.environ["CEREBRAS_API_KEY"] = "  env-key  "
try:
    check("the key is read from the environment and stripped",
          M._get_cerebras_api_key() == "env-key")
    c = M.CerebrasClient("cerebras:gpt-oss-120b")
    dumped = json.dumps(M.model_spec("cerebras:gpt-oss-120b"))
    check("the key is never part of anything an episode records",
          "env-key" not in dumped)
finally:
    os.environ.pop("CEREBRAS_API_KEY", None)
    if real_env is not None:
        os.environ["CEREBRAS_API_KEY"] = real_env

print("credential lookup across a git worktree")
import tempfile
with tempfile.TemporaryDirectory() as tmp:
    main = os.path.join(tmp, "repo")
    wt = os.path.join(main, ".claude", "worktrees", "feature")
    os.makedirs(os.path.join(main, ".git", "worktrees", "feature"))
    os.makedirs(wt)
    with open(os.path.join(wt, ".git"), "w") as f:
        f.write(f"gitdir: {os.path.join(main, '.git', 'worktrees', 'feature')}\n")
    paths = M.env_local_paths(wt)
    # A linked worktree does not carry the main checkout's untracked files, and
    # .env.local is gitignored -- so a key sitting at the main repo root was
    # invisible to any run started from a worktree, for every provider.
    check("a worktree reads its own root first",
          paths[0] == os.path.join(wt, ".env.local"), str(paths))
    check("and then the main checkout's root, derived from the gitdir file",
          os.path.join(main, ".env.local") in paths, str(paths))
    plain = os.path.join(tmp, "plain")
    os.makedirs(os.path.join(plain, ".git"))
    check("an ordinary checkout reads exactly one path",
          M.env_local_paths(plain) == [os.path.join(plain, ".env.local")],
          str(M.env_local_paths(plain)))


print("dispatch")
from mh.runtime import api_model_id, api_provider, is_api_model
check("Client routes a cerebras: name to the Cerebras client",
      type(build()) is M.CerebrasClient)
check("api_provider names the provider",
      api_provider("cerebras:gpt-oss-120b") == "cerebras"
      and api_provider("gemini-3.7-flash") == "gemini"
      and api_provider("qwen3.8:27b") is None)
check("a bare gpt-oss name is NOT claimed as remote (it is also a local tag)",
      not is_api_model("gpt-oss-120b") and not is_api_model("gemma-4-31b"))
check("api_model_id strips only the routing prefix",
      api_model_id("cerebras:gpt-oss-120b") == "gpt-oss-120b"
      and api_model_id("qwen3.8:27b") == "qwen3.8:27b")

print()
print(f"{len(PASS)} passed, {len(FAIL)} failed")
if FAIL:
    sys.exit(1)
