"""Cell assembly: pooling guards, end-to-end refusals, randomised stress.

No ollama and no GPU. Builds throwaway result trees under MH_RESULTS.
"""
import json
import os
import random
import subprocess
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mh.pool import (CONFIG_FLAGS, PROTOCOL_KEYS, arms_drift, config_drift,
                     contrast_conflicts, contrast_drift, duplicate_episodes,
                     malformed_reps, merge_conflicts, pooled_drift,
                     protocol_drift, ragged_reps, rep_denominators, reps_of,
                     seed_conflicts, seed_reuse, source_conflicts, tasks_of,
                     unseeded_cells)

HERE = os.path.dirname(os.path.abspath(__file__))
PASS, FAIL = [], []


def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print(f"  {'ok  ' if cond else 'FAIL'} {name}" + (f"  {detail}" if not cond and detail else ""))


# A complete protocol block, as run.py now writes one.
PROTO = {"max_steps": 0, "max_wall": 1800, "share_gpu": False, "num_ctx": 32768,
         "num_predict": 4096, "temperature": 0.6, "think": True,
         "env_node": "gh200-01", "env_gpu": "NVIDIA GH200 480GB",
         "env_platform": "Linux/aarch64", "env_ollama_version": "0.32.13"}
TASKS = ["alpha", "beta", "gamma", "delta"]
MODEL = "vendor/model:Q8"
OTHER = "vendor/other:Q4"


def src(dirname, reps, tasks=TASKS, proto=None, flags=None, cfg="full",
        seed_of=None, passed=None):
    """One results directory, as grouped() sees it."""
    seed_of = seed_of or (lambda r: r)
    passed = passed or (lambda t, r: True)
    rows = [{"task": t, "rep": rep, "seed": seed_of(rep),
             "passed": bool(passed(t, rep)), "steps": 5, "output_tokens": 100,
             "stop_reason": "finished", "wall_s": 10.0}
            for rep in reps for t in tasks]
    config = {f: True for f in CONFIG_FLAGS}
    config.update(name=cfg, max_steps=0, wall_s=1800)
    config.update(flags or {})
    return {"dir": dirname, "protocol": dict(proto or PROTO),
            "config": config, "rows": rows}


def write_tree(root, sources):
    for s in sources:
        d = os.path.join(root, s["dir"])
        os.makedirs(d, exist_ok=True)
        n = len(s["rows"])
        p_ = sum(1 for r in s["rows"] if r["passed"])
        summary = {"model": s.pop("_model", MODEL), "config": s["config"],
                   "n": n, "passed": p_,
                   "pass_rate": round(100.0 * p_ / n, 1) if n else 0.0,
                   "wall_s": round(sum(r["wall_s"] for r in s["rows"]), 1),
                   "output_tokens": sum(r["output_tokens"] for r in s["rows"]),
                   "protocol": s["protocol"], "rows": s["rows"]}
        with open(os.path.join(d, "summary.json"), "w") as f:
            json.dump(summary, f)


def run_compare(root, *args):
    env = dict(os.environ, MH_RESULTS=root)
    return subprocess.run([sys.executable, os.path.join(HERE, "compare.py"), *args],
                          capture_output=True, text=True, env=env, cwd=HERE)


# --------------------------------------------------------------- pure helpers
print("helpers")
rows = src("d", [0, 1], TASKS)["rows"]
check("reps_of", reps_of(rows) == [0, 1])
check("tasks_of", tasks_of(rows) == sorted(TASKS))
check("rep_denominators", rep_denominators(rows) == {0: 4, 1: 4})
check("even reps are not ragged", ragged_reps(rows) == [])
check("short repeat is ragged", ragged_reps(rows + [{"task": "alpha", "rep": 2}]) == [2])
check("empty rows safe", reps_of([]) == [] and rep_denominators(None) == {})
check("protocol_drift clean", protocol_drift(PROTO, dict(PROTO)) == [])
check("protocol_drift on share_gpu",
      protocol_drift(PROTO, dict(PROTO, share_gpu=True)) == ["share_gpu"])
check("missing block is total drift",
      set(protocol_drift(PROTO, None)) == set(PROTOCOL_KEYS))
check("absent key counts as drift",
      protocol_drift(PROTO, {k: v for k, v in PROTO.items() if k != "think"}) == ["think"])
check("config_drift ignores runtime fields",
      config_drift({"name": "full", "max_steps": 0}, {"name": "full", "max_steps": 9}) == [])
check("config_drift catches a flag",
      config_drift({f: True for f in CONFIG_FLAGS},
                   dict({f: True for f in CONFIG_FLAGS}, outcap=False)) == ["outcap"])

# ------------------------------------------------------------ merge conflicts
print("merge conflicts")
check("single source never conflicts", merge_conflicts([src("a", [0, 1])]) == [])
check("empty never conflicts", merge_conflicts([]) == [])
check("disjoint reps pool cleanly",
      merge_conflicts([src("a", range(5)), src("b", range(5, 20))]) == [])

c = merge_conflicts([src("a", range(5)), src("b", range(3, 8))])
check("overlapping reps refused", any("rep(s) [3, 4]" in x for x in c), str(c))

c = merge_conflicts([src("a", range(5)),
                     src("b", range(5, 10), proto=dict(PROTO, share_gpu=True))])
