#!/usr/bin/env python3
"""Paired three-arm benchmark of Manvi's batching policy, live against Anthropic.

Stdlib only; reuses the bench's own bootstrap (mh.stats.bootstrap_ci) for every
interval. It drives the built manvi binary exactly as an operator would
(`manvi run --json`) from a clean checkout at a fixed commit, parses the NDJSON
event stream, grades the final answer against tasks.json, and writes one
result row per run (metrics and the graded answer only, never tool output).

  run_batching.py run     --manvi BIN --repo CLEAN --out DIR [--reps 3] [--budget-usd 1.6]
  run_batching.py analyze --out DIR [--json-out FILE]
"""
import argparse
import itertools
import json
import os
import random
import re
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, ".."))
from mh.stats import bootstrap_ci  # noqa: E402

ARMS = {
    "A": {"MANVI_HARNESS_BATCH_TOOL": "off"},
    "B": {"MANVI_HARNESS_BATCH_TOOL": "on", "MANVI_HARNESS_SERIAL_LOOKUPS": "off"},
    "C": {"MANVI_HARNESS_BATCH_TOOL": "on"},
}
BASE_ENV = {
    "MANVI_LLM_PROVIDER_DEFAULT": "anthropic",
    "MANVI_LLM_TIER_MAIN": "claude-haiku-5-5",
}
# Arm C must run the *default* serial-lookups mode, so the variable is removed
# from the environment rather than set to "refuse": an inherited value would
# silently change the arm.
STRIP_ENV = ("MANVI_HARNESS_BATCH_TOOL", "MANVI_HARNESS_SERIAL_LOOKUPS")
BATCH_TOOL = "devcouncil_batch"
REFUSAL_RE = re.compile(r"refused, not run: (\d+) steps?")


def load_tasks(path=None):
    with open(path or os.path.join(HERE, "tasks.json")) as f:
        return json.load(f)


def build_prompt(spec, task):
    items = "\n".join("%d. %s" % (i + 1, s) for i, s in enumerate(task["items"]))
    return spec["preamble"] + "\n\n" + items


def schedule(tasks, reps):
    """Units are (task, rep); the arm order rotates per unit through all six
    permutations, so every arm holds every position equally often."""
    perms = list(itertools.permutations("ABC"))
    units = [(t["id"], r) for r in range(reps) for t in tasks]
    return [(tid, rep, "".join(perms[i % len(perms)])) for i, (tid, rep) in enumerate(units)]


def grade(text, facts):
    """A fact is right when its file basename appears in the answer and the
    identifier appears within 250 characters of a match of its value pattern.
    Returns (correct_count, per_fact_flags)."""
    flags = []
    for f in facts:
        ok = False
        if f["file"].lower() in text.lower():
            for m in re.finditer(re.escape(f["symbol"]), text, re.I):
                window = text[max(0, m.start() - 250): m.end() + 250]
                if re.search(f["value"], window, re.I):
                    ok = True
                    break
        flags.append(ok)
    return sum(flags), flags


def parse_stream(lines):
    """Reduce one manvi --json NDJSON stream to the run's metrics."""
    m = {"input_tokens": 0, "output_tokens": 0, "cache_read_tokens": 0,
         "cost_nano_usd": 0, "unpriced_steps": 0, "steps": 0, "tool_calls": 0,
         "tool_counts": {}, "batch_calls": 0, "batch_inner_calls": 0,
         "refusals": 0, "refused_steps": 0, "notices": [], "errors": [],
         "turn_usage_events": 0, "writes": 0, "bad_lines": 0}
    final = []  # assistant.text arrives as stream chunks; the answer is those after the last tool result
    for raw in lines:
        raw = raw.strip()
        if not raw:
            continue
        try:
            e = json.loads(raw)
        except ValueError:
            m["bad_lines"] += 1
            continue
        if e.get("agent"):
            continue  # a child's own loop; none is expected in this task
        k = e.get("kind")
        if k == "turn.usage":
            m["turn_usage_events"] += 1
            for f in ("input_tokens", "output_tokens", "cache_read_tokens", "cost_nano_usd", "unpriced_steps"):
                m[f] += e.get(f, 0)
        elif k == "step.usage":
            m["steps"] += 1
        elif k == "tool.start":
            name = e.get("tool", "")
            m["tool_calls"] += 1
            m["tool_counts"][name] = m["tool_counts"].get(name, 0) + 1
            if name == BATCH_TOOL:
                m["batch_calls"] += 1
                try:
                    m["batch_inner_calls"] += len(e.get("arguments", {}).get("calls", []))
                except AttributeError:
                    pass
            if name in ("devcouncil_write_file", "devcouncil_patch_file", "devcouncil_edit_file"):
                m["writes"] += 1
        elif k == "tool.result":
            final = []
            hit = REFUSAL_RE.search(e.get("text", "") or "")
            if hit:
                m["refusals"] += 1
                m["refused_steps"] += int(hit.group(1))
        elif k == "notice":
            m["notices"].append((e.get("text", "") or "")[:200])
        elif k == "error":
            m["errors"].append((e.get("text", "") or "")[:200])
        elif k == "assistant.text":
            final.append(e.get("text", "") or "")
    m["final_text"] = "".join(final)
    return m


