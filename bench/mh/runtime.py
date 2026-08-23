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
STARVE_ABORT_DEFAULT = 3


def keep_existing_episode(row, force=False):
    """Skip-existing must not treat a 0-token timeout as a finished episode."""
    if force:
        return False
    if not isinstance(row, dict) or not row.get("task"):
        return False
    return not is_starved_episode(row)


def should_retry_starved(row, attempt):
    """One retry after unstick. A wedged llama-server is not a harness fail."""
    return attempt == 0 and is_starved_episode(row)


def is_starved_episode(row):
    """True when the GPU never served the model.

    The Qwen+Ornith share-gpu failure mode is: first /api/chat times out,
    steps=1, output_tokens=0, stop_reason error:ModelError. That is not a
    harness ablation. Treating it as a real fail contaminates the cell.
    """
    if not isinstance(row, dict):
        return False
    if int(row.get("output_tokens") or 0) > 0:
        return False
    if int(row.get("steps") or 0) > 1:
        return False
    stop = str(row.get("stop_reason") or "")
    errs = " ".join(str(e) for e in (row.get("errors") or []))
    blob = (stop + " " + errs).lower()
    return (
        stop.startswith("error:")
        or stop == "wall_timeout"
        or "timed out" in blob
        or "timeouterror" in blob
    )


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


def unstick_server(model, host=HOST, restart=True):
    """Kill a wedged llama-server. Next /api/chat reloads the model.

    `ollama stop` is enough when the process can still accept RPC. If the
    model is still listed, or the API is down, restart the unit. Tests pass
    restart=False so they never touch systemd.
    """
    try:
        subprocess.run(["ollama", "stop", model], capture_output=True, timeout=60)
    except Exception:
        pass
    time.sleep(1)
    if not restart:
        return
    still = model in resident_models(host)
    if server_up(host) and not still:
        return
    try:
        subprocess.run(["sudo", "-n", "systemctl", "restart", "ollama"],
                       capture_output=True, timeout=90)
    except Exception:
        pass
    for _ in range(40):
        if server_up(host):
            return
        time.sleep(1)


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