check("protocol drift refused", any("protocol drift" in x for x in c), str(c))
check("drift names the values", any("share_gpu=False/True" in x for x in c), str(c))
check("drift waived by allow_drift",
      merge_conflicts([src("a", range(5)),
                       src("b", range(5, 10), proto=dict(PROTO, share_gpu=True))],
                      allow_drift=True) == [])

c = merge_conflicts([src("a", range(5)),
                     src("b", range(5, 10), proto=dict(PROTO, max_wall=3600))],
                    allow_drift=True)
check("allow_drift still refuses non-protocol faults",
      merge_conflicts([src("a", range(5)), src("b", range(4, 9))],
                      allow_drift=True) != [])

c = merge_conflicts([src("a", range(5)), src("b", range(5, 10), tasks=TASKS[:3])])
check("task-set mismatch refused", any("task sets differ" in x for x in c), str(c))
check("mismatch names the task", any("delta" in x for x in c), str(c))

c = merge_conflicts([src("a", range(5)), src("b", range(5, 10), flags={"outcap": False})])
check("same name different flags refused",
      any("different ablation flags" in x and "outcap" in x for x in c), str(c))

c = merge_conflicts([src("a", range(5)),
                     src("b", range(3, 8), tasks=TASKS[:2], flags={"outcap": False},
                         proto=dict(PROTO, think=False))])
check("every independent fault is reported", len(c) == 4, str(len(c)))

# order independence
a, b = src("a", range(5)), src("b", range(5, 20))
check("merge check is order independent",
      (merge_conflicts([a, b]) == []) == (merge_conflicts([b, a]) == []))

# three-way: fault between 2nd and 3rd must still surface
three = [src("a", range(5)), src("b", range(5, 10)), src("c", range(7, 12))]
check("three-way overlap caught", merge_conflicts(three) != [], str(merge_conflicts(three)))

check("pooled_drift empty when clean",
      pooled_drift([src("a", range(5)), src("b", range(5, 10))]) == [])
# --- arms_drift: the divergent arm need not be an extreme -------------------
# compare.py used to compare only the two INTERACTION arms (lowest and highest
# full-harness mean). With three arms and the odd one ranked between them, the
# check ran between the two matching arms, found nothing, and printed no
# caveat under a pass-rate table listing all three.
_LOCAL = dict(PROTO)
_HOSTED = dict(PROTO, num_ctx=65536, num_predict=16384, reasoning_effort="medium")

check("arms_drift is silent when every arm matches",
      arms_drift({"a": dict(_LOCAL), "b": dict(_LOCAL), "c": dict(_LOCAL)}) == [])
_mid = arms_drift({"weak": dict(_LOCAL), "mid": dict(_HOSTED), "strong": dict(_LOCAL)})
check("arms_drift catches a divergent MIDDLE arm",
      len(_mid) == 2 and all("num_ctx" in ks for _, _, ks in _mid),
      f"got {_mid}")
check("arms_drift names both offending pairs, not the matching one",
      sorted({p for a, b, _ in _mid for p in (a, b)}) == ["mid", "strong", "weak"]
      and all("mid" in (a, b) for a, b, _ in _mid),
      f"got {[(a, b) for a, b, _ in _mid]}")
check("arms_drift still handles the two-arm case",
      len(arms_drift({"a": dict(_LOCAL), "b": dict(_HOSTED)})) == 1)
check("arms_drift skips an arm with no recorded protocol",
      arms_drift({"a": dict(_LOCAL), "b": None}) == [])
check("arms_drift reports the differing keys, not just that some differ",
      "reasoning_effort" in arms_drift({"a": dict(_LOCAL), "b": dict(_HOSTED)})[0][2])

check("pooled_drift finds a late pair",
      pooled_drift([src("a", range(5)), src("b", range(5, 10)),
                    src("c", range(10, 15), proto=dict(PROTO, think=False))]) == ["think"])

# The contamination that caused the revision-2 correction: an MLX/Metal run
# pooled into a CUDA contrast. Indistinguishable on disk until the serving
# host was recorded.
CUDA = dict(PROTO)
METAL = dict(PROTO, env_gpu="apple-metal/arm64", env_platform="Darwin/arm64",
             env_node="Mac-12582.lan", env_ollama_version="0.32.13")
c = merge_conflicts([src("cuda", range(5), proto=CUDA),
                     src("metal", range(5, 20), proto=METAL)])
check("Metal run refused into a CUDA pool", c != [], str(c))
check("refusal names the GPU", any("env_gpu" in x for x in c), str(c))
check("same host pools cleanly",
      merge_conflicts([src("a", range(5), proto=CUDA),
                       src("b", range(5, 20), proto=dict(CUDA))]) == [])
check("unrecorded host is drift, not agreement",
      "env_gpu" in protocol_drift(CUDA, {k: v for k, v in PROTO.items()
                                         if not k.startswith("env_")}))
check("a different node on the same GPU still drifts",
      protocol_drift(CUDA, dict(CUDA, env_node="gh200-02")) == ["env_node"])

# ------------------------------------------------------------ seed conflicts
print("seed/rep pairing")
clean = {(MODEL, "full"): src("a", range(5))["rows"],
         (MODEL, "no-outcap"): src("b", range(5))["rows"]}
check("matching seeds pair cleanly", seed_conflicts(clean) == [])

