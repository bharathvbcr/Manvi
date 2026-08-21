"""Compute snapshot parsing and tok/s. No GPU required."""
import os
import sys

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


print()
print(f"{len(PASS)} passed, {len(FAIL)} failed")
if FAIL:
    sys.exit(1)
