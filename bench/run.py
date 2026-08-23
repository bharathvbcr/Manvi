"""Benchmark runner.

Enforces the one rule that this machine's memory budget makes non-negotiable:
exactly one model resident at a time. 64 GB of unified memory cannot hold a 38 GB
Q8 MoE and an 18 GB dense model at once, and when it tries, the loser is whatever
else was mid-inference.
"""
import argparse
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mh.bench import load_tasks
from mh.compute import Sampler, tok_s
from mh.harness import Config, Harness
from mh.model import Client
from mh.runtime import (STARVE_ABORT_DEFAULT, ensure_sole_tenant,
                        is_starved_episode, keep_existing_episode,
                        should_retry_starved, unstick_server, unload_all)

HERE = os.path.dirname(os.path.abspath(__file__))
RESULTS = os.path.join(HERE, "results")
WORK = os.path.join(HERE, ".work")


CONFIGS = {
    "full": lambda: Config(name="full"),
    "baseline": Config.baseline,
    "no-envboot": lambda: Config(name="no-envboot", envboot=False),
    "no-verifygate": lambda: Config(name="no-verifygate", verifygate=False),
    "no-checklist": lambda: Config(name="no-checklist", checklist=False),
    "no-groundfs": lambda: Config(name="no-groundfs", groundfs=False),
    "no-loopbreak": lambda: Config(name="no-loopbreak", loopbreak=False),
    "no-outcap": lambda: Config(name="no-outcap", outcap=False),
    "no-nativetools": lambda: Config(name="no-nativetools", nativetools=False),
}


