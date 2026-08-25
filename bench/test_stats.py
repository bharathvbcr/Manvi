"""Unit tests for bootstrap CIs, paired deltas, and seed pinning. No GPU."""
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mh.stats import (aligned_deltas, bootstrap_ci, capability_arms, interaction, mean,
                      pass_rates_by_repeat, role_of, usable_rows)
from run import seed_for_repeat

PASS, FAIL = [], []


def _raises(fn, exc):
    """True when `fn` raises `exc`. Fail-closed: returning normally is a fail."""
    try:
        fn()
    except exc:
        return True
    return False



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

print("starved rows excluded from pass rates")
# A genuine starved row carries the timeout evidence that defines it: the
# server never answered. Without that evidence it is an ordinary failure.
mixed = [
    {"task": "a", "rep": 0, "passed": True, "steps": 5, "output_tokens": 10,
     "stop_reason": "finished"},
    {"task": "b", "rep": 0, "passed": False, "steps": 1, "output_tokens": 0,
     "stop_reason": "error:ModelError",
     "errors": ["ModelError: TimeoutError: timed out"]},
]
check("usable drops starved", len(usable_rows(mixed)) == 1)
rates_u = pass_rates_by_repeat(mixed)
check("starved not in denominator", abs(rates_u[0] - 1.0) < 1e-12, str(rates_u))

# The guarantee that matters for a reported rate: a first-turn failure that is
# NOT a timeout is a real result and stays in the denominator. Dropping it
# would shrink the sample instead of scoring the failure.
real_fail = [
    {"task": "a", "rep": 0, "passed": True, "steps": 5, "output_tokens": 10,
     "stop_reason": "finished"},
    {"task": "b", "rep": 0, "passed": False, "steps": 1, "output_tokens": 0,
     "stop_reason": "error:ModelError",
     "errors": ["ModelError: HTTP 500: llama-server returned invalid tool "
                "call arguments"]},
]
check("non-timeout first-turn error is kept", len(usable_rows(real_fail)) == 2)
rates_r = pass_rates_by_repeat(real_fail)
check("non-timeout failure counts against the rate",
      abs(rates_r[0] - 0.5) < 1e-12, str(rates_r))
check("malformed-body failure is kept", len(usable_rows([
    {"task": "c", "rep": 0, "passed": False, "steps": 1, "output_tokens": 0,
     "stop_reason": "error:JSONDecodeError"}])) == 1)

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

# ---------------------------------------------------------------------------
# Regression tests for the statistics defects found in the pre-submission audit.
# Each block fails against the code as it stood before the corresponding fix.
# `probe` turns an exception into a recorded failure so reverting one hunk shows
# up as one failing check rather than aborting the file.

def _need(name):
    """mh.stats.<name>, or a stub that fails whichever check reaches for it.

    Importing these at the top would abort the whole file when one is missing;
    a missing statistic should fail its own checks and leave the rest readable.
    """
    import mh.stats
    fn = getattr(mh.stats, name, None)
    if fn is not None:
        return fn

    def absent(*_a, **_k):
        raise AttributeError(f"mh.stats.{name} does not exist")
    return absent


def probe(name, fn, detail=""):
    try:
        ok = bool(fn())
    except Exception as e:                                  # noqa: BLE001
        check(name, False, f"{type(e).__name__}: {e}")
        return None
    if callable(detail):
        try:
            detail = str(detail())
        except Exception as e:                              # noqa: BLE001
            detail = f"{type(e).__name__}: {e}"
    check(name, ok, detail)
    return ok


print("B1 interaction: seed-paired resampling")
_inter = _need("interaction")
aligned_interaction = _need("aligned_interaction")

# Arms whose per-repeat difference is a constant 0.1. Under the paired scheme
# every resample draws the same index for both arms, so every bootstrap replicate
# has difference exactly 0.1 and the interval has no width. Under the unpaired
# scheme the two arms are resampled independently, the shared per-seed variation
# never cancels, and the interval is wide. This is the whole defect in one case.
W = [0.50, 0.40, 0.30, 0.20, 0.10]
S = [0.40, 0.30, 0.20, 0.10, 0.00]
probe("paired keeps the seed pairing",
      lambda: abs(_inter(W, S, paired=True, rng_seed=4)["lo"] - 0.1) < 1e-12
      and abs(_inter(W, S, paired=True, rng_seed=4)["hi"] - 0.1) < 1e-12)