skewed = {(MODEL, "full"): src("a", range(5))["rows"],
          (MODEL, "no-outcap"): src("b", range(5), seed_of=lambda r: r + 100)["rows"]}
sc = seed_conflicts(skewed)
check("rep meaning two seeds refused", len(sc) == 5, str(sc))
check("seed conflict names both configs",
      all("full" in x and "no-outcap" in x for x in sc), str(sc))

crossmodel = {(MODEL, "full"): src("a", range(3))["rows"],
              (OTHER, "full"): src("b", range(3), seed_of=lambda r: r + 100)["rows"]}
check("different models may use different seeds", seed_conflicts(crossmodel) == [])
check("rows without a seed field are ignored",
      seed_conflicts({(MODEL, "full"): [{"task": "a", "rep": 0}]}) == [])

# --------------------------------------------------------------- end-to-end
print("end-to-end CLI")
with tempfile.TemporaryDirectory() as root:
    frozen = src("vendor_model_Q8__full__hard", range(5))
    ext = src("vendor_model_Q8__full__hard-ext", range(5, 20))
    abl_f = src("vendor_model_Q8__no-outcap__hard", range(5), cfg="no-outcap",
                flags={"outcap": False}, passed=lambda t, r: t != "alpha")
    abl_e = src("vendor_model_Q8__no-outcap__hard-ext", range(5, 20), cfg="no-outcap",
                flags={"outcap": False}, passed=lambda t, r: t != "alpha")
    write_tree(root, [frozen, ext, abl_f, abl_e])

    r = run_compare(root, "--tag", "hard")
    check("single tag succeeds", r.returncode == 0, r.stderr[-300:])

    out = os.path.join(root, "pooled.json")
    r = run_compare(root, "--tag", "hard,hard-ext", "--json-out", out)
    check("two tags pool", r.returncode == 0, r.stderr[-400:])
    if r.returncode == 0:
        rep = json.load(open(out))
        cell = rep["cells"][f"{MODEL}|full"]
        check("pooled cell has 20 repeats", cell["n_repeats"] == 20, str(cell["n_repeats"]))
        check("pooled cell has 80 episodes", cell["n"] == 80, str(cell["n"]))
        prov = rep["provenance"][f"{MODEL}|full"]
        check("provenance marks the pool", prov["pooled"] is True)
        check("provenance lists both dirs", len(prov["dirs"]) == 2, str(prov["dirs"]))
        check("provenance reps span 0-19", prov["reps"] == list(range(20)))
        check("provenance records no raggedness", prov["ragged_reps"] == [])
        check("drift not silently allowed", prov["protocol_drift_allowed"] is False)
        d = rep["deltas"][f"{MODEL}|no-outcap"]
        check("delta paired over 20 reps", d["n"] == 20, str(d["n"]))
        check("delta value is the known 1/4", abs(d["mean"] - 0.25) < 1e-9, str(d["mean"]))

    # a single tag must not silently see the extension
    r = run_compare(root, "--tag", "hard", "--json-out", os.path.join(root, "one.json"))
    one = json.load(open(os.path.join(root, "one.json")))
    check("unpooled cell stays at 5", one["cells"][f"{MODEL}|full"]["n_repeats"] == 5)
    check("unpooled provenance not marked pooled",
          one["provenance"][f"{MODEL}|full"]["pooled"] is False)

with tempfile.TemporaryDirectory() as root:
    write_tree(root, [src("vendor_model_Q8__full__hard", range(5)),
                      src("vendor_model_Q8__full__hard-ext", range(3, 8))])
    r = run_compare(root, "--tag", "hard,hard-ext")
    check("CLI refuses overlapping reps", r.returncode == 2, str(r.returncode))
    check("refusal explains the overlap", "rep(s) [3, 4]" in r.stderr, r.stderr[-300:])
    check("refusal goes to stderr", "REFUSED" in r.stderr and "REFUSED" not in r.stdout)

with tempfile.TemporaryDirectory() as root:
    write_tree(root, [src("vendor_model_Q8__full__hard", range(5)),
                      src("vendor_model_Q8__full__hard-ext", range(5, 10),
                          proto=dict(PROTO, share_gpu=True))])
    r = run_compare(root, "--tag", "hard,hard-ext")
    check("CLI refuses protocol drift", r.returncode == 2)
    check("drift refusal names share_gpu", "share_gpu" in r.stderr, r.stderr[-300:])

    out = os.path.join(root, "drift.json")
    r = run_compare(root, "--tag", "hard,hard-ext", "--allow-drift", "--json-out", out)
    check("--allow-drift permits the pool", r.returncode == 0, r.stderr[-300:])
    if r.returncode == 0:
        prov = json.load(open(out))["provenance"][f"{MODEL}|full"]
        check("drifted pool is stamped", prov["protocol_drift_allowed"] is True)
        check("stamp names the drifting key", prov["protocol_drift"] == ["share_gpu"],
              str(prov["protocol_drift"]))
        check("drifted pool announced on stdout", "PROTOCOL DRIFT WAIVED" in r.stdout)

