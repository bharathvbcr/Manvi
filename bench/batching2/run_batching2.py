#!/usr/bin/env python3
"""Pre-registered five-arm study of Manvi's batching policy (see PREREG.md).

Reuses bench/batching/run_batching.py for everything that is not particular to
this study: the NDJSON parser, the grader, the runner and the paired analysis.
This module holds only the arms, the task prompts, the arm orders, the primary
comparisons, the power arithmetic and the verdict rules the preregistration
fixes in advance.

  run_batching2.py run     --manvi BIN --repo CLEAN [--repo CLEAN2 ...] --out DIR --scratch DIR
                           [--reps 20] [--jobs 4] [--budget-usd 5.75] [--arms ACDSR] [--tasks ID,ID]
  run_batching2.py analyze --out DIR [--json-out FILE]
"""
import argparse
import json
import math
import os
import random
import shutil
import statistics
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "batching"))
import run_batching as rb  # noqa: E402

# Arm ids are one letter so an order is a string. D is the identical-config
# repeat of C (the noise floor); it is "C2" in the preregistration.
ARM_IDS = "ACDSR"
ARM_LABELS = {
    "A": "batch tool off",
    "C": "default: batch and script on, serial lookups refuse",
    "D": "C2: identical repeat of C (noise floor)",
    "S": "default plus a prompt nudge naming devcouncil_script",
    "R": "batch on, serial lookups off",
}
ARMS = {
    "A": {"MANVI_HARNESS_BATCH_TOOL": "off"},
    "C": {"MANVI_HARNESS_BATCH_TOOL": "on"},
    "D": {"MANVI_HARNESS_BATCH_TOOL": "on"},
    "S": {"MANVI_HARNESS_BATCH_TOOL": "on"},
    "R": {"MANVI_HARNESS_BATCH_TOOL": "on", "MANVI_HARNESS_SERIAL_LOOKUPS": "off"},
}
# Primary family: (name, x, y, task stratum, why). x minus y; a negative delta
# favours x. Three comparisons share alpha 0.05 (Bonferroni).
PRIMARY = (
    ("P1", "C", "A", None, "does the default batching policy save input tokens against no batch tool"),
    ("P2", "S", "C", None, "does naming devcouncil_script in the prompt save input tokens against the default"),
    ("P3", "C", "R", "chained", "does the serial-lookup refusal save input tokens, on the tasks where it can act"),
)
FAMILY_ALPHA = 0.05 / len(PRIMARY)
NOISE_FLOOR = ("D", "C")
CORRECTNESS_MARGIN = 0.05  # fraction of facts, as in the first study's rule
# P3 compares a policy with one that never acts, so it is read only if C's
# refusal fired in at least this many chained-task runs.
MIN_REFUSAL_RUNS = 10
CAP_USD = 6.00
SCHEDULE_SEED = 20261121
# Task strata by kind. "chained" is every task whose lookups depend on each
# other's answers, the only tasks on which the serial-lookup rule can act.
STRATA = {"chained": ("dependent", "trail"), "independent": ("independent",), "trail": ("trail",)}


def balanced_orders(arms=ARM_IDS):
    """Cyclic rotations of the arm string and their reversals: over the full set
    every arm holds every position equally often."""
    rots = [arms[i:] + arms[:i] for i in range(len(arms))]
    return rots + [r[::-1] for r in rots]


def schedule(tasks, reps, arms=ARM_IDS, seed=SCHEDULE_SEED):
    """(task, rep, order) per unit, rep-major. Consecutive blocks of 10 units
    each get a seeded shuffle of the 10 balanced orders, so over whole blocks
    every order, and so every arm at every position, occurs equally often
    (120 units is twelve blocks); a trailing partial block takes a prefix of its
    shuffle. The shuffle keeps an order from lining up with a task."""
    orders = balanced_orders(arms)
    rnd = random.Random(seed)
    units = [(t["id"], rep) for rep in range(reps) for t in tasks]
    out = []
    for start in range(0, len(units), len(orders)):
        block = orders[:]
        rnd.shuffle(block)
        out.extend((tid, rep, o) for (tid, rep), o in zip(units[start:start + len(orders)], block))
    return out


def build_prompt(spec, task, arm):
    parts = [spec["preambles"][task["kind"]]]
    if arm == "S":
        parts.append(spec["nudge"])
    parts.append("\n".join("%d. %s" % (i + 1, s) for i, s in enumerate(task["items"])))
    return "\n\n".join(parts)


def load_spec(path=None):
    return rb.load_tasks(path or os.path.join(HERE, "tasks.json"))


# --- power -----------------------------------------------------------------
def pairs_needed(sd, effect, alpha, power=0.8):
    """Pairs for a two-sided paired comparison to detect a mean delta of
    `effect` with probability `power` at level `alpha`: n = ((z_a + z_b) sd / d)^2
    under a normal approximation to the mean of the deltas."""
    nd = statistics.NormalDist()
    return math.ceil(((nd.inv_cdf(1 - alpha / 2) + nd.inv_cdf(power)) * sd / effect) ** 2)


