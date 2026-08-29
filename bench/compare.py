"""Summarise and compare benchmark runs.

Reads results/*/summary.json. Prints a per-task matrix, then bootstrap 95%
CIs on pass rates and ablation deltas. The headline test is whether
Δ_weak > Δ_strong.
"""
import argparse
import json
import math
import os
import sys
from collections import OrderedDict

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mh.stats import (aligned_interaction, bootstrap_ci,
                      bootstrap_coverage, capability_arms_detail,
                      ci_degeneracy, deltas_by_repeat, denominators_of,
                      interaction, mean, multiplicity_report,
                      pass_counts_by_repeat, pearson_r, rates_of,
                      role_of, sidak_alpha, usable_rows)
from mh.pool import (arm_drift, arms_drift, contrast_conflicts, contrast_drift,
                     merge_conflicts, pooled_drift, ragged_reps,
                     rep_denominators, reps_of, seed_conflicts, seed_reuse,
                     unseeded_cells)
from mh.runtime import is_starved_episode, is_unserved_episode

HERE = os.path.dirname(os.path.abspath(__file__))
# Overridable so tests can build a throwaway grid instead of writing
# into the real results tree.
RESULTS = os.environ.get("MH_RESULTS") or os.path.join(HERE, "results")
ABLATIONS = ("baseline", "no-envboot", "no-verifygate",
             "no-checklist", "no-loopbreak", "no-outcap", "no-groundfs",
             "no-nativetools")


class CellMergeError(RuntimeError):
    """Raised when directories cannot be soundly assembled into cells."""

    def __init__(self, problems):
        self.problems = list(problems)
        super().__init__(f"{len(self.problems)} cell-assembly conflict(s)")


def load(filter_sub=None, tags=(), exclude=()):
    """Summaries to report on. `tags` selects; naming several pools them."""
    tags = tuple(t for t in (tags or ()) if t)
    runs, problems = [], []
    if not os.path.isdir(RESULTS):
        return runs
    for d in sorted(os.listdir(RESULTS)):
        p = os.path.join(RESULTS, d, "summary.json")
        if not os.path.isfile(p):
            continue
        if filter_sub and filter_sub not in d:
            continue
        if any(x and x in d for x in exclude):
            continue
        if tags and not any(d.endswith("__" + t) for t in tags):
            continue
        try:
            with open(p) as f:
                s = json.load(f)
        except (OSError, ValueError) as e:
            problems.append(f"{d}: summary.json is unreadable ({e})")
            continue
        if not isinstance(s, dict):
            problems.append(f"{d}: summary.json is a {type(s).__name__}, "
                            f"not an object")
            continue
        missing = [k for k in ("model", "config", "rows") if k not in s]
        if missing:
            problems.append(f"{d}: summary.json has no {', '.join(missing)}")
            continue
        if not isinstance(s["config"], dict):
            problems.append(f"{d}: config is a {type(s['config']).__name__}, "
                            f"not an object")
            continue
        if not isinstance(s["rows"], list):
            problems.append(f"{d}: rows is a {type(s['rows']).__name__}, "
                            f"not a list")
            continue
        bad = [i for i, r in enumerate(s["rows"]) if not isinstance(r, dict)]
        if bad:
            problems.append(f"{d}: {len(bad)} row(s) are not objects "
                            f"(first at index {bad[0]})")
            continue
        s["dir"] = d
        runs.append(s)
    if problems:
        raise CellMergeError(problems)
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


def grouped(runs, allow_drift=False):
    """(model, config_name) -> merged rows, refusing an unsound merge.

    Two directories only pool into one cell when they agree on config flags
    and protocol, hold disjoint repeat indices, and score the same tasks.
    Anything else raises: a silent merge across protocols is the failure this
    whole path exists to prevent.
    """
    sources = OrderedDict()
    for r in runs:
        key = (r["model"], r["config"]["name"])
        sources.setdefault(key, []).append(
            {"dir": r["dir"], "protocol": r.get("protocol"),
             "config": r["config"], "rows": r["rows"], "summary": r})

    problems, out, prov = [], OrderedDict(), OrderedDict()
    for (model, cfg), srcs in sources.items():
        for c in merge_conflicts(srcs, allow_drift=allow_drift):
            problems.append(f"{model} [{cfg}]: {c}")
        rows = [row for s in srcs for row in s["rows"]]
        out[(model, cfg)] = {"model": model, "config": srcs[0]["config"],
                             "dir": srcs[0]["dir"], "rows": rows,
                             "summary": srcs[-1]["summary"]}
        prov[f"{model}|{cfg}"] = {
            "dirs": [s["dir"] for s in srcs],
            "pooled": len(srcs) > 1,
            "reps": reps_of(rows),
            "rep_denominators": rep_denominators(rows),
            "ragged_reps": ragged_reps(rows),
            "protocol": srcs[0].get("protocol"),
            "protocol_drift": pooled_drift(srcs),
            "protocol_drift_allowed": bool(allow_drift) and bool(pooled_drift(srcs)),
        }

    rows_by_cell = {k: v["rows"] for k, v in out.items()}
    for c in seed_conflicts(rows_by_cell):
        problems.append(f"seed/rep mismatch -- deltas pair by rep: {c}")
    for c in seed_reuse(rows_by_cell):
        problems.append(f"duplicated sample: {c}")
    for c in contrast_conflicts(rows_by_cell):
        problems.append(f"unpairable contrast: {c}")

    if problems:
        raise CellMergeError(problems)
    return out, prov