with tempfile.TemporaryDirectory() as root:
    ragged = src("vendor_model_Q8__full__hard", range(5))
    ragged["rows"] = [r for r in ragged["rows"] if not (r["rep"] == 4 and r["task"] != "alpha")]
    write_tree(root, [ragged])
    out = os.path.join(root, "r.json")
    r = run_compare(root, "--tag", "hard", "--json-out", out)
    check("ragged single cell still reports", r.returncode == 0, r.stderr[-200:])
    check("raggedness warned on stdout", "uneven repeats" in r.stdout, r.stdout[-300:])
    check("raggedness recorded in json",
          json.load(open(out))["provenance"][f"{MODEL}|full"]["ragged_reps"] == [4])

# ------------------------------------------------------------------- stress
print("randomised stress (400 trees)")
rng = random.Random(1234)
viol = {"missed": 0, "spurious": 0, "lost_rows": 0, "order": 0}
for trial in range(400):
    n_src = rng.randint(1, 4)
    sources, cursor = [], 0
    want_clean = True
    for i in range(n_src):
        span = rng.randint(1, 5)
        if rng.random() < 0.35 and cursor > 0:          # force an overlap
            start = max(0, cursor - rng.randint(1, 3)); want_clean = False
        else:
            start = cursor
        reps = list(range(start, start + span))
        cursor = max(cursor, start + span)
        proto = dict(PROTO)
        if rng.random() < 0.25 and i:
            proto[rng.choice(PROTOCOL_KEYS)] = "drifted"; want_clean = False
        tasks = list(TASKS)
        if rng.random() < 0.2 and i:
            tasks = TASKS[:rng.randint(1, 3)]; want_clean = False
        flags = None
        if rng.random() < 0.2 and i:
            flags = {rng.choice(CONFIG_FLAGS): False}; want_clean = False
        sources.append(src(f"d{i}", reps, tasks=tasks, proto=proto, flags=flags))

    got = merge_conflicts(sources)
    if want_clean and got:
        viol["spurious"] += 1
    if not want_clean and not got and n_src > 1:
        viol["missed"] += 1
    if not got:
        merged = [r for s in sources for r in s["rows"]]
        if len(merged) != sum(len(s["rows"]) for s in sources):
            viol["lost_rows"] += 1
        buckets = {}
        for s in sources:
            for rep in reps_of(s["rows"]):
                buckets.setdefault(rep, set()).add(s["dir"])
        if any(len(v) > 1 for v in buckets.values()):
            viol["lost_rows"] += 1
    shuffled = list(sources); rng.shuffle(shuffled)
    if bool(merge_conflicts(shuffled)) != bool(got):
        viol["order"] += 1

check("no clean tree was refused", viol["spurious"] == 0, str(viol))
check("no faulty tree was accepted", viol["missed"] == 0, str(viol))
check("accepted merges never mix a repeat", viol["lost_rows"] == 0, str(viol))
check("verdict is order independent", viol["order"] == 0, str(viol))

# --------------------------------------------- intra-source and new guards
print("single-directory soundness")
dup = src("d", [0, 1])
dup["rows"] = dup["rows"] + [dict(dup["rows"][0])]
check("duplicate (task, rep) found", duplicate_episodes(dup["rows"]) == [("alpha", 0)],
      str(duplicate_episodes(dup["rows"])))
check("one source with duplicates is refused", source_conflicts(dup) != [])
check("merge_conflicts checks a lone source too", merge_conflicts([dup]) != [],
      "a single directory used to skip every guard")
check("clean lone source still passes", merge_conflicts([src("d", [0, 1])]) == [])

bad = src("d", [0])
bad["rows"][0].pop("rep")
bad["rows"][1]["rep"] = "1"
m = malformed_reps(bad["rows"])
check("missing and non-int reps found", len(m) == 2, str(m))
check("bool is not an acceptable rep",
      malformed_reps([{"task": "a", "rep": True}]) != [])

print("unrecorded protocol never certifies agreement")
check("both absent is drift, not agreement",
      set(protocol_drift(None, None)) == set(PROTOCOL_KEYS))
check("two legacy dirs cannot pool silently",
      merge_conflicts([{"dir": "a", "protocol": None, "config": {"name": "full"},
                        "rows": src("a", [0])["rows"]},
                       {"dir": "b", "protocol": None, "config": {"name": "full"},
                        "rows": src("b", [1])["rows"]}]) != [])

print("seed reuse across repeats")
reuse = {(MODEL, "full"): src("a", range(5))["rows"]
         + src("b", range(5, 10), seed_of=lambda r: r - 5)["rows"]}
check("same seed on two repeats is refused", seed_reuse(reuse) != [], str(seed_reuse(reuse)))
check("distinct seeds pool cleanly",
      seed_reuse({(MODEL, "full"): src("a", range(5))["rows"]}) == [])
check("unseeded rows are reported as a gap",
      unseeded_cells({(MODEL, "full"): [{"task": "a", "rep": 0, "seed": None}]}) != [])

print("cross-config contrast validity")
mismatch = {(MODEL, "full"): src("a", range(5))["rows"],
            (MODEL, "no-outcap"): src("b", range(5), tasks=TASKS[:2])["rows"]}
check("arms scoring different tasks are refused", contrast_conflicts(mismatch) != [],
      str(contrast_conflicts(mismatch)))
check("matching arms pass",
      contrast_conflicts({(MODEL, "full"): src("a", range(5))["rows"],
                          (MODEL, "no-outcap"): src("b", range(5))["rows"]}) == [])
cd = contrast_drift({(MODEL, "full"): PROTO,
                     (MODEL, "no-outcap"): dict(PROTO, share_gpu=True)})
