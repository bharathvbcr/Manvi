"""Probe a model's chat contract before spending a grid on it.

    python3 probe.py qwen3.8:27b              # local ollama
    python3 probe.py cerebras:gpt-oss-120b    # hosted, needs CEREBRAS_API_KEY

For an API-served model this is the credential test: it reports whether a key
was found and where it was looked for, whether the endpoint accepts it, and
whether a real tool call round-trips. It prints usage, reasoning-token and
rate-limit facts, and never prints the key itself.
"""
import json, os, sys, time, urllib.error, urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

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

def probe_api(model):
    """One live round-trip per capability, through the client the grid uses."""
    from mh.model import CerebrasClient, ModelError, env_local_paths, model_spec
    from mh.runtime import api_provider

    spec = model_spec(model)
    print(f"=== {model} ({api_provider(model)}) ===", flush=True)
    print(f"[spec] num_ctx={spec['num_ctx']} num_predict={spec['num_predict']} "
          f"temperature={spec['temperature']} top_p={spec['top_p']} "
          f"top_k={spec['top_k']}")

    print("[credential] CEREBRAS_API_KEY in environment: "
          f"{'yes' if os.environ.get('CEREBRAS_API_KEY') else 'no'}")
    for path in env_local_paths():
        print(f"[credential] {path}: "
              f"{'present' if os.path.isfile(path) else 'absent'}")
    try:
        client = CerebrasClient(model, temperature=spec["temperature"],
                                top_p=spec["top_p"], num_ctx=spec["num_ctx"],
                                num_predict=spec["num_predict"], timeout=120,
                                seed=0)
    except ModelError as e:
        print(f"[credential] NOT FOUND -- {e}")
        return 2
    print("[credential] found (length and value not printed)")

    checks = [
        ("auth + plain completion",
         [{"role": "user", "content": "What is 17*23? Answer with just the number."}],
         None),
        ("tool call round-trip",
         [{"role": "user", "content": "List the files in /etc using the shell tool."}],
         TOOLS),
    ]
    rc = 0
    for label, msgs, tools in checks:
        try:
            reply = client.chat(msgs, tools=tools, retries=1)
        except ModelError as e:
            print(f"[{label}] FAILED  {e}")
            rc = 1
            continue
        raw = reply.raw or {}
        print(f"[{label}] {reply.latency_s:.2f}s  finish={reply.done_reason!r}  "
              f"in={reply.prompt_tokens} out={reply.output_tokens} "
              f"reasoning={raw.get('reasoning_tokens')} "
              f"cached={raw.get('cached_prompt_tokens')} "
              f"429s={raw.get('rate_limited')}")
        print(f"   fingerprint={raw.get('system_fingerprint')!r}")
        if reply.content:
            print(f"   content={reply.content[:120]!r}")
        if reply.reasoning:
            print(f"   reasoning[{len(reply.reasoning)}]="
                  f"{reply.reasoning[:100]!r}")
        if reply.tool_calls:
            print(f"   tool_calls={json.dumps(reply.tool_calls)[:250]}")
        elif tools:
            print("   NOTE: tools were offered and none was called")

    # Determinism is a preregistered pilot gate for this arm, not a nicety:
    # the analysis pairs by repeat index, and if the seed does nothing then a
    # paired-style interval is being computed over unpaired samples.
    #
    # Both halves are required. "Same seed twice -> same output" alone proves
    # nothing: a prompt the model answers near-greedily returns the same text
    # whatever the seed, which reads as perfect determinism while the seed is
    # in fact inert. The control is that a DIFFERENT seed must produce a
    # different completion. An open-ended prompt is used so there is something
    # for the sampler to vary.
    msgs = [{"role": "user", "content":
             "Write a two-sentence story about a lighthouse."}]
    try:
        client.options["seed"] = 0
        a1 = client.chat(msgs, retries=1).content.strip()
        a2 = client.chat(msgs, retries=1).content.strip()
        client.options["seed"] = 1
        b1 = client.chat(msgs, retries=1).content.strip()
        repeatable = a1 == a2
        varies = a1 != b1
        print(f"[seed] same seed twice -> "
              f"{'identical' if repeatable else 'DIFFERENT'};  "
              f"different seed -> {'different' if varies else 'IDENTICAL'}")
        if repeatable and varies:
            print("   the seed controls sampling: repeats are seeded replicates "
                  "and the paired bootstrap is valid for this arm")
        else:
            print("   GATE FAILED -- the seed does not control sampling here. "
                  "Repeats are not seeded replicates; report this arm unpaired "
                  "(see paper/extension-cerebras.md §5.1)")
            rc = 1
    except ModelError as e:
        print(f"[seed] FAILED  {e}")
        rc = 1
    return rc


model = sys.argv[1] if len(sys.argv) > 1 else ""
if not model:
    raise SystemExit(__doc__)
if model.startswith("cerebras:"):
    raise SystemExit(probe_api(model))

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
