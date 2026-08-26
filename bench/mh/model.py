"""Chat clients: local ollama, Google Gemini, Cerebras.

Deliberately tolerant on the response side. The reasoning channel and the shape of
tool-call arguments differ between servers and between builds, and getting either
wrong fails silently -- an answer filed as reasoning is stripped and the turn
returns nothing. So we read every spelling we have ever seen and coerce arguments
from either a dict or a JSON string.

`Client(model)` dispatches on `mh.runtime.api_provider`, which is the one place
that decides who serves a model.
"""
import json
import os
import random
import time
import urllib.error
import urllib.request

from .runtime import api_model_id, api_provider, is_gemini_model  # noqa: F401

DEFAULT_HOST = "http://127.0.0.1:11434"
CEREBRAS_URL = "https://api.cerebras.ai/v1/chat/completions"
# urllib's default User-Agent ("Python-urllib/3.x") is banned at this endpoint's
# CDN edge: it returns `HTTP 403: error code: 1010` before the request reaches
# the API at all. Measured 2026-08-25 -- the identical request with any explicit
# User-Agent reaches the API and comes back with a normal 401 for a bad key.
# Without this header every episode of a Cerebras arm fails with a 403 that
# reads like a credential or permissions problem, and the whole arm is
# `error:ModelError` for a reason nothing in the transcript explains.
CEREBRAS_USER_AGENT = "mh-bench/1.0 (stdlib urllib; harness-ablation benchmark)"

# --- per-model serving specification -----------------------------------------
#
# The suite's own defaults (32768 / 4096 / 0.6 / 0.95 / top_k 20) were chosen for
# the local ollama arms and are what every frozen cell ran under. They are wrong
# for a hosted model in ways that do not announce themselves: `max_tokens` is not
# a parameter Cerebras accepts at all, reasoning tokens are billed against
# `max_completion_tokens` on gpt-oss so a 4096 ceiling is spent thinking rather
# than answering, and Gemma 4 is documented to want a different temperature from
# the one the local arms use.
#
# So a model carries its own defaults, and LOCAL is the fallback -- byte-identical
# to what run.py used to hard-code, so no local cell changes because this table
# exists. Anything given on the command line still wins over both.
#
# Every value below is from Cerebras' own documentation, read 2026-08-25:
#   https://inference-docs.cerebras.ai/models/overview
#   https://inference-docs.cerebras.ai/models/openai-oss
#   https://inference-docs.cerebras.ai/models/gemma-4-31b
#   https://inference-docs.cerebras.ai/api-reference/chat-completions
#   https://inference-docs.cerebras.ai/support/rate-limits
LOCAL_SPEC = {
    "num_ctx": 32768,
    "num_predict": 4096,
    "temperature": 0.6,
    "top_p": 0.95,
    "top_k": 20,
    # think -> reasoning effort. Local ollama takes a bool, not an effort level.
    "effort": {True: None, False: None},
}

MODEL_SPECS = {
    "cerebras:gpt-oss-120b": {
        # 65k on free trial, 131k on Developer. 65k is the floor across tiers,
        # so a cell configured here cannot be refused for context on either --
        # and it stays a power of two above the 32768 the local arms use.
        "num_ctx": 65536,
        # Documented max output: 32k free / 40k paid. 16384 is well inside both.
        # It is 4x the local arms' 4096 on purpose: on this model
        # max_completion_tokens covers reasoning tokens as well as the answer,
        # and at the default "medium" effort a 4096 ceiling truncates mid-thought
        # -- which the harness scores as a truncated turn, not as a model failure.
        "num_predict": 16384,
        # Cerebras publishes no recommended sampling values for this model
        # ("standard sampling controls are supported, including temperature,
        # top_p, frequency_penalty, presence_penalty, seed, and logit_bias").
        # With no vendor recommendation to follow, hold the suite's values so the
        # arm differs from the local arms in as few respects as possible.
        "temperature": 0.6,
        "top_p": 0.95,
        # Not sent: top_k is absent from the documented parameter set. A declared
        # deviation, surfaced by the client at construction, not silently dropped.
        "top_k": None,
        # Vendor default is "medium"; --no-think drops to "low". "none" is not
        # used here because a non-reasoning gpt-oss is a different model to
        # compare, not this one with a flag off.
        "effort": {True: "medium", False: "low"},
        "notes": "120B. ~3000 tok/s. $0.35/M in, $0.75/M out. "
                 "Rejects `tools` and `response_format` in the same request; "
                 "may call tools that were not offered.",
    },
    "cerebras:gemma-4-31b": {
        "num_ctx": 65536,
        # Reasoning is off unless asked for, so the ceiling covers the answer
        # alone and does not need gpt-oss's headroom.
        "num_predict": 8192,
        # Vendor recommendation, verbatim: "Recommended starting parameters:
        # temperature=1.0, top_p=0.95." This is the one place a Cerebras arm
        # deliberately departs from the suite's temperature, and the departure
        # is stamped into every episode's protocol block.
        "temperature": 1.0,
        "top_p": 0.95,
        "top_k": None,
        # "Reasoning is disabled by default -- use the reasoning_effort parameter
        # to enable it." False therefore means "send nothing and take the
        # documented default", not "send an effort level that means off".
        "effort": {True: "medium", False: None},
        "notes": "31B dense, multimodal. ~1850 tok/s. $0.99/M in, $1.49/M out. "
                 "Parallel tool calling and constrained decoding supported.",
    },
}


