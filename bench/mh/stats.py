"""Bootstrap CIs and ablation deltas. Stdlib only.

Every (model, config) cell is a list of per-repeat pass rates. The headline
test is whether the ablation delta shrinks as model capability rises
(Δ_weak > Δ_strong). n=5 is small; the interval is the claim, not the point.

Two things a reader has to be told rather than left to assume, and which the
report therefore carries as numbers rather than as prose:

  * a percentile bootstrap on five points does not deliver its nominal 95%
    (`bootstrap_coverage`), and
  * a ladder of sixteen such intervals excludes zero somewhere by chance far
    more often than 5% of the time (`family_wise_error`).
"""
import math
import random

from .runtime import is_starved_episode


def usable_rows(rows):
    """Drop first-turn 0-token timeouts. Those are serving failures, not tasks."""
    return [r for r in (rows or []) if not is_starved_episode(r)]


def mean(xs):
    xs = list(xs)
    return sum(xs) / len(xs) if xs else float("nan")


def _weights_for(xs, weights):
    """Validate and materialise bootstrap weights. None means equal weight.

    Equal weight is `1.0` per sample rather than a separate code path: `1.0 * x`
    is exactly `x` and a sum of n ones is exactly n, so the weighted estimator
    reduces to the unweighted one bit-for-bit. There is one estimator here, not
    two that have to be kept in agreement.
    """
    n = len(xs)
    if weights is None:
        return [1.0] * n
    ws = [float(w) for w in weights]
    if len(ws) != n:
        raise ValueError(f"weights has {len(ws)} entries for {n} samples")
    for w in ws:
        if not math.isfinite(w) or w < 0.0:
            raise ValueError(f"weight {w!r} is not a finite, non-negative count")
    if sum(ws) <= 0.0:
        raise ValueError("weights sum to zero; the weighted mean is undefined")
    return ws


def weighted_mean(xs, weights=None):
    """Σ w·x / Σ w. With weights=None this is exactly `mean(xs)`."""
    xs = [float(x) for x in xs]
    if not xs:
        return float("nan")
    ws = _weights_for(xs, weights)
    num = 0.0
    den = 0.0
    for w, x in zip(ws, xs):
        num += w * x
        den += w
    return num / den


def bootstrap_ci(xs, n_boot=10000, alpha=0.05, rng_seed=0, weights=None):
    """Percentile bootstrap CI for the (optionally weighted) mean.

    Returns (mean, lo, hi). A single sample has a degenerate interval, and so
    does a sample with no observed variance -- ask `ci_degeneracy` before
    reporting the width of one of these as uncertainty.

    `weights` are the per-sample denominators (episodes behind each repeat's
    pass rate). Without them a repeat holding one task counts as much as a
    repeat holding eight. Passing None weights every sample 1.0, which is the
    unweighted estimator exactly.

    The interval this returns is labelled 95% by construction, not by
    measurement: see `bootstrap_coverage` for what it actually delivers at
    n=5.
    """
    xs = [float(x) for x in xs]
    n = len(xs)
    if n == 0:
        return (float("nan"), float("nan"), float("nan"))
    ws = _weights_for(xs, weights)
    m = weighted_mean(xs, ws)
    if n == 1:
        return (m, m, m)
    rng = random.Random(rng_seed)
    boots = [0.0] * n_boot
    for b in range(n_boot):
        num = 0.0
        den = 0.0
        for _ in range(n):
            i = rng.randrange(n)
            num += ws[i] * xs[i]
            den += ws[i]
        boots[b] = num / den if den else float("nan")
    boots.sort()
    lo_i = int((alpha / 2.0) * n_boot)
    hi_i = int((1.0 - alpha / 2.0) * n_boot)
    if hi_i >= n_boot:
        hi_i = n_boot - 1
    return (m, boots[lo_i], boots[hi_i])


def ci_degeneracy(xs):
    """Why this sample's interval carries no information, or None.

    A saturated cell (five repeats all 1.0) resamples to 1.0 every time and
    yields a zero-width interval. Plotted as an error bar it asserts certainty
    exactly where the estimator ran out of signal, and it is indistinguishable
    from a genuinely tight one. n=1 renders identically and is not even a
    resample. Both have to be named, not drawn.
    """
    xs = [float(x) for x in xs]
    n = len(xs)
    if n == 0:
        return "no repeats: there is no interval"
    if n == 1:
        return ("single repeat: lo and hi are the point estimate, not a "
                "bootstrap interval")
    if len({x for x in xs if x == x}) == 1:
        return (f"no observed variance across {n} repeats (every repeat "
                f"{xs[0]:g}): the interval is zero-width because the "
                f"estimator has no spread to resample, not because the "
                f"estimate is certain")
    return None