check("cross-config protocol drift is reported", cd and cd[0][1] == ["share_gpu"], str(cd))

print("ragged modal choice")
check("tie prefers the larger denominator as canonical",
      ragged_reps([{"task": f"t{i}", "rep": r} for r in (0, 1) for i in range(8)]
                  + [{"task": f"t{i}", "rep": r} for r in (2, 3) for i in range(4)])
      == [2, 3])

# ------------------------------------------------- WIRING (mutation-resistant)
# Each guard below must be reachable from the CLI. Deleting its call site in
# compare.py has to fail a test, or the guard is decoration.
print("guards are wired into the CLI")
with tempfile.TemporaryDirectory() as root:
    d = src("vendor_model_Q8__full__hard", [0, 1])
    d["rows"] = d["rows"] + [dict(d["rows"][0])]
    write_tree(root, [d])
    r = run_compare(root, "--tag", "hard")
    check("CLI refuses intra-directory duplicates", r.returncode == 2, r.stdout[-200:])
    check("duplicate refusal names the pair", "recorded more than once" in r.stderr)

with tempfile.TemporaryDirectory() as root:
    write_tree(root, [src("vendor_model_Q8__full__hard", range(5)),
                      src("vendor_model_Q8__no-outcap__hard", range(5),
                          cfg="no-outcap", flags={"outcap": False},
                          seed_of=lambda r: r + 100)])
    r = run_compare(root, "--tag", "hard")
    check("CLI refuses a seed/rep mismatch", r.returncode == 2, r.stdout[-200:])
    check("seed refusal explains the pairing", "deltas pair by rep" in r.stderr)

with tempfile.TemporaryDirectory() as root:
    write_tree(root, [src("vendor_model_Q8__full__hard", range(5)),
                      src("vendor_model_Q8__no-outcap__hard", range(5),
                          cfg="no-outcap", flags={"outcap": False}, tasks=TASKS[:2])])
    r = run_compare(root, "--tag", "hard")
    check("CLI refuses an unpairable contrast", r.returncode == 2, r.stdout[-200:])
    check("contrast refusal names the tasks", "unpairable contrast" in r.stderr)

with tempfile.TemporaryDirectory() as root:
    write_tree(root, [src("vendor_model_Q8__full__hard", range(5)),
                      src("vendor_model_Q8__full__hard-ext", range(5, 10),
                          seed_of=lambda r: r - 5)])
    r = run_compare(root, "--tag", "hard,hard-ext")
    check("CLI refuses duplicated samples", r.returncode == 2, r.stdout[-200:])
    check("reuse refusal names the seed", "duplicated sample" in r.stderr)

print("malformed input refuses, never tracebacks")
BROKEN = {
    "truncated json": '{"model": "m", "config"',
    "not an object": '[1,2,3]',
    "no rows": '{"model":"m","config":{"name":"full"}}',
    "no model": '{"config":{"name":"full"},"rows":[]}',
    "config not an object": '{"model":"m","config":"full","rows":[]}',
    "rows not a list": '{"model":"m","config":{"name":"full"},"rows":{}}',
    "row is a string": '{"model":"m","config":{"name":"full"},"rows":["x"]}',
}
for label, body in BROKEN.items():
    with tempfile.TemporaryDirectory() as root:
        d = os.path.join(root, "vendor_model_Q8__full__hard")
        os.makedirs(d)
        open(os.path.join(d, "summary.json"), "w").write(body)
        r = run_compare(root, "--tag", "hard")
        check(f"refuses cleanly: {label}", r.returncode == 2,
              f"rc={r.returncode} {r.stderr[-120:]}")
        check(f"no traceback: {label}", "Traceback" not in r.stderr)

print("cross-config drift warns but does not refuse")
with tempfile.TemporaryDirectory() as root:
    out = os.path.join(root, "d.json")
    write_tree(root, [src("vendor_model_Q8__full__hard", range(5)),
                      src("vendor_model_Q8__no-outcap__hard", range(5),
                          cfg="no-outcap", flags={"outcap": False},
                          proto=dict(PROTO, share_gpu=True))])
    r = run_compare(root, "--tag", "hard", "--json-out", out)
    check("asymmetric arms still report", r.returncode == 0, r.stderr[-300:])
    check("asymmetry is warned on stdout", "different protocols" in r.stdout)
    check("asymmetry is recorded in the json",
          json.load(open(out))["contrast_drift"][0]["keys"] == ["share_gpu"])

# ---------------------------------------------------------------------------
# Reporting defects, at the level they actually bite: the CLI. Each block fails
# against the code as it stood before the corresponding fix.
print("reporting defects, end to end")

PAPER_FIGS = os.path.join(HERE, "paper", "figures")


def _fig_state():
    """(name, size, mtime) for the committed paper figures."""
    if not os.path.isdir(PAPER_FIGS):
        return []
    return sorted((f, os.path.getsize(os.path.join(PAPER_FIGS, f)),
                   os.path.getmtime(os.path.join(PAPER_FIGS, f)))
                  for f in os.listdir(PAPER_FIGS))


