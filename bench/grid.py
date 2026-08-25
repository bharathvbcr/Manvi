"""Resumable (model × config × repeats) grid for the CUDA host.

3 models × 7 configs × 5 repeats = 105 episodes of the full suite.
Pins --seed to the repeat index so a cell is reproducible.

    python3 grid.py --dry-run
    python3 grid.py --smoke
    python3 grid.py --tag grid
"""
import argparse
import os
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mh.bench import load_tasks
from mh.runtime import CellError, cell_rows, is_starved_episode
from run import CONFIGS, seed_for_repeat

HERE = os.path.dirname(os.path.abspath(__file__))
# Overridable so tests can build a throwaway grid instead of writing
# into the real results tree.
RESULTS = os.environ.get("MH_RESULTS") or os.path.join(HERE, "results")

# Linux/CUDA tags. qwen3.8:27b is the CUDA peer of the Apple-only
# qwen3.8:27b-mlx build that saturated the 11-task suite. Do not pull
# an *-mlx tag onto this host — it will not run on GH200.
# Two models only. Roles are labels, not a capability axis: Ornith is
# larger than Qwen and scored worse on the hard suite.
MODELS = [
    ("mid", "qwen3.8:27b"),
    ("mid", "hf.co/ornith-ai/Ornith-1.5-35B-A3B-GGUF:Q8_0"),
]
GRID_CONFIGS = ["full", "baseline", "no-envboot", "no-verifygate",
                "no-checklist", "no-loopbreak", "no-outcap", "no-groundfs",
                "no-nativetools"]
REPEATS = 5
SEED_BASE = 0

HARD_TASKS = [
    "concurrency_race", "ast_transformer", "state_machine_fuzz",
    "cache_invalidation_dist", "pratt_parse", "ot_transform",
    "nfa_match", "json_patch",
]


def slug(model):
    return model.replace("/", "_").replace(":", "_")


def outdir(model, config, tag):
    return os.path.join(RESULTS, f"{slug(model)}__{config}__{tag}")


def cell_rows_on_disk(model, config, tag):
    """The cell's rows, read from its episode files, or None if unreadable.

    Whatever decides not to re-run a cell has to read the same source the
    summary is derived from. summary.json used to be rebuilt from whichever
    tasks one invocation was handed, so a cell could claim n=1 with nine
    episodes beside it -- and this skip check believed the claim.
    """
    d = outdir(model, config, tag)
    try:
        return cell_rows(d)
    except CellError as e:
        print(f"[grid] cannot read cell {d}: {e}", file=sys.stderr, flush=True)
        return None


def cell_has_starved(model, config, tag):
    return any(is_starved_episode(r)
               for r in (cell_rows_on_disk(model, config, tag) or []))