def json_safe(obj, path="report"):
    """(document, paths) with every non-finite float replaced by null.

    `json.dumps` writes bare `NaN` and `Infinity` for those, which is not JSON:
    strict parsers reject the file and permissive ones hand a downstream reader
    a float that quietly poisons any arithmetic it enters. A number we do not
    have has to read as absent, and the paths are returned so the absence gets
    named rather than silently substituted.
    """
    if isinstance(obj, float):
        return (obj, []) if math.isfinite(obj) else (None, [path])
    if isinstance(obj, dict):
        out, bad = {}, []
        for k, v in obj.items():
            clean, paths = json_safe(v, f"{path}.{k}")
            out[k] = clean
            bad.extend(paths)
        return out, bad
    if isinstance(obj, (list, tuple)):
        out, bad = [], []
        for i, v in enumerate(obj):
            clean, paths = json_safe(v, f"{path}[{i}]")
            out.append(clean)
            bad.extend(paths)
        return out, bad
    return obj, []


def write_report(report, path):
    """Write the stats JSON, refusing to emit anything that is not JSON."""
    doc, nonfinite = json_safe(report)
    if nonfinite:
        doc["nonfinite"] = sorted(nonfinite)
        print(f"\n## WARNING: {len(nonfinite)} non-finite value(s) written as "
              f"null (listed under `nonfinite` in the report)\n")
        for p in sorted(nonfinite)[:20]:
            print(f"  {p}")
    with open(path, "w") as f:
        # allow_nan=False so a value that escaped json_safe fails here rather
        # than shipping a file that is not JSON.
        json.dump(doc, f, indent=2, default=str, allow_nan=False)
    return doc


def _modal(values, default=None):
    """Most common value; ties break to the larger, so a short cell is the
    outlier rather than the standard."""
    counts = {}
    for v in values:
        counts[v] = counts.get(v, 0) + 1
    if not counts:
        return default
    return max(counts, key=lambda v: (counts[v], v))


def _median(values):
    xs = sorted(values)
    if not xs:
        return float("nan")
    mid = len(xs) // 2
    return xs[mid] if len(xs) % 2 else (xs[mid - 1] + xs[mid]) / 2.0


def interval_reliability(report, shape, trials=5000, n_boot=10000,
                         skip_reason=None):
    """Coverage and multiplicity blocks for the ladder this report just built.

    Both answer questions the tables cannot: a "95%" interval built from five
    points does not cover 95% of the time, and sixteen of them exclude zero
    somewhere far more often than 5% of the time. Neither number appeared
    anywhere in the code or the writeup, so "the only interval excluding zero"
    read as a finding.
    """
    ladder = report.get("deltas", {})
    n_excl = sum(1 for v in ladder.values()
                 if (v["lo"] > 0) or (v["hi"] < 0))
    if skip_reason:
        # A check that could not run must never read as one that ran and
        # passed, so the block is present and says so.
        coverage = {"skipped": True, "reason": skip_reason,
                    "nominal": 0.95, "measured": None}
        measured = None
    else:
        # `p_source` and `cell_mean_range` describe where the audited shape
        # came from; they travel with the number but are not settings.
        provenance = ("p_source", "cell_mean_range")
        coverage = bootstrap_coverage(
            trials=trials, n_boot=n_boot,
            **{k: v for k, v in shape.items() if k not in provenance})
        coverage.update({k: shape[k] for k in provenance if k in shape})
        measured = coverage["measured"]
    return coverage, multiplicity_report(len(ladder), n_excl,
                                         measured_coverage=measured)


