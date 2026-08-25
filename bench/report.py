"""Build the head-to-head report from results/, including paired per-task deltas."""
import json, glob, os, sys, statistics

HERE = os.path.dirname(os.path.abspath(__file__))
R = os.environ.get("MH_RESULTS") or os.path.join(HERE, "results")


def label(model):
    """Short display name. Must distinguish serving stacks, not just families:
    qwen3.8:27b and qwen3.8:27b-mlx are different arms, and folding them into
    one label silently averages CUDA and Metal runs."""
    m = model.split("/")[-1]
    return (m.replace("-GGUF:Q8_0", "-Q8").replace(":27b-mlx", "-27B-mlx")
             .replace(":27b", "-27B").replace(":e4b", "-E4B"))


def load(tag):
    runs = {}
    for d in sorted(os.listdir(R)):
        p = os.path.join(R, d, "summary.json")
        if not os.path.isfile(p) or not d.endswith("__" + tag):
            continue
        s = json.load(open(p))
        key = (label(s["model"]), s["config"]["name"])
        if key in runs:
            raise SystemExit(
                f"[report] {key[0]} [{key[1]}] appears in two directories for "
                f"tag {tag!r}: {runs[key]['dir']} and {d}. Reporting one of "
                f"them silently would hide the other.")
        s["dir"] = d
        runs[key] = s
    return runs


def rows_by_task(s):
    return {r["task"]: r for r in s["rows"]}


def main():
    tags = sys.argv[1:] or ["v2", "v1"]
    runs = {}
    for t in tags:
        for k, v in load(t).items():
            runs.setdefault(k, v)
    if not runs:
        print("no results"); return

    models = sorted({m for m, _ in runs})
    tasks = sorted({r["task"] for s in runs.values() for r in s["rows"]})

    print("## Pass rate\n")
    hdr = f"| task | " + " | ".join(f"{m} {c}" for m, c in sorted(runs)) + " |"
    print(hdr)
    print("|" + "---|" * (len(runs) + 1))
    for t in tasks:
        cells = []
        for k in sorted(runs):
            r = rows_by_task(runs[k]).get(t)
            cells.append("—" if not r else ("**PASS**" if r["passed"] else "fail"))
        print(f"| `{t}` | " + " | ".join(cells) + " |")
    cells = []
    for k in sorted(runs):
        s = runs[k]
        cells.append(f"**{s['passed']}/{s['n']}**")
    print("| **total** | " + " | ".join(cells) + " |")

    print("\n## Harness effect, paired per task\n")
    for m in models:
        full, base = runs.get((m, "full")), runs.get((m, "baseline"))
        if not (full and base):
            continue
        F, B = rows_by_task(full), rows_by_task(base)
        common = [t for t in tasks if t in F and t in B]
        print(f"\n### {m}\n")
        print("| task | full steps | baseline steps | full s | baseline s | speedup |")
        print("|---|---|---|---|---|---|")
        sp = []
        for t in common:
            f, b = F[t], B[t]
            r = b["wall_s"] / f["wall_s"] if f["wall_s"] else float("nan")
            sp.append(r)
            print(f"| `{t}` | {f['steps']} | {b['steps']} | {f['wall_s']:.0f} | "
                  f"{b['wall_s']:.0f} | {r:.2f}x |")
        fs = sum(F[t]["steps"] for t in common)
        bs = sum(B[t]["steps"] for t in common)
        fw = sum(F[t]["wall_s"] for t in common)
        bw = sum(B[t]["wall_s"] for t in common)
        ft = sum(F[t]["output_tokens"] for t in common)
        bt = sum(B[t]["output_tokens"] for t in common)
        print(f"| **total** | **{fs}** | **{bs}** | **{fw:.0f}** | **{bw:.0f}** | "
              f"**{bw/fw:.2f}x** |")
        print(f"\n- pass rate: full **{full['passed']}/{full['n']}** vs baseline "
              f"**{base['passed']}/{base['n']}**")
        print(f"- steps: {fs} vs {bs} ({100*(bs-fs)/bs:.0f}% fewer with the harness)")
        print(f"- output tokens: {ft:,} vs {bt:,} ({bt/ft:.2f}x)")
        print(f"- median per-task speedup: {statistics.median(sp):.2f}x")

    print("\n## Model comparison, same harness\n")
    for cfg in ("full", "baseline"):
        have = [(m, runs[(m, cfg)]) for m in models if (m, cfg) in runs]
        if len(have) < 2:
            continue
        print(f"\n**{cfg} harness**\n")
        print("| metric | " + " | ".join(m for m, _ in have) + " |")
        print("|" + "---|" * (len(have) + 1))
        for label, fn in (("passed", lambda s: f"{s['passed']}/{s['n']}"),
                          ("wall clock", lambda s: f"{s['wall_s']:.0f}s"),
                          ("output tokens", lambda s: f"{s['output_tokens']:,}"),
                          ("steps", lambda s: str(sum(r['steps'] for r in s['rows'])))):
            print(f"| {label} | " + " | ".join(fn(s) for _, s in have) + " |")


main()
