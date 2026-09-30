"""Preregistration §7 sensitivity analysis, derived from the committed stats JSONs.

§7 requires, for every cell, the primary rate (serving errors scored as
failures) AND a sensitivity rate with non-timeout serving errors removed,
beside the per-cell infrastructure-failure rate. `compare.py` now produces that
section from episode logs (`report["sensitivity"]`), but the episode logs of
the reported grids (results/*__v2, *__ext-cerebras) are not in this checkout.
This script derives what the committed reports alone determine, and says
exactly what they do not.

What a committed report holds per cell: the pass count P (per_task), the row
count n, the count E of `error:*` stop reasons (stops), and the per-repeat pass
rates and denominators. What it does not hold:

  * which repeat each errored row sits in,
  * whether an errored row's error text named a timeout (§6/§7 keep those), and
  * whether an errored row PASSED -- the final verifier runs after an error
    (mh/harness.py), so it can, and in the local v1-hard logs some did.

Hence, per cell:

  E == 0   the sensitivity analysis IS the primary one: rate and CI are exact.
  E == 1   every consistent allocation (which repeat, passed or failed, or kept
           as a timeout) is enumerated and bootstrapped with the primary
           estimator, so the reported CI is the exact RANGE of possible CIs.
  E >= 2   exact integer bounds on the point estimate only; no CI.

Deltas and confirmatory tests follow the same rule: exact or enumerated where
both arms allow it, otherwise bounds on a pooled-rate proxy (NOT the
registered paired-by-repeat estimator) with the CI marked not computable.

Usage: python3 bench/paper/sensitivity.py [--out bench/paper/stats-sensitivity.json]
Deterministic: the same committed JSONs and the same local results/ tree give
the same bytes. Only `local_evidence` reads results/, so it alone varies by
machine; `grids` depends on the committed JSONs only.
"""
import argparse
import glob
import hashlib
import itertools
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
BENCH = os.path.dirname(HERE)
sys.path.insert(0, BENCH)
from mh.stats import bootstrap_ci, is_infra_failure  # noqa: E402
from compare import ABLATIONS, _decide  # noqa: E402

SOURCES = ("stats-v2.json", "stats-ext-cerebras.json")
# Allocations bootstrapped per contrast before giving up on an exact range.
ENUMERATION_CAP = 400


class Underivable(RuntimeError):
    """The committed report cannot support this computation as stated."""