# The preregistered confirmatory tests, and only these. H1 is one test per
# model; H2 is one test. Everything else on the ladder is H3 and is exploratory
# by declaration, reported at 95% with the family-wise number beside it.
CONFIRMATORY = (("H1", "baseline", "full harness beats all-off baseline"),
                ("H2", "no-outcap", "the output cap does not hurt"))


def _confirmatory_block(report, delta_by_rep, pair_weights, cell_denoms):
    """Preregistered claims at their Šidák-corrected level, not at 95%.

    The ladder above prints 95% intervals for everything, which is correct for
    the exploratory family and wrong for a confirmatory claim: §5 of the
    registration splits alpha across the preregistered tests, so H1 and H2 are
    decided on ~98.3% intervals (three tests) and NOT on the 95% ones printed
    above. Nothing in this file used to say so, and a reader with the 95% table
    in front of them had no way to know which rows were the registered claims
    or that those rows are decided at a different width. That is how a marginal
    delta gets quoted as confirmed.

    The corrected interval is computed here rather than left as an exercise --
    a claim whose decision rule is documented but never evaluated is not a
    decision rule.
    """
    models = sorted({m for (m, _abl) in delta_by_rep})
    tests = [(h, abl, why, m) for (h, abl, why) in CONFIRMATORY
             for m in models if (m, abl) in delta_by_rep]
    if not tests:
        return
    k = len(tests)
    alpha = sidak_alpha(k, 0.05)
    level = 100.0 * (1.0 - alpha)
    print(f"\n## Preregistered confirmatory tests  ({k} test(s), Šidák "
          f"α={alpha:.4f}, i.e. {level:.1f}% intervals)\n")
    print(f"{'':<4} {'model':<30} {'contrast':<14} {'Δ':>7}  "
          f"{level:.1f}% CI{'':<8} verdict")
    print("-" * 104)
    out = {}
    for h, abl, why, model in tests:
        by_rep = delta_by_rep[(model, abl)]
        keys = sorted(by_rep)
        deltas = [by_rep[x] for x in keys]
        w = pair_weights(model, abl, keys)
        m_, lo, hi = bootstrap_ci(deltas, alpha=alpha, weights=w)
        m95, lo95, hi95 = bootstrap_ci(deltas, weights=w)
        supported = lo > 0 if h == "H1" else lo >= 0
        # A claim that clears 95% but not the corrected level is the case this
        # block exists for, so it is named rather than merely not-supported.
        marginal = (lo95 > 0 or hi95 < 0) and not (lo > 0 or hi < 0)
        verdict = ("supported" if supported else
                   "NOT supported (clears 95% but not the corrected level)"
                   if marginal else "not detected at this n")
        print(f"{h:<4} {model:<30} full−{abl:<9} {m_:+7.3f}  "
              f"[{lo:+.3f}, {hi:+.3f}]      {verdict}")
        out[f"{h}|{model}|{abl}"] = {
            "hypothesis": h, "claim": why, "model": model, "ablation": abl,
            "n": len(deltas), "delta": m_, "lo": lo, "hi": hi,
            "level": level, "sidak_alpha": alpha, "n_tests": k,
            "ci95": [lo95, hi95], "supported": bool(supported),
            "clears_95_only": bool(marginal),
        }
    report["confirmatory"] = out
    print("  H3 (every other ablation) is exploratory: read it at 95% above, "
          "with the family-wise number in the reliability block.")


