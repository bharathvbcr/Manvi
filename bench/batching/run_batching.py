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
import re
import queue
import subprocess
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor

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
SCRIPT_TOOL = "devcouncil_script"
REFUSAL_RE = re.compile(r"refused, not run: (\d+) steps?")
# The note the loop appends to the result of the third lone lookup in a row
# (agent/batch.go noteSerialLookup). The refusal text shares its opening words,
# so the note is told apart by the sentence that follows them.
SERIAL_NOTE_RE = re.compile(r"harness: \d+ steps in a row each made a single lookup\. Lookups that")


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
         "refusals": 0, "refused_steps": 0, "serial_notes": 0, "script_calls": 0,
         "notices": [], "errors": [],
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
            if name == SCRIPT_TOOL:
                m["script_calls"] += 1
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
            text = e.get("text", "") or ""
            if SERIAL_NOTE_RE.search(text):
                m["serial_notes"] += 1
            hit = REFUSAL_RE.search(text)
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


def scrub(value, repo):
    """The rows are committed, so the checkout path and the home directory a tool
    or an error message may quote are replaced before a row is stored."""
    if isinstance(value, list):
        return [scrub(v, repo) for v in value]
    for host, stand_in in ((os.path.realpath(repo), "<repo>"), (repo, "<repo>"),
                           (os.path.expanduser("~"), "~")):
        if host and host != "/":
            value = value.replace(host, stand_in)
    return value


def run_one(binary, repo, prompt, arm, scratch, timeout_s, arms=None):
    env = {k: v for k, v in os.environ.items() if k not in STRIP_ENV}
    env.update(BASE_ENV)
    env.update((arms or ARMS)[arm])
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
    for k in ("final_text", "stderr_tail", "notices", "errors"):
        m[k] = scrub(m[k], repo)
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


def run_valid(r):
    """A run that exited non-zero or produced no usage or no step is invalid,
    never a zero."""
    return r["exit_code"] == 0 and r["turn_usage_events"] > 0 and r["steps"] > 0


def run_study(a, spec, arms, orders, prompt_for, jobs=1, repos=None, after_run=None, retries=0):
    """Run every (task, rep, arm) of `orders` that results.jsonl does not hold,
    `jobs` units at a time (the arms of one unit stay in sequence), until the
    spend reaches a.budget_usd. Returns 3 when the budget stopped it. A unit
    already started is finished, so the spend can pass the budget by one unit.

    `repos` is the checkouts the runs execute in (default: a.repo for all). Each
    worker holds one for a unit, so a study may give every job its own and no run
    sees another's files; `after_run(repo)` is called after each run, for
    removing what the run left in its checkout. A run that is invalid (run_valid)
    is retried up to `retries` times; the stored row says how many attempts it took,
    and a run that is still invalid is stored as invalid, never zero-filled."""
    by_id = {t["id"]: t for t in spec["tasks"]}
    os.makedirs(a.out, exist_ok=True)
    raw_dir = os.path.join(a.scratch, "raw")
    os.makedirs(raw_dir, exist_ok=True)
    results_path = os.path.join(a.out, "runs.jsonl")
    done = set()
    spent = [0.0]
    if os.path.exists(results_path):
        for line in open(results_path):
            r = json.loads(line)
            done.add((r["task"], r["rep"], r["arm"]))
            spent[0] += (r["cost_nano_usd"] + r.get("discarded_cost_nano_usd", 0)) / 1e9
    lock = threading.Lock()
    stopped = []
    free = queue.Queue()
    for repo in (repos or [a.repo] * jobs):
        free.put(repo)

    def unit(tid, rep, order):
        repo = free.get()
        try:
            unit_in(repo, tid, rep, order)
        finally:
            free.put(repo)

    def unit_in(repo, tid, rep, order):
        for arm in order:
            if (tid, rep, arm) in done:
                continue
            with lock:
                if spent[0] >= a.budget_usd:
                    stopped.append(1)
                    return
            task = by_id[tid]
            attempts, discarded = 0, 0
            while True:
                attempts += 1
                m = run_one(a.manvi, repo, prompt_for(spec, task, arm), arm,
                            os.path.join(raw_dir, "%s-r%d-%s.ndjson" % (tid, rep, arm)), a.timeout, arms)
                if after_run:
                    after_run(repo)
                if run_valid(m) or attempts > retries:
                    break
                discarded += m["cost_nano_usd"]
            n_ok, flags = grade(m["final_text"], task["facts"])
            row = dict(m, task=tid, rep=rep, arm=arm, order=order, facts=len(task["facts"]),
                       correct=n_ok, fact_flags=flags, attempts=attempts, discarded_cost_nano_usd=discarded)
            with lock:
                with open(results_path, "a") as f:
                    f.write(json.dumps(row) + "\n")
                spent[0] += (m["cost_nano_usd"] + discarded) / 1e9
                print("%s r%d %s order=%s in=%d steps=%d calls=%d batches=%d scripts=%d ref=%d ok=%d/%d $%.4f (total $%.4f)" % (
                    tid, rep, arm, order, m["input_tokens"], m["steps"], m["tool_calls"], m["batch_calls"],
                    m["script_calls"], m["refusals"], n_ok, len(flags), m["cost_nano_usd"] / 1e9, spent[0]), flush=True)

    with ThreadPoolExecutor(max_workers=jobs) as pool:
        for f in [pool.submit(unit, *u) for u in orders]:
            f.result()
    if stopped:
        print("budget reached: spent $%.4f >= $%.2f; stopping" % (spent[0], a.budget_usd))
        return 3
    return 0


