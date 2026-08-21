"""Unit tests for bootstrap CIs, paired deltas, and seed pinning. No GPU."""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mh.stats import (aligned_deltas, bootstrap_ci, interaction, mean,
                      pass_rates_by_repeat, role_of)
from run import seed_for_repeat

PASS, FAIL = [], []


def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print(f"  {'ok  ' if cond else 'FAIL'} {name}"
          + (f"  {detail}" if not cond and detail else ""))


print("seed pinning")
check("base+rep", seed_for_repeat(0, 3, 5) == 3)
check("base 1000", seed_for_repeat(1000, 4, 5) == 1004)
check("multi unseeded pins index", seed_for_repeat(None, 2, 5) == 2)
check("single unseeded stays none", seed_for_repeat(None, 0, 1) is None)
check("single seeded uses base", seed_for_repeat(7, 0, 1) == 7)

print("bootstrap CI")
m, lo, hi = bootstrap_ci([0.5])
check("n=1 degenerate", (m, lo, hi) == (0.5, 0.5, 0.5))
m, lo, hi = bootstrap_ci([0.2, 0.2, 0.2, 0.2, 0.2])
check("identical samples", (m, lo, hi) == (0.2, 0.2, 0.2))
m, lo, hi = bootstrap_ci([0.0, 1.0, 0.0, 1.0, 0.0], rng_seed=1)
check("mean of mixed", abs(m - 0.4) < 1e-12, f"mean={m}")
check("interval around mean", lo <= m <= hi, f"[{lo}, {hi}] mean={m}")
check("empty is nan", bootstrap_ci([])[0] != bootstrap_ci([])[0])

print("pass rates and paired deltas")
rows = [
    {"task": "a", "rep": 0, "passed": True},
    {"task": "b", "rep": 0, "passed": False},
    {"task": "a", "rep": 1, "passed": True},
    {"task": "b", "rep": 1, "passed": True},
]
rates = pass_rates_by_repeat(rows)
check("rep0 1/2", abs(rates[0] - 0.5) < 1e-12)
check("rep1 2/2", abs(rates[1] - 1.0) < 1e-12)
full = {0: 0.75, 1: 0.50, 2: 0.25}
abl = {0: 0.25, 1: 0.50, 2: 0.00}
d, keys = aligned_deltas(full, abl)
check("paired keys", keys == [0, 1, 2])
check("paired values", d == [0.5, 0.0, 0.25])

print("interaction Δ_weak > Δ_strong")
# weak deltas sit well above strong deltas
inter = interaction([0.3, 0.4, 0.35, 0.5, 0.4],
                    [0.0, 0.05, -0.02, 0.01, 0.0], rng_seed=2)
check("positive point", inter["delta_weak_minus_strong"] > 0.2)
check("detectable", inter["detectable"] is True)
check("not underpowered when CI>0", inter["underpowered"] is False)
# overlapping: should include 0
inter0 = interaction([0.1, 0.0, 0.05, -0.05, 0.02],
                     [0.08, 0.02, 0.0, -0.02, 0.04], rng_seed=3)
check("overlap underpowered", inter0["underpowered"] is True)
check("overlap not detectable", inter0["detectable"] is False)

print("role mapping")
check("gemma weak", role_of("gemma4:e4b") == "weak")
check("qwen mid", role_of("qwen3.8:27b") == "mid")
check("ornith mid", role_of("hf.co/ornith-ai/Ornith-1.5-35B-A3B-GGUF:Q8_0") == "mid")
check("mean", abs(mean([1, 2, 3]) - 2) < 1e-12)

print("empirical capability arms")
from mh.stats import capability_arms
pair = capability_arms({"gemma": 0.2, "qwen": 0.9})
check("weaker first", pair == ("gemma", "qwen"))
triple = capability_arms({"gemma": 0.1, "qwen": 0.9, "ornith": 0.5})
check("min vs max skips mid", triple == ("gemma", "qwen"))
check("one model none", capability_arms({"qwen": 0.9}) is None)
check("empty none", capability_arms({}) is None)

print("one-flag configs")
from run import CONFIGS
from mh.harness import Config
from mh.tools import FILE_TOOLS, schemas_for
ONE_OFF = {
    "no-envboot": "envboot",
    "no-verifygate": "verifygate",
    "no-checklist": "checklist",
    "no-loopbreak": "loopbreak",
    "no-outcap": "outcap",
    "no-groundfs": "groundfs",
    "no-nativetools": "nativetools",
}
check("every flag has a one-off",
      set(ONE_OFF.values()) == set(Config.FLAGS))
for name, flag in ONE_OFF.items():
    cfg = CONFIGS[name]()
    check(f"{name} turns {flag} off", getattr(cfg, flag) is False)
    for f in Config.FLAGS:
        if f != flag:
            check(f"{name} keeps {f}", getattr(cfg, f) is True)
check("full schemas are five", len(schemas_for(True)) == 5)
shell_names = [s["function"]["name"] for s in schemas_for(False)]
check("shell schemas drop file tools",
      set(shell_names) == {"run_shell", "finish"})
check("FILE_TOOLS named", set(FILE_TOOLS) == {"read_file", "write_file", "edit_file"})
check("default episode wall 1800s", Config().wall_s == 1800)
check("as_dict carries wall_s", CONFIGS["full"]().as_dict()["wall_s"] == 1800)

print()
if FAIL:
    print(f"FAIL {len(FAIL)}/{len(PASS)+len(FAIL)}")
    sys.exit(1)
print(f"ok {len(PASS)} stats tests")
sys.exit(0)