def model_spec(model):
    """Serving defaults for `model`. Unknown models get the local suite's."""
    spec = dict(LOCAL_SPEC)
    spec.update(MODEL_SPECS.get(model, {}))
    return spec


def reasoning_effort_for(model, think):
    """The reasoning effort a model runs at, or None when it takes no such knob."""
    return model_spec(model)["effort"].get(bool(think))

# Reasoning has shipped under all three of these names on servers on this machine:
# ollama native /api/chat -> "thinking"; ollama /v1 -> "reasoning";
# omlx /v1 -> "reasoning_content".
REASONING_FIELDS = ("thinking", "reasoning_content", "reasoning")


# A chat response larger than this is a broken server, not a long answer.
MAX_RESPONSE_BYTES = 64 * 1024 * 1024


class ModelError(RuntimeError):
    pass


class AccountRefused(ModelError):
    """The provider refused the account, not the request.

    401 (bad key), 402 (quota or billing), 403 (forbidden). None of these
    becomes true by being retried, and none of them is a fact about the model.

    This exists because of what happened without it. A Cerebras key hit its
    quota partway through a grid, and every request after that returned
    `HTTP 402: Payment required`. The runner scored each one as an ordinary
    serving failure and carried on: 160 episodes of `no-envboot` and 154 of
    `no-verifygate` were written as 0-token failures, and both cells would have
    published a 0.0% pass rate that reads exactly like an ablation destroying
    the model. Nothing in the transcript distinguished that from a real result.

    So an account refusal stops the run instead of filling it. The episode is
    not written, the cell is aborted, and the grid stops rather than stepping
    to the next cell to fail the same way.
    """


class Reply:
    """One assistant turn, normalised."""

    __slots__ = ("content", "reasoning", "tool_calls", "raw",
                 "prompt_tokens", "output_tokens", "latency_s", "done_reason",
                 "eval_duration_ns", "prompt_eval_duration_ns")

    def __init__(self, content, reasoning, tool_calls, raw,
                 prompt_tokens, output_tokens, latency_s, done_reason,
                 eval_duration_ns=0, prompt_eval_duration_ns=0):
        self.content = content
        self.reasoning = reasoning
        self.tool_calls = tool_calls
        self.raw = raw
        self.prompt_tokens = prompt_tokens
        self.output_tokens = output_tokens
        self.latency_s = latency_s
        self.done_reason = done_reason
        self.eval_duration_ns = eval_duration_ns or 0
        self.prompt_eval_duration_ns = prompt_eval_duration_ns or 0

    @property
    def truncated(self):
        # ollama reports "length" when num_predict was the binding constraint.
        return self.done_reason == "length"


def _coerce_args(args):
    """Tool arguments arrive as a parsed dict on ollama native, but as a JSON
    string on OpenAI-compatible endpoints, and occasionally as a double-encoded
    string when a model emits JSON inside JSON."""
    seen = 0
    while isinstance(args, str) and seen < 3:
        stripped = args.strip()
        if not stripped:
            return {}
        try:
            args = json.loads(stripped)
        except json.JSONDecodeError:
            # A model sometimes emits a bare value for a single-parameter tool.
            return {"_raw": args}
        seen += 1
    return args if isinstance(args, dict) else {"_raw": args}


def _extract_reasoning(msg):
    for field in REASONING_FIELDS:
        val = msg.get(field)
        if isinstance(val, str) and val.strip():
            return val
    return ""


def _extract_tool_calls(msg):
    calls = []
    for i, tc in enumerate(msg.get("tool_calls") or []):
        fn = tc.get("function") or {}
        name = fn.get("name") or ""
        if not name:
            continue
        calls.append({
            "id": tc.get("id") or f"call_{i}",
            "name": name,
            "args": _coerce_args(fn.get("arguments")),
        })
    if not calls and msg.get("content"):
        c = msg["content"].strip()
        if c.startswith("{") and c.endswith("}"):
            try:
                parsed = json.loads(c)
                if isinstance(parsed, dict) and "name" in parsed:
                    calls.append({
                        "id": "call_0",
                        "name": parsed["name"],
                        "args": _coerce_args(parsed.get("arguments") or parsed.get("parameters") or {}),
                    })
            except Exception:
                pass
        elif "<tool_call>" in c:
            import re
            for i, match in enumerate(re.finditer(r"<tool_call>\s*(\{.*?\})\s*</tool_call>", c, re.DOTALL)):
                try:
                    parsed = json.loads(match.group(1))
                    if isinstance(parsed, dict) and "name" in parsed:
                        calls.append({
                            "id": f"call_{i}",
                            "name": parsed["name"],
                            "args": _coerce_args(parsed.get("arguments") or parsed.get("parameters") or {}),
                        })
                except Exception:
                    pass
    return calls


