"""Ollama chat client.

Deliberately tolerant on the response side. The reasoning channel and the shape of
tool-call arguments differ between servers and between builds, and getting either
wrong fails silently -- an answer filed as reasoning is stripped and the turn
returns nothing. So we read every spelling we have ever seen and coerce arguments
from either a dict or a JSON string.
"""
import json
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

    def chat(self, messages, tools=None, retries=5):
        system_instruction = ""
        has_tool_results = any(m.get("role") == "tool" for m in messages)
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
                if not has_tool_results and content:
                    wire_input.append({
                        "type": "model_output",
                        "content": [{"type": "text", "text": content}]
                    })
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

        body = {
            "model": self.model,
            "input": wire_input,
            "system_instruction": system_instruction,
            "store": False,
            "stream": True,
            "generation_config": {
                "temperature": self.temperature,
                "max_output_tokens": self.num_predict,
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

                return Reply(
                    content="".join(content_parts),
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
                time.sleep(min(30, 2 ** (attempt + 1)))
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