def complete(model, config, tag, n_tasks, repeats, rep_offset=0):
    """True when this cell already holds every episode this run would produce.

    Counted from the episodes on disk. An extension is complete on its own
    repeat window, not on the frozen cell's: `n` alone cannot tell reps 5-19
    from reps 0-14, so the check is on the repeat indices present.

    A cell whose episodes are all present but whose summary.json is missing is
    NOT complete: the summary is what the rest of the pipeline reads, and
    re-deriving it is cheap because every episode is skipped.
    """
    if not os.path.isfile(os.path.join(outdir(model, config, tag),
                                       "summary.json")):
        return False
    rows = cell_rows_on_disk(model, config, tag)
    if rows is None:
        return False
    want_reps = set(range(rep_offset, rep_offset + repeats))
    have = {}
    for r in rows:
        have[r.get("rep", 0)] = have.get(r.get("rep", 0), 0) + 1
    if not want_reps <= set(have):
        return False
    if any(have[rep] < n_tasks for rep in want_reps):
        return False
    return not any(is_starved_episode(r) for r in rows)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tag", default="grid")
    ap.add_argument("--repeats", type=int, default=REPEATS)
    ap.add_argument("--seed", type=int, default=SEED_BASE)
    ap.add_argument("--models", default="",
                    help="comma-separated roles or tags: weak,mid,strong or a model name")
    ap.add_argument("--configs", default="",
                    help="comma-separated configs (default: all 7)")
    ap.add_argument("--tasks", default="",
                    help="comma-separated task names (default: full suite)")
    ap.add_argument("--smoke", action="store_true",
                    help="one task, one repeat, full config only — prove the host")
    ap.add_argument("--hard", action="store_true",
                    help="only the high-difficulty tasks (skips the saturated easy 11)")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--force", action="store_true",
                    help="re-run cells that already have a complete summary")
    ap.add_argument("--force-starved", action="store_true",
                    help="re-run cells that contain first-turn 0-token timeouts")
    ap.add_argument("--max-steps", type=int, default=0,
                    help="passed to run.py; 0 means no LLM-turn ceiling")
    ap.add_argument("--max-wall", type=int, default=1800,
                    help="passed to run.py; episode wall-clock fail line in seconds")
    ap.add_argument("--share-gpu", action="store_true",
                    help="passed to run.py; allow one peer model on this GPU")
    ap.add_argument("--rep-offset", type=int, default=0,
                    help="passed to run.py; start repeat indices at N to add "
                         "seeds to an already-run cell. Use a fresh --tag: the "
                         "runner refuses to touch an existing episode.")
    args = ap.parse_args()

    models = list(MODELS)
    if args.models:
        want = {x.strip() for x in args.models.split(",") if x.strip()}
        models = [(r, m) for r, m in MODELS if r in want or m in want]
        extra = [x for x in want if x not in {r for r, _ in MODELS}
                 and x not in {m for _, m in MODELS}]
        for name in extra:
            models.append(("custom", name))
    configs = ([c.strip() for c in args.configs.split(",") if c.strip()]
               or list(GRID_CONFIGS))
    for c in configs:
        if c not in CONFIGS:
            raise SystemExit(f"unknown config {c!r}")
    repeats = args.repeats
    if args.hard and not args.tasks:
        args.tasks = ",".join(HARD_TASKS)
    if args.smoke:
        configs = ["full"]
        repeats = 1
        if not args.tasks:
            args.tasks = "binsearch"

    n_tasks = len(load_tasks([t for t in args.tasks.split(",") if t.strip()] or None))
    plan = []
    for role, model in models:
        for cfg in configs:
            plan.append((role, model, cfg))

    print(f"[grid] {len(models)} models × {len(configs)} configs × {repeats} repeats "
          f"× {n_tasks} tasks  tag={args.tag} seed_base={args.seed}")
    reps = list(range(args.rep_offset, args.rep_offset + repeats))
    print(f"[grid] reps {reps[0]}-{reps[-1]}  seeds per repeat: "
          + ", ".join(str(seed_for_repeat(args.seed, r, repeats + args.rep_offset))
                      for r in reps))
    skipped = 0
    ran = 0
    failed = 0
    t0 = time.time()
    for i, (role, model, cfg) in enumerate(plan, 1):
        skip = False
        if not args.force:
            if complete(model, cfg, args.tag, n_tasks, repeats, args.rep_offset):
                if args.force_starved and cell_has_starved(model, cfg, args.tag):
                    skip = False
                else:
                    skip = True
        if skip:
            print(f"[grid] {i}/{len(plan)} skip complete {role} {model} {cfg}")
            skipped += 1
            continue
        cmd = [sys.executable, os.path.join(HERE, "run.py"),
               "--model", model, "--config", cfg,
               "--repeat", str(repeats), "--seed", str(args.seed),
               "--rep-offset", str(args.rep_offset),
               "--tag", args.tag, "--max-steps", str(args.max_steps),
               "--max-wall", str(args.max_wall)]
        if args.share_gpu:
            cmd.append("--share-gpu")
        if args.force:
            cmd.append("--force")
        elif args.force_starved:
            cmd.append("--force-starved")
        if args.tasks:
            cmd.extend(["--tasks", args.tasks])
        print(f"[grid] {i}/{len(plan)} {role} {model} {cfg}", flush=True)
        print("       " + " ".join(cmd), flush=True)
        if args.dry_run:
            continue
        env = dict(os.environ)
        env["PYTHONUNBUFFERED"] = "1"
        rc = subprocess.call(cmd, cwd=HERE, env=env)
        if rc != 0:
            print(f"[grid] FAILED rc={rc} {model} {cfg}", flush=True)
            failed += 1
            continue
        ran += 1
    print(f"[grid] done ran={ran} skipped={skipped} failed={failed} "
          f"wall={time.time() - t0:.0f}s")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