def env_local_paths(root=None):
    """Every .env.local this checkout should read, nearest first.

    A linked git worktree does not carry the main checkout's untracked files,
    and .env.local is gitignored, so a run from `.claude/worktrees/<name>/bench`
    could not see the key sitting at the main repo root. It reported "no
    credential found" -- correct about what it looked at, wrong about what the
    machine had. The `.git` of a linked worktree is a file holding
    `gitdir: <main>/.git/worktrees/<name>`, so the main checkout's root is
    derivable rather than guessed at.
    """
    if root is None:
        root = os.path.dirname(os.path.dirname(os.path.dirname(
            os.path.abspath(__file__))))
    paths = [os.path.join(root, ".env.local")]
    dotgit = os.path.join(root, ".git")
    if os.path.isfile(dotgit):
        try:
            with open(dotgit) as f:
                for line in f:
                    if line.startswith("gitdir:"):
                        gitdir = line.split(":", 1)[1].strip()
                        marker = os.sep + ".git" + os.sep + "worktrees" + os.sep
                        if marker in gitdir:
                            main_root = gitdir.split(marker, 1)[0]
                            paths.append(os.path.join(main_root, ".env.local"))
                        break
        except OSError:
            pass
    return paths


def _credential(*names):
    """First of `names` found in the environment, else in a .env.local.

    One reader for every provider: the Gemini-only version was about to be
    copied for Cerebras, and two copies is how one of them stops looking in
    .env.local. Never logged, never written into an episode -- only the
    provider and endpoint are recorded, never the key.
    """
    for name in names:
        val = os.environ.get(name)
        if val:
            return val.strip()
    wanted = tuple(f"{n}=" for n in names)
    for env_local in env_local_paths():
        if not os.path.isfile(env_local):
            continue
        try:
            with open(env_local) as f:
                for line in f:
                    line = line.strip()
                    if line.startswith("#"):
                        continue
                    for prefix in wanted:
                        if line.startswith(prefix):
                            return line.split("=", 1)[1].strip().strip("\"'")
        except OSError:
            continue
    return None


def _get_gemini_api_key():
    return _credential("GEMINI_API_KEY", "GOOGLE_API_KEY")


def _get_cerebras_api_key():
    return _credential("CEREBRAS_API_KEY")


