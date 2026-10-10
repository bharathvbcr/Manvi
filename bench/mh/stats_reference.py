"""The bootstrap as it was before the draws were inlined: `rng.randrange(n)`.

This is the reference oracle for `mh.stats.bootstrap_ci` / `interaction`. The
committed paper/stats-*.json come from this exact RNG stream, so the fast
versions must reproduce it bit for bit; test_stats.py asserts that over a grid
and stats_bench.py times the two against each other. Do not optimise this file.
"""
import random

from .stats import _weights_for, weighted_mean


def ref_bootstrap_ci(xs, n_boot, alpha, rng_seed, weights):
    """bootstrap_ci as it was before the draw was inlined: rng.randrange(n)."""
    n = len(xs)
    ws = _weights_for(xs, weights)
    m = weighted_mean(xs, ws)
    if n == 1:
        return (m, m, m)
    rng = random.Random(rng_seed)
    boots = [0.0] * n_boot
    for b in range(n_boot):
        num = den = 0.0
        for _ in range(n):
            i = rng.randrange(n)
            num += ws[i] * xs[i]
            den += ws[i]
        boots[b] = num / den if den else float("nan")
    boots.sort()
    lo_i = int((alpha / 2.0) * n_boot)
    hi_i = min(int((1.0 - alpha / 2.0) * n_boot), n_boot - 1)
    return (m, boots[lo_i], boots[hi_i])


def ref_interaction(dw, ds, n_boot, rng_seed, paired, weights):
    rng = random.Random(rng_seed)
    nw, ns = len(dw), len(ds)
    ww = _weights_for(dw, weights)
    diff = weighted_mean(dw, ww) - weighted_mean(ds, ww if paired else None)
    boots = []
    for _ in range(n_boot):
        if paired:
            num_w = num_s = den = 0.0
            for _ in range(nw):
                i = rng.randrange(nw)
                num_w += ww[i] * dw[i]
                num_s += ww[i] * ds[i]
                den += ww[i]
            mw_b, ms_b = num_w / den, num_s / den
        else:
            mw_b = sum(dw[rng.randrange(nw)] for _ in range(nw)) / nw
            ms_b = sum(ds[rng.randrange(ns)] for _ in range(ns)) / ns
        boots.append(mw_b - ms_b)
    boots.sort()
    return (diff, boots[int(0.025 * n_boot)],
            boots[min(int(0.975 * n_boot), n_boot - 1)])
