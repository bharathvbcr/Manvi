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
from mh.runtime import (CellError, cell_rows, ensure_sole_tenant,
                        is_api_model, is_starved_episode, is_unserved_episode,
                        observed_parallel_slots)
from run import ACCOUNT_REFUSED_RC, CONFIGS, seed_for_repeat

HERE = os.path.dirname(os.path.abspath(__file__))
# Overridable so tests can build a throwaway grid instead of writing
# into the real results tree.
RESULTS = os.environ.get("MH_RESULTS") or os.path.join(HERE, "results")

# Linux/CUDA tags. qwen3.8:27b is the CUDA peer of the Apple-only
# qwen3.8:27b-mlx build that saturated the 11-task suite. Do not pull
# an *-mlx tag onto this host — it will not run on GH200.
# Two models only. Roles are labels, not a capability axis: Ornith is
# larger than Qwen and scored worse on the hard suite.
#
# The Cerebras arms (`cerebras:gpt-oss-120b`, `cerebras:gemma-4-31b`) are
# deliberately NOT in this list. This grid is the preregistered design, which
# fixes two local models; a third arm appearing here by default would silently
# change the registered experiment. Run one explicitly instead --
# `--models cerebras:gpt-oss-120b --tag <extension-tag>` -- into its own tag,
# and report it as the declared extension it is.
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


def _reap_one(inflight):
    """Block until one in-flight cell exits; return (rc, model, cfg, index)."""
    while True:
        for idx, (proc, model, cfg, i) in enumerate(inflight):
            rc = proc.poll()
            if rc is not None:
                inflight.pop(idx)
                return rc, model, cfg, i
        time.sleep(1.0)


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