def run_one(binary, repo, prompt, arm, scratch, timeout_s):
    env = {k: v for k, v in os.environ.items() if k not in STRIP_ENV}
    env.update(BASE_ENV)
    env.update(ARMS[arm])
    cmd = [os.path.expanduser("~/.local/bin/anthropic-account"), "exec", "--", binary, "run",
           "--json", "--max-steps", "60", "--timeout", "10m", "-p", prompt]
    t0 = time.time()
    p = subprocess.run(cmd, cwd=repo, env=env, capture_output=True, text=True, timeout=timeout_s)
    wall = time.time() - t0
    with open(scratch, "w") as f:
        f.write(p.stdout)
    m = parse_stream(p.stdout.splitlines())
    m["exit_code"] = p.returncode
    m["wall_s"] = round(wall, 1)
    m["stderr_tail"] = p.stderr[-300:]
    return m


def cmd_reparse(a):
    """Rebuild every row of runs.jsonl from its saved NDJSON, keeping the run's
    identity (task, rep, arm, order) and grading it again. Used when only the
    parser or the grader changed, so no run is repeated or paid for twice."""
    spec = load_tasks()
    by_id = {t["id"]: t for t in spec["tasks"]}
    path = os.path.join(a.out, "runs.jsonl")
    rows = [json.loads(l) for l in open(path)]
    out = []
    for r in rows:
        raw = os.path.join(a.scratch, "raw", "%s-r%d-%s.ndjson" % (r["task"], r["rep"], r["arm"]))
        m = parse_stream(open(raw).read().splitlines())
        for k in ("exit_code", "wall_s", "stderr_tail"):
            m[k] = r[k]
        n_ok, flags = grade(m["final_text"], by_id[r["task"]]["facts"])
        out.append(dict(m, task=r["task"], rep=r["rep"], arm=r["arm"], order=r["order"],
                        facts=len(flags), correct=n_ok, fact_flags=flags))
    with open(path, "w") as f:
        for r in out:
            f.write(json.dumps(r) + "\n")
    print("reparsed %d rows" % len(out))
    return 0


def cmd_run(a):
    spec = load_tasks()
    by_id = {t["id"]: t for t in spec["tasks"]}
    os.makedirs(a.out, exist_ok=True)
    raw_dir = os.path.join(a.scratch, "raw")
    os.makedirs(raw_dir, exist_ok=True)
    results_path = os.path.join(a.out, "runs.jsonl")
    done = set()
    spent = 0.0
    if os.path.exists(results_path):
        for line in open(results_path):
            r = json.loads(line)
            done.add((r["task"], r["rep"], r["arm"]))
            spent += r["cost_nano_usd"] / 1e9
    for tid, rep, order in schedule(spec["tasks"], a.reps):
        for arm in order:
            if (tid, rep, arm) in done:
                continue
            if spent >= a.budget_usd:
                print("budget reached: spent $%.4f >= $%.2f; stopping" % (spent, a.budget_usd))
                return 3
            task = by_id[tid]
            m = run_one(a.manvi, a.repo, build_prompt(spec, task), arm,
                        os.path.join(raw_dir, "%s-r%d-%s.ndjson" % (tid, rep, arm)), a.timeout)
            n_ok, flags = grade(m["final_text"], task["facts"])
            row = dict(m, task=tid, rep=rep, arm=arm, order=order, facts=len(task["facts"]),
                       correct=n_ok, fact_flags=flags)
            with open(results_path, "a") as f:
                f.write(json.dumps(row) + "\n")
            spent += m["cost_nano_usd"] / 1e9
            print("%s r%d %s order=%s in=%d steps=%d calls=%d batches=%d ref=%d ok=%d/%d $%.4f (total $%.4f)" % (
                tid, rep, arm, order, m["input_tokens"], m["steps"], m["tool_calls"], m["batch_calls"],
                m["refusals"], n_ok, len(flags), m["cost_nano_usd"] / 1e9, spent), flush=True)
    return 0