probe("unpaired discards it and is wide",
      lambda: (_inter(W, S, rng_seed=4)["hi"]
               - _inter(W, S, rng_seed=4)["lo"]) > 0.15)
probe("both schemes agree on the point estimate",
      lambda: abs(_inter(W, S, paired=True, rng_seed=4)["delta_weak_minus_strong"]
                  - _inter(W, S, rng_seed=4)["delta_weak_minus_strong"]) < 1e-12)
# Correlated but not perfectly: paired must still be the narrower interval.
WC = [0.30, 0.42, 0.35, 0.50, 0.40]
SC = [0.10, 0.20, 0.16, 0.27, 0.19]
probe("paired is narrower whenever the arms correlate",
      lambda: (_inter(WC, SC, paired=True, rng_seed=7)["hi"]
               - _inter(WC, SC, paired=True, rng_seed=7)["lo"])
      < (_inter(WC, SC, rng_seed=7)["hi"] - _inter(WC, SC, rng_seed=7)["lo"]))
probe("paired refuses arms that are not index-aligned",
      lambda: _raises(lambda: _inter([0.1, 0.2], [0.1], paired=True), ValueError))
probe("paired is deterministic in the seed",
      lambda: _inter(WC, SC, paired=True, rng_seed=1)
      == _inter(WC, SC, paired=True, rng_seed=1))
probe("weights are rejected on the unpaired scheme",
      lambda: _raises(lambda: _inter(W, S, weights=[1, 1, 1, 1, 1]), ValueError))

print("B7 interaction alignment keeps its repeat keys")
probe("aligned_interaction pairs on the shared repeats",
      lambda: aligned_interaction({0: 0.1, 1: 0.2, 4: 0.3},
                                  {0: 0.0, 1: 0.1, 5: 0.9})
      == ([0.1, 0.2], [0.0, 0.1], [0, 1], [4, 5]))
probe("a 2-of-5 pairing names the repeats it dropped",
      lambda: aligned_interaction({r: 0.1 for r in range(5)},
                                  {0: 0.0, 3: 0.0})[3] == [1, 2, 4])
probe("a complete pairing drops nothing",
      lambda: aligned_interaction({r: 0.1 for r in range(5)},
                                  {r: 0.0 for r in range(5)})[3] == [])

print("B6 denominators survive the rate")
denominators_of = _need("denominators_of")
pass_counts_by_repeat = _need("pass_counts_by_repeat")
RAGGED = ([{"task": f"t{i}", "rep": 0, "passed": i < 4} for i in range(8)]
          + [{"task": "t0", "rep": 1, "passed": True}])
probe("counts carry passed and scored",
      lambda: pass_counts_by_repeat(RAGGED) == {0: (4, 8), 1: (1, 1)})
probe("denominators_of reads them off",
      lambda: denominators_of(pass_counts_by_repeat(RAGGED)) == {0: 8, 1: 1})
probe("rates still look identical, which is the problem",
      lambda: pass_rates_by_repeat(RAGGED) == {0: 0.5, 1: 1.0})
# A one-task repeat carrying the same weight as an eight-task one moves the
# headline mean by 19.4 points here.
probe("unweighted mean over-counts the thin repeat",
      lambda: abs(bootstrap_ci([0.5, 1.0])[0] - 0.75) < 1e-12)
probe("weighted mean is the pooled rate",
      lambda: abs(bootstrap_ci([0.5, 1.0], weights=[8, 1])[0] - 5.0 / 9.0) < 1e-12)
probe("equal weights change nothing at all",
      lambda: bootstrap_ci([0.0, 1.0, 0.0, 1.0, 0.0], rng_seed=1)
      == bootstrap_ci([0.0, 1.0, 0.0, 1.0, 0.0], rng_seed=1, weights=[3, 3, 3, 3, 3]))
