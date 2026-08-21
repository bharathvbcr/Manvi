"""GPU compute snapshots. GPU-Util % is not the headline.

On a serial agent loop, utilization.gpu stays near 100% while the SM is
decode-bound at batch=1. That number does not mean the GH200 is saturated.
The quantities that do:

- decode tok/s from ollama eval_count / eval_duration (actual generated work)
- prompt tok/s from prompt_eval_count / prompt_eval_duration
- power.draw / power.limit (compute envelope; GH200 cap is 900 W)
- memory.used / memory.total (HBM occupancy; nvidia-smi reports ~96 GiB here)
- utilization.memory (DRAM controller, not SM occupancy)
- clocks.sm vs clocks.max.sm (throttling)
- loadavg / nproc (Grace CPU occupancy; independent of GPU-Util)

`utilization.gpu` is stored as gpu_busy_pct so it can be compared, not trusted.
"""
import json
import os
import subprocess
import sys
import threading
import time

QUERY = (
    "timestamp,utilization.gpu,utilization.memory,memory.used,memory.total,"
    "clocks.current.sm,clocks.max.sm,power.draw,power.limit,temperature.gpu"
)


def _num(s):
    if s is None:
        return None
    t = str(s).strip()
    if not t or t == "[N/A]":
        return None
    for suf in (" %", "%", " MiB", " MHz", " W", " C"):
        if t.endswith(suf):
            t = t[: -len(suf)].strip()
            break
    try:
        return float(t)
    except ValueError:
        return None


def parse_smi_csv(line):
    """Parse one nvidia-smi csv row (with or without units)."""
    if not line or not line.strip() or line.lstrip().startswith("#"):
        return None
    parts = [p.strip() for p in line.strip().split(",")]
    if len(parts) < 10:
        return None
    gpu = _num(parts[1])
    memc = _num(parts[2])
    used = _num(parts[3])
    total = _num(parts[4])
    sm = _num(parts[5])
    sm_max = _num(parts[6])
    power = _num(parts[7])
    cap = _num(parts[8])
    temp = _num(parts[9])
    hbm_frac = (used / total) if used is not None and total else None
    power_frac = (power / cap) if power is not None and cap else None
    return {
        "ts": parts[0],
        "gpu_busy_pct": gpu,
        "mem_controller_pct": memc,
        "hbm_used_mib": used,
        "hbm_total_mib": total,
        "hbm_frac": None if hbm_frac is None else round(hbm_frac, 4),
        "sm_clock_mhz": sm,
        "sm_clock_max_mhz": sm_max,
        "power_w": power,
        "power_cap_w": cap,
        "power_frac": None if power_frac is None else round(power_frac, 4),
        "temp_c": temp,
    }


def cpu_snapshot(loadavg=None, nproc=None):
    """Grace/host load. Independent of GPU-Util. Works without /proc."""
    n = nproc if nproc is not None else (os.cpu_count() or 1)
    if loadavg is None:
        try:
            loadavg = os.getloadavg()
        except OSError:
            return {"nproc": n}
    load1 = float(loadavg[0])
    return {
        "load1": round(load1, 2),
        "nproc": n,
        "cpu_load_frac": round(load1 / n, 4) if n else None,
    }


def snapshot(timeout=5):
    """One nvidia-smi sample plus host CPU load, or None if no GPU."""
    try:
        out = subprocess.check_output(
            ["nvidia-smi", f"--query-gpu={QUERY}",
             "--format=csv,noheader,nounits"],
            timeout=timeout, text=True, stderr=subprocess.DEVNULL,
        )
    except (FileNotFoundError, subprocess.SubprocessError, OSError):
        return None
    for line in out.splitlines():
        parsed = parse_smi_csv(line)
        if parsed:
            parsed.update(cpu_snapshot())
            return parsed
    return None


def tok_s(count, duration_ns):
    """Tokens per second from ollama's nanosecond duration fields."""
    if not count or not duration_ns:
        return None
    sec = duration_ns / 1e9
    if sec <= 0:
        return None
    return round(count / sec, 2)


def summarize(samples):
    """Mean/max of the compute envelope across samples. Omits gpu_busy as headline."""
    if not samples:
        return None
    keys = ("mem_controller_pct", "hbm_frac", "power_w", "power_frac",
            "sm_clock_mhz", "gpu_busy_pct", "temp_c", "hbm_used_mib",
            "cpu_load_frac", "load1")
    out = {"n": len(samples)}
    for k in keys:
        vals = [s[k] for s in samples if s.get(k) is not None]
        if not vals:
            continue
        out[k + "_mean"] = round(sum(vals) / len(vals), 4)
        out[k + "_max"] = round(max(vals), 4)
    last = samples[-1]
    out["hbm_total_mib"] = last.get("hbm_total_mib")
    out["power_cap_w"] = last.get("power_cap_w")
    out["sm_clock_max_mhz"] = last.get("sm_clock_max_mhz")
    out["nproc"] = last.get("nproc")
    return out


class Sampler:
    """Background nvidia-smi sampler for one episode. No-ops without a GPU."""

    def __init__(self, interval=2.0):
        self.interval = interval
        self.samples = []
        self._stop = threading.Event()
        self._thread = None

    def start(self):
        first = snapshot()
        if first is None:
            return self
        self.samples.append(first)
        self._thread = threading.Thread(target=self._loop, daemon=True)
        self._thread.start()
        return self

    def _loop(self):
        while not self._stop.wait(self.interval):
            s = snapshot()
            if s:
                self.samples.append(s)

    def stop(self):
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=self.interval + 2)
            self._thread = None
        last = snapshot()
        if last:
            self.samples.append(last)
        return summarize(self.samples)


def watch(out_path, interval=2.0):
    """Sidecar: JSONL of snapshots. GPU-Util is recorded, not promoted."""
    os.makedirs(os.path.dirname(os.path.abspath(out_path)) or ".", exist_ok=True)
    with open(out_path, "a") as f:
        while True:
            s = snapshot()
            if s:
                s["unix"] = time.time()
                f.write(json.dumps(s) + "\n")
                f.flush()
                cpu = s.get("cpu_load_frac")
                load = s.get("load1")
                nproc = s.get("nproc")
                cpu_bit = ""
                if cpu is not None and load is not None and nproc:
                    cpu_bit = f"  cpu={cpu:.0%} ({load}/{nproc})"
                print(
                    f"{s['ts']}  power={s['power_w']}/{s['power_cap_w']}W "
                    f"({s['power_frac']})  hbm={s['hbm_frac']}  "
                    f"memctl={s['mem_controller_pct']}%  "
                    f"sm={s['sm_clock_mhz']}MHz{cpu_bit}  "
                    f"gpu_busy={s['gpu_busy_pct']}% (occupied, not saturated)",
                    flush=True,
                )
            time.sleep(interval)


if __name__ == "__main__":
    ap_out = "compute.jsonl"
    interval = 2.0
    args = sys.argv[1:]
    i = 0
    while i < len(args):
        if args[i] in ("-o", "--out") and i + 1 < len(args):
            ap_out = args[i + 1]
            i += 2
        elif args[i] in ("-i", "--interval") and i + 1 < len(args):
            interval = float(args[i + 1])
            i += 2
        else:
            sys.exit(f"usage: python3 -m mh.compute [--out PATH] [--interval SEC]")
    watch(ap_out, interval)
