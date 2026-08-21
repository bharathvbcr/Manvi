"""Bootstrap CIs and ablation deltas. Stdlib only.

Every (model, config) cell is a list of per-repeat pass rates. The headline
test is whether the ablation delta shrinks as model capability rises
(Δ_weak > Δ_strong). n=5 is small; the interval is the claim, not the point.
"""
import random


def mean(xs):
    xs = list(xs)
    return sum(xs) / len(xs) if xs else float("nan")


def bootstrap_ci(xs, n_boot=10000, alpha=0.05, rng_seed=0):
    """Percentile bootstrap CI for the mean.

    Returns (mean, lo, hi). A single sample has a degenerate interval.
    """
    xs = [float(x) for x in xs]
    n = len(xs)
    if n == 0:
        return (float("nan"), float("nan"), float("nan"))
    m = mean(xs)
    if n == 1:
        return (m, m, m)
    rng = random.Random(rng_seed)
    boots = [0.0] * n_boot
    for b in range(n_boot):
        s = 0.0
        for _ in range(n):
            s += xs[rng.randrange(n)]
        boots[b] = s / n
    boots.sort()
    lo_i = int((alpha / 2.0) * n_boot)
    hi_i = int((1.0 - alpha / 2.0) * n_boot)
    if hi_i >= n_boot:
        hi_i = n_boot - 1
    return (m, boots[lo_i], boots[hi_i])


def pass_rates_by_repeat(rows):
    """Map rep -> passed/n_tasks for that repeat. Missing reps are omitted."""
    by = {}
    for r in rows:
        rep = r.get("rep", 0)
        rec = by.setdefault(rep, [0, 0])
        rec[1] += 1
        if r.get("passed"):
            rec[0] += 1
    return {rep: (p / n if n else 0.0) for rep, (p, n) in sorted(by.items())}


def aligned_deltas(full_by_rep, abl_by_rep):
    """Paired Δ = full - ablation on the intersection of repeat indices."""
    keys = sorted(set(full_by_rep) & set(abl_by_rep))
    return [full_by_rep[k] - abl_by_rep[k] for k in keys], keys


def interaction(delta_weak, delta_strong, n_boot=10000, rng_seed=0):
    """CI on (mean Δ_weak - mean Δ_strong). Positive supports the claim.

    Also reports whether the 95% interval lies entirely above 0 (detectable)
    or includes 0 (underpowered / not detected at this n).
    """
    rng = random.Random(rng_seed)
    dw = [float(x) for x in delta_weak]
    ds = [float(x) for x in delta_strong]
    if not dw or not ds:
        return {
            "delta_weak_minus_strong": float("nan"),
            "lo": float("nan"),
            "hi": float("nan"),
            "detectable": False,
            "underpowered": True,
        }
    mw, sw = mean(dw), mean(ds)
    diff = mw - sw
    n_boot = int(n_boot)
    boots = []
    nw, ns = len(dw), len(ds)
    for _ in range(n_boot):
        mw_b = sum(dw[rng.randrange(nw)] for _ in range(nw)) / nw
        ms_b = sum(ds[rng.randrange(ns)] for _ in range(ns)) / ns
        boots.append(mw_b - ms_b)
    boots.sort()
    lo = boots[int(0.025 * n_boot)]
    hi = boots[min(int(0.975 * n_boot), n_boot - 1)]
    return {
        "delta_weak_minus_strong": diff,
        "lo": lo,
        "hi": hi,
        "detectable": lo > 0,
        "underpowered": not (lo > 0 or hi < 0),
    }


def role_of(model):
    """Name-based label only. Capability for the interaction test is empirical."""
    m = (model or "").lower()
    if "gemini" in m:
        return "strong"
    if "gemma" in m:
        return "weak"
    if "qwen" in m or "ornith" in m:
        return "mid"
    return "unknown"


def capability_arms(full_means):
    """(weaker, stronger) by empirical full-harness mean pass rate.

    Parameter count is not a capability axis. Returns None unless two or more
    models have a finite full-harness mean. Ties are broken by model name so
    the pair is stable.
    """
    scored = []
    for model, mu in full_means.items():
        try:
            x = float(mu)
        except (TypeError, ValueError):
            continue
        if x != x:  # nan
            continue
        scored.append((x, model))
    if len(scored) < 2:
        return None
    scored.sort(key=lambda t: (t[0], t[1]))
    return scored[0][1], scored[-1][1]
