"""Cell assembly guards.

A cell is (model, config) and may draw rows from more than one results
directory -- that is how extra seeds get added to an already-run cell. The
merge is only sound when the sources agree on protocol and config, cover
disjoint repeat indices, and score the same task set.

Merging silently is how a protocol change becomes a headline. compare.py
merged any two directories sharing a (model, config) key, so a grid and a
re-run under a different turn ceiling landed in one cell, and two rows for
the same repeat index were averaged inside that repeat's bucket. Every merge
here is either checked or refused.
"""

# Fields that change what the model does, or whether an episode survives its
# wall clock. Two sources disagreeing on any of them are not the same
# experiment. share_gpu is included: a contended GPU moves wall time, and a
# wall timeout scores fail.
# Fields that change what the model does, or whether an episode survives its
# wall clock. env_* identify the machine actually serving the model: a Metal
# run and a CUDA run are not the same experiment, and an absent value is
# treated as drift rather than as agreement.
# env_api_provider / env_api_endpoint identify the serving side of an API arm,
# the way env_gpu / env_ollama_version identify it for a local one. env_client_node
# is deliberately absent: on an API arm the box that ran the loop is provenance,
# not protocol, and making it a key would refuse to pool two identical remote
# cells that happened to be launched from different laptops. That omission is
# the same fact as mh.runtime.PROVENANCE_KEYS, and the two must agree: this is
# an allowlist and that is a denylist over the same stamp, so a key treated as
# provenance here and as protocol there makes the two disagree about whether
# one cell is one experiment. Measured once, on a cell resumed from a second
# laptop: mh.pool said the episodes pooled, protocol_diff said they did not.
PROTOCOL_KEYS = ("max_steps", "max_wall", "num_ctx", "num_predict",
                 "temperature", "top_p", "think", "reasoning_effort",
                 "share_gpu", "concurrency",
                 "env_node", "env_gpu", "env_platform", "env_ollama_version",
                 "env_api_provider", "env_api_endpoint")

# The ablation flags themselves, mirroring harness.Config.FLAGS. A config name
# is a label; these are what it is supposed to mean. Runtime settings that also
# ride in the config dict (max_steps, wall_s) are protocol, checked above, and
# are deliberately not duplicated here.
CONFIG_FLAGS = ("envboot", "nativetools", "outcap", "checklist",
                "verifygate", "loopbreak", "groundfs")


def reps_of(rows):
    """Repeat indices present in these rows."""
    return sorted({r.get("rep", 0) for r in (rows or [])})


def tasks_of(rows):
    """Task names present in these rows."""
    return sorted({r.get("task") for r in (rows or []) if r.get("task")})


def protocol_drift(a, b):
    """Protocol keys on which two sources disagree.

    A key present in one block and absent from the other counts as drift:
    an unrecorded setting is not evidence of a matching one.
    """
    if not a or not b:
        # An absent block is not evidence of a matching one. Two sources that
        # both predate protocol recording are the case this module exists for:
        # the Metal and CUDA archives are indistinguishable without it.
        return list(PROTOCOL_KEYS)
    return [k for k in PROTOCOL_KEYS if a.get(k) != b.get(k)]


def config_drift(a, b):
    """Ablation flags on which two configs sharing a name disagree."""
    a, b = a or {}, b or {}
    return [k for k in CONFIG_FLAGS if a.get(k) != b.get(k)]


def rep_denominators(rows):
    """rep -> number of episodes recorded in that repeat.

    A per-repeat pass rate is passed/n. Repeats with different n are not
    exchangeable samples, and a repeat holding one episode contributes a
    rate of 0.0 or 1.0 with the same weight as a repeat holding eight --
    which is why `mh.stats.bootstrap_ci` takes these as weights.

    This counts every recorded row, because it exists to describe what a
    directory holds. The denominator a *rate* is computed over is
    `mh.stats.denominators_of(mh.stats.pass_counts_by_repeat(rows))`, which
    additionally drops starved 0-token timeouts. The two answer different
    questions and will legitimately disagree on a cell with starved rows; do
    not collapse them.
    """
    out = {}
    for r in (rows or []):
        rep = r.get("rep", 0)
        out[rep] = out.get(rep, 0) + 1
    return dict(sorted(out.items()))


def ragged_reps(rows):
    """Repeats whose episode count differs from the cell's most common one."""
    dens = rep_denominators(rows)
    if len(set(dens.values())) <= 1:
        return []
    counts = {}
    for n in dens.values():
        counts[n] = counts.get(n, 0) + 1
    # Most common denominator wins; on a tie the larger one is canonical, so a
    # short repeat is flagged rather than a full one.
    modal = max(counts, key=lambda n: (counts[n], n))
    return [rep for rep, n in dens.items() if n != modal]