probe("weighted resampling moves the interval, not just the point",
      lambda: bootstrap_ci([0.5, 0.5, 0.5, 0.5, 1.0],
                           weights=[8, 8, 8, 8, 1])[2]
      < bootstrap_ci([0.5, 0.5, 0.5, 0.5, 1.0])[2])
probe("a negative weight is refused",
      lambda: _raises(lambda: bootstrap_ci([0.5, 1.0], weights=[-1, 1]), ValueError))
probe("a mis-sized weight vector is refused",
      lambda: _raises(lambda: bootstrap_ci([0.5, 1.0], weights=[1]), ValueError))

print("B8 an interval with no width says so")
ci_degeneracy = _need("ci_degeneracy")
probe("saturated cell is flagged",
      lambda: "no observed variance" in (ci_degeneracy([1.0] * 5) or ""))
probe("saturated cell really does render as zero width",
      lambda: bootstrap_ci([1.0] * 5)[1] == bootstrap_ci([1.0] * 5)[2])
probe("n=1 is flagged as not a bootstrap",
      lambda: "single repeat" in (ci_degeneracy([0.5]) or ""))
probe("n=0 is flagged",
      lambda: ci_degeneracy([]) is not None)
probe("a cell with spread is not flagged",
      lambda: ci_degeneracy([0.5, 0.625, 0.5]) is None)
probe("a replicated zero-variance cell is distinguishable from n=1",
      lambda: ci_degeneracy([1.0] * 5) != ci_degeneracy([1.0]))

print("B4 measured coverage of the 95% interval")
bootstrap_coverage = _need("bootstrap_coverage")
try:
    COV = bootstrap_coverage(n_repeats=5, n_tasks=8, p=0.65, trials=600,
                             n_boot=2000)
except Exception as _e:                                          # noqa: BLE001
    COV, _COV_ERR = {}, _e
probe("the audit runs at all", lambda: bool(COV))
probe("nominal is stated", lambda: COV["nominal"] == 0.95)
probe("measured is far below nominal",
      lambda: 0.70 < COV["measured"] < 0.93, lambda: COV["measured"])
probe("the measurement carries its own error bar",
      lambda: 0.0 < COV["mc_stderr"] < 0.05)
probe("zero-width intervals are counted, not assumed away",
      lambda: 0.0 <= COV["zero_width_fraction"] < 0.2)
probe("the settings travel with the number",
      lambda: (COV["trials"], COV["n_repeats"], COV["n_tasks"], COV["p"])
      == (600, 5, 8, 0.65))
probe("deterministic in its seed",
      lambda: bootstrap_coverage(trials=200, n_boot=500, rng_seed=3)["measured"]
      == bootstrap_coverage(trials=200, n_boot=500, rng_seed=3)["measured"])
probe("zero trials is refused",
      lambda: _raises(lambda: bootstrap_coverage(trials=0), ValueError))

print("B5 family-wise error across the ladder")
family_wise_error = _need("family_wise_error")
multiplicity_report = _need("multiplicity_report")
sidak_alpha = _need("sidak_alpha")
probe("16 nominal intervals exclude zero somewhere 56% of the time",
      lambda: abs(family_wise_error(16, 0.95) - 0.5599) < 5e-4,
      lambda: family_wise_error(16, 0.95))
probe("at the measured coverage it is 88%",
      lambda: abs(family_wise_error(16, 0.876) - 0.8798) < 5e-4,
      lambda: family_wise_error(16, 0.876))
probe("one interval is just alpha",
      lambda: abs(family_wise_error(1, 0.95) - 0.05) < 1e-12)
probe("no intervals is zero", lambda: family_wise_error(0) == 0.0)
probe("sidak alpha for 16 intervals",
      lambda: abs(sidak_alpha(16, 0.05) - 0.0032) < 5e-5, lambda: sidak_alpha(16))
probe("a coverage outside [0,1] is refused",
      lambda: _raises(lambda: family_wise_error(4, 1.5), ValueError))
