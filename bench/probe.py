"""Probe the ollama chat contract: tools + thinking field names, for one model."""
import json, sys, time, urllib.request

HOST = "http://127.0.0.1:11434"

def chat(model, messages, tools=None, think=None, timeout=600):
    body = {"model": model, "messages": messages, "stream": False,
            "options": {"num_predict": 512, "temperature": 0.6, "top_p": 0.95, "top_k": 20}}
    if tools: body["tools"] = tools
    if think is not None: body["think"] = think
    req = urllib.request.Request(HOST + "/api/chat",
        data=json.dumps(body).encode(), headers={"Content-Type": "application/json"})
    t0 = time.time()
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read()), time.time() - t0

TOOLS = [{"type": "function", "function": {
    "name": "run_shell", "description": "Run a shell command and return its output.",
    "parameters": {"type": "object", "properties": {
        "cmd": {"type": "string", "description": "The command to run"}},
        "required": ["cmd"]}}}]

model = sys.argv[1]
print(f"=== {model} ===", flush=True)
for label, kw in [("plain", {}), ("think", {"think": True}),
                  ("tools+think", {"tools": TOOLS, "think": True})]:
    msgs = [{"role": "user", "content":
             "List the files in /etc using the shell tool." if "tools" in kw
             else "What is 17*23? Answer with just the number."}]
    try:
        resp, dt = chat(model, msgs, **kw)
        m = resp.get("message", {})
        print(f"[{label}] {dt:.1f}s  msg_keys={sorted(m.keys())}")
        print(f"   content={m.get('content','')[:120]!r}")
        if m.get("thinking"): print(f"   thinking[{len(m['thinking'])}]={m['thinking'][:100]!r}")
        if m.get("tool_calls"): print(f"   tool_calls={json.dumps(m['tool_calls'])[:250]}")
        print(f"   eval_count={resp.get('eval_count')} prompt_eval={resp.get('prompt_eval_count')}")
    except Exception as e:
        print(f"[{label}] ERROR {type(e).__name__}: {e}")