def arm_drift(protocol_a, protocol_b):
    """Protocol keys on which two ARMS (different models) disagree.

    Distinct in purpose from `protocol_drift`, which asks whether two sources
    are the same cell and refuses them if not. Two arms are *supposed* to be
    different models, and they may legitimately be served under different
    protocols -- a hosted 120B at num_ctx 65536 against a local 27B at 32768.

    The interaction statistic is a difference of within-model deltas, and each
    delta is seed-paired inside one arm under one protocol, so a protocol
    difference BETWEEN arms does not invalidate it. Pass rates across those
    same arms are not comparable at all. Nothing said either of those things
    out loud, so this returns the keys that differ and the caller prints them:
    an interaction across two protocols is a valid statistic with a caveat, and
    the caveat has to travel with it.
    """
    return protocol_drift(protocol_a, protocol_b)


def pooled_drift(sources):
    """Protocol keys that actually disagree somewhere in this pool.

    Reported so a waiver names what was waived: a clean pool must never carry
    the same drift stamp as one that needed the override.
    """
    keys = set()
    for i in range(len(sources)):
        for j in range(i + 1, len(sources)):
            keys.update(protocol_drift(sources[i].get("protocol"),
                                       sources[j].get("protocol")))
    return sorted(keys)


def duplicate_episodes(rows):
    """(task, rep) pairs recorded more than once.

    Two rows for one repeat are averaged inside that repeat's bucket -- the
    same corruption as merging two directories that share a repeat, except it
    fits in a single directory where no cross-source guard can see it.
    """
    seen = {}
    for r in (rows or []):
        key = (r.get("task"), r.get("rep", 0))
        seen[key] = seen.get(key, 0) + 1
    return sorted((k for k, n in seen.items() if n > 1),
                  key=lambda t: (str(t[0]), str(t[1])))


def malformed_reps(rows):
    """Rows whose repeat index is absent or not an integer.

    `rep` defaults to 0 everywhere it is read, so a row without one is silently
    asserted to belong to repeat 0, and a string "0" is a different repeat from
    the integer 0 for every set operation in this module.
    """
    bad = []
    for r in (rows or []):
        if "rep" not in r:
            bad.append((r.get("task"), "missing"))
        elif not isinstance(r["rep"], int) or isinstance(r["rep"], bool):
            bad.append((r.get("task"), repr(r["rep"])))
    return bad


def source_conflicts(source):
    """Reasons ONE directory is not internally sound."""
    out = []
    rows = source.get("rows")
    dup = duplicate_episodes(rows)
    if dup:
        shown = ", ".join(f"{t}.rep{r}" for t, r in dup[:5])
        out.append(f"{source['dir']}: {len(dup)} (task, rep) pair(s) recorded "
                   f"more than once: {shown}"
                   f"{' ...' if len(dup) > 5 else ''}")
    bad = malformed_reps(rows)
    if bad:
        shown = ", ".join(f"{t}:{v}" for t, v in bad[:5])
        out.append(f"{source['dir']}: {len(bad)} row(s) with an unusable "
                   f"repeat index ({shown}"
                   f"{' ...' if len(bad) > 5 else ''})")
    return out


def merge_conflicts(sources, allow_drift=False):
    """Reasons these sources must not become one cell. [] means safe.

    `sources` is [{"dir", "protocol", "config", "rows"}]. A single source is
    always safe -- there is nothing to merge.
    """
    out = []
    for src in sources:
        out.extend(source_conflicts(src))
    if len(sources) < 2:
        return out
    # Every pair, not each against the first: a fault between the second and
    # third source is just as disqualifying, and comparing only to a head
    # would let it through.
    for i in range(len(sources)):
        for j in range(i + 1, len(sources)):
            head, other = sources[i], sources[j]
            pair = f"{head['dir']} + {other['dir']}"

            differing = config_drift(head.get("config"), other.get("config"))
            if differing:
                out.append(f"{pair}: same config name, different ablation flags "
                           f"({', '.join(differing)})")

            drift = protocol_drift(head.get("protocol"), other.get("protocol"))
            if drift and not allow_drift:
                detail = ", ".join(
                    f"{k}={(head.get('protocol') or {}).get(k)!r}"
                    f"/{(other.get('protocol') or {}).get(k)!r}" for k in drift)
                out.append(f"{pair}: protocol drift on {detail}")

            overlap = sorted(set(reps_of(head["rows"])) & set(reps_of(other["rows"])))
            if overlap:
                out.append(f"{pair}: both hold rep(s) {overlap}; merging would "
                           f"average two protocols inside one repeat")

            ht, ot = tasks_of(head["rows"]), tasks_of(other["rows"])
            if ht != ot:
                missing = sorted(set(ht) ^ set(ot))
                out.append(f"{pair}: task sets differ ({', '.join(missing)}); "
                           f"per-repeat rates would use different denominators")
    return out