def _sha256(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def cell_inputs(report, key):
    """(P, n, E, rates, dens) for one cell, cross-checked, or Underivable."""
    c = report["cells"][key]
    if c.get("n_starved"):
        raise Underivable(f"{key}: {c['n_starved']} starved rows; the row "
                          f"counts below would not be the scored ones")
    dens = {int(r): int(d) for r, d in
            report["provenance"][key]["rep_denominators"].items()}
    rates = {int(r): float(x) for r, x in c["rates"].items()}
    if set(dens) != set(rates):
        raise Underivable(f"{key}: rate and denominator repeats disagree")
    passes = {r: round(rates[r] * dens[r]) for r in rates}
    for r in rates:
        if abs(passes[r] - rates[r] * dens[r]) > 1e-9:
            raise Underivable(f"{key}: rep {r} rate {rates[r]} is not k/{dens[r]}")
    P = sum(v["passed"] for v in report["per_task"][key].values())
    n = c["n_usable"]
    if P != sum(passes.values()) or n != sum(dens.values()) or n != c["n"]:
        raise Underivable(f"{key}: per_task, rates and provenance disagree "
                          f"(P={P}, n={n})")
    E = sum(k for s, k in report["stops"][key].items() if s.startswith("error:"))
    return P, n, E, passes, dens


def rate_bounds(P, m, e_max):
    """Exact (min, max) of the sensitivity rate over every consistent case.

    j of the at most `e_max` errored rows are removed (the rest named a
    timeout and stay), q of the removed ones had passed. q <= min(j, P) and
    j - q <= m - P. Rate = (P - q) / (m - j).
    """
    lo, hi = None, None
    for j in range(0, e_max + 1):
        if m - j <= 0:
            continue
        q_min = max(0, j - (m - P))
        q_max = min(j, P)
        if q_min > q_max:
            continue
        a, b = (P - q_max) / (m - j), (P - q_min) / (m - j)
        lo = a if lo is None else min(lo, a)
        hi = b if hi is None else max(hi, b)
    return lo, hi


def allocations(passes, dens, E):
    """Every per-repeat (passes, dens) the cell could have after removal.

    None when E is too large to enumerate. E == 0 is the one primary sample.
    For E == 1 the errored row is in some repeat r and was either a failure
    (k/(d-1)), a pass ((k-1)/(d-1)), or named a timeout and stays (primary).
    """
    if E == 0:
        return [("primary", passes, dens)]
    if E != 1:
        return None
    out = [("kept (timeout-text error)", passes, dens)]
    for r in sorted(passes):
        k, d = passes[r], dens[r]
        if d < 2:
            continue
        if k < d:
            out.append((f"rep {r}: failed row removed",
                        passes, {**dens, r: d - 1}))
        if k >= 1:
            out.append((f"rep {r}: passed row removed",
                        {**passes, r: k - 1}, {**dens, r: d - 1}))
    return out


def _ci(passes, dens, alpha=0.05):
    reps = sorted(dens)
    xs = [passes[r] / dens[r] for r in reps]
    return bootstrap_ci(xs, alpha=alpha, weights=[dens[r] for r in reps])


def _paired(f, a, alpha):
    """Primary paired estimator on one allocation of each arm."""
    fp, fd = f
    ap, ad = a
    keys = sorted(set(fd) & set(ad))
    deltas = [fp[r] / fd[r] - ap[r] / ad[r] for r in keys]
    w = [min(fd[r], ad[r]) for r in keys]
    m, lo, hi = bootstrap_ci(deltas, alpha=alpha, weights=w)
    return m, lo, hi, deltas


def cell_block(report, key):
    P, n, E, passes, dens = cell_inputs(report, key)
    c = report["cells"][key]
    out = {"n": n, "passes": P, "n_error_stops": E,
           "infra_failure_rate_upper_bound": E / n,
           "primary": {"mean": c["mean"], "lo": c["lo"], "hi": c["hi"]}}
    # Same check as delta_block: the primary sample must reproduce the
    # published interval exactly before any variant of it is reported.
    if tuple(_ci(passes, dens)) != (c["mean"], c["lo"], c["hi"]):
        raise Underivable(f"{key}: re-derived primary {_ci(passes, dens)} != "
                          f"published {(c['mean'], c['lo'], c['hi'])}")
    if E == 0:
        out.update(status="exact",
                   sensitivity={"mean": c["mean"], "lo": c["lo"], "hi": c["hi"]},
                   infra_failure_rate=0.0,
                   why="no error:* row in this cell: nothing to remove")
        return out
    lo, hi = rate_bounds(P, n, E)
    out["sensitivity_point_bounds"] = [lo, hi]
    out["sensitivity_if_every_error_is_a_failed_serving_error"] = P / (n - E)
    allocs = allocations(passes, dens, E)
    if allocs is not None:
        cis = [_ci(p, d) for _label, p, d in allocs]
        out.update(status="enumerated", n_allocations=len(allocs),
                   sensitivity_ci_range={
                       "mean": [min(x[0] for x in cis), max(x[0] for x in cis)],
                       "lo": [min(x[1] for x in cis), max(x[1] for x in cis)],
                       "hi": [min(x[2] for x in cis), max(x[2] for x in cis)]})
    else:
        out.update(status="bounds_only",
                   why=(f"{E} error rows in unknown repeats, of unknown kind "
                        f"(timeout-text or not) and unknown verdict: the point "
                        f"estimate is bounded exactly, the interval is not "
                        f"computable without per-repeat error counts"))
    return out


def _paired_subset(passes, dens, keys, E):
    """(P, m, e_max) restricted to the paired repeats `keys`."""
    P = sum(passes[r] for r in keys)
    m = sum(dens[r] for r in keys)
    outside = sum(dens.values()) - m
    return P, m, min(E, m), max(0, E - outside)


def delta_block(report, model, abl, alpha, level):
    fk, ak = f"{model}|full", f"{model}|{abl}"
    Pf, nf, Ef, fpass, fden = cell_inputs(report, fk)
    Pa, na, Ea, apass, aden = cell_inputs(report, ak)
    keys = sorted(set(fden) & set(aden))
    prim = report["deltas"][f"{model}|{abl}"]
    out = {"n_reps": len(keys), "n_error_stops": {"full": Ef, abl: Ea},
           "primary": {"mean": prim["mean"], "lo": prim["lo"], "hi": prim["hi"]}}
    fa = allocations(fpass, fden, Ef)
    aa = allocations(apass, aden, Ea)
    if fa is not None and aa is not None and len(fa) * len(aa) <= ENUMERATION_CAP:
        # Allocations outside the paired repeats collapse onto one pairing.
        seen, res = set(), []
        for (_l1, fp, fd), (_l2, ap, ad) in itertools.product(fa, aa):
            sig = tuple((fp[r], fd[r], ap[r], ad[r]) for r in keys)
            if sig in seen:
                continue
            seen.add(sig)
            m95, lo95, hi95, deltas = _paired((fp, fd), (ap, ad), 0.05)
            _m, lo, hi, _ = _paired((fp, fd), (ap, ad), alpha)
            res.append((m95, lo95, hi95, lo, hi, _decide(deltas, lo, hi)[0]))
        # The first pairing is primary x primary. Re-deriving it must return
        # the published interval to the bit, or this script's estimator is not
        # the report's and nothing below it can be trusted.
        if (res[0][0], res[0][1], res[0][2]) != (prim["mean"], prim["lo"], prim["hi"]):
            raise Underivable(f"{model}|{abl}: re-derived primary "
                              f"{res[0][:3]} != published "
                              f"{(prim['mean'], prim['lo'], prim['hi'])}")
        out.update(
            status="exact" if len(res) == 1 else "enumerated",
            n_allocations=len(res),
            sensitivity={"mean": [min(r[0] for r in res), max(r[0] for r in res)],
                         "lo95": [min(r[1] for r in res), max(r[1] for r in res)],
                         "hi95": [min(r[2] for r in res), max(r[2] for r in res)],
                         "lo_corrected": [min(r[3] for r in res), max(r[3] for r in res)],
                         "hi_corrected": [min(r[4] for r in res), max(r[4] for r in res)],
                         "corrected_level": level},
            supported_under_every_allocation=all(r[5] for r in res),
            supported_under_some_allocation=any(r[5] for r in res))
        return out
    fP, fm, fe_max, fe_min = _paired_subset(fpass, fden, keys, Ef)
    aP, am, ae_max, ae_min = _paired_subset(apass, aden, keys, Ea)
    flo, fhi = rate_bounds(fP, fm, fe_max)
    alo, ahi = rate_bounds(aP, am, ae_max)
    out.update(
        status="bounds_only",
        estimator=("pooled-rate difference over the paired repeats, a proxy: "
                   "the registered estimator is the weighted mean of "
                   "per-repeat deltas, which needs the per-repeat error "
                   "counts the report does not hold"),
        pooled_primary=fP / fm - aP / am,
        sensitivity_point_bounds=[flo - ahi, fhi - alo],
        sign_robust=(flo - ahi > 0) or (fhi - alo < 0),
        ci="not computable from the committed report")
    if fe_min == fe_max and ae_min == ae_max:
        # Defined only when the paired repeats hold every errored row of both
        # arms; otherwise how many of them fall in the pairing is unknown.
        out["sensitivity_if_every_error_is_a_failed_serving_error"] = (
            fP / (fm - fe_max) - aP / (am - ae_max))
    else:
        out["error_rows_in_paired_repeats"] = {"full": [fe_min, fe_max],
                                               abl: [ae_min, ae_max]}
    return out


def grid_block(name):
    path = os.path.join(HERE, name)
    report = json.load(open(path))
    out = {"sha256": _sha256(path), "cells": {}, "deltas": {},
           "confirmatory": {}}
    for key in report["cells"]:
        out["cells"][key] = cell_block(report, key)
    conf = report.get("confirmatory") or {}
    alpha = next(iter(conf.values()))["sidak_alpha"] if conf else 0.05
    level = next(iter(conf.values()))["level"] if conf else 95.0
    for key in report["deltas"]:
        model, abl = key.rsplit("|", 1)
        if abl not in ABLATIONS:
            continue
        out["deltas"][key] = delta_block(report, model, abl, alpha, level)
    for key, v in conf.items():
        d = out["deltas"][f"{v['model']}|{v['ablation']}"]
        entry = {"hypothesis": v["hypothesis"], "model": v["model"],
                 "ablation": v["ablation"], "n_tests": v["n_tests"],
                 "level": v["level"], "primary_delta": v["delta"],
                 "primary_ci": [v["lo"], v["hi"]],
                 "primary_supported": v["supported"], "status": d["status"]}
        if d["status"] in ("exact", "enumerated"):
            every = d["supported_under_every_allocation"]
            some = d["supported_under_some_allocation"]
            entry.update(
                sensitivity_delta_range=d["sensitivity"]["mean"],
                sensitivity_lo_range=d["sensitivity"]["lo_corrected"],
                sensitivity_hi_range=d["sensitivity"]["hi_corrected"],
                sensitivity_supported=(every if every == some else "depends"),
                verdict_changes=("no" if every == some == v["supported"] else
                                 "yes" if every == some else "possibly"))
        else:
            lo, hi = d["sensitivity_point_bounds"]
            entry.update(
                sensitivity_point_bounds=[lo, hi],
                sign_robust=d["sign_robust"],
                sensitivity_supported="not computable (no per-repeat error counts)",
                verdict_changes=("sign cannot flip; the corrected-level "
                                 "interval is not computable" if d["sign_robust"]
                                 else "the sign itself is not determined; the "
                                      "corrected-level interval is not computable"))
        out["confirmatory"][key] = entry
    return out


def local_errored_passes():
    """Errored rows that passed, in whatever episode logs ARE local.

    Not the reported grids: this is evidence about the instrument (an error
    does not imply a failed episode), not about any published number.
    """
    rows_by_dir = {}
    for p in sorted(glob.glob(os.path.join(BENCH, "results", "*", "summary.json"))):
        try:
            s = json.load(open(p))
        except (OSError, ValueError):
            continue
        infra = [r for r in s.get("rows", []) if is_infra_failure(r)]
        if infra:
            rows_by_dir[os.path.basename(os.path.dirname(p))] = {
                "infra_rows": len(infra),
                "infra_rows_passed": sum(1 for r in infra if r.get("passed") is True)}
    tot = sum(v["infra_rows"] for v in rows_by_dir.values())
    pas = sum(v["infra_rows_passed"] for v in rows_by_dir.values())
    return {"what": ("non-timeout error:* rows in the local results/ tree "
                     "(v1 and exploratory tags, not v2 or ext-cerebras) and how "
                     "many of them passed"),
            "infra_rows": tot, "infra_rows_passed": pas, "by_dir": rows_by_dir}


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--out", default=os.path.join(HERE, "stats-sensitivity.json"))
    args = ap.parse_args()
    doc = {
        "_what": ("Preregistration §7 dual report for the reported grids, "
                  "derived from the committed stats JSONs alone (the episode "
                  "logs are not in this checkout). See sensitivity.py for the "
                  "derivation. `primary` values are copied from the source "
                  "report; nothing here replaces them."),
        "_rule": ("Sensitivity sample = primary sample minus non-timeout "
                  "error:* episodes (mh.stats.is_infra_failure)."),
        "_status_legend": {
            "exact": "no error:* row involved: sensitivity == primary, CI included",
            "enumerated": ("one error:* row: every consistent allocation "
                           "bootstrapped; ranges are exact"),
            "bounds_only": ("two or more error:* rows: exact bounds on the "
                            "point estimate; no CI"),
        },
        "_not_derivable": [
            "per-repeat placement of error:* rows (needed for any CI once E >= 2)",
            "whether an error:* row's text names a timeout (such rows stay in "
            "both analyses), so n_error_stops is an UPPER bound on the "
            "infrastructure-failure count",
            "whether an error:* row passed (the final verifier runs after an "
            "error), so 'errored rows are failures' is an assumption, reported "
            "separately as sensitivity_if_every_error_is_a_failed_serving_error",
        ],
        "grids": {name: grid_block(name) for name in SOURCES},
        "local_evidence": local_errored_passes(),
    }
    with open(args.out, "w") as f:
        json.dump(doc, f, indent=2, allow_nan=False)
        f.write("\n")
    print(f"wrote {args.out}")
    for name, g in doc["grids"].items():
        print(f"\n{name}")
        for key, v in g["confirmatory"].items():
            rng = v.get("sensitivity_delta_range") or v.get("sensitivity_point_bounds")
            print(f"  {key:<62} primary Δ {v['primary_delta']:+.3f} "
                  f"supported={v['primary_supported']}  sensitivity Δ in "
                  f"[{rng[0]:+.3f}, {rng[1]:+.3f}]  ({v['status']})  "
                  f"verdict change: {v['verdict_changes']}")


if __name__ == "__main__":
    main()