def unusable(row):
    """Episodes that make a cell incomplete however many files it holds.

    Two ways an episode can occupy a (task, rep) slot without being a
    measurement: the local server never served it (starved), or the provider
    refused the account (unserved). Both must keep the cell out of `complete`,
    or the grid skips it and the numbers stand.

    The second was missed once, and the miss is the reason this function
    exists rather than an inline `is_starved_episode`. run.py's
    keep_existing_episode already refused to keep a 402 row -- but
    grid.complete() decided the cell was finished and run.py was never invoked
    for it. A cell of 160 zero-token refusals reported 0.0% and the grid
    printed "skip complete".
    """
    return is_starved_episode(row) or is_unserved_episode(row)


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
    return not any(unusable(r) for r in rows)


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
    ap.add_argument("--parallel", type=int, default=1, metavar="N",
                    help="run N cells at once against ONE resident model. This "
                         "is the only parallel shape the runner supports: the "
                         "weights stay loaded and N episodes are in flight. Two "
                         "MODELS at once is --share-gpu, is a different "
                         "protocol, and is refused in combination with this. N "
                         "is stamped on every episode as `concurrency`, so "
                         "cells run at different N will not pool -- a contended "
                         "GPU moves wall clock and --max-wall scores a fail.")
    ap.add_argument("--rep-offset", type=int, default=0,
                    help="passed to run.py; start repeat indices at N to add "
                         "seeds to an already-run cell. Use a fresh --tag: the "
                         "runner refuses to touch an existing episode.")
    args = ap.parse_args()

    if args.parallel < 1:
        raise SystemExit(f"[grid] --parallel must be >= 1, got {args.parallel}")
    if args.parallel > 1 and args.share_gpu:
        raise SystemExit(
            "[grid] --parallel and --share-gpu together put two models and N "
            "episodes on one GPU at once. v1 already recorded what one axis of "
            "that did: concurrent Qwen+Ornith starved Qwen's first turn into a "
            "0-token timeout scored as a failure. Pick one.")

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

    if args.parallel > 1 and len(models) > 1:
        raise SystemExit(
            f"[grid] --parallel runs N cells against ONE resident model, and "
            f"this plan has {len(models)}: "
            f"{', '.join(m for _r, m in models)}. Parallel cells of two models "
            f"would each try to evict the other's weights mid-episode. Run one "
            f"model at a time with --models.")
    # `not args.dry_run`: a dry run prints a plan and must not touch the
    # machine. Without it, `--parallel N --dry-run` evicted the resident model
    # before printing that it intended to run nothing.
    if args.parallel > 1 and not args.dry_run and not is_api_model(models[0][1]):
        # The parallel cells all carry --keep-resident so they cannot evict
        # each other, which also means none of them clears a model left over
        # from an earlier run. Do it once, here, before anything launches --
        # otherwise every cell refuses with "still resident" and the grid does
        # nothing at all.
        try:
            evicted = ensure_sole_tenant(models[0][1], results_root=RESULTS)
        except RuntimeError as e:
            raise SystemExit(f"[grid] {e}")
        if evicted:
            print(f"[grid] evicted {', '.join(evicted)} so "
                  f"{models[0][1]} runs alone across {args.parallel} cells",
                  flush=True)
        # ollama serialises requests beyond OLLAMA_NUM_PARALLEL. If it is below
        # N, the extra cells do not decode concurrently -- they queue, each
        # episode's wall clock grows by the queue depth, and --max-wall scores
        # the overflow as model failures. The variable belongs to the ollama
        # SERVER process, so this can only check what is visible from here; an
        # unset value is reported as unknown rather than as agreement.
        want = args.parallel
        # The runner's own -np is authoritative. OLLAMA_NUM_PARALLEL is a
        # ceiling the scheduler need not take, and on ollama 0.33.0 it did not:
        # the variable read 4 and the runner launched with -np 1, so four
        # concurrent cells would have queued behind one slot at 1.01x the
        # serial rate while every episode's wall clock grew.
        slots = observed_parallel_slots()
        if slots is not None and slots < want:
            raise SystemExit(
                f"[grid] the loaded runner has {slots} decode slot(s), below "
                f"--parallel {want}. OLLAMA_NUM_PARALLEL is only a ceiling; "
                f"this is what the scheduler actually chose. The extra cells "
                f"would queue rather than decode, inflating every episode's "
                f"wall clock into --max-wall timeouts scored as model "
                f"failures. Run with --parallel {slots}, or make the server "
                f"allocate more slots and re-check.")
        if slots is None:
            print(f"[grid] NOTE: no runner is loaded yet, so the real decode-"
                  f"slot count could not be read. OLLAMA_NUM_PARALLEL is a "
                  f"ceiling, not a guarantee -- verify with "
                  f"`ps -eo args | grep llama-server` once a cell is running "
                  f"that it shows -np >= {want}.", flush=True)
        got = os.environ.get("OLLAMA_NUM_PARALLEL")
        if got is None:
            print(f"[grid] NOTE: OLLAMA_NUM_PARALLEL is not visible from this "
                  f"process. It is read by the ollama server, not by the "
                  f"client, so confirm on the server that it is >= "
                  f"{want}. Below {want}, the extra cells queue instead of "
                  f"decoding, and the added wall clock is scored as failures.",
                  flush=True)
        else:
            try:
                ok = int(got) >= want
            except ValueError:
                ok = False
            if not ok:
                raise SystemExit(
                    f"[grid] OLLAMA_NUM_PARALLEL={got!r} is below --parallel "
                    f"{want}. The extra cells would queue rather than decode, "
                    f"inflating every episode's wall clock into --max-wall "
                    f"timeouts that score as model failures. Raise it on the "
                    f"ollama server and restart it, or lower --parallel.")

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
    inflight = []
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
               "--max-wall", str(args.max_wall),
               "--concurrency", str(args.parallel)]
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
        if args.parallel > 1:
            # One resident model, N cells in flight. The first cell loads the
            # weights; the rest must not evict them out from under it, which is
            # what --keep-resident means. run.py's lease still refuses a
            # sibling holding a DIFFERENT model, so this cannot silently become
            # the two-model case.
            inflight.append((subprocess.Popen(cmd + ["--keep-resident"],
                                              cwd=HERE, env=env), model, cfg, i))
            if len(inflight) < args.parallel:
                continue
            rc, model, cfg, i = _reap_one(inflight)
        else:
            rc = subprocess.call(cmd, cwd=HERE, env=env)
        if rc == ACCOUNT_REFUSED_RC:
            # The provider refused the account. Every remaining cell fails the
            # same way, and a grid that keeps going fills itself with 0-token
            # rows that read like the ablations working. Stop the whole grid.
            print(f"[grid] ABORTING GRID: provider refused the account during "
                  f"{model} {cfg}. {len(plan) - i} cell(s) not attempted. Fix "
                  f"the account and re-run this tag; the episodes that died "
                  f"this way are re-run automatically.", flush=True)
            failed += 1
            break
        if rc != 0:
            print(f"[grid] FAILED rc={rc} {model} {cfg}", flush=True)
            failed += 1
            continue
        ran += 1
    # Nothing may be reported while a cell is still writing episodes: `done`
    # over a live runner is a completeness claim about a directory that is
    # still changing.
    while inflight:
        rc, model, cfg, i = _reap_one(inflight)
        if rc == ACCOUNT_REFUSED_RC:
            print(f"[grid] ABORTING GRID: provider refused the account during "
                  f"{model} {cfg}.", flush=True)
            failed += 1
        elif rc != 0:
            print(f"[grid] FAILED rc={rc} {model} {cfg}", flush=True)
            failed += 1
        else:
            ran += 1
    print(f"[grid] done ran={ran} skipped={skipped} failed={failed} "
          f"wall={time.time() - t0:.0f}s")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