class GeminiClient:
    """Speaks Google Gemini Interactions SSE API."""

    def __init__(self, model="gemini-3.7-flash", temperature=0.6, top_p=0.95,
                 top_k=20, num_ctx=32768, num_predict=16384, think=True,
                 timeout=1800, seed=None, reasoning_effort=None):
        if model == "live-gemini":
            model = "gemini-3.7-flash"
        self.model = model
        self.temperature = temperature
        self.num_ctx = num_ctx
        self.num_predict = num_predict or 16384
        self.timeout = timeout
        self.seed = seed
        # The endpoint takes a discrete thinking level, not a temperature.
        if reasoning_effort is None:
            self.thinking_level = "high" if think else "low"
        elif reasoning_effort in ("low", "high"):
            self.thinking_level = reasoning_effort
        else:
            raise ModelError(
                f"thinking_level on this endpoint is 'low' or 'high'; got "
                f"reasoning_effort={reasoning_effort!r}")
        self.api_key = _get_gemini_api_key()
        if not self.api_key:
            raise ModelError("No Gemini credential found (set GEMINI_API_KEY or .env.local)")
        self.url = "https://generativelanguage.googleapis.com/v1beta/interactions?alt=sse"
        self.options = {
            "temperature": temperature,
            "num_ctx": num_ctx,
            "num_predict": self.num_predict,
        }
        if seed is not None:
            self.options["seed"] = seed

    # This endpoint returns HTTP 500 on a large fraction of substantial
    # tool-call generations -- measured at roughly 71% of requests for
    # gemini-3.7-flash, and not rate limiting (45 s spacing did not help).
    # Sibling models on the same endpoint show no 500s at all. The retry budget
    # below is sized for that measured failure rate rather than for a healthy
    # service; without it a multi-step episode almost never finishes.
    # Measured: 45 s spacing between attempts did NOT improve the success rate
    # (2/6) over back-to-back attempts (1/5). These 500s are not load-related, so
    # backing off buys nothing -- it only converts a fast failure into a slow one.
    # An earlier 60 s-cap backoff spent 57 minutes of a single episode asleep for
    # 76 output tokens. Retry quickly and often instead; failed attempts return no
    # usage block and so cost wall-clock rather than tokens.
    # This endpoint rejects every generation_config key except thinking_level
    # (measured: adding any other key returns HTTP 500), so a --seed cannot be
    # sent. Episodes from this arm must not record one: a seed that never
    # reached the model would otherwise be indistinguishable, on disk, from one
    # that did.
    ACCEPTS_SEED = False

    DEFAULT_RETRIES = 16
    BACKOFF_CAP_S = 5.0

    def chat(self, messages, tools=None, retries=None):
        if retries is None:
            retries = self.DEFAULT_RETRIES
        # The wire shape below is not guessed. It matches a recorded capture of a
        # working MANVI run against this endpoint (bench/results/live-gemini/
        # gemini-wire.log): 1371 function_call items paired 1:1 with 1371
        # function_result items, every function_call carrying a non-empty
        # thought signature, and store=false throughout.
        #
        # The previous version of this method emitted function_result entries
        # with no preceding function_call and no signature, so the server saw
        # results for calls it had never been told about. The model lost its own
        # action history every turn and degenerated into prose, which the harness
        # then scored as no_tool_call. That is what produced the 315-episode
        # gemini arm with zero `finished` stops.
        system_instruction = ""
        call_names = {}
        for msg in messages:
            if msg.get("role") == "assistant":
                for tc in msg.get("tool_calls") or []:
                    cid = tc.get("id") or "call_0"
                    fn = tc.get("function") or {}
                    call_names[cid] = fn.get("name") or tc.get("name") or "tool"

        wire_input = []
        for msg in messages:
            role = msg.get("role")
            content = msg.get("content") or ""
            if role == "system":
                system_instruction = content
            elif role == "user":
                if content:
                    wire_input.append({
                        "type": "user_input",
                        "content": [{"type": "text", "text": content}]
                    })
            elif role == "assistant":
                calls = msg.get("tool_calls") or []
                # A turn that only talked still belongs in the history; a turn
                # that acted is represented by its function_call items, which is
                # how the recorded working traffic carries assistant state.
                if content and not calls:
                    wire_input.append({
                        "type": "model_output",
                        "content": [{"type": "text", "text": content}]
                    })
                for i, tc in enumerate(calls):
                    fn = tc.get("function") or {}
                    args = fn.get("arguments", tc.get("args"))
                    if isinstance(args, str):
                        args = _coerce_args(args)
                    if not isinstance(args, dict):
                        args = {}
                    item = {
                        "type": "function_call",
                        "id": tc.get("id") or f"call_{i}",
                        "name": fn.get("name") or tc.get("name") or "tool",
                        "arguments": args,
                    }
                    # Required by this endpoint for multi-turn tool use: every
                    # function_call in the recorded capture carries one. Omitting
                    # it drops the model's own reasoning thread between turns.
                    sig = tc.get("signature")
                    if sig:
                        item["signature"] = sig
                    wire_input.append(item)
            elif role == "tool":
                call_id = msg.get("tool_call_id") or "call_0"
                name = msg.get("name") or call_names.get(call_id) or "tool"
                wire_input.append({
                    "type": "function_result",
                    "call_id": call_id,
                    "name": name,
                    "result": [{"type": "text", "text": str(content)}]
                })

        wire_tools = []
        if tools:
            for t in tools:
                fn = t.get("function") or t
                wire_tools.append({
                    "type": "function",
                    "name": fn.get("name"),
                    "description": fn.get("description", ""),
                    "parameters": fn.get("parameters", {}),
                })

        # generation_config carries thinking_level and NOTHING else.
        #
        # This is not a style choice. Measured against the live endpoint on a
        # generation large enough to matter (a ~100-line file written through a
        # tool call):
        #
        #   {"temperature":.., "max_output_tokens":..}   -> HTTP 500
        #   {"max_output_tokens":..}                     -> HTTP 500
        #   {}  / omitted                                -> HTTP 500, or a
        #                                                   completed interaction
        #                                                   with 0 output tokens
        #   {"thinking_level":"high"}                    -> 1 tool call, 5511 out tok
        #   {"thinking_level":"low"}                     -> 1 tool call, 5069 out tok
        #   {"thinking_level":"high","max_output_tokens":..} -> HTTP 500
        #
        # It also matches the recorded working capture, where 302 of 303
        # requests sent exactly {"thinking_level": ...} and nothing else.
        #
        # The cost is that this arm cannot honour the suite's temperature or
        # num_predict. That is a declared protocol deviation, not an oversight:
        # the endpoint rejects both.
        body = {
            "model": self.model,
            "input": wire_input,
            "system_instruction": system_instruction,
            "store": False,
            "stream": True,
            "generation_config": {
                "thinking_level": self.thinking_level,
            }
        }
        if wire_tools:
            body["tools"] = wire_tools

        payload = json.dumps(body).encode()
        headers = {
            "Content-Type": "application/json",
            "X-Goog-Api-Key": self.api_key,
        }

        last = None
        for attempt in range(retries + 1):
            req = urllib.request.Request(self.url, data=payload, headers=headers)
            t0 = time.time()
            thought_sig = ""
            content_parts = []
            call_id = None
            call_name = None
            call_args = ""
            tool_calls = []
            total_in = 0
            total_out = 0
            saw_completion = False

            try:
                with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                    for line in resp:
                        s = line.decode("utf-8", "replace").strip()
                        if not s.startswith("data: "):
                            continue
                        data_str = s[6:].strip()
                        if not data_str:
                            continue
                        if data_str == "[DONE]":
                            saw_completion = True
                            break
                        try:
                            ev = json.loads(data_str)
                        except Exception:
                            continue

                        if ev.get("error"):
                            err_msg = ev["error"].get("message") or "API error"
                            raise ModelError(f"Gemini API error: {err_msg}")

                        ev_type = ev.get("event_type")
                        if ev_type == "error":
                            err_msg = (ev.get("error") or {}).get("message") or "API error"
                            raise ModelError(f"Gemini API error: {err_msg}")

                        delta = ev.get("delta") or {}
                        step = ev.get("step") or {}
                        interaction = ev.get("interaction") or {}

                        if delta.get("type") == "thought_signature":
                            thought_sig = delta.get("signature") or ""

                        if step.get("type") == "function_call":
                            if call_name:
                                tool_calls.append({
                                    "id": call_id or f"call_{len(tool_calls)}",
                                    "name": call_name,
                                    "args": _coerce_args(call_args),
                                    "signature": thought_sig,
                                })
                            call_id = step.get("id")
                            call_name = step.get("name")
                            call_args = ""

                        if delta.get("type") == "arguments_delta" or delta.get("arguments"):
                            call_args += str(delta.get("arguments") or "")

                        if delta.get("text"):
                            content_parts.append(str(delta["text"]))

                        if ev_type == "step.stop" and call_name:
                            tool_calls.append({
                                "id": call_id or f"call_{len(tool_calls)}",
                                "name": call_name,
                                "args": _coerce_args(call_args),
                                "signature": thought_sig,
                            })
                            call_id = None
                            call_name = None
                            call_args = ""

                        if ev_type == "interaction.completed":
                            usage = interaction.get("usage") or {}
                            total_in = usage.get("total_input_tokens") or usage.get("total_tokens") or 0
                            total_out = usage.get("total_output_tokens") or 0
                            saw_completion = True
                            break

                if call_name:
                    tool_calls.append({
                        "id": call_id or f"call_{len(tool_calls)}",
                        "name": call_name,
                        "args": _coerce_args(call_args),
                        "signature": thought_sig,
                    })

                # A completed interaction that produced no tool call, no text
                # and zero output tokens is a degenerate response, not the model
                # electing to stay silent. Returning it as an empty Reply makes
                # the harness see "no tool call", nudge, get another empty, and
                # stop the episode with no_tool_call -- which is how a transient
                # API degeneracy becomes a scored harness failure. The recorded
                # working capture has zero such responses in 263 completions
                # (minimum output was 13 tokens), so treating this as retryable
                # matches what a healthy stream looks like.
                content_text = "".join(content_parts)
                if not tool_calls and not content_text.strip() and not total_out:
                    last = ModelError(
                        "empty interaction: completed with no tool call, no text "
                        "and 0 output tokens")
                    if attempt < retries:
                        base = min(self.BACKOFF_CAP_S, 0.5 * 2.0 ** attempt)
                        time.sleep(base * (0.5 + random.random() * 0.5))
                        continue
                    raise last

                return Reply(
                    content=content_text,
                    reasoning=thought_sig,
                    tool_calls=tool_calls,
                    raw={"stream_completed": saw_completion},
                    prompt_tokens=total_in,
                    output_tokens=total_out,
                    latency_s=time.time() - t0,
                    done_reason="stop" if not tool_calls else "tool_calls",
                )
            except urllib.error.HTTPError as e:
                detail = e.read()[:400].decode("utf-8", "replace")
                last = ModelError(f"HTTP {e.code}: {detail}")
                # Only raise non-retryable 4xx client errors (excluding 429 rate-limit)
                if e.code != 429 and e.code != 500 and e.code != 503 and 400 <= e.code < 500:
                    raise last
            except (urllib.error.URLError, TimeoutError, OSError, ModelError) as e:
                last = e if isinstance(e, ModelError) else ModelError(f"{type(e).__name__}: {e}")
            if attempt < retries:
                # Jitter matters here: a grid runs cells back to back against the
                # same endpoint, and a bare power-of-two backoff re-synchronises
                # every retry onto the same instants.
                base = min(self.BACKOFF_CAP_S, 0.5 * 2.0 ** attempt)
                time.sleep(base * (0.5 + random.random() * 0.5))
        else:
            raise last or ModelError("chat failed")