def cmd_run(a):
    spec = load_tasks()
    return run_study(a, spec, ARMS, schedule(spec["tasks"], a.reps), lambda s, t, arm: build_prompt(s, t))


def median(xs):
    s = sorted(xs)
    n = len(s)
    return float("nan") if n == 0 else (s[n // 2] if n % 2 else (s[n // 2 - 1] + s[n // 2]) / 2)


def valid_pairs(rows, arms="ABC"):
    """Only task-reps with every arm valid enter a paired comparison. A run
    that exited non-zero or produced no usage is invalid, never a zero."""
    cells = {}
    for r in rows:
        cells.setdefault((r["task"], r["rep"]), {})[r["arm"]] = r
    out, dropped = [], []
    for key, cell in sorted(cells.items()):
        ok = set(cell) == set(arms) and all(run_valid(x) for x in cell.values())
        (out if ok else dropped).append((key, cell))
    return out, dropped


def analyze(rows, arms="ABC", comparisons=(("B", "A"), ("C", "A"), ("C", "B")), alpha=0.05):
    """Per-arm summaries and paired deltas (x minus y, for each pair in
    `comparisons`). `alpha` is the bootstrap's two-sided level, so a family of k
    primary comparisons passes 0.05/k."""
    pairs, dropped = valid_pairs(rows, arms)
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
    for arm in arms:
        rs = [p[1][arm] for p in pairs]
        d = {"n": len(rs), "batch_calls_total": sum(r["batch_calls"] for r in rs),
             "batch_inner_calls_total": sum(r["batch_inner_calls"] for r in rs),
             "refusals_total": sum(r["refusals"] for r in rs),
             "runs_with_refusal": sum(1 for r in rs if r["refusals"] > 0),
             "runs_with_script": sum(1 for r in rs if r.get("script_calls", 0) > 0),
             "script_calls_total": sum(r.get("script_calls", 0) for r in rs),
             "serial_notes_total": sum(r.get("serial_notes", 0) for r in rs),
             "notices_total": sum(len(r["notices"]) for r in rs),
             "writes_total": sum(r["writes"] for r in rs),
             "facts_correct": sum(r["correct"] for r in rs), "facts_total": sum(r["facts"] for r in rs),
             "runs_all_correct": sum(1 for r in rs if r["correct"] == r["facts"]),
             "spend_usd": sum(r["cost_nano_usd"] for r in rs) / 1e9}
        for name, fn in metrics.items():
            xs = [fn(r) for r in rs]
            mean, lo, hi = bootstrap_ci(xs, alpha=alpha)
            d[name] = {"mean": mean, "median": median(xs), "ci95": [lo, hi], "ci_level": round(1 - alpha, 6)}
        rep["arms"][arm] = d
    for x, y in comparisons:
        key = "%s-%s" % (x, y)
        rep["deltas"][key] = {}
        for name, fn in metrics.items():
            ds = [fn(p[1][x]) - fn(p[1][y]) for p in pairs]
            mean, lo, hi = bootstrap_ci(ds, alpha=alpha)
            rep["deltas"][key][name] = {
                "mean": mean, "median": median(ds), "ci95": [lo, hi], "ci_level": round(1 - alpha, 6),
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