try:
    MR = multiplicity_report(16, 1, measured_coverage=0.876)
except Exception:                                          # noqa: BLE001
    MR = {}
probe("the report names how many excluded zero",
      lambda: MR["n_excluding_zero"] == 1 and MR["n_intervals"] == 16)
probe("the report carries both family-wise rates",
      lambda: MR["fwer_at_nominal_coverage"] < MR["fwer_at_measured_coverage"])
probe("and states that no correction was applied",
      lambda: MR["correction_applied"] is False)
probe("measured coverage is optional but its absence is visible",
      lambda: "fwer_at_measured_coverage" not in multiplicity_report(16, 1))

print("B9 capability arms")
capability_arms_detail = _need("capability_arms_detail")
probe("an infinite mean is not a capability",
      lambda: capability_arms({"a": 0.5, "b": float("inf")}) is None)
probe("negative infinity is dropped too",
      lambda: capability_arms({"a": 0.5, "b": float("-inf")}) is None)
probe("a non-finite arm is named, not silently dropped",
      lambda: any("not finite" in x for x in capability_arms_detail(
          {"a": 0.5, "b": 0.7, "c": float("inf")})["ignored"]))
probe("two arms of identical capability are flagged",
      lambda: capability_arms_detail({"a": 0.5, "b": 0.5})["tied"] is True)
probe("a real gap is not flagged",
      lambda: capability_arms_detail({"a": 0.2, "b": 0.9})["tied"] is False)
probe("the gap itself is reported",
      lambda: abs(capability_arms_detail({"a": 0.2, "b": 0.9})["capability_gap"]
                  - 0.7) < 1e-12)
probe("the thin accessor still returns the pair",
      lambda: capability_arms({"gemma": 0.2, "qwen": 0.9}) == ("gemma", "qwen"))

print("B2 figure captions are derived, not written down")
import figures
REPORT_A = {
    "cells": {"m|full": {"n": 40, "n_repeats": 5, "n_starved": 0,
                         "mean": 0.9, "lo": 0.8, "hi": 1.0,
                         "rates": {"0": 0.9}}},
    "per_task": {"m|full": {f"t{i}": {"passed": 1, "n": 5} for i in range(8)}},
    "interaction": {"a": {"delta_weak_minus_strong": 0.3, "lo": 0.1, "hi": 0.5},
                    "b": {"delta_weak_minus_strong": 0.0, "lo": -0.2, "hi": 0.2}},
}
REPORT_B = {
    "cells": {"m|full": {"n": 12, "n_repeats": 3, "n_starved": 2,
                         "mean": 0.5, "lo": 0.2, "hi": 0.8,
                         "rates": {"0": 0.5}}},
    "per_task": {"m|full": {f"t{i}": {"passed": 1, "n": 3} for i in range(4)}},
    "interaction": {"b": {"delta_weak_minus_strong": 0.0, "lo": -0.2, "hi": 0.2}},
}
probe("episode caption reads the real shape",
      lambda: figures.episode_caption(REPORT_A).startswith(
          "40 episodes per cell (8 tasks &#215; 5 pinned seeds)"),
      lambda: figures.episode_caption(REPORT_A))
probe("a different shape relabels itself",
      lambda: figures.episode_caption(REPORT_B).startswith(
          "12 episodes per cell (4 tasks &#215; 3 pinned seeds)"),
      lambda: figures.episode_caption(REPORT_B))
probe("excluded episodes are named in the caption",
      lambda: "2 starved episodes excluded" in figures.episode_caption(REPORT_B))
probe("a ragged grid is captioned as a range",
      lambda: "32–40 episodes" in figures.episode_caption({
          "cells": {"a": {"n": 40, "n_repeats": 5, "n_starved": 0},
                    "b": {"n": 32, "n_repeats": 4, "n_starved": 0}},
          "per_task": {}}))
probe("an interaction that excludes zero is not captioned as crossing it",
      lambda: "cross zero" not in figures.interaction_caption(
          REPORT_A, sorted(REPORT_A["interaction"].items())))