def seed_for_repeat(base, rep, n_repeat):
    """Pin a seed per repeat index so a cell is reproducible.

    `--seed N --repeat 5` uses N, N+1, N+2, N+3, N+4. Multi-repeat runs
    with no `--seed` still pin to the repeat index (0, 1, ...). A single
    unseeded run stays unseeded.
    """
    if base is not None:
        return base + rep
    if n_repeat > 1:
        return rep
    return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", required=True)
    ap.add_argument("--config", default="full", choices=sorted(CONFIGS))
    ap.add_argument("--tasks", default="")
    ap.add_argument("--max-steps", type=int, default=0,
                    help="LLM-turn ceiling; 0 means no ceiling (stop on finish / "
                         "no_tool_call / context_exhausted)")
    ap.add_argument("--max-wall", type=int, default=1800,
                    help="episode wall-clock budget in seconds; 0 disables. "
                         "Exceeding it stops the loop and scores fail (default 1800)")
    ap.add_argument("--num-ctx", type=int, default=32768)
    ap.add_argument("--num-predict", type=int, default=4096)
    ap.add_argument("--temperature", type=float, default=0.6)
    ap.add_argument("--seed", type=int, default=None)
    ap.add_argument("--repeat", type=int, default=1)
    ap.add_argument("--tag", default="")
    ap.add_argument("--think", action=argparse.BooleanOptionalAction, default=True,
                    help="request reasoning channel from model (auto-falls back if unsupported)")
    ap.add_argument("--keep-resident", action="store_true",
                    help="skip the pre-run eviction (only when already sole tenant)")
    ap.add_argument("--share-gpu", action="store_true",
                    help="allow one peer model on this GPU (GH200 Qwen+Ornith)")
    ap.add_argument("--force", action="store_true",
                    help="re-run episodes even if {task}.rep{rep}.json exists")
    ap.add_argument("--force-starved", action="store_true",
                    help="re-run only first-turn 0-token timeout artefacts; keep real episodes")
    ap.add_argument("--starve-abort", type=int, default=STARVE_ABORT_DEFAULT,
                    help="abort the cell after this many consecutive 0-token timeouts "
                         "(0 disables). Prevents burning 40×30 min on a wedged GPU.")
    args = ap.parse_args()

    names = [t for t in args.tasks.split(",") if t.strip()] or None
    tasks = load_tasks(names)
    cfg = CONFIGS[args.config]()
    cfg.max_steps = args.max_steps
    cfg.wall_s = args.max_wall

    try:
        evicted = ensure_sole_tenant(args.model, evict=not args.keep_resident,
                                     share_gpu=args.share_gpu)
    except RuntimeError as e:
        raise SystemExit(f"[runner] {e}")
    if evicted:
        print(f"[runner] evicted {', '.join(evicted)} so {args.model} runs alone")
    elif args.share_gpu:
        print(f"[runner] share-gpu: {args.model} may coexist with one peer")

    client = Client(args.model, temperature=args.temperature, top_p=0.95, top_k=20,
                    num_ctx=args.num_ctx, num_predict=args.num_predict,
                    think=args.think, seed=args.seed,
                    timeout=args.max_wall if args.max_wall > 0 else 1800)

    slug = args.model.replace("/", "_").replace(":", "_")
    tag = args.tag or time.strftime("%Y%m%d-%H%M%S")
    outdir = os.path.join(RESULTS, f"{slug}__{cfg.name}__{tag}")
    os.makedirs(outdir, exist_ok=True)

    rows = []
    cap = "uncapped" if not args.max_steps else str(args.max_steps)
    share_gpu = bool(args.share_gpu)
    starve_streak = 0
    starved_abort = False
    print(f"[runner] model={args.model} config={cfg.name} tasks={len(tasks)} "
          f"repeat={args.repeat} seed_base={args.seed} max_steps={cap} "
          f"max_wall={args.max_wall or 'off'} share_gpu={share_gpu}")
    for rep in range(args.repeat):
        if starved_abort:
            break
        pinned = seed_for_repeat(args.seed, rep, args.repeat)
        if pinned is not None:
            client.options["seed"] = pinned
        else:
            client.options.pop("seed", None)
        for task in tasks:
            if starved_abort:
                break
            ep_path = os.path.join(outdir, f"{task.name}.rep{rep}.json")
            if os.path.isfile(ep_path) and not args.force:
                try:
                    prev = json.load(open(ep_path))
                    row = prev.get("row") or {}
                except (OSError, json.JSONDecodeError):
                    row = {}
                if keep_existing_episode(row, force=args.force):
                    rows.append(row)
                    mark = "PASS" if row.get("passed") else "fail"
                    print(f"  {task.name:24s} {mark}  skip existing "
                          f"{task.name}.rep{rep}.json  "
                          f"{row.get('stop_reason', '')}",
                          flush=True)
                    continue
            sandbox = os.path.join(WORK, f"{slug}-{cfg.name}-{task.name}-{rep}")
            os.makedirs(os.path.dirname(sandbox), exist_ok=True)
            row = None
            verify_output, events = "", []
            for attempt in (0, 1):
                task.materialise(sandbox)
                h = Harness(client, cfg, sandbox, task, log_dir=outdir)
                sampler = Sampler(interval=2.0)
                sampler.start()
                try:
                    res = h.run()
                except Exception as e:
                    sampler.stop()
                    print(f"  {task.name:24s} RUNNER-ERROR {type(e).__name__}: {e}")
                    row = {"task": task.name, "rep": rep, "seed": pinned,
                           "passed": False, "steps": 0, "output_tokens": 0,
                           "stop_reason": f"runner_error:{type(e).__name__}",
                           "errors": [str(e)]}
                    break
                compute = sampler.stop()
                decode_tok_s = tok_s(res.output_tokens, res.eval_duration_ns)
                prompt_tok_s = tok_s(res.prompt_tokens, res.prompt_eval_duration_ns)
                row = {"task": task.name, "rep": rep, "seed": pinned,
                       "passed": res.passed,
                       "finished": res.finished, "stop_reason": res.stop_reason,
                       "steps": res.steps, "tool_calls": res.tool_calls,
                       "wall_s": round(res.wall_s, 1),
                       "model_s": round(res.model_latency_s, 1),
                       "prompt_tokens": res.prompt_tokens,
                       "output_tokens": res.output_tokens,
                       "tool_errors": len(res.errors),
                       "errors": res.errors[:8],
                       "peak_prompt_tokens": res.peak_prompt_tokens,
                       "eval_duration_ns": res.eval_duration_ns,
                       "prompt_eval_duration_ns": res.prompt_eval_duration_ns,
                       "decode_tok_s": decode_tok_s,
                       "prompt_tok_s": prompt_tok_s,
                       "compute": compute}
                if should_retry_starved(row, attempt):
                    print(f"[runner] starved first turn on {task.name}; "
                          f"unsticking server and retrying as sole tenant",
                          flush=True)
                    if share_gpu:
                        unloaded = unload_all(keep=args.model)
                        if unloaded:
                            print(f"[runner] evicted {', '.join(unloaded)}",
                                  flush=True)
                        share_gpu = False
                    unstick_server(args.model)
                    continue
                verify_output = res.verify_output
                events = res.events
                break
            if row is None:
                continue
            rows.append(row)
            with open(ep_path, "w") as f:
                json.dump({"model": args.model, "config": cfg.as_dict(),
                           "task": task.name, "row": row,
                           "verify_output": verify_output,
                           "events": events}, f, indent=1)
            mark = "PASS" if row.get("passed") else "fail"
            extra = ""
            compute = row.get("compute") or {}
            dts = row.get("decode_tok_s")
            if dts is not None:
                extra += f"  decode={dts:.0f} tok/s"
            power = compute.get("power_frac_mean") if compute else None
            if power is not None:
                extra += f"  power={power:.0%}"
            print(f"  {task.name:24s} {mark}  steps={row.get('steps', 0):<3d} "
                  f"calls={row.get('tool_calls', 0):<3d} {row.get('wall_s', 0):6.1f}s  "
                  f"out_tok={row.get('output_tokens', 0):<6d} "
                  f"{row.get('stop_reason', '')}{extra}",
                  flush=True)
            if is_starved_episode(row):
                starve_streak += 1
                limit = args.starve_abort
                if limit and starve_streak >= limit:
                    print(f"[runner] aborting cell: {starve_streak} consecutive "
                          f"0-token timeouts (starve-abort={limit})",
                          flush=True)
                    starved_abort = True
            else:
                starve_streak = 0

    n = len(rows)
    p = sum(1 for r in rows if r.get("passed"))
    summary = {"model": args.model, "config": cfg.as_dict(), "n": n, "passed": p,
               "pass_rate": round(100.0 * p / n, 1) if n else 0.0,
               "wall_s": round(sum(r.get("wall_s", 0) for r in rows), 1),
               "output_tokens": sum(r.get("output_tokens", 0) for r in rows),
               "seed_base": args.seed, "repeat": args.repeat,
               "protocol": {
                   "max_steps": args.max_steps,
                   "max_wall": args.max_wall,
                   "share_gpu": bool(args.share_gpu),
                   "share_gpu_demoted": bool(args.share_gpu) and not share_gpu,
                   "starved_abort": starved_abort,
                   "num_ctx": args.num_ctx,
                   "num_predict": args.num_predict,
                   "temperature": args.temperature,
                   "think": bool(args.think),
               },
               "rows": rows}
    with open(os.path.join(outdir, "summary.json"), "w") as f:
        json.dump(summary, f, indent=1)
    print(f"[runner] {p}/{n} passed ({summary['pass_rate']}%)  "
          f"total {summary['wall_s']}s  -> {outdir}")
    if starved_abort:
        raise SystemExit(2)


if __name__ == "__main__":
    main()
