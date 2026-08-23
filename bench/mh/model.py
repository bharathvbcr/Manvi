"""Ollama chat client.

Deliberately tolerant on the response side. The reasoning channel and the shape of
tool-call arguments differ between servers and between builds, and getting either
wrong fails silently -- an answer filed as reasoning is stripped and the turn
returns nothing. So we read every spelling we have ever seen and coerce arguments
from either a dict or a JSON string.
"""
import json
import random
import time
import urllib.error
import urllib.request

DEFAULT_HOST = "http://127.0.0.1:11434"

# Reasoning has shipped under all three of these names on servers on this machine:
# ollama native /api/chat -> "thinking"; ollama /v1 -> "reasoning";
# omlx /v1 -> "reasoning_content".
REASONING_FIELDS = ("thinking", "reasoning_content", "reasoning")


class ModelError(RuntimeError):
    pass


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


def _get_gemini_api_key():
    import os
    key = os.environ.get("GEMINI_API_KEY") or os.environ.get("GOOGLE_API_KEY")
    if key:
        return key
    root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    env_local = os.path.join(root, ".env.local")
    if os.path.isfile(env_local):
        with open(env_local) as f:
            for line in f:
                line = line.strip()
                if line.startswith("GEMINI_API_KEY="):
                    return line.split("=", 1)[1].strip("\"'")
                elif line.startswith("GOOGLE_API_KEY="):
                    return line.split("=", 1)[1].strip("\"'")
    return None


class GeminiClient:
    """Speaks Google Gemini Interactions SSE API."""

    def __init__(self, model="gemini-3.7-flash", temperature=0.6, top_p=0.95,
                 top_k=20, num_ctx=32768, num_predict=16384, think=True,
                 timeout=1800, seed=None):
        if model == "live-gemini":
            model = "gemini-3.7-flash"
        self.model = model
        self.temperature = temperature
        self.num_ctx = num_ctx
        self.num_predict = num_predict or 16384
        self.timeout = timeout
        self.seed = seed
        # The endpoint takes a discrete thinking level, not a temperature.
        self.thinking_level = "high" if think else "low"
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


def is_gemini_model(model):
    return (model.startswith("gemini") or 
            model.startswith("models/gemini") or 
            model == "live-gemini")


class Client:
    def __new__(cls, model, *args, **kwargs):
        if is_gemini_model(model):
            return GeminiClient(model, *args, **kwargs)
        return super().__new__(cls)

    def __init__(self, model, host=DEFAULT_HOST, temperature=0.6, top_p=0.95,
                 top_k=20, num_ctx=32768, num_predict=4096, think=True,
                 timeout=1800, seed=None):
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
                    raw = json.loads(r.read())
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