def _finish(report, degenerate, cell_denoms, coverage_trials,
            coverage_skip_reason, unserved_cells=None):
    """Sections every report carries, however far the interaction got.

    Split out so the "not enough models to rank arms" exit cannot skip
    the degeneracy, coverage and multiplicity blocks: a report missing
    them reads as a report whose intervals were fine.
    """
    unserved_cells = unserved_cells or {}
    report["unserved_cells"] = dict(unserved_cells)
    if unserved_cells:
        total = sum(unserved_cells.values())
        print("\n## REFUSING TO CERTIFY: episodes the provider never served\n")
        print(f"  {total} episode(s) across {len(unserved_cells)} cell(s) ended "
              f"in an account refusal (HTTP 401/402/403). They are NOT excluded "
              f"from any denominator -- the registered exclusion rule is "
              f"timeouts only -- so every rate and delta above counts them as "
              f"model failures.")
        for k, n in sorted(unserved_cells.items()):
            print(f"      {k}: {n}")
        print("  These are not measurements. Re-run the affected cells (they "
              "are first-turn failures, so no --force is needed) before "
              "reporting anything here.")
    report["degenerate_intervals"] = degenerate
    if degenerate:
        print("\n## WARNING: intervals that carry no width, and why\n")
        for k, why in sorted(degenerate.items()):
            print(f"  {k}\n      {why}")

    paired_rows = [(k, v) for k, v in report["interaction_paired"].items()
                   if not k.startswith("_")]
    if paired_rows:
        print("\n## Interaction, seed-paired  (the scheme the protocol "
              "describes)\n")
        print(f"{'ablation':<16} {'n':>2} {'Δw−Δs':>8}  95% CI"
              f"              {'r':>6}  width vs unpaired")
        print("-" * 78)
        for abl, v in paired_rows:
            ratio = v["width_ratio_unpaired_over_paired"]
            ratio_s = f"{ratio:5.2f}x" if ratio == ratio else "  n/a"
            r_s = (f"{v['pair_correlation']:+6.3f}"
                   if v["pair_correlation"] == v["pair_correlation"] else "   n/a")
            print(f"{abl:<16} {v['n']:>2} "
                  f"{v['delta_weak_minus_strong']:+8.3f}  "
                  f"[{v['lo']:+.3f}, {v['hi']:+.3f}]  {r_s}  {ratio_s}")
        print("\nThe paper cites the UNPAIRED block. These two intervals come "
              "from\ndifferent procedures; publishing one alongside a power "
              "analysis computed\nunder the other is the contradiction this "
              "section exists to expose.")

    # The audited shape is read off this grid rather than written down, so it
    # cannot drift away from the data the way a literal in a caption does.
    # p is the weaker arm's full-harness mean when the arms were ranked -- the
    # cell the paper's power discussion is about -- and the median cell mean
    # otherwise. Coverage depends on p, so the audited value and the range it
    # sits in are both reported.
    cell_means = sorted(c["mean"] for c in report["cells"].values()
                        if c["mean"] == c["mean"])
    arms = report.get("interaction_paired", {}).get("_arms") or {}
    p = arms.get("weaker_full_mean")
    p_source = "weaker arm's full-harness mean"
    if p is None or p != p:
        p, p_source = _median(cell_means), "median cell mean"
    shape = {"n_repeats": _modal([c["n_repeats"] for c in
                                  report["cells"].values()], 5) or 5,
             "n_tasks": _modal([n for d in cell_denoms.values()
                                for n in d.values()], 8) or 8,
             "p": p, "p_source": p_source,
             "cell_mean_range": [cell_means[0], cell_means[-1]]
                                if cell_means else []}
    skip = coverage_skip_reason
    if skip is None and not (math.isfinite(shape["p"]) and 0.0 < shape["p"] < 1.0):
        # No measurable cell means: there is no truth to check coverage
        # against. Say that, rather than auditing against a meaningless p and
        # reporting the resulting number as a coverage measurement.
        skip = (f"no usable cell mean to audit against (p={shape['p']!r} from "
                f"the {shape['p_source']})")
    coverage, multiplicity = interval_reliability(
        report, shape, trials=coverage_trials, skip_reason=skip)
    report["coverage"] = coverage
    report["multiplicity"] = multiplicity

    print("\n## Interval reliability\n")
    if coverage.get("skipped"):
        print(f"  coverage audit SKIPPED: {coverage['reason']}")
        print("  the reported intervals are 95% by construction, unmeasured")
    else:
        rng_ = coverage.get("cell_mean_range") or [float("nan")] * 2
        print(f"  cell shape audited: {coverage['n_repeats']} repeats x "
              f"{coverage['n_tasks']} Bernoulli tasks, p={coverage['p']:.3f} "
              f"({coverage['p_source']}; cells span "
              f"{rng_[0]:.3f}-{rng_[-1]:.3f})")
        print(f"  nominal coverage   {100 * coverage['nominal']:5.1f}%")
        print(f"  measured coverage  {100 * coverage['measured']:5.1f}%  "
              f"(+/- {100 * coverage['mc_stderr']:.1f} over "
              f"{coverage['trials']} Monte-Carlo trials)")
        print(f"  zero-width intervals in {100 * coverage['zero_width_fraction']:.1f}% "
              f"of trials")
    n_int = multiplicity["n_intervals"]
    print(f"  {n_int} ladder intervals, {multiplicity['n_excluding_zero']} "
          f"excluding zero")
    print(f"  P(>=1 excludes zero | global null) = "
          f"{100 * multiplicity['fwer_at_nominal_coverage']:.0f}% at nominal "
          f"coverage", end="")
    if "fwer_at_measured_coverage" in multiplicity:
        print(f", {100 * multiplicity['fwer_at_measured_coverage']:.0f}% at "
              f"measured coverage")
    else:
        print()
    print(f"  no correction is applied; Sidak alpha for a 5% family-wise rate "
          f"would be {multiplicity['sidak_alpha_for_fwer']:.4f}")

    print()
    print("A CI that includes 0 is not a failed experiment; n=5 with ~15 tasks")
    print("is a small sample. 'We could not detect it at this n' is the claim.")
    return report