def two_model_tree(root, **kw):
    """A grid with two models, full + baseline, so the arms can be ranked."""
    sources = []
    for model, tag, rate in ((MODEL, "m", 0.5), (OTHER, "o", 1.0)):
        for cfg in ("full", "baseline"):
            passed = ((lambda t, r: True) if cfg == "full" and rate == 1.0
                      else (lambda t, r: (hash((t, r)) % 4) > 0
                            if cfg == "full" else (hash((t, r)) % 2) == 0))
            src_ = src(f"vendor_{tag}__{cfg}__hard", range(5), cfg=cfg,
                       flags={} if cfg == "full" else {"envboot": False},
                       passed=passed, **kw)
            src_["config"]["name"] = cfg
            src_["_model"] = model
            sources.append(src_)
    write_tree(root, sources)


with tempfile.TemporaryDirectory() as root:
    out = os.path.join(root, "r.json")
    figdir = os.path.join(root, "figs")
    two_model_tree(root)
    before = _fig_state()
    r = run_compare(root, "--tag", "hard", "--json-out", out,
                    "--coverage-trials", "200")
    check("json-out alone succeeds", r.returncode == 0, r.stderr[-400:])
    check("json-out renders no figures at all",
          "wrote" not in r.stdout.replace(f"wrote {out}", ""), r.stdout[-300:])
    check("json-out does not touch the committed paper figures",
          _fig_state() == before)
    check("json-out no longer swallows a figures failure",
          "figures.py skipped" not in r.stdout)
    check("no bare NaN in the written report",
          "NaN" not in open(out).read() and "Infinity" not in open(out).read())
    doc = json.load(open(out))

    # B4/B5: the numbers that say how to read the intervals.
    check("report carries a coverage block", "coverage" in doc)
    check("coverage states nominal and measured",
          doc["coverage"]["nominal"] == 0.95
          and 0.0 < doc["coverage"]["measured"] < 1.0, str(doc.get("coverage")))
    check("measured coverage is below nominal at this n",
          doc["coverage"]["measured"] < doc["coverage"]["nominal"],
          str(doc["coverage"]["measured"]))
    check("report carries a multiplicity block", "multiplicity" in doc)
    check("multiplicity counts the ladder",
          doc["multiplicity"]["n_intervals"] == len(doc["deltas"]))
    check("multiplicity gives the family-wise rate",
          doc["multiplicity"]["fwer_at_nominal_coverage"] > 0.05)
    check("multiplicity says no correction was applied",
          doc["multiplicity"]["correction_applied"] is False)

    # B1/B7: the paired interaction, with its pairing on the record.
    check("report carries the paired interaction",
          any(not k.startswith("_") for k in doc.get("interaction_paired", {})))
    check("the frozen unpaired block is still there",
          any(not k.startswith("_") for k in doc["interaction"]))
    paired_entries = [v for k, v in doc["interaction_paired"].items()
                      if not k.startswith("_")]
    check("every paired entry records how many repeats it paired",
          all("n" in v and "reps" in v and "dropped_reps" in v
              for v in paired_entries))
    check("paired and unpaired agree on the point estimate",
          all(abs(doc["interaction_paired"][k]["delta_weak_minus_strong"]
                  - v["delta_weak_minus_strong"]) < 1e-12
              for k, v in doc["interaction"].items() if not k.startswith("_")))
    check("the paired scheme is documented in the report",
          "paired by seed" in doc["interaction_paired"].get("_scheme", ""))
    check("the arms block carries the capability gap and tie flag",
          "capability_gap" in doc["interaction_paired"]["_arms"]
          and "tied" in doc["interaction_paired"]["_arms"])
    check("the frozen _arms block is unchanged in shape",
          set(doc["interaction"]["_arms"]) == {
              "weaker", "stronger", "weaker_full_mean", "stronger_full_mean"})

    # B3: figures only when asked for, into the directory asked for.
    r2 = run_compare(root, "--tag", "hard", "--json-out", out,
                     "--coverage-trials", "200", "--figures", figdir)
    check("--figures renders on request", r2.returncode == 0, r2.stderr[-400:])
    check("--figures writes where it was told",
          os.path.isdir(figdir)
          and any(f.endswith(".svg") for f in os.listdir(figdir)),
          str(os.listdir(root)))
    check("--figures still leaves the paper figures alone", _fig_state() == before)
    check("a paired interaction figure is rendered alongside",
          "interaction_paired.generated.svg" in os.listdir(figdir))
    cap = open(os.path.join(figdir, "pass_rates.generated.svg")).read()
    check("the rendered caption matches this grid, not the paper's",
          f"{len(TASKS)} tasks" in cap and "8 tasks" not in cap, cap[:600])

    # A figures failure is an error, not a printed shrug.
    r3 = run_compare(root, "--tag", "hard", "--json-out", out,
                     "--coverage-trials", "200",
                     "--figures", os.path.join(out, "nope"))
    check("an unwritable figures directory fails the run", r3.returncode != 0)
    check("and fails as a real error, not an argparse rejection",
          "usage:" not in r3.stderr, r3.stderr[-200:])
    check("and never reports the run as fine", "figures.py skipped" not in r3.stdout)

    # The coverage audit can be skipped, but never silently.
    r4 = run_compare(root, "--tag", "hard", "--json-out", out,
                     "--no-coverage-audit")
    check("skipping the coverage audit is allowed", r4.returncode == 0,
          r4.stderr[-300:])
    doc4 = json.load(open(out))
    check("a skipped audit is marked skipped, not absent",
          doc4["coverage"].get("skipped") is True
          and doc4["coverage"].get("measured") is None)
    check("a skipped audit still leaves the multiplicity numbers",
          doc4["multiplicity"]["n_intervals"] == len(doc4["deltas"]))
    check("a skipped audit says so on stdout", "SKIPPED" in r4.stdout)