def _openai_messages(messages):
    """The harness's message list on the OpenAI wire.

    Two shape differences matter and both fail quietly if missed. Tool-call
    arguments travel as a JSON *string*, not the dict the harness holds -- a
    dict is rejected by the schema. And `signature` is a Gemini-only field the
    harness carries on every assistant tool call; sending it to an endpoint
    that does not know it risks a 400 on a request that is otherwise fine.
    """
    out = []
    for msg in messages:
        role = msg.get("role")
        content = msg.get("content") or ""
        if role == "assistant":
            wire = {"role": "assistant", "content": content}
            calls = []
            for i, tc in enumerate(msg.get("tool_calls") or []):
                fn = tc.get("function") or {}
                args = fn.get("arguments", tc.get("args"))
                if not isinstance(args, str):
                    args = json.dumps(args if isinstance(args, dict) else {})
                calls.append({
                    "id": tc.get("id") or f"call_{i}",
                    "type": "function",
                    "function": {
                        "name": fn.get("name") or tc.get("name") or "tool",
                        "arguments": args,
                    },
                })
            if calls:
                wire["tool_calls"] = calls
            out.append(wire)
        elif role == "tool":
            # No `name` key: the pairing is by tool_call_id, and the field is
            # deprecated on this role.
            out.append({"role": "tool",
                        "tool_call_id": msg.get("tool_call_id") or "call_0",
                        "content": str(content)})
        else:
            out.append({"role": role, "content": content})
    return out


