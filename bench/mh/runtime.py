"""Model residency control.

On a 64 GB Mac, a 38 GB Q8 MoE and an 18 GB dense model cannot coexist;
the runner evicts everything else and refuses to start if it cannot.
On GH200 (~96 GiB HBM) two models fit; `--share-gpu` allows exactly one
peer so Qwen and Ornith can decode at the same time. A third model is
still refused.
"""
import json
import os
import platform
import re
import subprocess
import tempfile
import time
import urllib.request

HOST = "http://127.0.0.1:11434"
# One peer besides `model`. Qwen 27B Q4 (~21 GiB resident) + Ornith 35B Q8
# (~39 GiB weights) leaves HBM headroom; three would not.
SHARE_GPU_MAX_PEERS = 1
STARVE_ABORT_DEFAULT = 3

# --- who serves a model ------------------------------------------------------
#
# A model name routes to exactly one provider, and that routing decides three
# separate things: which client is built, whether the runner may evict a local
# model for it, and what the episode's protocol stamp claims about the serving
# hardware. One table, so a drift cannot route a request one way and manage the
# GPU the other.
#
# Cerebras models are addressed with an explicit `cerebras:` prefix rather than
# by bare id. `gpt-oss-120b` is also a local ollama tag on this machine, and a
# bare name that silently means "the remote 120B" on one host and "the local
# quantised build" on another is precisely the confusion the protocol stamp
# exists to prevent.
API_PREFIXES = {"cerebras:": "cerebras"}
API_ENDPOINTS = {
    "gemini": "https://generativelanguage.googleapis.com/v1beta/interactions",
    "cerebras": "https://api.cerebras.ai/v1/chat/completions",
}


def api_provider(model):
    """The provider serving `model`, or None when local ollama serves it.

    Canonical owner. `is_api_model` and `is_gemini_model` are both derived from
    it, and mh.model imports it rather than keeping a second copy.
    """
    if not model:
        return None
    for prefix, provider in API_PREFIXES.items():
        if model.startswith(prefix):
            return provider
    if (model.startswith("gemini")
            or model.startswith("models/gemini")
            or model == "live-gemini"):
        return "gemini"
    return None


def api_model_id(model):
    """The id the provider itself expects, with any routing prefix stripped."""
    for prefix in API_PREFIXES:
        if model.startswith(prefix):
            return model[len(prefix):]
    return model


def keep_existing_episode(row, force=False):
    """Skip-existing must not treat a 0-token timeout as a finished episode."""
    if force:
        return False
    if not isinstance(row, dict) or not row.get("task"):
        return False
    return not is_first_turn_failure(row)


def should_retry_starved(row, attempt):
    """One retry after unstick. A wedged llama-server is not a harness fail."""
    return attempt == 0 and is_first_turn_failure(row)


def serving_env(host=None, model=None):
    """Where this run is actually being served.

    Nothing in the summary recorded the machine, GPU or serving backend, so a
    Metal/MLX run and a CUDA/llama-server run were indistinguishable once
    written to disk -- which is how MLX rows were pooled into a CUDA contrast.
    Values are best-effort; an unknown field records None rather than a guess,
    and an unrecorded field never certifies a match.

    An API-served model is NOT served by this box, and stamping the local GPU
    on it is not a harmless extra field: env_gpu is a pooling key, so a remote
    cell launched from the GH200 claimed the GH200 as its serving hardware and
    would have refused to pool with the identical cell launched from a laptop.
    For those models the serving environment is the provider endpoint, and the
    local machine is recorded as `client_node` -- provenance, deliberately not
    a pooling key.
    """
    provider = api_provider(model) if model else None
    if provider:
        env = {"node": None, "gpu": None, "ollama_version": None,
               "platform": None, "api_provider": provider,
               "api_endpoint": API_ENDPOINTS[provider], "client_node": None}
        try:
            env["client_node"] = platform.node() or None
        except Exception:
            pass
        return env
    env = {"node": None, "gpu": None, "ollama_version": None, "platform": None,
           "api_provider": None, "api_endpoint": None, "client_node": None}
    try:
        env["node"] = platform.node() or None
        env["platform"] = f"{platform.system()}/{platform.machine()}"
    except Exception:
        pass
    try:
        out = subprocess.run(["nvidia-smi", "--query-gpu=name",
                              "--format=csv,noheader"],
                             capture_output=True, text=True, timeout=10)
        if out.returncode == 0 and out.stdout.strip():
            names = [x.strip() for x in out.stdout.strip().splitlines() if x.strip()]
            env["gpu"] = ", ".join(names) or None
    except Exception:
        pass
    if env["gpu"] is None and (env["platform"] or "").startswith("Darwin"):
        env["gpu"] = f"apple-metal/{platform.machine()}"
    try:
        with urllib.request.urlopen((host or HOST) + "/api/version", timeout=5) as r:
            env["ollama_version"] = json.loads(r.read()).get("version") or None
    except Exception:
        pass
    env["client_node"] = env["node"]
    return env