# B6: a ragged cell is weighted by its denominators, end to end.
with tempfile.TemporaryDirectory() as root:
    out = os.path.join(root, "r.json")
    # reps 0-3 score all four tasks and fail them; rep 4 scores one task and
    # passes it. Mean of per-repeat rates = 1/5 = 20.0%; the pooled rate is
    # 1/17 = 5.9%. A 14.1-point swing hanging on a single-task repeat.
    full = src("vendor_model_Q8__full__hard", range(4), passed=lambda t, r: False)
    full["rows"].append({"task": "alpha", "rep": 4, "seed": 4, "passed": True,
                         "steps": 5, "output_tokens": 100,
                         "stop_reason": "finished", "wall_s": 10.0})
    write_tree(root, [full])
    r = run_compare(root, "--tag", "hard", "--json-out", out,
                    "--coverage-trials", "200")
    check("a ragged grid still reports", r.returncode == 0, r.stderr[-400:])
    cell = json.load(open(out))["cells"][f"{MODEL}|full"]
    check("ragged repeats are weighted by their denominators",
          abs(cell["mean"] - 1.0 / 17.0) < 1e-12,
          f"{cell['mean']} (unweighted would be 0.2)")
    check("raggedness is still warned about", "uneven repeats" in r.stdout)

# B8: a saturated cell is named as degenerate rather than drawn as certain.
with tempfile.TemporaryDirectory() as root:
    out = os.path.join(root, "r.json")
    figdir = os.path.join(root, "figs")
    write_tree(root, [src("vendor_model_Q8__full__hard", range(5),
                          passed=lambda t, r: True)])
    r = run_compare(root, "--tag", "hard", "--json-out", out,
                    "--coverage-trials", "200", "--figures", figdir)
    check("a saturated grid still reports", r.returncode == 0, r.stderr[-400:])
    doc = json.load(open(out))
    cell = doc["cells"][f"{MODEL}|full"]
    check("the saturated interval really is zero width", cell["lo"] == cell["hi"])
    check("and the report says why it is zero width",
          "no observed variance" in
          doc["degenerate_intervals"].get(f"cells:{MODEL}|full", ""),
          str(doc.get("degenerate_intervals")))
    check("the warning reaches stdout", "DEGENERATE INTERVAL" in r.stdout)
    svg = open(os.path.join(figdir, "pass_rates.generated.svg")).read()
    check("the figure marks it instead of drawing a confident error bar",
          "no observed variance" in svg, svg[:200])

# json.dumps writes a bare NaN, which is not JSON. Build a cell that really
# produces one -- every episode a first-turn timeout, so nothing is left in the
# denominator -- and check what lands on disk.
print("a report with no measurable cell is still valid JSON")
with tempfile.TemporaryDirectory() as root:
    out = os.path.join(root, "r.json")
    starved = src("vendor_model_Q8__full__hard", range(5))
    for row in starved["rows"]:
        row.update(steps=1, output_tokens=0, passed=False,
                   stop_reason="error:ModelError",
                   errors=["ModelError: TimeoutError: timed out"])
    write_tree(root, [starved])
    r = run_compare(root, "--tag", "hard", "--json-out", out,
                    "--coverage-trials", "200")
    check("an all-starved cell reports rather than crashing", r.returncode == 0,
          r.stderr[-400:])
    raw = open(out).read()
    check("the cell really has no measurable mean",
          json.load(open(out))["cells"][f"{MODEL}|full"]["mean"] is None, raw[:300])
    check("no bare NaN is written", "NaN" not in raw)
    check("no bare Infinity is written", "Infinity" not in raw)
    def _strict_parse():
        """json.loads accepts bare NaN by default; make it reject like a
        conforming parser would, and report the rejection as a failure rather
        than letting it abort the file."""
        try:
            json.loads(raw, parse_constant=lambda c: (_ for _ in ()).throw(
                ValueError(f"bare {c}")))
            return True, ""
        except ValueError as e:
            return False, str(e)

    _ok, _why = _strict_parse()
    check("a strict parser accepts the file", _ok, _why)
    check("the coverage audit refuses a meaningless truth rather than "
          "reporting one",
          json.load(open(out))["coverage"].get("skipped") is True
          and "no usable cell mean" in
          json.load(open(out))["coverage"].get("reason", ""),
          str(json.load(open(out))["coverage"])[:200])
    check("the missing numbers are named, not just blanked",
          any("cells" in p for p in json.load(open(out)).get("nonfinite", [])),
          str(json.load(open(out)).get("nonfinite"))[:200])

