"""Benchmark runner.

Enforces the one rule that this machine's memory budget makes non-negotiable:
exactly one model resident at a time. 64 GB of unified memory cannot hold a 38 GB
Q8 MoE and an 18 GB dense model at once, and when it tries, the loser is whatever
else was mid-inference.
"""
import argparse
import atexit
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mh.bench import load_tasks
from mh.compute import Sampler, tok_s
from mh.harness import Config, Harness
from mh.model import (AccountRefused, Client, ModelError, model_spec,
                      reasoning_effort_for)
from mh.runtime import (STARVE_ABORT_DEFAULT, CellError, cell_rows,
                        ensure_sole_tenant, register_runner, release_runner,
                        episode_name, extension_guard, extension_reps,
                        is_api_model, is_starved_episode, keep_existing_episode,
                        protocol_block, read_episode, resume_conflicts,
                        should_retry_starved, sibling_overlap, unserved_count,
                        unstick_server, unload_all, write_episode, write_summary)

HERE = os.path.dirname(os.path.abspath(__file__))
# Overridable so tests can build a throwaway grid instead of writing
# into the real results tree.
RESULTS = os.environ.get("MH_RESULTS") or os.path.join(HERE, "results")
WORK = os.path.join(HERE, ".work")


# grid.py keys its whole-grid abort on this exit code; it is imported from
# here so the two cannot drift into disagreeing about what 3 means.
ACCOUNT_REFUSED_RC = 3

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
    unseeded run stays unseeded. Callers pass the cell's total repeat count,
    so an extension (`--rep-offset`) keeps the seed == rep index invariant
    the frozen cells were run under.
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
    # These four default to the model's own specification (mh.model.MODEL_SPECS),
    # which is the suite's historical 32768 / 4096 / 0.6 / 0.95 for every local
    # model and the vendor's documented values for a hosted one. Passing any of
    # them on the command line still wins; the resolved value is what the
    # episode's protocol stamp records.
    ap.add_argument("--num-ctx", type=int, default=None)
    ap.add_argument("--num-predict", type=int, default=None)
    ap.add_argument("--temperature", type=float, default=None)
    ap.add_argument("--top-p", type=float, default=None)
    ap.add_argument("--reasoning-effort", default=None,
                    help="explicit reasoning level for an API-served model "
                         "(e.g. low/medium/high on Cerebras). Default is the "
                         "model spec's mapping of --think/--no-think. Local "
                         "ollama models have no such knob and refuse it.")
    ap.add_argument("--seed", type=int, default=None)
    ap.add_argument("--repeat", type=int, default=1)
    ap.add_argument("--rep-offset", type=int, default=0,
                    help="start repeat indices at N instead of 0, to add seeds "
                         "to an already-run cell. Writes rep{N}.. and pins "
                         "seed == rep. Refuses any repeat this (model, config) "
                         "already holds under ANY tag, and refuses to land "
                         "beside another run's episodes; it may resume its own "
                         "interrupted output. Point it at a fresh --tag.")
    ap.add_argument("--tag", default="")
    ap.add_argument("--think", action=argparse.BooleanOptionalAction, default=True,
                    help="request reasoning channel from model (auto-falls back if unsupported)")
    ap.add_argument("--keep-resident", action="store_true",
                    help="skip the pre-run eviction (only when already sole tenant)")
    ap.add_argument("--share-gpu", action="store_true",
                    help="allow one peer model on this GPU (GH200 Qwen+Ornith)")
    ap.add_argument("--concurrency", type=int, default=1,
                    help="how many cells are deliberately in flight on this box, "
                         "declared so it can be stamped on every episode. A "
                         "contended GPU moves wall clock and --max-wall scores a "
                         "fail, so cells run at different concurrency are not the "
                         "same experiment and will not pool. Set by grid.py "
                         "--parallel; set it yourself if you launch runners by "
                         "hand.")
    ap.add_argument("--force", action="store_true",
                    help="re-run episodes even if {task}.rep{rep}.json exists")
    ap.add_argument("--force-starved", action="store_true",
                    help="keep real episodes and re-run only the first-turn "
                         "0-token artefacts, even when the kept episodes ran "
                         "under a different protocol. The cell's summary then "
                         "records protocol_variants instead of one protocol.")
    ap.add_argument("--starve-abort", type=int, default=STARVE_ABORT_DEFAULT,
                    help="abort the cell after this many consecutive 0-token timeouts "
                         "(0 disables). Prevents burning 40×30 min on a wedged GPU.")
    args = ap.parse_args()

    # Before makedirs, before eviction. `--repeat 0` used to get as far as
    # evicting the resident model and creating the cell directory, then die on
    # an IndexError in the banner.
    if args.repeat < 1:
        raise SystemExit(f"[runner] --repeat must be >= 1, got {args.repeat}")
    if args.rep_offset < 0:
        raise SystemExit(f"[runner] --rep-offset must be >= 0, got "
                         f"{args.rep_offset}")
    if args.concurrency < 1:
        raise SystemExit(f"[runner] --concurrency must be >= 1, got "
                         f"{args.concurrency}")
    if args.concurrency > 1 and args.share_gpu:
        # Two models resident AND several episodes in flight is uncontrolled
        # contention on both axes at once, and v1 already recorded what one
        # axis did: --share-gpu starved Qwen's first turn into a 0-token
        # timeout. Refused rather than stamped.
        raise SystemExit(
            "[runner] --share-gpu and --concurrency > 1 together put two "
            "models and N episodes on one GPU at once. Pick one: parallel "
            "cells of a single model, or two models run serially.")
    if args.starve_abort < 0:
        raise SystemExit(f"[runner] --starve-abort must be >= 0 (0 disables), "
                         f"got {args.starve_abort}")
    if args.max_wall < 0:
        raise SystemExit(f"[runner] --max-wall must be >= 0 (0 disables), got "
                         f"{args.max_wall}")
    if args.max_steps < 0:
        raise SystemExit(f"[runner] --max-steps must be >= 0 (0 disables), got "
                         f"{args.max_steps}")

    spec = model_spec(args.model)
    num_ctx = args.num_ctx if args.num_ctx is not None else spec["num_ctx"]
    num_predict = (args.num_predict if args.num_predict is not None
                   else spec["num_predict"])
    temperature = (args.temperature if args.temperature is not None
                   else spec["temperature"])
    top_p = args.top_p if args.top_p is not None else spec["top_p"]
    effort = (args.reasoning_effort if args.reasoning_effort is not None
              else reasoning_effort_for(args.model, args.think))
    if num_ctx < 1:
        raise SystemExit(f"[runner] --num-ctx must be >= 1, got {num_ctx}")
    if num_predict < 1:
        raise SystemExit(f"[runner] --num-predict must be >= 1, got {num_predict}")

    names = [t for t in args.tasks.split(",") if t.strip()] or None
    tasks = load_tasks(names)
    cfg = CONFIGS[args.config]()
    cfg.max_steps = args.max_steps
    cfg.wall_s = args.max_wall

    slug = args.model.replace("/", "_").replace(":", "_")
    tag = args.tag or time.strftime("%Y%m%d-%H%M%S")
    outdir = os.path.join(RESULTS, f"{slug}__{cfg.name}__{tag}")
    reps = extension_reps(args.rep_offset, args.repeat)
    task_names = [t.name for t in tasks]
    share_gpu = bool(args.share_gpu)
    # Stamped on every episode this run writes. Provenance travels WITH the
    # episode: that is what stops a later invocation from describing it.
    protocol = protocol_block(max_steps=args.max_steps, max_wall=args.max_wall,
                              share_gpu=share_gpu, num_ctx=num_ctx,
                              num_predict=num_predict,
                              temperature=temperature,
                              think=bool(args.think), model=args.model,
                              top_p=top_p, reasoning_effort=effort,
                              concurrency=args.concurrency)
    run_meta = {"tag": tag, "seed_base": args.seed, "repeat": args.repeat,
                "rep_offset": args.rep_offset,
                "started": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}

    refusal = extension_guard(outdir, task_names, reps, args.rep_offset,
                              args.force, protocol=protocol)
    if refusal:
        raise SystemExit(f"[runner] {refusal}")
    conflicts, unattributed = resume_conflicts(outdir, task_names, reps,
                                               protocol, force=args.force)
    if conflicts and not args.force_starved:
        lines = "\n".join(f"           {n}: differs on {', '.join(d)}"
                          for n, d in conflicts[:5])
        more = (f"\n           ... and {len(conflicts) - 5} more"
                if len(conflicts) > 5 else "")
        raise SystemExit(
            f"[runner] {len(conflicts)} episode(s) in {outdir} would be kept "
            f"but ran under a different protocol:\n{lines}{more}\n"
            f"           Re-run them with --force, keep them with "
            f"--force-starved (the cell then records no single protocol), or "
            f"write the new repeats under a distinct --tag.")
    if conflicts:
        print(f"[runner] --force-starved: keeping {len(conflicts)} episode(s) "
              f"that ran under a different protocol; this cell's summary will "
              f"record protocol_variants, not one protocol", flush=True)
    if unattributed:
        shown = ", ".join(unattributed[:3])
        print(f"[runner] {len(unattributed)} existing episode(s) record no "
              f"protocol ({shown}{' ...' if len(unattributed) > 3 else ''}); "
              f"they are kept, and this run's protocol will NOT be recorded "
              f"for them", flush=True)
    dup = sibling_overlap(outdir, task_names, reps)
    if dup:
        total = sum(len(v) for v in dup.values())
        where = ", ".join(os.path.basename(d) for d in sorted(dup))
        print(f"[runner] WARNING: {total} of the (task, rep) pairs this run "
              f"writes already exist for this (model, config) under {where}; "
              f"the two tags are duplicate samples and must not be pooled",
              flush=True)
    os.makedirs(outdir, exist_ok=True)

    # Announced before the tenancy check so a sibling starting at the same
    # instant sees this runner too, and released in the finally below however
    # this process exits.
    lease = register_runner(RESULTS, args.model)
    # atexit rather than a try/finally around the whole run: it covers the
    # SystemExit paths below as well as a normal return. A hard kill leaves the
    # file behind, which is why active_runners treats a lease whose pid is gone
    # as stale and deletes it instead of believing it.
    atexit.register(release_runner, lease)
    try:
        evicted = ensure_sole_tenant(args.model, evict=not args.keep_resident,
                                     share_gpu=args.share_gpu,
                                     results_root=RESULTS)
    except RuntimeError as e:
        release_runner(lease)
        raise SystemExit(f"[runner] {e}")
    if evicted:
        print(f"[runner] evicted {', '.join(evicted)} so {args.model} runs alone")
    elif args.share_gpu:
        print(f"[runner] share-gpu: {args.model} may coexist with one peer")

    try:
        client = Client(args.model, temperature=temperature, top_p=top_p,
                        top_k=spec["top_k"], num_ctx=num_ctx,
                        num_predict=num_predict, think=args.think,
                        seed=args.seed, reasoning_effort=effort,
                        timeout=args.max_wall if args.max_wall > 0 else 1800)
    except ModelError as e:
        raise SystemExit(f"[runner] {e}")
    # Whether a pinned seed actually reaches the model is a property of the
    # endpoint, not of the flag. An arm that cannot send one records None, so a
    # seed that never left this process is never indistinguishable, on disk,
    # from one the model actually sampled under.
    seeds_honoured = bool(getattr(client, "ACCEPTS_SEED", True))
    if not seeds_honoured and (args.seed is not None or args.repeat > 1):
        print(f"[runner] NOTE: {args.model} is served by an endpoint that does "
              f"not accept a seed; episodes record seed=null and repeats are "
              f"independent samples, not seeded replicates", flush=True)

    cap = "uncapped" if not args.max_steps else str(args.max_steps)
    written = kept = 0
    starve_streak = 0
    starved_abort = False
    print(f"[runner] model={args.model} config={cfg.name} tasks={len(tasks)} "
          f"repeat={args.repeat} reps={reps[0]}-{reps[-1]} "
          f"seed_base={args.seed} max_steps={cap} "
          f"max_wall={args.max_wall or 'off'} share_gpu={share_gpu}")
    print(f"[runner] spec num_ctx={num_ctx} num_predict={num_predict} "
          f"temperature={temperature} top_p={top_p} think={bool(args.think)} "
          f"reasoning_effort={effort or 'n/a'}")
    for rep in reps:
        if starved_abort:
            break
        pinned = seed_for_repeat(args.seed, rep, args.repeat + args.rep_offset)
        if pinned is not None:
            client.options["seed"] = pinned
        else:
            client.options.pop("seed", None)
        recorded_seed = pinned if seeds_honoured else None
        for task in tasks:
            if starved_abort:
                break
            ep_path = os.path.join(outdir, episode_name(task.name, rep))
            if os.path.isfile(ep_path) and not args.force:
                try:
                    prev = read_episode(ep_path, task.name, rep)
                    row = prev.get("row") or {}
                except CellError as e:
                    print(f"  {task.name:24s} re-running unreadable episode: {e}",
                          flush=True)
                    row = {}
                if keep_existing_episode(row, force=args.force):
                    kept += 1
                    mark = "PASS" if row.get("passed") else "fail"
                    print(f"  {task.name:24s} {mark}  skip existing "
                          f"{episode_name(task.name, rep)}  "
                          f"{row.get('stop_reason', '')}",
                          flush=True)
                    # A kept real episode breaks the run of consecutive
                    # 0-token timeouts. Leaving the streak standing let
                    # --starve-abort fire on starvation that was never
                    # consecutive.
                    if not is_starved_episode(row):
                        starve_streak = 0
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
                except AccountRefused as e:
                    # Every remaining episode would fail identically, and each
                    # would be written as a 0-token failure indistinguishable
                    # from the ablation working. Stop instead, writing nothing
                    # for this episode.
                    sampler.stop()
                    # SystemExit(str) exits 1 and would be indistinguishable
                    # from any other runner refusal; grid.py keys the
                    # whole-grid abort on this specific code.
                    print(f"[runner] ABORTING: the provider refused the "
                          f"account, not the request -- {e}\n"
                          f"           No episode was written for "
                          f"{task.name}.rep{rep}. Nothing after this point "
                          f"would be a measurement: fix the account, then "
                          f"re-run this tag. Episodes already on disk that "
                          f"died this way are first-turn failures and will be "
                          f"re-run automatically (no --force needed).",
                          flush=True)
                    raise SystemExit(ACCOUNT_REFUSED_RC)
                except Exception as e:
                    sampler.stop()
                    print(f"  {task.name:24s} RUNNER-ERROR {type(e).__name__}: {e}")
                    row = {"task": task.name, "rep": rep,
                           "seed": recorded_seed,
                           "passed": False, "steps": 0, "output_tokens": 0,
                           "stop_reason": f"runner_error:{type(e).__name__}",
                           "errors": [str(e)]}
                    break
                compute = sampler.stop()
                decode_tok_s = tok_s(res.output_tokens, res.eval_duration_ns)
                prompt_tok_s = tok_s(res.prompt_tokens, res.prompt_eval_duration_ns)
                row = {"task": task.name, "rep": rep, "seed": recorded_seed,
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
                    # An API-served model has no local server to unstick, and
                    # evicting a resident local model for a remote failure
                    # kills an unrelated cell. ensure_sole_tenant already makes
                    # this distinction; the retry path must make it too.
                    if is_api_model(args.model):
                        print(f"[runner] first-turn failure on {task.name} "
                              f"({args.model} is API-served); retrying without "
                              f"touching the local server", flush=True)
                        continue
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
            written += 1
            # share_gpu is the value in force for THIS episode: a cell demoted
            # mid-run really did run its earlier episodes with a peer resident.
            ep_protocol = (protocol if share_gpu == protocol["share_gpu"]
                           else dict(protocol, share_gpu=share_gpu))
            write_episode(outdir, task.name, rep,
                          {"model": args.model, "config": cfg.as_dict(),
                           "task": task.name, "row": row,
                           "protocol": ep_protocol, "run": run_meta,
                           "verify_output": verify_output,
                           "events": events})
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

    # Counted from the episodes on disk, so a cell poisoned by an earlier run
    # is caught even when this invocation wrote nothing. A pass rate over rows
    # the provider never served is not a pass rate.
    try:
        unserved = unserved_count(cell_rows(outdir))
    except CellError:
        unserved = 0
    outcomes = {
        "run": run_meta,
        "unserved": unserved,
        "share_gpu_requested": bool(args.share_gpu),
        "share_gpu_demoted": bool(args.share_gpu) and not share_gpu,
        "starved_abort": starved_abort,
        "force": bool(args.force),
        "force_starved": bool(args.force_starved),
        "episodes_written": written,
        "episodes_kept": kept,
        "sibling_overlap": {os.path.basename(d): len(v)
                            for d, v in sorted(dup.items())},
    }
    # Derived from the episodes on disk, never from `rows` this invocation
    # happens to hold: --tasks selects what to RUN, never what the cell
    # contains, and a summary rebuilt from one invocation's rows is how a cell
    # came to report n=1 over nine episodes.
    try:
        summary = write_summary(outdir, args.model, cfg.as_dict(),
                                protocol=protocol, outcomes=outcomes)
    except CellError as e:
        raise SystemExit(f"[runner] refusing to write summary.json: {e}")
    if unserved:
        print(f"[runner] WARNING: {unserved} of this cell's episodes were never "
              f"served -- the provider refused the account (401/402/403). They "
              f"are scored as failures by every reported rate, which is "
              f"indistinguishable from the model failing. This cell MUST NOT be "
              f"reported until they are re-run; they are first-turn failures, "
              f"so re-running this tag replaces them without --force.",
              flush=True)
    print(f"[runner] cell {summary['passed']}/{summary['n']} passed "
          f"({summary['pass_rate']}%)  total {summary['wall_s']}s  "
          f"(this run: {written} written, {kept} kept)  -> {outdir}")
    if summary["n"] and summary.get("protocol") is None:
        groups = summary.get("protocol_variants") or []
        print(f"[runner] NOTE: this cell has no single protocol; summary.json "
              f"records protocol_variants over {len(groups)} group(s). It "
              f"will not pool with another cell until that is resolved.")
    if starved_abort:
        raise SystemExit(2)


if __name__ == "__main__":
    main()