class CerebrasClient:
    """Speaks the Cerebras OpenAI-compatible Chat Completions API.

    Non-streaming on purpose: the harness consumes a whole turn at a time, and
    the Gemini arm's SSE parser exists only because that endpoint has no
    non-streaming mode. There is nothing to gain here from re-deriving one.

    Two protocol deviations from the local arms, both declared rather than
    absorbed:

    - `top_k` is not in the documented parameter set and is not sent. The local
      arms run at top_k=20.
    - `seed` is documented as best-effort ("For deterministic sampling"), not as
      a determinism guarantee, and the serving stack behind the endpoint can
      change under a fixed model id. A pinned seed makes this arm *replayable in
      intent*, not bit-reproducible the way a pinned local build is.
    """

    # Sized for a rate limiter, not for a broken service: a 429 is expected
    # traffic shaping on the free tier (5 RPM) and the endpoint tells us how
    # long to wait. 5xx gets the same budget with jittered exponential backoff.
    # Documented as "for deterministic sampling (best-effort)" -- sent, and so
    # recorded, but see the class docstring on what that does and does not buy.
    ACCEPTS_SEED = True

    DEFAULT_RETRIES = 6
    BACKOFF_CAP_S = 30.0
    # A Retry-After longer than this is not worth honouring inside an episode:
    # the sleep comes out of --max-wall, and an episode that times out while
    # asleep is scored as a failure of the model. Fail fast and let the row say
    # rate_limited instead of burying it in a wall timeout.
    RETRY_AFTER_CAP_S = 60.0

    def __init__(self, model, host=None, temperature=0.6, top_p=0.95,
                 top_k=None, num_ctx=65536, num_predict=16384, think=True,
                 timeout=1800, seed=None, reasoning_effort=None):
        self.model = api_model_id(model)
        self.qualified = model
        self.timeout = timeout
        self.think = think
        spec = model_spec(model)
        self.reasoning_effort = (reasoning_effort if reasoning_effort is not None
                                 else spec["effort"].get(bool(think)))
        self.api_key = _get_cerebras_api_key()
        if not self.api_key:
            raise ModelError(
                "No Cerebras credential found. Set CEREBRAS_API_KEY in the "
                "environment, or add a CEREBRAS_API_KEY= line to .env.local at "
                "the repo root. Keys are issued at cloud.cerebras.ai.")
        self.url = CEREBRAS_URL
        # `options` is the interface mh.harness reads num_ctx from and run.py
        # writes the per-repeat seed into, so the seed is read from it at request
        # time rather than captured here. The Gemini arm cannot do this at all --
        # its endpoint rejects every generation_config key except thinking_level,
        # so no seed reaches the wire and ACCEPTS_SEED is False there.
        self.options = {
            "temperature": temperature,
            "top_p": top_p,
            "num_ctx": num_ctx,
            "num_predict": num_predict,
        }
        if seed is not None:
            self.options["seed"] = seed
        if top_k is not None:
            print(f"[cerebras] top_k={top_k} is not a documented parameter of "
                  f"this endpoint and will not be sent; this arm runs without "
                  f"it", flush=True)

    def _body(self, messages, tools):
        opts = self.options
        body = {
            "model": self.model,
            "messages": _openai_messages(messages),
            "stream": False,
            "temperature": opts["temperature"],
            "top_p": opts["top_p"],
            # `max_tokens` is not accepted by this endpoint. On gpt-oss this
            # ceiling covers reasoning tokens as well as the reply.
            "max_completion_tokens": opts["num_predict"],
        }
        if opts.get("seed") is not None:
            body["seed"] = opts["seed"]
        if self.reasoning_effort:
            body["reasoning_effort"] = self.reasoning_effort
        if tools:
            # Already in OpenAI tool shape where mh.harness builds it, so it
            # goes on the wire unchanged. `response_format` is deliberately
            # never set: gpt-oss-120b rejects any request carrying both.
            body["tools"] = tools
            body["tool_choice"] = "auto"
        return body

    def chat(self, messages, tools=None, retries=None):
        if retries is None:
            retries = self.DEFAULT_RETRIES
        payload = json.dumps(self._body(messages, tools)).encode()
        headers = {"Content-Type": "application/json",
                   "User-Agent": CEREBRAS_USER_AGENT,
                   "Authorization": f"Bearer {self.api_key}"}
        last = None
        rate_limited = 0
        for attempt in range(retries + 1):
            req = urllib.request.Request(self.url, data=payload, headers=headers)
            t0 = time.time()
            sleep_s = None
            try:
                with urllib.request.urlopen(req, timeout=self.timeout) as r:
                    body_bytes = r.read(MAX_RESPONSE_BYTES + 1)
                if len(body_bytes) > MAX_RESPONSE_BYTES:
                    raise ModelError(f"response exceeded {MAX_RESPONSE_BYTES} bytes")
                try:
                    raw = json.loads(body_bytes)
                except ValueError as e:
                    raise ModelError(f"malformed JSON from {self.url}: {e}; "
                                     f"body starts {body_bytes[:120]!r}")
                if not isinstance(raw, dict):
                    raise ModelError(f"expected a JSON object from {self.url}, "
                                     f"got {type(raw).__name__}")
                reply = self._reply(raw, t0, rate_limited)
                if reply is not None:
                    return reply
                # Degenerate completion: no call, no text, no output tokens.
                # Same judgement as the Gemini arm -- a transient API degeneracy
                # scored as no_tool_call is a harness failure attributed to the
                # model.
                last = ModelError("empty completion: no tool call, no text and "
                                  "0 completion tokens")
            except urllib.error.HTTPError as e:
                detail = e.read()[:400].decode("utf-8", "replace")
                last = ModelError(f"HTTP {e.code}: {detail}")
                if e.code in (401, 402, 403):
                    # Not this request's problem and not the model's. Every
                    # later episode fails identically; see AccountRefused.
                    raise AccountRefused(f"HTTP {e.code}: {detail}")
                if e.code == 429:
                    rate_limited += 1
                    sleep_s = self._retry_after(e)
                elif 400 <= e.code < 500:
                    # A contract error. Retrying re-sends the same body.
                    raise last
            except (urllib.error.URLError, TimeoutError, OSError, ModelError) as e:
                last = e if isinstance(e, ModelError) else ModelError(f"{type(e).__name__}: {e}")
                if isinstance(e, TimeoutError) or "timed out" in str(e).lower():
                    # The episode's own wall clock is the budget; re-sending a
                    # 30-minute request cannot fit inside what is left of it.
                    raise last
            if attempt >= retries:
                break
            if sleep_s is None:
                sleep_s = min(self.BACKOFF_CAP_S, 0.5 * 2.0 ** attempt)
                sleep_s *= 0.5 + random.random() * 0.5
            time.sleep(sleep_s)
        raise last or ModelError("chat failed")

    def _retry_after(self, err):
        """Honour Retry-After, but never past RETRY_AFTER_CAP_S."""
        try:
            hdr = err.headers.get("Retry-After") if err.headers else None
        except Exception:
            hdr = None
        try:
            wait = float(hdr)
        except (TypeError, ValueError):
            return None
        if wait > self.RETRY_AFTER_CAP_S:
            raise ModelError(
                f"rate limited with Retry-After={wait:.0f}s, above this "
                f"client's {self.RETRY_AFTER_CAP_S:.0f}s cap; refusing to spend "
                f"the episode's wall clock asleep")
        return max(0.0, wait)

    def _reply(self, raw, t0, rate_limited):
        """A Reply, or None when the completion was degenerate and retryable."""
        choices = raw.get("choices") or []
        if not choices or not isinstance(choices[0], dict):
            raise ModelError(f"no choices in response: {str(raw)[:200]}")
        choice = choices[0]
        msg = choice.get("message")
        if not isinstance(msg, dict):
            raise ModelError(f"expected an object for 'message', got "
                             f"{type(msg).__name__}")
        usage = raw.get("usage") or {}
        out_tok = usage.get("completion_tokens") or 0
        content = msg.get("content") or ""
        calls = _extract_tool_calls(msg)
        if not calls and not content.strip() and not out_tok:
            return None
        # The endpoint reports its own queue/prompt/completion split. These are
        # the provider's timings, exactly as decode_tok_s on a local arm is
        # ollama's -- neither is measured by us. Converted to the nanoseconds
        # mh.compute.tok_s expects so one formula serves both.
        info = raw.get("time_info") or {}
        return Reply(
            content=content,
            reasoning=_extract_reasoning(msg),
            tool_calls=calls,
            raw={"finish_reason": choice.get("finish_reason"),
                 "rate_limited": rate_limited,
                 "system_fingerprint": raw.get("system_fingerprint"),
                 "reasoning_tokens": (usage.get("completion_tokens_details") or {})
                                     .get("reasoning_tokens"),
                 "cached_prompt_tokens": (usage.get("prompt_tokens_details") or {})
                                         .get("cached_tokens")},
            prompt_tokens=usage.get("prompt_tokens") or 0,
            output_tokens=out_tok,
            latency_s=time.time() - t0,
            done_reason=choice.get("finish_reason") or "",
            eval_duration_ns=int((info.get("completion_time") or 0) * 1e9),
            prompt_eval_duration_ns=int((info.get("prompt_time") or 0) * 1e9),
        )