def seed_reuse(cells):
    """A seed used for two different repeat indices of the same model.

    Repeat disjointness makes an extension look sound, but if the extension
    re-used the frozen cell's seeds it added no information: n doubles and the
    interval narrows on duplicated samples.
    """
    seen = {}
    for (model, cfg), rows in cells.items():
        for r in (rows or []):
            seed = r.get("seed")
            if seed is None:
                continue
            seen.setdefault((model, cfg, seed), set()).add(r.get("rep", 0))
    out = []
    for (model, cfg, seed), reps in sorted(
            seen.items(), key=lambda kv: (str(kv[0][0]), str(kv[0][1]), str(kv[0][2]))):
        if len(reps) > 1:
            out.append(f"{model} [{cfg}]: seed {seed} used for repeats "
                       f"{sorted(reps)} -- those repeats are the same sample")
    return out


def unseeded_cells(cells):
    """Cells holding rows with no seed recorded.

    seed_conflicts and seed_reuse can say nothing about these. Reporting the
    gap is the point: a check that could not run must not read as one that ran
    and passed.
    """
    out = []
    for (model, cfg), rows in sorted(cells.items(), key=lambda kv: (str(kv[0][0]), str(kv[0][1]))):
        n = sum(1 for r in (rows or []) if r.get("seed") is None)
        if n:
            out.append(f"{model} [{cfg}]: {n} row(s) with no seed recorded; "
                       f"seed pairing is unverifiable for this cell")
    return out


def contrast_conflicts(cells):
    """Reasons a model's configs cannot form a valid paired contrast.

    A delta pairs `full` against an ablation repeat by repeat. If the two arms
    scored different tasks, the per-repeat rates have different denominators
    and their difference is not a measurement of the flag.
    """
    by_model = {}
    for (model, cfg), rows in cells.items():
        by_model.setdefault(model, {})[cfg] = tasks_of(rows)
    out = []
    for model, cfgs in sorted(by_model.items(), key=lambda kv: str(kv[0])):
        ref = cfgs.get("full")
        if ref is None:
            continue
        for cfg, ts in sorted(cfgs.items()):
            if cfg == "full" or ts == ref:
                continue
            diff = sorted(set(ref) ^ set(ts))
            out.append(f"{model}: `full` and `{cfg}` scored different tasks "
                       f"({', '.join(diff)}); their paired delta is not a "
                       f"measurement of the flag")
    return out


def contrast_drift(cell_protocols):
    """Protocol drift BETWEEN a model's configs, as (label, keys) pairs.

    Reported rather than refused: the frozen grid is itself asymmetric here
    (`full` ran sole-tenant, `no-outcap` shared the GPU), so refusing would
    make the published result irreproducible. Naming it keeps the asymmetry
    visible instead of implied-absent.
    """
    by_model = {}
    for (model, cfg), proto in cell_protocols.items():
        by_model.setdefault(model, {})[cfg] = proto
    out = []
    for model, cfgs in sorted(by_model.items(), key=lambda kv: str(kv[0])):
        ref = cfgs.get("full")
        if ref is None:
            continue
        for cfg, proto in sorted(cfgs.items()):
            if cfg == "full":
                continue
            drift = protocol_drift(ref, proto)
            if drift:
                out.append((f"{model}: full vs {cfg}", drift))
    return out


def seed_conflicts(cells):
    """Repeat indices that mean different seeds across a model's configs.

    Deltas are paired by repeat index. If rep 5 is seed 5 in `full` and seed
    105 in the ablation, the pairing compares different samples and the
    paired difference is meaningless.

    `cells` is {(model, config): rows}.
    """
    seen = {}
    for (model, cfg), rows in cells.items():
        for r in (rows or []):
            if "seed" not in r:
                continue
            key = (model, r.get("rep", 0))
            seen.setdefault(key, {}).setdefault(r["seed"], set()).add(cfg)
    out = []
    for (model, rep), by_seed in sorted(seen.items(), key=lambda kv: (kv[0][0], kv[0][1])):
        if len(by_seed) > 1:
            detail = "; ".join(
                f"seed {s} in {', '.join(sorted(cfgs))}"
                for s, cfgs in sorted(by_seed.items(), key=lambda kv: str(kv[0])))
            out.append(f"{model} rep {rep}: {detail}")
    return out