def median(xs):
    s = sorted(xs)
    n = len(s)
    return float("nan") if n == 0 else (s[n // 2] if n % 2 else (s[n // 2 - 1] + s[n // 2]) / 2)


def valid_pairs(rows):
    """Only task-reps with all three arms valid enter a paired comparison. A run
    that exited non-zero or produced no usage is invalid, never a zero."""
    cells = {}
    for r in rows:
        cells.setdefault((r["task"], r["rep"]), {})[r["arm"]] = r
    out, dropped = [], []
    for key, arms in sorted(cells.items()):
        ok = (set(arms) == set("ABC") and all(
            x["exit_code"] == 0 and x["turn_usage_events"] > 0 and x["steps"] > 0 for x in arms.values()))
        (out if ok else dropped).append((key, arms))
    return out, dropped


def analyze(rows):
    pairs, dropped = valid_pairs(rows)
    rep = {"n_runs": len(rows), "n_pairs": len(pairs),
           "dropped_pairs": [list(k) for k, _ in dropped], "arms": {}, "deltas": {}}
    metrics = {
        "input_tokens": lambda r: r["input_tokens"],
        "cost_usd": lambda r: r["cost_nano_usd"] / 1e9,
        "steps": lambda r: r["steps"],
        "output_tokens": lambda r: r["output_tokens"],
        "tool_calls": lambda r: r["tool_calls"],
        "correct_frac": lambda r: r["correct"] / r["facts"],
    }
    for arm in "ABC":
        rs = [p[1][arm] for p in pairs]
        d = {"n": len(rs), "batch_calls_total": sum(r["batch_calls"] for r in rs),
             "batch_inner_calls_total": sum(r["batch_inner_calls"] for r in rs),
             "refusals_total": sum(r["refusals"] for r in rs),
             "notices_total": sum(len(r["notices"]) for r in rs),
             "writes_total": sum(r["writes"] for r in rs),
             "facts_correct": sum(r["correct"] for r in rs), "facts_total": sum(r["facts"] for r in rs),
             "runs_all_correct": sum(1 for r in rs if r["correct"] == r["facts"]),
             "spend_usd": sum(r["cost_nano_usd"] for r in rs) / 1e9}
        for name, fn in metrics.items():
            xs = [fn(r) for r in rs]
            mean, lo, hi = bootstrap_ci(xs)
            d[name] = {"mean": mean, "median": median(xs), "ci95": [lo, hi]}
        rep["arms"][arm] = d
    for x, y in (("B", "A"), ("C", "A"), ("C", "B")):
        key = "%s-%s" % (x, y)
        rep["deltas"][key] = {}
        for name, fn in metrics.items():
            ds = [fn(p[1][x]) - fn(p[1][y]) for p in pairs]
            mean, lo, hi = bootstrap_ci(ds)
            rep["deltas"][key][name] = {
                "mean": mean, "median": median(ds), "ci95": [lo, hi],
                "negative": sum(1 for v in ds if v < 0), "positive": sum(1 for v in ds if v > 0),
                "zero": sum(1 for v in ds if v == 0), "n": len(ds),
                "excludes_zero": bool(ds) and (lo > 0 or hi < 0)}
    return rep


def cmd_analyze(a):
    rows = [json.loads(l) for l in open(os.path.join(a.out, "runs.jsonl"))]
    if a.max_rep is not None:
        rows = [r for r in rows if r["rep"] <= a.max_rep]
    rep = analyze(rows)
    total = sum(r["cost_nano_usd"] for r in rows) / 1e9
    rep["total_spend_usd_all_runs"] = total
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
    r.add_argument("--repo", required=True)
    r.add_argument("--out", required=True)
    r.add_argument("--scratch", required=True)
    r.add_argument("--reps", type=int, default=3)
    r.add_argument("--budget-usd", type=float, default=1.6)
    r.add_argument("--timeout", type=int, default=700)
    q = sub.add_parser("reparse")
    q.add_argument("--out", required=True)
    q.add_argument("--scratch", required=True)
    n = sub.add_parser("analyze")
    n.add_argument("--out", required=True)
    n.add_argument("--json-out")
    n.add_argument("--max-rep", type=int, help="keep only reps 0..N (the pre-registered batch is reps 0-2)")
    a = ap.parse_args()
    return {"run": cmd_run, "reparse": cmd_reparse, "analyze": cmd_analyze}[a.cmd](a)


if __name__ == "__main__":
    sys.exit(main())