def detectable_effect(sd, n, alpha, power=0.8):
    nd = statistics.NormalDist()
    return (nd.inv_cdf(1 - alpha / 2) + nd.inv_cdf(power)) * sd / math.sqrt(n)


def planning_sds(stats):
    """Standard deviation of the paired input-token deltas of the first study,
    recovered from its bootstrap CI: sd = sqrt(n) * (hi - lo) / (2 * 1.96)."""
    out = {}
    for key, d in stats["deltas"].items():
        m = d["input_tokens"]
        out[key] = math.sqrt(m["n"]) * (m["ci95"][1] - m["ci95"][0]) / (2 * 1.96)
    return out


# --- analysis --------------------------------------------------------------
def compare(rows, x, y, stratum=None, tasks_kind=None, alpha=FAMILY_ALPHA):
    rows = [r for r in rows if r["arm"] in (x, y)]
    if stratum:
        keep = {t["id"] for t in tasks_kind if t["kind"] in STRATA[stratum]}
        rows = [r for r in rows if r["task"] in keep]
    return rb.analyze(rows, arms=x + y, comparisons=[(x, y)], alpha=alpha)


def verdict(delta, correct_delta, gate_ok, exercised=True):
    """The preregistered reading of one primary comparison's input-token delta.
    `correct_delta` is the mean paired correct_frac difference (x minus y).
    `exercised` is false when the mechanism under test never ran enough."""
    if not gate_ok:
        return "not interpretable: the noise-floor check failed"
    if not exercised:
        return "not exercised: the refusal fired in fewer than %d runs of C, so there is nothing to compare" % MIN_REFUSAL_RUNS
    if correct_delta < -CORRECTNESS_MARGIN:
        return "refuted: correctness fell by more than 5 points"
    lo, hi = delta["ci95"]
    if hi < 0:
        return "supported: the CI lies wholly below zero"
    if lo > 0:
        return "harm shown: the CI lies wholly above zero"
    return "not shown: the CI contains zero"


def arm_checks(rows):
    """Does each arm's treatment show in its runs? A is withheld both container
    tools; R's refusal is off, so it can neither note nor refuse. A violation
    means the arm did not receive the treatment it is named for."""
    checks = {"A": ("a batch or script call", lambda r: r["batch_calls"] > 0 or r.get("script_calls", 0) > 0),
              "R": ("a serial-lookup note or refusal", lambda r: r.get("serial_notes", 0) > 0 or r["refusals"] > 0)}
    return {arm: {"violations": sum(1 for r in rows if r["arm"] == arm and bad(r)), "looked_for": what}
            for arm, (what, bad) in checks.items()}