probe("the caption counts the intervals that exclude zero",
      lambda: "1 of 2 intervals excludes zero" in figures.interaction_caption(
          REPORT_A, sorted(REPORT_A["interaction"].items())),
      lambda: figures.interaction_caption(
          REPORT_A, sorted(REPORT_A["interaction"].items())))
probe("all-crossing is stated as all-crossing",
      lambda: "All 1 intervals cross zero." in figures.interaction_caption(
          REPORT_B, sorted(REPORT_B["interaction"].items()))
      or "The interval crosses zero." in figures.interaction_caption(
          REPORT_B, sorted(REPORT_B["interaction"].items())))
probe("an empty ladder says so",
      lambda: "No interaction" in figures.interaction_caption(REPORT_A, []))

import tempfile as _tf


def _render_interaction(report):
    with _tf.TemporaryDirectory() as d:
        path = os.path.join(d, "i.svg")
        figures.interaction_svg(report, path)
        return open(path).read()


probe("the rendered SVG does not carry the old literal caption",
      lambda: "Every interval crosses zero" not in _render_interaction(REPORT_A))
probe("the rendered SVG counts what it drew",
      lambda: "1 of 2 intervals excludes zero" in _render_interaction(REPORT_A))
probe("a ladder that all crosses zero is rendered as such",
      lambda: "crosses zero" in _render_interaction(REPORT_B)
      or "cross zero" in _render_interaction(REPORT_B))


def _render_pass_rates(report):
    with _tf.TemporaryDirectory() as d:
        path = os.path.join(d, "p.svg")
        figures.from_report(report, d)
        return open(os.path.join(d, "pass_rates.generated.svg")).read()


probe("the rendered pass-rate SVG does not carry the old literal shape",
      lambda: "40 episodes per cell (8 tasks &#215; 5 pinned seeds)"
      not in _render_pass_rates(REPORT_B))
probe("it carries this report's shape instead",
      lambda: "12 episodes per cell (4 tasks &#215; 3 pinned seeds)"
      in _render_pass_rates(REPORT_B))

print("B3 figures never render by accident")
import inspect
probe("from_report has no default output directory",
      lambda: inspect.signature(figures.from_report)
      .parameters["outdir"].default is inspect.Parameter.empty)
probe("an empty output directory is refused",
      lambda: _raises(lambda: figures.from_report(REPORT_A, ""), ValueError))
probe("compare.py exposes an explicit --figures flag",
      lambda: "--figures" in __import__("compare").main.__doc__ if
      __import__("compare").main.__doc__ else
      "--figures" in inspect.getsource(__import__("compare").main))
probe("compare.py's json-out path no longer imports figures",
      lambda: "from figures import from_report"
      not in inspect.getsource(__import__("compare").main).split("--figures")[0])

print("json output is always valid JSON")
import compare as _cmp
probe("a nan becomes null and is named",
      lambda: _cmp.json_safe({"a": float("nan"), "b": 1.0})
      == ({"a": None, "b": 1.0}, ["report.a"]))
probe("infinities are caught too",
      lambda: _cmp.json_safe([float("inf"), float("-inf")])[0] == [None, None])
probe("nested paths are reported",
      lambda: _cmp.json_safe({"x": {"y": [1.0, float("nan")]}})[1]
      == ["report.x.y[1]"])
probe("clean documents are untouched",
      lambda: _cmp.json_safe({"a": [1, "s", True, None]})
      == ({"a": [1, "s", True, None]}, []))
probe("the cleaned document survives allow_nan=False",
      lambda: json.dumps(_cmp.json_safe(
          {"a": float("nan"), "b": [float("inf")]})[0], allow_nan=False)
      == '{"a": null, "b": [null]}')
probe("an uncleaned document would not have",
      lambda: _raises(lambda: json.dumps({"a": float("nan")}, allow_nan=False),
                      ValueError))

print()
if FAIL:
    print(f"FAIL {len(FAIL)}/{len(PASS)+len(FAIL)}")
    sys.exit(1)
print(f"ok {len(PASS)} stats tests")
sys.exit(0)