def pass_counts_by_repeat(rows):
    """Map rep -> (passed, scored) for that repeat. Missing reps are omitted.

    The denominator is half the measurement. A repeat that scored one task and
    a repeat that scored eight both produce a rate in [0, 1], and averaging
    those rates weights them equally; `pass_rates_by_repeat` alone cannot tell
    a caller that happened.

    Starved 0-token timeouts are excluded so a wedged server cannot look like
    a harness ablation.
    """
    by = {}
    for r in usable_rows(rows):
        rep = r.get("rep", 0)
        rec = by.setdefault(rep, [0, 0])
        rec[1] += 1
        if r.get("passed"):
            rec[0] += 1
    return {rep: (p, n) for rep, (p, n) in sorted(by.items())}


def rates_of(counts_by_rep):
    """rep -> passed/scored, from `pass_counts_by_repeat` output.

    The one place the rate is divided, so a caller that already has the counts
    does not re-derive it and drift.
    """
    return {rep: (p / n if n else 0.0) for rep, (p, n) in counts_by_rep.items()}


def pass_rates_by_repeat(rows):
    """Map rep -> passed/n_tasks for that repeat. Missing reps are omitted.

    Rates only. Callers that intend to average or bootstrap these need the
    denominators too -- see `pass_counts_by_repeat` and the `weights` argument
    to `bootstrap_ci`.
    """
    return rates_of(pass_counts_by_repeat(rows))


def denominators_of(counts_by_rep):
    """rep -> scored-episode count, from `pass_counts_by_repeat` output.

    Scored, not recorded: starved 0-token timeouts are already gone. For what
    a directory physically holds, see `mh.pool.rep_denominators`.
    """
    return {rep: n for rep, (_p, n) in counts_by_rep.items()}


def aligned_deltas(full_by_rep, abl_by_rep):
    """Paired Δ = full - ablation on the intersection of repeat indices.

    Returns (deltas, keys). `keys` is not decoration: a contrast that paired 2
    of 5 repeats and one that paired all 5 produce lists of different length
    and nothing else distinguishes them. Callers that drop `keys` are asserting
    a pairing they did not check.
    """
    keys = sorted(set(full_by_rep) & set(abl_by_rep))
    return [full_by_rep[k] - abl_by_rep[k] for k in keys], keys


def deltas_by_repeat(full_by_rep, abl_by_rep):
    """rep -> paired Δ, keyed so a second contrast can be aligned against it."""
    deltas, keys = aligned_deltas(full_by_rep, abl_by_rep)
    return dict(zip(keys, deltas))


def aligned_interaction(weak_deltas_by_rep, strong_deltas_by_rep):
    """Align two models' per-repeat deltas on the repeats both actually hold.

    Returns (weak, strong, reps, dropped). Repeat r pins seed r in both arms,
    so pairing by repeat index is pairing by seed -- that is the whole reason
    the interaction can be computed paired. `dropped` names repeats present in
    one arm only; they cannot enter a paired statistic and their absence is
    otherwise invisible in the result.
    """
    reps = sorted(set(weak_deltas_by_rep) & set(strong_deltas_by_rep))
    dropped = sorted(set(weak_deltas_by_rep) ^ set(strong_deltas_by_rep))
    return ([weak_deltas_by_rep[r] for r in reps],
            [strong_deltas_by_rep[r] for r in reps],
            reps, dropped)


