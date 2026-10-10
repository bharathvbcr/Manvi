"""Interleaved min-of-N timing: inlined bootstrap draws vs the randrange oracle.

    python3 stats_bench.py [n_boot=10000] [repeats=7]

Prints one row per sample size. The two are bit-identical (test_stats.py), so
the ratio is pure speed.

Go/Rust kernel, evaluated and not built (2026-10-10): a native bootstrap needs
MT19937 plus CPython's init_by_array seeding and getrandbits(k) word-splicing to
reproduce the stream the committed paper/stats-*.json came from, with this
Python as the oracle. After the inlining a 10,000-resample bootstrap at n=8 costs
about 5 ms, the whole of test_stats.py about 2 s, and test_pool.py's wall time is
subprocess start-up (31 compare.py runs), which a kernel does not touch. A
kernel would add a build and FFI surface to save milliseconds, so the policy's
bar (parity test, committed benchmark, a hot path that is measured) is not met.
Revisit if the repeat counts or n_boot grow by an order of magnitude.

Not part of verify.sh: wall-clock ratios are evidence
to read, not a gate to flake on.
"""
import os
import random
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mh.stats import bootstrap_ci, interaction
from mh.stats_reference import ref_bootstrap_ci, ref_interaction


def best_of(fn, repeats):
    best = float("inf")
    for _ in range(repeats):
        t0 = time.perf_counter()
        fn()
        best = min(best, time.perf_counter() - t0)
    return best


def main():
    n_boot = int(sys.argv[1]) if len(sys.argv) > 1 else 10000
    repeats = int(sys.argv[2]) if len(sys.argv) > 2 else 7
    gen = random.Random(1)
    print(f"python {sys.version.split()[0]}  n_boot={n_boot}  min of {repeats}")
    print(f"{'fn':<12}{'n':>4}{'old s':>10}{'new s':>10}{'speedup':>9}")
    for n in (5, 8, 40):
        xs = [gen.random() for _ in range(n)]
        ys = [gen.random() - 0.5 for _ in range(n)]
        cases = (
            ("bootstrap_ci",
             lambda: ref_bootstrap_ci(xs, n_boot, 0.05, 0, None),
             lambda: bootstrap_ci(xs, n_boot=n_boot, rng_seed=0)),
            ("interaction",
             lambda: ref_interaction(xs, ys, n_boot, 0, True, None),
             lambda: interaction(xs, ys, n_boot=n_boot, rng_seed=0, paired=True)),
        )
        for name, old, new in cases:
            olds, news = [], []
            for _ in range(repeats):          # interleaved A/B
                olds.append(best_of(old, 1))
                news.append(best_of(new, 1))
            o, w = min(olds), min(news)
            print(f"{name:<12}{n:>4}{o:>10.4f}{w:>10.4f}{o / w:>8.2f}x")


if __name__ == "__main__":
    main()