def analyze_all(rows, spec, secondary=True):
    tasks = spec["tasks"]
    out = {"n_runs": len(rows), "family_alpha": FAMILY_ALPHA, "primary": {}, "secondary": {}}
    x, y = NOISE_FLOOR
    floor = compare(rows, x, y, tasks_kind=tasks)
    fd = floor["deltas"]["%s-%s" % (x, y)]["input_tokens"]
    gate_ok = fd["n"] > 0 and not fd["excludes_zero"]
    out["noise_floor"] = {"comparison": "C2-C", "input_tokens": fd, "gate_ok": gate_ok}
    out["arm_checks"] = arm_checks(rows)
    bad_arms = {arm for arm, v in out["arm_checks"].items() if v["violations"]}
    chained = {t["id"] for t in tasks if t["kind"] in STRATA["chained"]}
    fired = sum(1 for r in rows if r["arm"] == "C" and r["task"] in chained and r["exit_code"] == 0
                and r["turn_usage_events"] > 0 and r["steps"] > 0 and r["refusals"] > 0)
    out["c_chained_runs_with_refusal"] = fired
    for name, px, py, stratum, why in PRIMARY:
        rep = compare(rows, px, py, stratum, tasks)
        d = rep["deltas"]["%s-%s" % (px, py)]
        cd = d["correct_frac"]["mean"] if d["input_tokens"]["n"] else 0.0
        out["primary"][name] = {
            "question": why, "x": px, "y": py, "stratum": stratum,
            "input_tokens": d["input_tokens"], "correct_frac_delta_mean": cd,
            "verdict": verdict(d["input_tokens"], cd, gate_ok and not ({px, py} & bad_arms),
                               name != "P3" or fired >= MIN_REFUSAL_RUNS),
            "n_pairs": rep["n_pairs"], "dropped_pairs": rep["dropped_pairs"]}
    # Secondary, exploratory, no multiplicity control: every other arm pair, all
    # metrics, at the 95% level, overall and per stratum. Skipped when asked, as
    # the bootstraps dominate the run time of a test.
    pairs = (("C", "A"), ("S", "C"), ("C", "R"), ("S", "A"), ("R", "A"), ("D", "C"))
    for strat in (None, "chained", "trail", "independent") if secondary else ():
        for px, py in pairs:
            rep = compare(rows, px, py, strat, tasks, alpha=0.05)
            out["secondary"]["%s-%s/%s" % (px, py, strat or "all")] = {
                "n_pairs": rep["n_pairs"], "deltas": rep["deltas"]["%s-%s" % (px, py)]}
    # Which mechanisms ran, per arm and stratum, over every valid run.
    out["usage"] = {}
    for strat in (None, "chained", "trail", "independent"):
        keep = None if strat is None else {t["id"] for t in tasks if t["kind"] in STRATA[strat]}
        valid = [r for r in rows if r["exit_code"] == 0 and r["turn_usage_events"] > 0 and r["steps"] > 0
                 and (keep is None or r["task"] in keep)]
        for arm in ARM_IDS:
            rs = [r for r in valid if r["arm"] == arm]
            out["usage"]["%s/%s" % (arm, strat or "all")] = {
                "runs": len(rs),
                "runs_with_refusal": sum(1 for r in rs if r["refusals"] > 0),
                "refusals": sum(r["refusals"] for r in rs),
                "runs_with_serial_note": sum(1 for r in rs if r.get("serial_notes", 0) > 0),
                "runs_with_script": sum(1 for r in rs if r.get("script_calls", 0) > 0),
                "script_calls": sum(r.get("script_calls", 0) for r in rs),
                "runs_with_batch": sum(1 for r in rs if r["batch_calls"] > 0),
                "mean_input_tokens": statistics.fmean([r["input_tokens"] for r in rs]) if rs else None,
                "mean_steps": statistics.fmean([r["steps"] for r in rs]) if rs else None,
                "facts_correct": sum(r["correct"] for r in rs), "facts_total": sum(r["facts"] for r in rs),
                "spend_usd": sum(r["cost_nano_usd"] for r in rs) / 1e9}
    out["total_spend_usd_all_runs"] = sum(r["cost_nano_usd"] + r.get("discarded_cost_nano_usd", 0) for r in rows) / 1e9
    out["unpriced_steps_total"] = sum(r["unpriced_steps"] for r in rows)
    out["runs_retried"] = sum(1 for r in rows if r.get("attempts", 1) > 1)
    out["runs_invalid"] = sum(1 for r in rows if not rb.run_valid(r))
    return out


# --- commands ----------------------------------------------------------------
def clear_sessions(repo):
    """A run's session log holds every answer it read. Left in the checkout it
    could be read by a later run's own search, so it is removed after each run."""
    shutil.rmtree(os.path.join(repo, ".devcouncil", "sessions"), ignore_errors=True)


def cmd_run(a):
    if len(a.repo) != a.jobs:
        raise SystemExit("give one --repo per job (%d jobs, %d repos): runs in parallel must not share a checkout"
                         % (a.jobs, len(a.repo)))
    spec = load_spec()
    ids = set(a.tasks.split(",")) if a.tasks else None
    tasks = [t for t in spec["tasks"] if ids is None or t["id"] in ids]
    unknown = (ids or set()) - {t["id"] for t in tasks}
    if unknown:
        raise SystemExit("unknown task id(s): %s" % ", ".join(sorted(unknown)))
    bad = set(a.arms) - set(ARM_IDS)
    if bad:
        raise SystemExit("unknown arm(s): %s" % "".join(sorted(bad)))
    orders = [(tid, rep, "".join(c for c in order if c in a.arms))
              for tid, rep, order in schedule(tasks, a.reps)]
    return rb.run_study(a, spec, ARMS, orders, build_prompt, jobs=a.jobs, repos=a.repo,
                        after_run=clear_sessions, retries=1)


def cmd_analyze(a):
    spec = load_spec()
    rows = [json.loads(line) for line in open(os.path.join(a.out, "runs.jsonl"))]
    rep = analyze_all(rows, spec)
    if a.json_out:
        with open(a.json_out, "w") as f:
            json.dump(rep, f, indent=1)
    print(json.dumps(rep, indent=1))
    return 0


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("run")
    r.add_argument("--manvi", required=True)
    r.add_argument("--repo", required=True, action="append", help="a clean checkout; repeat once per job")
    r.add_argument("--out", required=True)
    r.add_argument("--scratch", required=True)
    r.add_argument("--reps", type=int, default=20)
    r.add_argument("--jobs", type=int, default=4)
    r.add_argument("--budget-usd", type=float, default=5.75)
    r.add_argument("--timeout", type=int, default=700)
    r.add_argument("--arms", default=ARM_IDS)
    r.add_argument("--tasks")
    n = sub.add_parser("analyze")
    n.add_argument("--out", required=True)
    n.add_argument("--json-out")
    a = ap.parse_args()
    return {"run": cmd_run, "analyze": cmd_analyze}[a.cmd](a)


if __name__ == "__main__":
    sys.exit(main())