def stats_report(runs, allow_drift=False, coverage_trials=5000,
                 coverage_skip_reason=None):
    cells, prov = grouped(runs, allow_drift=allow_drift)
    if not cells:
        return {}
    # `cells`, `deltas`, `interaction`, `per_task` and `stops` are the sections
    # paper/stats-hard.json froze; their contents and key order are a
    # reproduction contract. New findings go in new sections.
    report = {"cells": {}, "deltas": {}, "interaction": {}, "per_task": {},
              "stops": {}, "provenance": prov,
              "interaction_paired": {}, "degenerate_intervals": {}}

    pooled = [k for k, v in prov.items() if v["pooled"]]
    if pooled:
        print(f"\n## Pooled cells ({len(pooled)})\n")
        for k in pooled:
            v = prov[k]
            drift = (f"   PROTOCOL DRIFT WAIVED on {', '.join(v['protocol_drift'])}"
                     if v["protocol_drift_allowed"] else "")
            print(f"  {k}\n      dirs {', '.join(v['dirs'])}\n"
                  f"      reps {v['reps']}{drift}")
    drift = contrast_drift({(m, c): prov[f"{m}|{c}"]["protocol"]
                            for (m, c) in cells})
    if drift:
        print("\n## WARNING: the two arms of a paired contrast ran under "
              "different protocols\n")
        for label, keys in drift:
            print(f"  {label}: {', '.join(keys)}")
        report["contrast_drift"] = [{"contrast": l, "keys": k} for l, k in drift]
    unseeded = unseeded_cells({k: v["rows"] for k, v in cells.items()})
    if unseeded:
        print("\n## WARNING: seed pairing is unverifiable for some cells\n")
        for u in unseeded:
            print(f"  {u}")
        report["unseeded"] = unseeded

    ragged = {k: v["ragged_reps"] for k, v in prov.items() if v["ragged_reps"]}
    if ragged:
        print("\n## WARNING: uneven repeats (rates use different denominators)\n")
        for k, reps in ragged.items():
            print(f"  {k}  reps {reps} of {prov[k]['rep_denominators']}")

    print("\n## Pass rates with 95% bootstrap CI (per-repeat)\n")
    print(f"{'model':<42} {'config':<16} {'n':>3}  mean [95% CI]")
    print("-" * 88)
    degenerate = {}
    unserved_cells = {}
    cell_denoms = {}
    rates_by_cell = {}
    for (model, cfg), cell in cells.items():
        counts = pass_counts_by_repeat(cell["rows"])
        rates = rates_of(counts)
        dens = denominators_of(counts)
        cell_denoms[f"{model}|{cfg}"] = dens
        rates_by_cell[(model, cfg)] = rates
        xs = list(rates.values())
        # Weighting by each repeat's scored-episode count is what stops a
        # one-task repeat from counting as much as an eight-task one. With
        # equal denominators the weighted estimator is the unweighted one.
        ci = bootstrap_ci(xs, weights=[dens[r] for r in rates])
        n_starved = sum(1 for r in cell["rows"] if is_starved_episode(r))
        # An episode the provider refused (401/402/403) is not a measurement,
        # and unlike a starved one it is NOT excluded from the denominator --
        # the registered exclusion rule is timeouts only. So it is scored as a
        # failure, and a cell full of them reports 0.0% exactly as an ablation
        # that destroys the model would. That happened: 160 refusals published
        # 0.0% and nothing in this file could see it. Counted here so the rate
        # is never printed without the fact beside it.
        n_unserved = sum(1 for r in cell["rows"] if is_unserved_episode(r))
        usable = usable_rows(cell["rows"])
        report["cells"][f"{model}|{cfg}"] = {
            "role": role_of(model),
            "n_repeats": len(xs),
            "n": len(cell["rows"]),
            "n_usable": len(usable),
            "n_starved": n_starved,
            "n_unserved": n_unserved,
            "rates": rates,
            "mean": ci[0], "lo": ci[1], "hi": ci[2],
        }
        if n_unserved:
            unserved_cells[f"{model}|{cfg}"] = n_unserved
        why = ci_degeneracy(xs)
        if why:
            degenerate[f"cells:{model}|{cfg}"] = why
        warn = f"  STARVED={n_starved} excluded" if n_starved else ""
        if n_unserved:
            warn += f"  UNSERVED={n_unserved} SCORED AS FAILURES"
        if why:
            warn += "  DEGENERATE INTERVAL"
        print(f"{model:<42} {cfg:<16} {len(xs):>3}  {_fmt_ci(ci)}{warn}")

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
        # How often the model closed the episode through the `finish` tool
        # rather than trailing off. This is not decoration: the verify gate can
        # only fire on a `finish`, so a cell where the model finishes half the
        # time is one where the gate was exercised half the time, and the
        # measured value of `verifygate` on that cell is a floor rather than an
        # estimate. Measured on gpt-oss-120b: baseline finished 145/160 while
        # the full harness finished 72/160, so the number moves with the very
        # flags being ablated and cannot be assumed constant across a ladder.
        n_rows = sum(stops.values())
        n_fin = stops.get("finished", 0)
        report["cells"][key]["finished"] = n_fin
        report["cells"][key]["finish_rate"] = (n_fin / n_rows) if n_rows else None

    print("\n## Ablation deltas  Δ = full − ablation  (paired by repeat)\n")
    print(f"{'model':<42} {'ablation':<16} {'n':>3}  Δ mean [95% CI]")
    print("-" * 88)
    by_model = {}
    for (model, cfg), rates in rates_by_cell.items():
        by_model.setdefault(model, {})[cfg] = rates

    def pair_weights(model, abl, reps):
        """Episodes behind each paired repeat: the thinner arm bounds the pair.

        `contrast_conflicts` already refuses arms that scored different tasks,
        so these agree on the frozen grid; taking the minimum keeps a pair no
        stronger than its weaker half if they ever do not.
        """
        f = cell_denoms.get(f"{model}|full", {})
        a = cell_denoms.get(f"{model}|{abl}", {})
        return [min(f.get(r, 0), a.get(r, 0)) for r in reps]

    delta_by_rep = {}
    for model, cfgs in by_model.items():
        full = cfgs.get("full")
        if not full:
            continue
        for abl in ABLATIONS:
            other = cfgs.get(abl)
            if not other:
                continue
            by_rep = deltas_by_repeat(full, other)
            delta_by_rep[(model, abl)] = by_rep
            keys = sorted(by_rep)
            deltas = [by_rep[k] for k in keys]
            ci = bootstrap_ci(deltas, weights=pair_weights(model, abl, keys))
            report["deltas"][f"{model}|{abl}"] = {
                "role": role_of(model),
                "n": len(deltas), "reps": keys,
                "mean": ci[0], "lo": ci[1], "hi": ci[2],
            }
            why = ci_degeneracy(deltas)
            if why:
                degenerate[f"deltas:{model}|{abl}"] = why
            flag = "  DEGENERATE INTERVAL" if why else ""
            print(f"{model:<42} {abl:<16} {len(deltas):>3}  "
                  f"{_fmt_ci(ci, as_pct=False)}{flag}")

    _confirmatory_block(report, delta_by_rep, pair_weights, cell_denoms)

    print("\n## Interaction  Δ_weaker > Δ_stronger  (empirical full-harness means)\n")
    print(f"{'ablation':<16} {'Δw−Δs':>8}  95% CI                verdict")
    print("-" * 70)
    full_means = {}
    for model, cfgs in by_model.items():
        if "full" in cfgs and "baseline" in cfgs:
            full_means[model] = mean(cfgs["full"].values())
    detail = capability_arms_detail(full_means)
    if not detail:
        print("need ≥2 models with both full and baseline in this tag")
        print("arms are ranked by full-harness mean, not parameter count")
        report["interaction"]["_note"] = "need two models with full+baseline"
        report["interaction_paired"]["_note"] = (
            "need two models with full+baseline")
        return _finish(report, degenerate, cell_denoms, coverage_trials,
                       coverage_skip_reason, unserved_cells)
    weak, strong = detail["weaker"], detail["stronger"]
    # Two arms may legitimately run under different protocols -- that is the
    # normal case once one of them is API-served. The interaction stays valid
    # (each Δ is seed-paired within one arm) but the difference has to be
    # declared, and the pass-rate table above must not be read across arms.
    # Every arm in the pool, not just the two the interaction happens to pick.
    # Comparing only (weak, strong) missed a divergent arm ranked between them.
    protos = {m: prov.get(f"{m}|full", {}).get("protocol")
              for m in by_model if prov.get(f"{m}|full", {}).get("protocol")}
    pairs = arms_drift(protos)
    arm_keys = sorted({k for _, _, ks in pairs for k in ks})
    report["arm_protocol_drift"] = arm_keys
    report["arm_protocol_drift_pairs"] = [
        {"a": a, "b": b, "keys": ks} for a, b, ks in pairs]
    if pairs:
        print("\n## NOTE: arms in this pool ran under different protocols\n")
        for a, b, ks in pairs:
            print(f"  {a}\n  {b}\n  differ on: {', '.join(ks)}\n")
        print("  Each Δ below is seed-paired inside ONE arm under ONE "
              "protocol, so the interaction remains valid across these "
              "differences. The pass rates above are NOT comparable between "
              "the arms named here and must not be quoted as a capability "
              "comparison.\n")
    # The frozen `_arms` block carries four keys and nothing else; the gap,
    # the tie flag and the models dropped for a non-finite mean go in the
    # paired section, which is not under a reproduction contract.
    report["interaction"]["_arms"] = {
        "weaker": weak, "stronger": strong,
        "weaker_full_mean": full_means[weak],
        "stronger_full_mean": full_means[strong],
    }
    report["interaction_paired"]["_arms"] = detail
    report["interaction_paired"]["_scheme"] = (
        "One bootstrap index vector per resample, applied to both arms: "
        "repeat r pins seed r in both, so the arms are paired by seed. The "
        "`interaction` section above uses independent index streams per arm, "
        "which is what the published intervals were computed with and what "
        "this file therefore keeps reproducing. Where the arms correlate, the "
        "unpaired interval is the wider one and the paired one is correct.")
    print(f"weaker={weak}  (full mean {full_means[weak]:.3f})")
    print(f"stronger={strong}  (full mean {full_means[strong]:.3f})")
    if detail["tied"]:
        print("\n## WARNING: the two arms have identical measured capability "
              f"({detail['capability_gap']:+.3f} gap); an interaction across "
              "no capability difference tests nothing\n")
    for note in detail["ignored"]:
        print(f"  arm dropped, not ranked: {note}")

    for abl in ABLATIONS:
        dw = report["deltas"].get(f"{weak}|{abl}")
        ds = report["deltas"].get(f"{strong}|{abl}")
        if not (dw and ds):
            continue
        w_by_rep = delta_by_rep[(weak, abl)]
        s_by_rep = delta_by_rep[(strong, abl)]
        # Unpaired, on each arm's own repeats: the published statistic.
        inter = interaction(list(w_by_rep.values()), list(s_by_rep.values()))
        report["interaction"][abl] = inter
        # Paired, on the repeats both arms hold. Keeping the keys is the whole
        # point: a contrast that paired 2 of 5 repeats and one that paired all
        # 5 are otherwise indistinguishable in the output.
        w_d, s_d, reps, dropped = aligned_interaction(w_by_rep, s_by_rep)
        if reps:
            ws = [min(a, b) for a, b in zip(pair_weights(weak, abl, reps),
                                            pair_weights(strong, abl, reps))]
            paired = interaction(w_d, s_d, paired=True, weights=ws)
        else:
            paired = interaction([], [])
        paired.update({
            "n": len(reps), "reps": reps, "dropped_reps": dropped,
            "weak_reps": sorted(w_by_rep), "strong_reps": sorted(s_by_rep),
            "pair_correlation": pearson_r(w_d, s_d),
            "paired_width": paired["hi"] - paired["lo"],
            "unpaired_width": inter["hi"] - inter["lo"],
        })
        pw, uw = paired["paired_width"], paired["unpaired_width"]
        paired["width_ratio_unpaired_over_paired"] = (
            uw / pw if pw and pw == pw else float("nan"))
        report["interaction_paired"][abl] = paired
        if dropped:
            print(f"  WARNING {abl}: paired on {len(reps)} repeat(s) "
                  f"{reps}; repeat(s) {dropped} are in one arm only")
        if inter["detectable"]:
            verdict = "detectable (CI > 0)"
        elif inter["hi"] < 0:
            verdict = "reversed (CI < 0)"
        else:
            verdict = "underpowered / not detected at this n"
        print(f"{abl:<16} {inter['delta_weak_minus_strong']:+8.3f}  "
              f"[{inter['lo']:+.3f}, {inter['hi']:+.3f}]  {verdict}")

    return _finish(report, degenerate, cell_denoms, coverage_trials,
                   coverage_skip_reason, unserved_cells)


