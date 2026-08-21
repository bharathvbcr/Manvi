"""Summarise and compare benchmark runs.

Reads results/*/summary.json. Prints a per-task matrix, then bootstrap 95%
CIs on pass rates and ablation deltas. The headline test is whether
Δ_weak > Δ_strong.
"""
import argparse
import json
import os
import sys
from collections import OrderedDict

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mh.stats import (aligned_deltas, bootstrap_ci, capability_arms, interaction,
                      mean, pass_rates_by_repeat, role_of)

HERE = os.path.dirname(os.path.abspath(__file__))
RESULTS = os.path.join(HERE, "results")
ABLATIONS = ("baseline", "no-envboot", "no-verifygate",
             "no-checklist", "no-loopbreak", "no-outcap", "no-groundfs",
             "no-nativetools")


def load(filter_sub=None, tag=None):
    runs = []
    if not os.path.isdir(RESULTS):
        return runs
    for d in sorted(os.listdir(RESULTS)):
        p = os.path.join(RESULTS, d, "summary.json")
        if not os.path.isfile(p):
            continue
        if filter_sub and filter_sub not in d:
            continue
        if tag and not d.endswith("__" + tag):
            continue
        with open(p) as f:
            s = json.load(f)
        s["dir"] = d
        runs.append(s)
    return runs


def short(run):
    m = run["model"].split("/")[-1]
    m = m.replace("-GGUF:Q8_0", "-Q8").replace(":27b-mlx", "-27B")
    m = m.replace(":27b", "-27B").replace(":e4b", "-E4B")
    return f"{m} [{run['config']['name']}]"


def _fmt_ci(trip, as_pct=True):
    m, lo, hi = trip
    if m != m:  # nan
        return "n/a"
    if as_pct:
        return f"{100 * m:5.1f}%  [{100 * lo:5.1f}, {100 * hi:5.1f}]"
    return f"{m:+.3f}  [{lo:+.3f}, {hi:+.3f}]"


def grouped(runs):
    """(model, config_name) -> merged rows (last summary wins on metadata)."""
    out = OrderedDict()
    for r in runs:
        key = (r["model"], r["config"]["name"])
        if key not in out:
            out[key] = {"model": r["model"], "config": r["config"],
                        "dir": r["dir"], "rows": []}
        out[key]["rows"].extend(r["rows"])
        out[key]["summary"] = r
    return out


