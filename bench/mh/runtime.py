"""Model residency control.

On a 64 GB Mac, a 38 GB Q8 MoE and an 18 GB dense model cannot coexist;
the runner evicts everything else and refuses to start if it cannot.
On GH200 (~96 GiB HBM) two models fit; `--share-gpu` allows exactly one
peer so Qwen and Ornith can decode at the same time. A third model is
still refused.
"""
import json
import subprocess
import time
import urllib.request

HOST = "http://127.0.0.1:11434"
# One peer besides `model`. Qwen 27B Q4 (~21 GiB resident) + Ornith 35B Q8
# (~39 GiB weights) leaves HBM headroom; three would not.
SHARE_GPU_MAX_PEERS = 1


def resident_models(host=HOST):
    """Models currently loaded in memory. [] also means 'server unreachable'."""
    try:
        with urllib.request.urlopen(host + "/api/ps", timeout=5) as r:
            return [m.get("name") or m.get("model")
                    for m in (json.loads(r.read()).get("models") or [])]
    except Exception:
        return []


def blocking_residents(model, resident, share_gpu=False):
    """Peers that must not remain if this model is to run."""
    others = [m for m in resident if m != model]
    if not share_gpu:
        return others
    return others[SHARE_GPU_MAX_PEERS:]


def server_up(host=HOST):
    try:
        with urllib.request.urlopen(host + "/api/version", timeout=5) as r:
            json.loads(r.read())
        return True
    except Exception:
        return False


def unload_all(host=HOST, keep=None, settle_s=30):
    """Evict every resident model except `keep`. Returns what it evicted."""
    evicted = []
    for name in resident_models(host):
        if keep and name == keep:
            continue
        try:
            subprocess.run(["ollama", "stop", name], capture_output=True, timeout=120)
            evicted.append(name)
        except Exception:
            pass
    for _ in range(settle_s):
        if not [m for m in resident_models(host) if not keep or m != keep]:
            break
        time.sleep(1)
    return evicted


def is_api_model(model):
    return (model.startswith("gemini") or 
            model.startswith("models/gemini") or 
            model == "live-gemini")


def ensure_sole_tenant(model, host=HOST, evict=True, share_gpu=False):
    """Make `model` runnable, or raise.

    Distinguishes 'nothing else is loaded' from 'I could not tell', because a
    server we cannot reach must not read as a clear machine.

    share_gpu: do not evict a single peer (GH200 Qwen+Ornith). Still refuse a
    third model. On Mac this flag must stay off.
    """
    if is_api_model(model):
        return []
    if not server_up(host):
        raise RuntimeError(f"ollama at {host} is not reachable; cannot confirm "
                           f"that no other model is resident")
    if share_gpu:
        still = blocking_residents(model, resident_models(host), share_gpu=True)
        if still:
            raise RuntimeError(
                f"refusing to start: {still} resident with share-gpu "
                f"(cap is {SHARE_GPU_MAX_PEERS} peer besides {model})")
        return []
    evicted = unload_all(host, keep=model) if evict else []
    still = blocking_residents(model, resident_models(host), share_gpu=False)
    if still:
        raise RuntimeError(f"refusing to start: {still} still resident. Two models "
                           f"at once will thrash unified memory.")
    return evicted