# --------------------------------------------- account refusals reach the report
#
# A cell of 160 HTTP 402s published 0.0% and every downstream stage believed it.
# compare.py is the last stage that could have caught it and could not: it knows
# about starved rows and nothing else. These rows are deliberately NOT excluded
# from the denominator (the registered exclusion rule is timeouts only), so the
# only defence is that the rate is never printed without the fact beside it.
print("compare.py refuses to certify episodes the provider never served")
with tempfile.TemporaryDirectory() as root:
    poisoned = src("m__full__t", [0, 1], TASKS)
    for r in poisoned["rows"][:4]:
        r.update(passed=False, steps=1, output_tokens=0,
                 stop_reason="error:ModelError",
                 errors=['ModelError: HTTP 402: {"message":"Payment required"}'])
    clean = src("m__baseline__t", [0, 1], TASKS, cfg="baseline",
                flags={"envboot": False})
    write_tree(root, [poisoned, clean])
    r = run_compare(root, "--tag", "t")
    check("the report says it is refusing to certify",
          "REFUSING TO CERTIFY" in r.stdout, r.stdout[-400:])
    check("it names the count and the cell",
          "4 episode(s)" in r.stdout and f"{MODEL}|full" in r.stdout,
          r.stdout[-600:])
    check("the cell line carries the fact next to its rate",
          "UNSERVED=4 SCORED AS FAILURES" in r.stdout, r.stdout[-800:])
    out = os.path.join(root, "poisoned.json")
    run_compare(root, "--tag", "t", "--json-out", out)
    rep = json.load(open(out))
    check("the machine-readable report carries it too",
          rep.get("unserved_cells", {}).get(f"{MODEL}|full") == 4,
          str(rep.get("unserved_cells")))
    check("and the per-cell block records n_unserved",
          rep["cells"][f"{MODEL}|full"]["n_unserved"] == 4
          and rep["cells"][f"{MODEL}|baseline"]["n_unserved"] == 0,
          str(rep["cells"][f"{MODEL}|full"])[:200])

with tempfile.TemporaryDirectory() as root:
    write_tree(root, [src("m__full__t", [0, 1], TASKS),
                      src("m__baseline__t", [0, 1], TASKS, cfg="baseline",
                          flags={"envboot": False})])
    r = run_compare(root, "--tag", "t")
    check("a clean grid is not accused of anything",
          "REFUSING TO CERTIFY" not in r.stdout and "UNSERVED" not in r.stdout,
          r.stdout[-300:])

# ------------------------------------------------- H4 across two tags and arms
#
# The interaction is the reason the hosted arm exists, and it could not be
# computed: compare.py needs both models under one --tag, the local arms live
# under `hard`, and the hosted one under `ext-cerebras`. --tag already pools a
# comma-separated list, so the plumbing was there; what was missing was any
# statement that the two arms ran under DIFFERENT protocols. A reader would
# have seen two pass rates in one table and compared them.
print("interaction across two tags, two arms, two protocols")
with tempfile.TemporaryDirectory() as root:
    local = dict(PROTO)                       # 32768 / 4096, on a GH200
    hosted = dict(PROTO, num_ctx=65536, num_predict=16384,
                  env_gpu=None, env_node=None, env_ollama_version=None,
                  env_api_provider="cerebras",
                  env_api_endpoint="https://api.cerebras.ai/v1/chat/completions")
    srcs = []
    for cfg, flags in (("full", None), ("baseline", {"envboot": False})):
        a = src(f"local__{cfg}__hard", [0, 1, 2], TASKS, proto=local, cfg=cfg,
                flags=flags, passed=lambda t, r, c=cfg: c == "full" or t != "alpha")
        a["_model"] = MODEL
        b = src(f"hosted__{cfg}__ext", [0, 1, 2], TASKS, proto=hosted, cfg=cfg,
                flags=flags, passed=lambda t, r, c=cfg: c == "full" or t == "alpha")
        b["_model"] = OTHER
        srcs += [a, b]
    write_tree(root, srcs)
    out = os.path.join(root, "h4.json")
    r = run_compare(root, "--tag", "hard,ext", "--json-out", out)
    check("compare.py loads both tags as two arms", r.returncode == 0,
          r.stdout[-500:] + r.stderr[-500:])
    check("the interaction is actually computed",
          "need >=2 models" not in r.stdout
          and "need \u22652 models" not in r.stdout
          and "need ≥2 models" not in r.stdout, r.stdout[-600:])
    rep = json.load(open(out))
    check("and lands in the report", bool(rep.get("interaction")),
          str(list(rep.get("interaction", {}))[:4]))
    check("the differing protocol between arms is declared",
          "different protocols" in r.stdout, r.stdout[-800:])
    drift = rep.get("arm_protocol_drift") or []
    check("and the differing keys are named in the report",
          "num_ctx" in drift and "num_predict" in drift, str(drift))
    check("the note says pass rates are not comparable across arms",
          "NOT comparable" in r.stdout, r.stdout[-800:])

with tempfile.TemporaryDirectory() as root:
    srcs = []
    for cfg, flags in (("full", None), ("baseline", {"envboot": False})):
        a = src(f"a__{cfg}__t", [0, 1, 2], TASKS, cfg=cfg, flags=flags)
        a["_model"] = MODEL
        b = src(f"b__{cfg}__t", [0, 1, 2], TASKS, cfg=cfg, flags=flags,
                passed=lambda t, r: t != "alpha")
        b["_model"] = OTHER
        srcs += [a, b]
    write_tree(root, srcs)
    r = run_compare(root, "--tag", "t")
    check("two arms on the SAME protocol are not accused of differing",
          "different protocols" not in r.stdout, r.stdout[-400:])

print()
print(f"{len(PASS)} passed, {len(FAIL)} failed")
if FAIL:
    sys.exit(1)