def stats_report(runs):
    cells = grouped(runs)
    if not cells:
        return {}
    report = {"cells": {}, "deltas": {}, "interaction": {}, "per_task": {},
              "stops": {}}

    print("\n## Pass rates with 95% bootstrap CI (per-repeat)\n")
    print(f"{'model':<42} {'config':<16} {'n':>3}  mean [95% CI]")
    print("-" * 88)
    for (model, cfg), cell in cells.items():
        rates = pass_rates_by_repeat(cell["rows"])
        xs = list(rates.values())
        ci = bootstrap_ci(xs)
        report["cells"][f"{model}|{cfg}"] = {
            "role": role_of(model),
            "n_repeats": len(xs),
            "rates": rates,
            "mean": ci[0], "lo": ci[1], "hi": ci[2],
        }
        print(f"{model:<42} {cfg:<16} {len(xs):>3}  {_fmt_ci(ci)}")

    for (model, cfg), cell in cells.items():
        key = f"{model}|{cfg}"
        by = {}
        stops = {}
        for r in cell["rows"]:
            t = r.get("task", "?")
            rec = by.setdefault(t, [0, 0])
            rec[1] += 1
            rec[0] += int(bool(r.get("passed")))
            sr = r.get("stop_reason", "?")
            stops[sr] = stops.get(sr, 0) + 1
        report["per_task"][key] = {t: {"passed": p, "n": n} for t, (p, n) in by.items()}
        report["stops"][key] = stops

    print("\n## Ablation deltas  Δ = full − ablation  (paired by repeat)\n")
    print(f"{'model':<42} {'ablation':<16} {'n':>3}  Δ mean [95% CI]")
    print("-" * 88)
    by_model = {}
    for (model, cfg), cell in cells.items():
        by_model.setdefault(model, {})[cfg] = pass_rates_by_repeat(cell["rows"])
    for model, cfgs in by_model.items():
        full = cfgs.get("full")
        if not full:
            continue
        for abl in ABLATIONS:
            other = cfgs.get(abl)
            if not other:
                continue
            deltas, keys = aligned_deltas(full, other)
            ci = bootstrap_ci(deltas)
            report["deltas"][f"{model}|{abl}"] = {
                "role": role_of(model),
                "n": len(deltas), "reps": keys,
                "mean": ci[0], "lo": ci[1], "hi": ci[2],
            }
            print(f"{model:<42} {abl:<16} {len(deltas):>3}  {_fmt_ci(ci, as_pct=False)}")

    print("\n## Interaction  Δ_weaker > Δ_stronger  (empirical full-harness means)\n")
    print(f"{'ablation':<16} {'Δw−Δs':>8}  95% CI                verdict")
    print("-" * 70)
    full_means = {}
    for model, cfgs in by_model.items():
        if "full" in cfgs and "baseline" in cfgs:
            full_means[model] = mean(cfgs["full"].values())
    arms = capability_arms(full_means)
    if not arms:
        print("need ≥2 models with both full and baseline in this tag")
        print("arms are ranked by full-harness mean, not parameter count")
        report["interaction"]["_note"] = "need two models with full+baseline"
        return report
    weak, strong = arms
    report["interaction"]["_arms"] = {
        "weaker": weak, "stronger": strong,
        "weaker_full_mean": full_means[weak],
        "stronger_full_mean": full_means[strong],
    }
    print(f"weaker={weak}  (full mean {full_means[weak]:.3f})")
    print(f"stronger={strong}  (full mean {full_means[strong]:.3f})")
    for abl in ABLATIONS:
        dw = report["deltas"].get(f"{weak}|{abl}")
        ds = report["deltas"].get(f"{strong}|{abl}")
        if not (dw and ds):
            continue
        # rebuild per-repeat deltas
        w_full = by_model[weak].get("full")
        w_abl = by_model[weak].get(abl)
        s_full = by_model[strong].get("full")
        s_abl = by_model[strong].get(abl)
        w_d, _ = aligned_deltas(w_full, w_abl)
        s_d, _ = aligned_deltas(s_full, s_abl)
        inter = interaction(w_d, s_d)
        report["interaction"][abl] = inter
        if inter["detectable"]:
            verdict = "detectable (CI > 0)"
        elif inter["hi"] < 0:
            verdict = "reversed (CI < 0)"
        else:
            verdict = "underpowered / not detected at this n"
        print(f"{abl:<16} {inter['delta_weak_minus_strong']:+8.3f}  "
              f"[{inter['lo']:+.3f}, {inter['hi']:+.3f}]  {verdict}")
    print()
    print("A CI that includes 0 is not a failed experiment; n=5 with ~15 tasks")
    print("is a small sample. 'We could not detect it at this n' is the claim.")
    return report


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("filter", nargs="?", default=None,
                    help="substring of the results directory name")
    ap.add_argument("--tag", default="",
                    help="only directories ending in __TAG")
    ap.add_argument("--json-out", default="",
                    help="write the stats report as JSON")
    args = ap.parse_args()
    runs = load(args.filter, args.tag or None)
    if not runs:
        print("no results yet")
        return
    tasks = OrderedDict()
    for r in runs:
        for row in r["rows"]:
            tasks.setdefault(row["task"], None)

    labels = [short(r) for r in runs]
    w = max(len(l) for l in labels) + 2
    print("(cell shows verdict and step count)\n")
    print(f"{'task':<24}" + "".join(f"{l:<{w}}" for l in labels))
    print("-" * (24 + w * len(labels)))
    for t in tasks:
        line = f"{t:<24}"
        for r in runs:
            rows = [x for x in r["rows"] if x["task"] == t]
            if not rows:
                cell = "-"
            else:
                p = sum(1 for x in rows if x.get("passed"))
                cell = ("PASS" if p == len(rows) else
                        "fail" if p == 0 else f"{p}/{len(rows)}")
                st = rows[0].get("steps")
                cell += f" ({st})" if st is not None else ""
            line += f"{cell:<{w}}"
        print(line)
    print("-" * (24 + w * len(labels)))
    for key, fmt in (("pass_rate", "{:.1f}%"), ("wall_s", "{:.0f}s"),
                     ("output_tokens", "{:,}")):
        line = f"{key:<24}"
        for r in runs:
            line += f"{fmt.format(r[key]):<{w}}"
        print(line)
    print()
    for r in runs:
        stops = {}
        for row in r["rows"]:
            stops[row.get("stop_reason", "?")] = stops.get(row.get("stop_reason", "?"), 0) + 1
        print(f"{short(r):<42} {r['passed']}/{r['n']}  stop_reasons={stops}")

    report = stats_report(runs)
    if args.json_out:
        with open(args.json_out, "w") as f:
            json.dump(report, f, indent=2, default=str)
        print(f"\nwrote {args.json_out}")
        try:
            from figures import from_report
            from_report(report)
        except Exception as e:
            print(f"figures.py skipped: {e}")


if __name__ == "__main__":
    main()