def extension_reps(rep_offset, repeat):
    """Effective repeat indices for a run. Offset extends an existing cell."""
    return [rep_offset + i for i in range(repeat)]


# --- the cell on disk --------------------------------------------------------
#
# A cell is a directory of `{task}.rep{n}.json` episodes, and those files ARE
# the record. summary.json is a view of them, re-derived from them on every
# write.
#
# It used to be assembled from one invocation's in-memory rows, so it described
# only the tasks that invocation was handed and overwrote whatever the rest of
# the directory said. One cell shipped `"n": 1` with nine episodes beside it;
# another published three globmatch repeats out of a directory that also held
# three navigate ones. Nothing ever read the episodes back, so nothing could
# notice. Deriving the summary is what makes the count, the protocol block and
# grid.complete() answer for the whole cell instead of for one invocation.

EPISODE_RE = re.compile(r"^(?P<task>.+)\.rep(?P<rep>\d+)\.json$")

class CellError(RuntimeError):
    """A cell on disk cannot be read soundly.

    Raised, never swallowed into a default: a summary that quietly drops an
    unreadable episode reports exactly what a summary that read every episode
    and found them all reports.
    """


def write_json_atomic(path, obj, indent=1):
    """Write JSON so no reader ever sees a truncated or half-written file.

    `open(path, "w")` truncates before the replacement bytes exist. A crash,
    a kill, or a full disk inside that window leaves an empty episode where a
    frozen one was -- and `expand_hard.sh` drives `--force` straight over
    frozen cells. Write a sibling temp file, fsync it, rename (atomic within a
    directory), then fsync the directory so the rename itself survives.
    """
    d = os.path.dirname(os.path.abspath(path)) or "."
    os.makedirs(d, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=d, prefix=".tmp-summary-" if
                               os.path.basename(path) == "summary.json"
                               else ".tmp-episode-", suffix=".json")
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(obj, f, indent=indent)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
        tmp = None
    finally:
        if tmp is not None:
            try:
                os.unlink(tmp)
            except OSError:
                pass
    try:
        dfd = os.open(d, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(dfd)
    except OSError:
        pass
    finally:
        os.close(dfd)


def episode_name(task, rep):
    return f"{task}.rep{rep}.json"


def parse_episode_name(fname):
    """(task, rep) for an episode filename, or None if it is not one."""
    m = EPISODE_RE.match(fname)
    if not m:
        return None
    return m.group("task"), int(m.group("rep"))


def episode_files(outdir):
    """[(task, rep, path)] for the cell, ordered by (rep, task).

    A missing directory is an empty cell. A directory that cannot be listed is
    an error: "I could not look" must not read as "there is nothing there".
    """
    try:
        names = os.listdir(outdir)
    except FileNotFoundError:
        return []
    except OSError as e:
        raise CellError(f"{outdir}: cannot list the cell ({e})")
    out = []
    for n in names:
        parsed = parse_episode_name(n)
        if parsed:
            out.append((parsed[0], parsed[1], os.path.join(outdir, n)))
    out.sort(key=lambda x: (x[1], x[0]))
    return out


def read_episode(path, task=None, rep=None):
    """One episode, or CellError. The filename and the row must agree."""
    try:
        with open(path) as f:
            ep = json.load(f)
    except (OSError, ValueError) as e:
        raise CellError(f"{path}: unreadable episode ({e})")
    if not isinstance(ep, dict):
        raise CellError(f"{path}: episode is a {type(ep).__name__}, not an object")
    row = ep.get("row")
    if not isinstance(row, dict):
        raise CellError(f"{path}: episode has no row object")
    if not row.get("task"):
        raise CellError(f"{path}: episode row has no task")
    if task is not None and row.get("task") != task:
        raise CellError(f"{path}: row is task {row.get('task')!r} but the "
                        f"filename says {task!r}")
    if rep is not None and int(row.get("rep", rep)) != rep:
        raise CellError(f"{path}: row is rep {row.get('rep')!r} but the "
                        f"filename says {rep}")
    return ep


def load_cell(outdir):
    """[(filename, episode)] for every episode in the cell, ordered (rep, task).

    Refuses the whole cell if one episode is unreadable. Skipping it would put
    the runner back to reporting a subset as if it were everything.
    """
    return [(os.path.basename(p), read_episode(p, t, r))
            for t, r, p in episode_files(outdir)]


def cell_rows(outdir):
    """Every scored row in the cell, read from the episodes themselves."""
    return [ep["row"] for _, ep in load_cell(outdir)]


def protocol_block(max_steps, max_wall, share_gpu, num_ctx, num_predict,
                   temperature, think, env=None, model=None, top_p=None,
                   reasoning_effort=None, concurrency=None):
    """The protocol stamp for one episode: intent plus the machine serving it.

    share_gpu is the value in force for THIS episode, not the flag as typed:
    a cell demoted mid-run really did run its first episodes with a peer on
    the GPU, and the stamp has to say so.

    `reasoning_effort` and `top_p` are stamped because on an API arm they are
    settings that change what the model does and `think` alone cannot express
    them: gpt-oss-120b at effort "low" and at effort "high" are two different
    experiments that both record think=True. They are None on a local arm,
    and a stamp written before this key existed also reads None, so adding
    them does not make two older cells look like they disagree.
    """
    env = serving_env(model=model) if env is None else env
    block = {"max_steps": int(max_steps), "max_wall": int(max_wall),
             "share_gpu": bool(share_gpu), "num_ctx": int(num_ctx),
             "num_predict": int(num_predict), "temperature": float(temperature),
             "think": bool(think),
             "top_p": None if top_p is None else float(top_p),
             "reasoning_effort": reasoning_effort,
             # How many cells were deliberately in flight on this box. A
             # contended GPU moves wall time, --max-wall scores a fail, and a
             # cell run four-up is therefore not the same experiment as one run
             # alone even when every other setting matches. share_gpu already
             # records the two-models case; this records the N-episodes-one-
             # model case, which is the shape that actually saves rent.
             "concurrency": None if concurrency is None else int(concurrency)}
    block.update({f"env_{k}": v for k, v in (env or {}).items()})
    return block


def protocol_of(ep):
    """The protocol an episode records, or None when it records none."""
    p = (ep or {}).get("protocol")
    return p if isinstance(p, dict) else None


# Recorded on every episode, and deliberately NOT part of what makes two
# episodes the same experiment. env_client_node names the box that drove the
# loop; for an API arm that box did not serve the model, so two episodes that
# differ only in it ran the same experiment.
#
# This has to be honoured in both directions or it is worse than not existing.
# mh.pool.PROTOCOL_KEYS is an allowlist and simply omits it; protocol_diff is a
# denylist over every key present, so without this it flagged the difference and
# a cell resumed from a second laptop was declared to have no single protocol
# and refused pooling -- while mh.pool, asked the same question, said the two
# agreed. Two definitions of "protocol", disagreeing. Measured on a real cell:
# 159 episodes from one host and 1 from another published protocol_variants over
# two groups for no reason but the hostname.
PROVENANCE_KEYS = ("env_client_node",)


def protocol_identity(stamp):
    """The part of a stamp that decides whether two episodes are one experiment.

    Everything except PROVENANCE_KEYS. Every comparison of two protocols goes
    through this or through protocol_diff, which is built on the same set --
    that is the point. Four separate places had grown their own answer to "are
    these the same protocol": whole-dict `!=` in resume_conflicts, whole-dict
    grouping in _merge_protocols, an allowlist in mh.pool, and this denylist.
    They agreed until a key existed that one of them should ignore, and then a
    cell resumed from a second machine was simultaneously poolable, unresumable,
    and possessed of two protocols.
    """
    if not isinstance(stamp, dict):
        return None
    return {k: v for k, v in stamp.items() if k not in PROVENANCE_KEYS}


def protocol_diff(a, b):
    """Keys on which two protocol stamps disagree.

    An absent stamp disagrees on everything. An unrecorded setting is not
    evidence of a matching one. Provenance keys are excluded: see
    PROVENANCE_KEYS.
    """
    if not isinstance(a, dict) or not isinstance(b, dict):
        return ["<unrecorded>"]
    keys = (set(a) | set(b)) - set(PROVENANCE_KEYS)
    return [k for k in sorted(keys) if a.get(k) != b.get(k)]


def _peek_protocol(path):
    """An episode's protocol stamp without loading the cell.

    None means "cannot attest": no stamp, or a file that will not parse. Both
    are treated as another run's work by every caller here.
    """
    try:
        with open(path) as f:
            ep = json.load(f)
    except (OSError, ValueError):
        return None
    return protocol_of(ep if isinstance(ep, dict) else None)


def classify_episodes(outdir, protocol):
    """(mine, foreign) episode filenames, split by provenance.

    "mine" means the episode records exactly this invocation's protocol, which
    is the only evidence that resuming it is a resume rather than an overwrite.
    """
    mine, foreign = [], []
    for _, _, path in episode_files(outdir):
        name = os.path.basename(path)
        stamp = _peek_protocol(path)
        if protocol is not None and stamp is not None and stamp == protocol:
            mine.append(name)
        else:
            foreign.append(name)
    return mine, foreign


def cell_key(outdir):
    """(model slug, config) for a `slug__config__tag` directory, else None.

    The tag is a directory name; the cell is (model, config). compare.py pools
    by (model, config) across tags, so that is the unit a duplicate has to be
    checked against.
    """
    parts = os.path.basename(os.path.abspath(outdir)).split("__")
    if len(parts) != 3 or not all(parts[:2]):
        return None
    return parts[0], parts[1]


def sibling_cells(outdir):
    """Directories holding the same (model, config) under a different tag."""
    key = cell_key(outdir)
    if key is None:
        return []
    root = os.path.dirname(os.path.abspath(outdir))
    me = os.path.basename(os.path.abspath(outdir))
    try:
        names = sorted(os.listdir(root))
    except OSError:
        return []
    out = []
    for n in names:
        if n == me:
            continue
        p = os.path.join(root, n)
        if os.path.isdir(p) and cell_key(p) == key:
            out.append(p)
    return out


def sibling_overlap(outdir, task_names, reps):
    """{sibling dir: [episode names]} this run's (task, rep) window duplicates.

    extension_guard used to look only inside the new tag's own directory, so
    `--rep-offset 3 --tag fresh` was allowed against an empty directory and
    re-ran repeats the (model, config) already had under the frozen tag.
    """
    out = {}
    for d in sibling_cells(outdir):
        hits = [episode_name(t, r) for r in reps for t in task_names
                if os.path.isfile(os.path.join(d, episode_name(t, r)))]
        if hits:
            out[d] = hits
    return out


def _group_by_value(pairs):
    """[(name, value)] -> [{"value": v, "episodes": [names]}], input order kept."""
    groups = {}
    for name, value in pairs:
        key = json.dumps(value, sort_keys=True, default=str)
        groups.setdefault(key, {"value": value, "episodes": []})["episodes"].append(name)
    return list(groups.values())


def _merge_protocols(cell):
    """(protocol, variants) across a cell's episodes.

    Unanimous and recorded -> that block. Anything else -> None plus a variant
    list naming which episodes ran under what. A cell assembled from two
    protocols has no protocol, and it has to say so: the old summary stamped
    the LAST invocation's settings onto episodes it had merely read off disk,
    so resuming a cell with a different --max-steps rewrote history.
    """
    groups = _group_by_value([(n, protocol_identity(protocol_of(ep)))
                              for n, ep in cell])
    if not groups:
        return None, []
    if len(groups) == 1 and groups[0]["value"] is not None:
        # One experiment. Provenance is reattached rather than dropped: a key
        # every episode agrees on keeps its value, and one they do not agree on
        # records None, which every drift check already reads as "not one
        # value". A cell legitimately resumed from a second machine gets a
        # single protocol and an honest env_client_node of None -- not two
        # protocols, and not a claim that all 160 episodes ran on whichever
        # host happened to be first in the directory listing.
        common = dict(groups[0]["value"])
        stamps = [protocol_of(ep) or {} for _, ep in cell]
        for key in PROVENANCE_KEYS:
            if not any(key in st for st in stamps):
                # No episode recorded it. Writing None would invent a key the
                # stamps never had, and an absent field is not a field whose
                # value is None -- every drift check here rests on that
                # distinction.
                continue
            seen = {st.get(key) for st in stamps}
            common[key] = seen.pop() if len(seen) == 1 else None
        return common, []
    return None, [{"protocol": g["value"], "episodes": g["episodes"]}
                  for g in groups]


def _merge_configs(cell, fallback):
    """(config, variants) across a cell's episodes.

    The config NAME must agree -- two names in one directory is not one cell.
    Individual flags that disagree are recorded as None, which every downstream
    drift check reads as disagreement rather than as a value.
    """
    if not cell:
        return dict(fallback or {}), []
    bad = [n for n, ep in cell if not isinstance(ep.get("config"), dict)]
    if bad:
        raise CellError(f"{len(bad)} episode(s) record no config object "
                        f"({', '.join(bad[:5])})")
    names = sorted({ep["config"].get("name") for _, ep in cell}, key=str)
    if len(names) > 1:
        raise CellError(f"cell holds episodes for {len(names)} configs: "
                        f"{', '.join(repr(n) for n in names)}")
    groups = _group_by_value([(n, ep["config"]) for n, ep in cell])
    if len(groups) == 1:
        return groups[0]["value"], []
    merged = {}
    for key in sorted({k for g in groups for k in g["value"]}):
        vals = [g["value"].get(key) for g in groups]
        merged[key] = vals[0] if all(v == vals[0] for v in vals) else None
    merged["name"] = names[0]
    return merged, [{"config": g["value"], "episodes": g["episodes"]}
                    for g in groups]


def _unanimous_run_field(cell, field):
    """One invocation-level value, or None when the cell holds more than one."""
    seen = []
    for _, ep in cell:
        v = (ep.get("run") or {}).get(field)
        if v not in seen:
            seen.append(v)
    return seen[0] if len(seen) == 1 else None


def _refuse_uncovered(outdir, rows):
    """Never let a rebuild silently shrink a cell.

    The episodes are the record -- but if summary.json already claims a
    (task, rep) with no episode file behind it, they are not the WHOLE record,
    and rebuilding from them would delete a result. Refuse, and name what is
    missing.
    """
    p = os.path.join(outdir, "summary.json")
    if not os.path.isfile(p):
        return
    try:
        with open(p) as f:
            old = json.load(f)
    except (OSError, ValueError) as e:
        raise CellError(f"{p}: the existing summary is unreadable ({e}); "
                        f"refusing to overwrite it. Move it aside once you "
                        f"have looked at it.")
    if not isinstance(old, dict) or not isinstance(old.get("rows"), list):
        raise CellError(f"{p}: the existing summary has no rows list; refusing "
                        f"to overwrite it.")
    have = {(r.get("task"), int(r.get("rep") or 0)) for r in rows}
    lost = sorted({(r.get("task"), int(r.get("rep") or 0))
                   for r in old["rows"] if isinstance(r, dict)} - have,
                  key=lambda t: (str(t[0]), t[1]))
    if lost:
        shown = ", ".join(f"{t}.rep{r}" for t, r in lost[:5])
        raise CellError(f"{p}: {len(lost)} recorded row(s) have no episode file "
                        f"on disk ({shown}{' ...' if len(lost) > 5 else ''}); "
                        f"refusing to rebuild the summary without them.")


def build_summary(outdir, model, config, protocol=None, outcomes=None):
    """summary.json for a cell, derived from the episodes in it.

    Every count, every row, the protocol block and the repeat window come from
    the files on disk -- not from the invocation doing the writing. That is the
    difference between a summary that describes a cell and one that describes
    whichever tasks were named in `--tasks` this time.
    """
    cell = load_cell(outdir)
    rows = [ep["row"] for _, ep in cell]
    _refuse_uncovered(outdir, rows)
    wrong = sorted({ep.get("model") for _, ep in cell} - {model}, key=str)
    if wrong:
        raise CellError(f"{outdir}: episodes record model(s) "
                        f"{', '.join(repr(m) for m in wrong)}, not {model!r}")
    cfg, cfg_variants = _merge_configs(cell, config)
    want = (config or {}).get("name")
    if cell and want is not None and cfg.get("name") != want:
        raise CellError(f"{outdir}: episodes record config {cfg.get('name')!r}, "
                        f"not {want!r}")
    proto, proto_variants = _merge_protocols(cell)
    if not cell:
        proto = protocol
    n = len(rows)
    passed = sum(1 for r in rows if r.get("passed"))
    reps = sorted({int(r.get("rep") or 0) for r in rows})
    summary = {
        "model": model,
        "config": cfg,
        "n": n,
        "passed": passed,
        "pass_rate": round(100.0 * passed / n, 1) if n else 0.0,
        "wall_s": round(sum(r.get("wall_s", 0) or 0 for r in rows), 1),
        "output_tokens": sum(r.get("output_tokens", 0) or 0 for r in rows),
        "seed_base": _unanimous_run_field(cell, "seed_base"),
        "repeat": _unanimous_run_field(cell, "repeat"),
        "rep_offset": _unanimous_run_field(cell, "rep_offset"),
        "reps": [reps[0], reps[-1]] if reps else [],
        "protocol": proto,
    }
    if proto_variants:
        summary["protocol_variants"] = proto_variants
    if cfg_variants:
        summary["config_variants"] = cfg_variants
    if outcomes:
        summary["outcomes"] = dict(outcomes)
    summary["derived_from"] = {
        "episodes": n,
        "runs": [{"run": g["value"], "episodes": len(g["episodes"])}
                 for g in _group_by_value([(n_, ep.get("run"))
                                           for n_, ep in cell])],
    }
    summary["rows"] = rows
    return summary


def write_episode(outdir, task, rep, payload):
    """Write one episode atomically. Returns its path."""
    path = os.path.join(outdir, episode_name(task, rep))
    write_json_atomic(path, payload)
    return path


def write_summary(outdir, model, config, protocol=None, outcomes=None):
    """Re-derive summary.json from the cell and write it atomically."""
    summary = build_summary(outdir, model, config, protocol, outcomes)
    write_json_atomic(os.path.join(outdir, "summary.json"), summary)
    return summary


def extension_guard(outdir, task_names, reps, rep_offset=0, force=False,
                    protocol=None):
    """Refusal reason for this run, or None when it is safe.

    Extending a cell must only ever ADD repeats, and "the cell" is
    (model, config) -- not one directory. The guard therefore looks at three
    things, because the frozen tree was damaged through all three:

    - sibling cells. It used to inspect only the new tag's own directory, so
      `--rep-offset 3 --tag fresh` sailed through against an empty directory
      and re-ran repeats the (model, config) already had under the frozen tag.
    - this directory's episodes, split by provenance. An extension may resume
      its OWN interrupted output -- refusing that left a crashed or
      starve-aborted extension permanently unresumable, with neither --force
      nor --force-starved able to help -- but it must never land beside
      another run's episodes.
    - summary.json, only as evidence of a cell whose episodes are gone. The
      summary is derived now, so rewriting it is no longer the hazard;
      rewriting it when there is nothing left to re-derive it from is.

    `protocol` is this invocation's stamp. Without one, nothing on disk can be
    shown to be ours, so every existing episode counts as another run's.
    """
    if rep_offset < 0:
        return f"--rep-offset must be >= 0, got {rep_offset}"
    if cell_key(outdir) is None:
        return (f"cannot identify the (model, config) cell from "
                f"{os.path.basename(os.path.abspath(outdir))!r}: expected a "
                f"directory named slug__config__tag, so sibling cells cannot "
                f"be checked for the repeats this run would duplicate")
    overlap = sibling_overlap(outdir, task_names, reps)
    if rep_offset and overlap:
        total = sum(len(v) for v in overlap.values())
        first = sorted(overlap)[0]
        return (f"--rep-offset {rep_offset} lands on {total} episode(s) this "
                f"(model, config) already has under another tag: "
                f"{os.path.basename(first)}/{overlap[first][0]}"
                f"{' ...' if total > 1 else ''}. An extension adds repeats; it "
                f"must not re-run ones the cell already holds.")
    if not rep_offset:
        return None
    if force:
        return ("--force with --rep-offset would overwrite frozen episodes; "
                "an extension only adds repeats")
    mine, foreign = classify_episodes(outdir, protocol)
    window = {episode_name(t, r) for r in reps for t in task_names}
    hits = [n for n in foreign if n in window]
    if hits:
        shown = ", ".join(hits[:5]) + (" ..." if len(hits) > 5 else "")
        return (f"--rep-offset {rep_offset} lands on {len(hits)} existing "
                f"episode(s) in {outdir}: {shown}")
    if foreign and os.path.isfile(os.path.join(outdir, "summary.json")):
        return (f"{outdir} already holds a summary.json over {len(foreign)} "
                f"episode(s) from another run; an extension must not rewrite "
                f"it. Use a distinct --tag for the new repeats.")
    if foreign:
        shown = ", ".join(foreign[:5]) + (" ..." if len(foreign) > 5 else "")
        return (f"{outdir} already holds {len(foreign)} episode(s) from "
                f"another run ({shown}); an extension must not add repeats "
                f"beside them. Use a distinct --tag.")
    if not mine and os.path.isfile(os.path.join(outdir, "summary.json")):
        return (f"{outdir} holds a summary.json with no episodes beside it; "
                f"the cell cannot be re-derived, so refusing to extend it.")
    return None


def resume_conflicts(outdir, task_names, reps, protocol, force=False):
    """Episodes this run would KEEP that did not run under `protocol`.

    Returns (conflicts, unattributed): filenames whose recorded protocol
    disagrees with this invocation's, and filenames that record none at all.

    Resuming a cell with different flags used to replay those rows out of the
    episode files and then stamp THIS invocation's arguments over the lot, so
    the summary described episodes that never ran under it. Conflicts are
    refused; unattributed episodes are kept but leave the cell's protocol
    unrecorded rather than silently adopting ours.
    """
    conflicts, unattributed = [], []
    if force:
        return conflicts, unattributed
    for rep in reps:
        for task in task_names:
            path = os.path.join(outdir, episode_name(task, rep))
            if not os.path.isfile(path):
                continue
            try:
                ep = read_episode(path, task, rep)
            except CellError:
                continue          # unreadable: it gets re-run, not kept
            if not keep_existing_episode(ep.get("row") or {}):
                continue
            stamp = protocol_of(ep)
            if stamp is None:
                unattributed.append(os.path.basename(path))
                continue
            if protocol is None:
                continue
            # Decided BY protocol_diff, not by dict inequality. Deciding one way
            # and explaining the other is how this refused 90 episodes and then
            # printed "differs on " with nothing after it: the stamps differed
            # only in env_client_node, which protocol_diff excludes as
            # provenance, so the whole-dict `!=` saw a conflict that the diff
            # could not name. A cell resumed from a second machine could not be
            # resumed at all, and the message did not say why.
            diff = protocol_diff(stamp, protocol)
            if diff:
                conflicts.append((os.path.basename(path), diff))
    return conflicts, unattributed


def _died_on_first_turn(row):
    """Episode produced nothing and never got past its first turn."""
    if not isinstance(row, dict):
        return False
    if int(row.get("output_tokens") or 0) > 0:
        return False
    if int(row.get("steps") or 0) > 1:
        return False
    return True


def is_starved_episode(row):
    """True when the GPU never served the model: a first-turn TIMEOUT.

    The Qwen+Ornith share-gpu failure mode is: first /api/chat times out,
    steps=1, output_tokens=0, stop_reason error:ModelError. That is not a
    harness ablation. Treating it as a real fail contaminates the cell.

    Deliberately NARROW. This predicate alone decides what leaves a reported
    denominator (mh.stats.usable_rows), so it must catch only the case where
    the instrument failed to take a measurement. A first-turn failure of any
    other kind -- a malformed response, an HTTP 500, a tool crash -- is a real
    result and must be scored, not dropped: excluding it shrinks the sample
    instead of counting the failure, and a rate computed over a denominator
    that quietly moved is worse than no rate.

    For the operational question "should this episode be re-run", use
    is_first_turn_failure, which is broader on purpose.
    """
    if not _died_on_first_turn(row):
        return False
    stop = str(row.get("stop_reason") or "")
    errs = " ".join(str(e) for e in (row.get("errors") or []))
    blob = (stop + " " + errs).lower()
    return (stop == "wall_timeout"
            or "timed out" in blob
            or "timeouterror" in blob)


# 401 / 402 / 403 as they appear in a recorded row. Episodes written before
# AccountRefused existed carry `error:ModelError` with the HTTP code in the
# error text; ones written after carry `error:AccountRefused`. Both are the
# same thing and both must be recognised, or the 314 rows the quota incident
# already produced stay invisible to the tooling that exists to catch them.
_REFUSAL_CODES = ("HTTP 401", "HTTP 402", "HTTP 403")


def is_unserved_episode(row):
    """True when the provider refused the ACCOUNT, so nothing was measured.

    Distinct from is_starved_episode, which is about a local server that failed
    to serve a request. This is about a key that is out of quota, revoked, or
    unauthorised: no episode after the first one is a measurement, and a cell
    holding these must never be reported as a pass rate. `no-envboot` came out
    of the quota incident at 0.0% over 160 such rows, which is indistinguishable
    from an ablation that destroys the model unless something names it.

    Not wired into mh.stats.usable_rows. The registered exclusion rule is
    'zero output tokens, at most one step, stopped on a timeout -- and that is
    the only exclusion', and quietly adding a second one would move a
    preregistered denominator. These rows are not to be rescued statistically;
    they are to be re-run, which keep_existing_episode already arranges.
    """
    if not isinstance(row, dict):
        return False
    stop = str(row.get("stop_reason") or "")
    if stop.startswith("error:AccountRefused"):
        return True
    if not stop.startswith("error:"):
        return False
    return any(code in e for e in (row.get("errors") or [])
               for code in _REFUSAL_CODES)


def unserved_count(rows):
    """How many of these rows were never served because the account was refused."""
    return sum(1 for r in (rows or []) if is_unserved_episode(r))


def is_first_turn_failure(row):
    """True when an episode died on turn one with nothing generated, for any
    reason. Drives retry and resume, where re-running is cheap and safe.

    Never use this to exclude a row from a reported rate.
    """
    if not _died_on_first_turn(row):
        return False
    stop = str(row.get("stop_reason") or "")
    return (stop.startswith("error:")
            or stop.startswith("runner_error:")
            or is_starved_episode(row))


def resident_models(host=HOST):
    """Models currently loaded in memory. [] also means 'server unreachable'."""
    try:
        with urllib.request.urlopen(host + "/api/ps", timeout=5) as r:
            return [m.get("name") or m.get("model")
                    for m in (json.loads(r.read()).get("models") or [])]
    except Exception:
        return []


def blocking_residents(model, resident, share_gpu=False):
    """Peers that must not remain if this model is to run."""
    others = [m for m in resident if m != model]
    if not share_gpu:
        return others
    return others[SHARE_GPU_MAX_PEERS:]


def server_up(host=HOST):
    try:
        with urllib.request.urlopen(host + "/api/version", timeout=5) as r:
            json.loads(r.read())
        return True
    except Exception:
        return False


def unload_all(host=HOST, keep=None, settle_s=30):
    """Evict every resident model except `keep`. Returns what it evicted."""
    evicted = []
    for name in resident_models(host):
        if keep and name == keep:
            continue
        try:
            subprocess.run(["ollama", "stop", name], capture_output=True, timeout=120)
            evicted.append(name)
        except Exception:
            pass
    for _ in range(settle_s):
        if not [m for m in resident_models(host) if not keep or m != keep]:
            break
        time.sleep(1)
    return evicted


# --- concurrent runners on one GPU -------------------------------------------
#
# Parallelising the grid is the obvious way to use a rented GH200, and the
# runner as written makes it unsafe in a way nothing would report. Every runner
# calls unload_all(keep=its own model), which evicts every OTHER resident model
# -- including one a sibling runner is mid-episode on. The victim's next
# /api/chat returns 0 tokens and times out, which lands as a scored failure. It
# is exactly the starvation the v1 grid already suffered under --share-gpu, and
# nothing in the episode says the cause was another process.
#
# So a runner announces the model it holds, and refuses to start if a live
# sibling holds a different one. Parallel cells of the SAME model are the
# supported shape: one set of weights resident, N episodes in flight. Two
# models at once is what --share-gpu means, it is a different protocol, and it
# is not made safe by this.
RUNNERS_DIRNAME = ".runners"


def _runners_dir(results_root):
    return os.path.join(results_root, RUNNERS_DIRNAME)


def _pid_alive(pid):
    try:
        os.kill(int(pid), 0)
    except (OSError, ValueError, TypeError):
        return False
    return True


def active_runners(results_root, exclude_pid=None):
    """Live sibling runners and the model each holds.

    A lease whose process is gone is stale and is deleted rather than believed:
    a crashed runner must not lock the GPU against every later one.
    """
    out = []
    d = _runners_dir(results_root)
    if not os.path.isdir(d):
        return out
    for name in sorted(os.listdir(d)):
        if not name.endswith(".json"):
            continue
        path = os.path.join(d, name)
        try:
            with open(path) as f:
                lease = json.load(f)
        except (OSError, ValueError):
            lease = None
        pid = (lease or {}).get("pid")
        if not lease or not _pid_alive(pid):
            try:
                os.remove(path)
            except OSError:
                pass
            continue
        if exclude_pid is not None and int(pid) == int(exclude_pid):
            continue
        out.append(lease)
    return out


def register_runner(results_root, model, pid=None):
    """Announce that this process holds `model`. Returns the lease path."""
    pid = os.getpid() if pid is None else int(pid)
    d = _runners_dir(results_root)
    os.makedirs(d, exist_ok=True)
    path = os.path.join(d, f"{pid}.json")
    write_json_atomic(path, {"pid": pid, "model": model,
                             "started": time.strftime("%Y-%m-%dT%H:%M:%SZ",
                                                      time.gmtime())})
    return path


def release_runner(path):
    try:
        os.remove(path)
    except OSError:
        pass


def conflicting_runners(results_root, model, pid=None):
    """Live siblings holding a model other than `model`."""
    pid = os.getpid() if pid is None else pid
    return [r for r in active_runners(results_root, exclude_pid=pid)
            if r.get("model") != model]


def is_api_model(model):
    """True for a model served over someone else's API, not by local ollama."""
    return api_provider(model) is not None


# mh.model used to carry a byte-identical copy of this predicate under the
# older name; the alias keeps that import working against the one definition.
is_gemini_model = is_api_model


def unstick_server(model, host=HOST, restart=True):
    """Kill a wedged llama-server. Next /api/chat reloads the model.

    `ollama stop` is enough when the process can still accept RPC. If the
    model is still listed, or the API is down, restart the unit. Tests pass
    restart=False so they never touch systemd.
    """
    try:
        subprocess.run(["ollama", "stop", model], capture_output=True, timeout=60)
    except Exception:
        pass
    time.sleep(1)
    if not restart:
        return
    still = model in resident_models(host)
    if server_up(host) and not still:
        return
    try:
        subprocess.run(["sudo", "-n", "systemctl", "restart", "ollama"],
                       capture_output=True, timeout=90)
    except Exception:
        pass
    for _ in range(40):
        if server_up(host):
            return
        time.sleep(1)


def ensure_sole_tenant(model, host=HOST, evict=True, share_gpu=False,
                       results_root=None, pid=None):
    """Make `model` runnable, or raise.

    Distinguishes 'nothing else is loaded' from 'I could not tell', because a
    server we cannot reach must not read as a clear machine.

    share_gpu: do not evict a single peer (GH200 Qwen+Ornith). Still refuse a
    third model. On Mac this flag must stay off.
    """
    if is_api_model(model):
        return []
    # Checked before the server check and before any eviction: the failure this
    # prevents is destroying another process's work, which is worse than not
    # starting.
    if results_root:
        clash = conflicting_runners(results_root, model, pid=pid)
        if clash:
            held = ", ".join(f"{c['model']} (pid {c['pid']})" for c in clash)
            raise RuntimeError(
                f"refusing to start: another runner on this box holds {held}. "
                f"Starting {model} would evict it mid-episode and score its "
                f"cell as a 0-token failure. Run parallel cells of the SAME "
                f"model, or wait for that runner to finish.")
    if not server_up(host):
        raise RuntimeError(f"ollama at {host} is not reachable; cannot confirm "
                           f"that no other model is resident")
    if share_gpu:
        still = blocking_residents(model, resident_models(host), share_gpu=True)
        if still:
            raise RuntimeError(
                f"refusing to start: {still} resident with share-gpu "
                f"(cap is {SHARE_GPU_MAX_PEERS} peer besides {model})")
        return []
    evicted = unload_all(host, keep=model) if evict else []
    still = blocking_residents(model, resident_models(host), share_gpu=False)
    if still:
        raise RuntimeError(f"refusing to start: {still} still resident. Two models "
                           f"at once will thrash unified memory.")
    return evicted
