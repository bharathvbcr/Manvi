"""Compute snapshot parsing and tok/s. No GPU required."""
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mh.compute import parse_smi_csv, summarize, tok_s

PASS, FAIL = [], []


def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print(f"  {'ok  ' if cond else 'FAIL'} {name}"
          + (f"  {detail}" if not cond and detail else ""))


print("parse nvidia-smi csv")
nounits = ("2026/08/20 18:44:31.211, 94, 26, 20671, 97871, "
           "1980, 1980, 500.69, 900.00, 54")
s = parse_smi_csv(nounits)
check("nounits gpu_busy stored", s["gpu_busy_pct"] == 94.0)
check("nounits mem controller", s["mem_controller_pct"] == 26.0)
check("nounits hbm frac ~21%", abs(s["hbm_frac"] - round(20671 / 97871, 4)) < 1e-9)
check("nounits power frac ~56%", abs(s["power_frac"] - round(500.69 / 900, 4)) < 1e-9)
check("nounits sm at cap", s["sm_clock_mhz"] == 1980.0)

with_units = ("2026/08/20 18:44:31.211, 94 %, 26 %, 20671 MiB, 97871 MiB, "
              "1980 MHz, 1980 MHz, 500.69 W, 900.00 W, 54")
u = parse_smi_csv(with_units)
check("units stripped", u["power_w"] == 500.69 and u["mem_controller_pct"] == 26.0)
check("blank is none", parse_smi_csv("") is None)
check("header skipped", parse_smi_csv("# gpu sm mem") is None)
check("short row none", parse_smi_csv("a,b,c") is None)

print("tok/s from ollama ns durations")
check("decode 100 tok / 1s", tok_s(100, 1_000_000_000) == 100.0)
check("zero duration none", tok_s(100, 0) is None)
check("zero count none", tok_s(0, 1_000_000_000) is None)
# 9602 tokens in 93.9s of wall-clock model time is NOT decode tok/s;
# eval_duration is the generation clock. 9602 tok in 80s decode = 120.02
check("round to 2dp", tok_s(9602, 80_000_000_000) == round(9602 / 80, 2))

print("host CPU load (Grace), independent of GPU-Util")
from mh.compute import cpu_snapshot
c = cpu_snapshot((8.54, 11.44, 12.89), 64)
check("load1 rounded", c["load1"] == 8.54)
check("nproc 64", c["nproc"] == 64)
check("cpu_load_frac ~13%", c["cpu_load_frac"] == round(8.54 / 64, 4))
live = cpu_snapshot()
check("live nproc >= 1", (live.get("nproc") or 0) >= 1)
check("live cpu_load_frac in [0, inf)", live.get("cpu_load_frac") is None or live["cpu_load_frac"] >= 0)

print("summarize")
check("empty none", summarize([]) is None)
summ = summarize([s, u])
check("n=2", summ["n"] == 2)
check("does not headline gpu_busy as saturation",
      "gpu_busy_pct_mean" in summ and "power_frac_mean" in summ)
check("power mean", abs(summ["power_frac_mean"] - s["power_frac"]) < 1e-6)


print("a probe that could not run is not a probe that found no GPU")
import contextlib, io, subprocess as _sp, threading
import mh.compute as C
from mh.compute import SNAPSHOT_TIMEOUT_S, Sampler

CSV = ("2026/08/20 18:44:31.211, 94, 26, 20671, 97871, "
       "1980, 1980, 500.69, 900.00, 54\n")


def raising(exc):
    def f(*a, **k):
        raise exc
    return f


_real_check_output = C.subprocess.check_output
try:
    C.subprocess.check_output = raising(FileNotFoundError())
    check("no nvidia-smi means no GPU to measure", C.probe()[1] == "no_gpu")
    check("no GPU still records nothing at all", Sampler().start().stop() is None)

    C.subprocess.check_output = raising(_sp.TimeoutExpired("nvidia-smi", 5))
    check("a wedged nvidia-smi is an error, not an absence",
          C.probe()[1] == "error:TimeoutExpired", str(C.probe()))
    rec = Sampler(interval=0.01).start().stop()
    check("a measurement that could not run is recorded as one",
          isinstance(rec, dict) and rec.get("measured") is False, str(rec))
    check("and it says why", (rec or {}).get("status") == "error:TimeoutExpired",
          str(rec))
    check("which is not what 'no GPU' looks like", rec is not None)

    C.subprocess.check_output = lambda *a, **k: "not a csv row\n"
    check("an answer nothing can parse is not a clean absence",
          C.probe()[1] == "unparseable")
finally:
    C.subprocess.check_output = _real_check_output

print("the sampler thread is joined, or the record says it was not")
s = Sampler(interval=2.0)
check("the join outlasts a probe running to its own timeout",
      s.join_timeout() > s.interval + SNAPSHOT_TIMEOUT_S,
      f"{s.join_timeout()} vs {s.interval + SNAPSHOT_TIMEOUT_S}")
check("the old interval + 2 bound did not",
      s.interval + 2 < s.interval + SNAPSHOT_TIMEOUT_S)

release = threading.Event()
calls = []


def wedging(*a, **k):
    calls.append(1)
    if len(calls) == 2:          # the loop's probe: outlives the join
        release.wait(5.0)
    return CSV


try:
    C.subprocess.check_output = wedging
    s = Sampler(interval=0.01, timeout=0.05)
    s.JOIN_MARGIN_S = 0.01       # join gives up while the probe is still in flight
    s.start()
    time.sleep(0.05)
    err = io.StringIO()
    with contextlib.redirect_stderr(err):
        rec = s.stop()
    check("a sampler that would not stop is reported", "did not stop" in err.getvalue(),
          err.getvalue())
    check("and the episode's compute record says so",
          (rec or {}).get("sampler_thread_leaked") is True, str(rec))
    check("the record's status names the wedge",
          (rec or {}).get("status") == "error:sampler_thread_wedged", str(rec))
    check("the thread reference is kept, not dropped", s._thread is not None)
    before = len(s.samples)
    release.set()
    if s._thread is not None:
        s._thread.join(timeout=5.0)
    check("the leaked thread is still joinable",
          s._thread is not None and not s._thread.is_alive())
    check("a probe that outlived stop() does not touch the samples",
          len(s.samples) == before, f"{before} -> {len(s.samples)}")
finally:
    C.subprocess.check_output = _real_check_output
    release.set()


print()
print(f"{len(PASS)} passed, {len(FAIL)} failed")
if FAIL:
    sys.exit(1)