def interaction(delta_weak, delta_strong, n_boot=10000, rng_seed=0,
                paired=False, weights=None):
    """CI on (mean Δ_weak - mean Δ_strong). Positive supports the claim.

    Also reports whether the 95% interval lies entirely above 0 (detectable)
    or includes 0 (underpowered / not detected at this n).

    `paired` selects the resampling scheme, and it changes the answer:

      paired=False draws each arm's bootstrap indices from its own stream, as
      if the two arms were independent samples. That is what the published
      `interaction` block in paper/stats-hard.json was computed with, and it
      is why that block is still produced this way -- the frozen file has to
      keep reproducing.

      paired=True draws ONE index per resample and applies it to both arms.
      That is the scheme the protocol describes: repeat r pins seed r in both
      arms, so Δ_weak[r] and Δ_strong[r] share a seed and their difference is
      a paired difference. When the arms correlate the paired interval is the
      narrower and the correct one; the unpaired interval inflates it by the
      shared variance it refuses to cancel.

    The paper cites the unpaired block. `compare.py` reports both, under
    `interaction` (unpaired, frozen) and `interaction_paired`.

    paired=True requires the two arms to be index-aligned by repeat; see
    `aligned_interaction`.

    `weights` are the per-repeat episode counts behind each delta, shared by
    both arms because a paired contrast scores the same tasks in both. None
    weights every repeat 1.0, which is the unweighted estimator exactly.
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
    nw, ns = len(dw), len(ds)
    if paired and nw != ns:
        raise ValueError(
            f"paired interaction needs index-aligned arms, got {nw} weak and "
            f"{ns} strong deltas; align them with aligned_interaction() first")
    if weights is not None and not paired:
        raise ValueError("weights only apply to the paired scheme; the two "
                         "arms of an unpaired resample have no shared repeat "
                         "to weight")
    ww = _weights_for(dw, weights)
    mw, sw = weighted_mean(dw, ww), weighted_mean(ds, ww if paired else None)
    diff = mw - sw
    n_boot = int(n_boot)
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
    lo = boots[int(0.025 * n_boot)]
    hi = boots[min(int(0.975 * n_boot), n_boot - 1)]
    return {
        "delta_weak_minus_strong": diff,
        "lo": lo,
        "hi": hi,
        "detectable": lo > 0,
        "underpowered": not (lo > 0 or hi < 0),
    }


def pearson_r(xs, ys):
    """Correlation between two index-aligned samples, or nan.

    Reported alongside the paired interaction because it is the number that
    says how much the unpaired scheme was throwing away: at r near 1 the
    unpaired interval is roughly sqrt(2/(1-r)) times too wide.
    """
    xs = [float(x) for x in xs]
    ys = [float(y) for y in ys]
    n = len(xs)
    if n < 2 or len(ys) != n:
        return float("nan")
    mx, my = mean(xs), mean(ys)
    sxy = sum((x - mx) * (y - my) for x, y in zip(xs, ys))
    sxx = sum((x - mx) ** 2 for x in xs)
    syy = sum((y - my) ** 2 for y in ys)
    if sxx <= 0.0 or syy <= 0.0:
        return float("nan")
    return sxy / math.sqrt(sxx * syy)


def bootstrap_coverage(n_repeats=5, n_tasks=8, p=0.65, trials=5000,
                       n_boot=10000, alpha=0.05, rng_seed=20260824):
    """Measured coverage of `bootstrap_ci` at a given cell shape.

    A "95% CI" from five points is 95% by construction, not by measurement.
    This runs the actual estimator against a known truth: each trial draws
    `n_repeats` per-repeat pass rates as Binomial(`n_tasks`, `p`)/`n_tasks`
    and asks whether the percentile interval contains `p`. At the grid's own
    shape (5 repeats x 8 Bernoulli tasks) the answer is nowhere near 95%, and
    a reader who is not told that will read the interval as one.

    Returns a dict carrying the settings, the measured coverage, the Monte
    Carlo standard error on it (the measurement has a CI too), and the share
    of trials whose interval came out zero-width -- the failure `ci_degeneracy`
    describes, counted rather than asserted.

    Deterministic in `rng_seed`. Intervals are memoised on the sorted sample,
    which for this shape collapses thousands of trials onto a few hundred
    distinct bootstraps.
    """
    if trials <= 0:
        raise ValueError("trials must be positive")
    rng = random.Random(rng_seed)
    cache = {}
    covered = 0
    degenerate = 0
    for _ in range(trials):
        sample = tuple(sorted(
            sum(1 for _ in range(n_tasks) if rng.random() < p) / n_tasks
            for _ in range(n_repeats)))
        got = cache.get(sample)
        if got is None:
            _m, lo, hi = bootstrap_ci(list(sample), n_boot=n_boot, alpha=alpha)
            got = (lo, hi, hi <= lo)
            cache[sample] = got
        lo, hi, flat = got
        if lo <= p <= hi:
            covered += 1
        if flat:
            degenerate += 1
    measured = covered / trials
    return {
        "nominal": 1.0 - alpha,
        "measured": measured,
        "mc_stderr": math.sqrt(max(measured * (1.0 - measured), 0.0) / trials),
        "zero_width_fraction": degenerate / trials,
        "trials": trials,
        "n_repeats": n_repeats,
        "n_tasks": n_tasks,
        "p": p,
        "n_boot": n_boot,
        "rng_seed": rng_seed,
        "distinct_samples": len(cache),
        "note": ("Monte-Carlo coverage of the percentile bootstrap against a "
                 "known Binomial truth at this grid's cell shape. Measured "
                 "well below nominal is the known small-n deficiency of the "
                 "percentile method, not an implementation fault -- but an "
                 "interval reported as 95% that covers less than that is a "
                 "reader-facing error either way."),
    }


def family_wise_error(n_intervals, per_interval_coverage=0.95):
    """P(at least one of `n_intervals` excludes the truth), under a global null.

    Sixteen ladder intervals at a genuine 95% each already exclude zero
    somewhere 56% of the time when nothing is going on; at the coverage those
    intervals actually deliver it is higher still. "The only interval
    excluding zero" is therefore not evidence on its own, and this is the
    number that says so.

    Assumes independence across intervals. The ladder's intervals share arms
    (every delta contains the same `full` cell), so this is an approximation
    rather than an exact family-wise rate -- but the dependence is positive,
    which makes the true rate lower than this bound, not higher than 5%.
    """
    n = int(n_intervals)
    if n <= 0:
        return 0.0
    c = float(per_interval_coverage)
    if not 0.0 <= c <= 1.0:
        raise ValueError(f"per-interval coverage {c!r} is not a probability")
    return 1.0 - c ** n


def sidak_alpha(n_intervals, fwer=0.05):
    """Per-interval alpha giving `fwer` across `n_intervals` (Šidák)."""
    n = int(n_intervals)
    if n <= 0:
        return float(fwer)
    return 1.0 - (1.0 - float(fwer)) ** (1.0 / n)


def multiplicity_report(n_intervals, n_excluding_zero,
                        measured_coverage=None, nominal_coverage=0.95,
                        fwer_target=0.05):
    """The family-wise numbers for a ladder of `n_intervals` intervals."""
    out = {
        "n_intervals": int(n_intervals),
        "n_excluding_zero": int(n_excluding_zero),
        "nominal_per_interval_coverage": float(nominal_coverage),
        "fwer_at_nominal_coverage": family_wise_error(n_intervals,
                                                      nominal_coverage),
        "sidak_alpha_for_fwer": sidak_alpha(n_intervals, fwer_target),
        "fwer_target": float(fwer_target),
        "correction_applied": False,
        "note": ("No multiplicity correction is applied to the reported "
                 "intervals; these are the numbers needed to read them. With "
                 "this many intervals, one excluding zero is the expected "
                 "outcome under a global null, not a finding."),
    }
    if measured_coverage is not None:
        out["measured_per_interval_coverage"] = float(measured_coverage)
        out["fwer_at_measured_coverage"] = family_wise_error(
            n_intervals, measured_coverage)
    return out


def role_of(model):
    """Name-based label only. Capability for the interaction test is empirical."""
    m = (model or "").lower()
    # Every rule below was written for a specific local build. None of them was
    # written for a hosted model, and applying them by substring gets it wrong
    # in the direction that matters: "gemma" was the 4B-class local e4b build,
    # so it would label Cerebras' 31B dense Gemma 4 "weak" in a published table
    # before a single episode has been run against it. An unmeasured arm is
    # unknown; capability_arms_detail assigns the real roles from measured
    # full-harness means anyway.
    if m.startswith("cerebras:"):
        return "unknown"
    if "gemini" in m:
        return "strong"
    if "gemma" in m:
        return "weak"
    if "qwen" in m or "ornith" in m:
        return "mid"
    return "unknown"


def capability_arms_detail(full_means):
    """Arms by empirical full-harness mean, with the gap and what was dropped.

    Parameter count is not a capability axis. Returns None unless two or more
    models have a finite full-harness mean -- `float("inf")` is not a
    capability either, and is dropped rather than crowned the stronger arm.

    `tied` is the flag `capability_arms` cannot carry: two arms of identical
    measured capability still produce a (weaker, stronger) pair, and an
    interaction computed across a zero-capability gap tests nothing. Ties are
    broken by model name so the pair is stable, which makes a tie look exactly
    like a real ordering unless it is named.
    """
    scored, ignored = [], []
    for model, mu in full_means.items():
        try:
            x = float(mu)
        except (TypeError, ValueError):
            ignored.append((model, f"{mu!r} is not a number"))
            continue
        if not math.isfinite(x):
            ignored.append((model, f"{mu!r} is not finite"))
            continue
        scored.append((x, model))
    if len(scored) < 2:
        return None
    scored.sort(key=lambda t: (t[0], t[1]))
    (lo_mu, lo_m), (hi_mu, hi_m) = scored[0], scored[-1]
    gap = hi_mu - lo_mu
    return {
        "weaker": lo_m,
        "stronger": hi_m,
        "weaker_full_mean": lo_mu,
        "stronger_full_mean": hi_mu,
        "capability_gap": gap,
        "tied": gap <= 0.0,
        "n_models_ranked": len(scored),
        "ignored": [f"{m}: {why}" for m, why in sorted(ignored)],
    }


def capability_arms(full_means):
    """(weaker, stronger) by empirical full-harness mean pass rate.

    Thin accessor over `capability_arms_detail`; use that one if you need to
    know whether the two arms differ at all.
    """
    d = capability_arms_detail(full_means)
    return None if d is None else (d["weaker"], d["stronger"])