def _refuse(err):
    """Print a cell-assembly refusal and exit 2. Never returns."""
    print("\n## REFUSED: these directories cannot be assembled into cells\n",
          file=sys.stderr)
    shown = err.problems[:20]
    for prob in shown:
        print(f"  - {prob}", file=sys.stderr)
    if len(err.problems) > len(shown):
        print(f"  ... and {len(err.problems) - len(shown)} more "
              f"({len(err.problems)} conflicts total)", file=sys.stderr)
    print("\nNarrow the selection with --tag, or pool only tags that agree.\n"
          "--allow-drift overrides a protocol mismatch and stamps the report.",
          file=sys.stderr)
    raise SystemExit(2)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("filter", nargs="?", default=None,
                    help="substring of the results directory name")
    ap.add_argument("--tag", default="",
                    help="only directories ending in __TAG. Comma-separate to "
                         "pool several tags into one cell (e.g. hard,hard-ext); "
                         "the merge is refused unless the tags agree on config "
                         "and protocol and hold disjoint repeats.")
    ap.add_argument("--allow-drift", action="store_true",
                    help="pool tags whose protocol blocks disagree. Recorded in "
                         "the report as protocol_drift_allowed -- a drifted pool "
                         "must never read as a clean one.")
    ap.add_argument("--json-out", default="",
                    help="write the stats report as JSON. Writes that file and "
                         "nothing else -- figures are a separate, explicit "
                         "request (--figures).")
    ap.add_argument("--figures", default="", metavar="DIR",
                    help="ALSO render the SVG figures into DIR. Off by "
                         "default: this used to run on every --json-out and "
                         "overwrite the committed paper/figures from whatever "
                         "filtered or exploratory selection happened to be in "
                         "hand. DIR is overwritten in place, and a failure "
                         "here is an error, not a printed shrug.")
    ap.add_argument("--coverage-trials", type=int, default=5000, metavar="N",
                    help="Monte-Carlo trials for the interval-coverage audit "
                         "(default 5000). Raise for a tighter measurement.")
    ap.add_argument("--no-coverage-audit", action="store_true",
                    help="skip measuring what the 95%% intervals actually "
                         "cover. The report still carries the coverage block, "
                         "marked skipped -- an unmeasured interval must not "
                         "read like a measured one.")
    ap.add_argument("--exclude", action="append", default=[], metavar="SUBSTR",
                    help="skip result directories containing SUBSTR; repeatable. "
                         "An arm excluded from a reported grid must be declared in "
                         "the writeup -- silently dropping one is how a capped "
                         "sample gets presented as complete coverage.")
    args = ap.parse_args()
    tags = tuple(t.strip() for t in args.tag.split(",") if t.strip())
    try:
        runs = load(args.filter, tags, tuple(args.exclude))
    except CellMergeError as e:
        _refuse(e)
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
            v = r.get(key)
            line += f"{(fmt.format(v) if isinstance(v, (int, float)) else 'n/a'):<{w}}"
        print(line)
    print()
    for r in runs:
        stops = {}
        for row in r["rows"]:
            stops[row.get("stop_reason", "?")] = stops.get(row.get("stop_reason", "?"), 0) + 1
        n_rows = sum(stops.values())
        fin = stops.get("finished", 0)
        # The gate can only fire on a `finish`, so this rate bounds how much of
        # `verifygate` was ever exercised in this cell.
        rate = f"  finish_rate={100.0 * fin / n_rows:.0f}%" if n_rows else ""
        print(f"{short(r):<42} {r.get('passed', '?')}/{r.get('n', '?')}  "
              f"stop_reasons={stops}{rate}")

    if args.coverage_trials <= 0:
        print("--coverage-trials must be positive", file=sys.stderr)
        raise SystemExit(2)
    try:
        report = stats_report(
            runs, allow_drift=args.allow_drift,
            coverage_trials=args.coverage_trials,
            coverage_skip_reason=("--no-coverage-audit was passed"
                                  if args.no_coverage_audit else None))
    except CellMergeError as e:
        _refuse(e)
    if args.json_out:
        write_report(report, args.json_out)
        print(f"\nwrote {args.json_out}")
    if args.figures:
        # Explicit, loud, and not wrapped in a try: a figure that failed to
        # render must not leave the previous one in place looking current.
        from figures import from_report
        outdir = os.path.abspath(args.figures)
        if outdir == os.path.abspath(os.path.join(HERE, "paper", "figures")):
            print(f"\n## overwriting the COMMITTED paper figures in {outdir}\n")
        else:
            print(f"\n## rendering figures into {outdir}\n")
        from_report(report, outdir,
                    source=os.path.basename(args.json_out) or None)


if __name__ == "__main__":
    main()