class Client:
    """Local ollama, or the provider `mh.runtime.api_provider` names.

    One canonical owner for the routing decision. It picks both which client is
    built and whether the runner may evict a local model for it; two copies of
    the predicate meant a drift would route a request one way and manage the
    GPU the other.
    """

    def __new__(cls, model, *args, **kwargs):
        provider = api_provider(model)
        if provider == "gemini":
            return GeminiClient(model, *args, **kwargs)
        if provider == "cerebras":
            return CerebrasClient(model, *args, **kwargs)
        return super().__new__(cls)

    ACCEPTS_SEED = True

    def __init__(self, model, host=DEFAULT_HOST, temperature=0.6, top_p=0.95,
                 top_k=20, num_ctx=32768, num_predict=4096, think=True,
                 timeout=1800, seed=None, reasoning_effort=None):
        if reasoning_effort is not None:
            # ollama takes a boolean `think`, not an effort level. Accepting the
            # argument and ignoring it would let the protocol stamp record an
            # effort the model never ran at.
            raise ModelError(
                f"reasoning_effort={reasoning_effort!r} is not a setting local "
                f"ollama has; use --think / --no-think for {model}")
        self.model = model
        self.host = host.rstrip("/")
        self.think = think
        self.timeout = timeout
        self.http_timeout = timeout
        self.options = {
            "temperature": temperature,
            "top_p": top_p,
            "top_k": top_k,
            "num_ctx": num_ctx,
            # num_predict is the only thing bounding a runaway generation. Raising it
            # to rescue a looping turn just buys a longer loop -- measured, twice.
            "num_predict": num_predict,
        }
        if seed is not None:
            self.options["seed"] = seed

    def chat(self, messages, tools=None, retries=2):
        body = {
            "model": self.model,
            "messages": messages,
            "stream": False,
            "options": dict(self.options),
        }
        if tools:
            body["tools"] = tools
        if self.think is not None:
            body["think"] = self.think

        payload = json.dumps(body).encode()
        last = None
        for attempt in range(retries + 1):
            req = urllib.request.Request(
                self.host + "/api/chat", data=payload,
                headers={"Content-Type": "application/json"})
            t0 = time.time()
            try:
                with urllib.request.urlopen(req, timeout=self.timeout) as r:
                    body_bytes = r.read(MAX_RESPONSE_BYTES + 1)
                if len(body_bytes) > MAX_RESPONSE_BYTES:
                    raise ModelError(
                        f"response exceeded {MAX_RESPONSE_BYTES} bytes")
                try:
                    raw = json.loads(body_bytes)
                except ValueError as e:
                    raise ModelError(
                        f"malformed JSON from {self.host}: {e}; "
                        f"body starts {body_bytes[:120]!r}")
                if not isinstance(raw, dict):
                    raise ModelError(
                        f"expected a JSON object from {self.host}, got "
                        f"{type(raw).__name__}: {body_bytes[:120]!r}")
                break
            except urllib.error.HTTPError as e:
                detail = e.read()[:400].decode("utf-8", "replace")
                last = ModelError(f"HTTP {e.code}: {detail}")
                if e.code == 400 and "does not support thinking" in detail and self.think is not None:
                    self.think = None
                    if "think" in body:
                        del body["think"]
                    payload = json.dumps(body).encode()
                    continue
                # 4xx other than 429 is a contract error; retrying cannot fix it and
                # a cached-failure 409 will burn every attempt.
                if e.code != 429 and 400 <= e.code < 500:
                    raise last
            except ModelError as e:
                # A malformed body is the server contradicting its contract.
                # Retrying re-reads the same broken proxy; surface it instead
                # of burning the budget and then reporting a timeout.
                raise e
            except (urllib.error.URLError, TimeoutError, OSError) as e:
                last = ModelError(f"{type(e).__name__}: {e}")
                timed_out = (
                    isinstance(e, TimeoutError)
                    or "timed out" in str(e).lower())
                if timed_out:
                    raise last
            if attempt < retries:
                time.sleep(2 ** attempt)
        else:
            raise last or ModelError("chat failed")

        msg = raw.get("message") or {}
        if not isinstance(msg, dict):
            raise ModelError(
                f"expected an object for 'message', got {type(msg).__name__}")
        return Reply(
            content=msg.get("content") or "",
            reasoning=_extract_reasoning(msg),
            tool_calls=_extract_tool_calls(msg),
            raw=raw,
            prompt_tokens=raw.get("prompt_eval_count") or 0,
            output_tokens=raw.get("eval_count") or 0,
            latency_s=time.time() - t0,
            done_reason=raw.get("done_reason") or "",
            eval_duration_ns=raw.get("eval_duration") or 0,
            prompt_eval_duration_ns=raw.get("prompt_eval_duration") or 0,
        )
