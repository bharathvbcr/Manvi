"""Residency policy. No ollama required."""
import contextlib
import io
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mh.runtime import blocking_residents

PASS, FAIL = [], []


def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print(f"  {'ok  ' if cond else 'FAIL'} {name}"
          + (f"  {detail}" if not cond and detail else ""))


qwen = "qwen3.8:27b"
ornith = "hf.co/ornith-ai/Ornith-1.5-35B-A3B-GGUF:Q8_0"
gemma = "gemma4:e4b"

print("starved-episode detector")
from mh.runtime import is_starved_episode, STARVE_ABORT_DEFAULT
check("default abort after 3", STARVE_ABORT_DEFAULT == 3)
check("timeout 0 tok is starved",
      is_starved_episode({"steps": 1, "output_tokens": 0,
                          "stop_reason": "error:ModelError",
                          "errors": ["ModelError: TimeoutError: timed out"]}))
check("wall_timeout 0 tok is starved",
      is_starved_episode({"steps": 1, "output_tokens": 0,
                          "stop_reason": "wall_timeout"}))
check("real fail with tokens is not starved",
      not is_starved_episode({"steps": 8, "output_tokens": 5381,
                              "stop_reason": "error:ModelError"}))
check("finished pass is not starved",
      not is_starved_episode({"steps": 12, "output_tokens": 4000,
                              "stop_reason": "finished", "passed": True}))
check("empty not starved", not is_starved_episode({}))

# The split: only a timeout leaves the denominator; every other first-turn
# death is a real result that must be scored, but is still worth re-running.
from mh.runtime import is_first_turn_failure, keep_existing_episode
http500 = {"steps": 1, "output_tokens": 0, "stop_reason": "error:ModelError",
           "errors": ["ModelError: HTTP 500: invalid tool call arguments"]}
badjson = {"steps": 1, "output_tokens": 0,
           "stop_reason": "error:JSONDecodeError"}
check("http 500 on turn 1 is NOT starved", not is_starved_episode(http500))
check("malformed body is NOT starved", not is_starved_episode(badjson))
check("http 500 on turn 1 is a first-turn failure",
      is_first_turn_failure(http500))
check("malformed body is a first-turn failure", is_first_turn_failure(badjson))
check("timeout is both", is_starved_episode({
    "steps": 1, "output_tokens": 0, "stop_reason": "error:ModelError",
    "errors": ["ModelError: TimeoutError: timed out"]}))
check("a real fail with tokens is neither",
      not is_first_turn_failure({"steps": 8, "output_tokens": 5381,
                                 "stop_reason": "error:ModelError"}))
check("non-timeout failure is still re-run on resume",
      not keep_existing_episode(http500))

from mh.runtime import keep_existing_episode, should_retry_starved
starve = {"task": "x", "steps": 1, "output_tokens": 0,
          "stop_reason": "error:ModelError"}
real = {"task": "x", "steps": 8, "output_tokens": 100,
        "stop_reason": "finished", "passed": True}
check("skip keeps real episode", keep_existing_episode(real))
check("skip does not keep starved", not keep_existing_episode(starve))
check("force reruns real", not keep_existing_episode(real, force=True))
check("retry starved first attempt", should_retry_starved(starve, 0))
check("no second retry", not should_retry_starved(starve, 1))
check("no retry of real fail", not should_retry_starved(real, 0))

print("complete() reads the episodes, not the summary's word for them")
import json, shutil, tempfile
# Point grid at a throwaway tree BEFORE importing it: a test must never be able
# to leave a directory behind in the real results tree, where compare.py would
# then pick it up as a cell.
_sandbox = tempfile.mkdtemp(prefix="mh-runtime-test-")
os.environ["MH_RESULTS"] = _sandbox
from grid import complete, cell_has_starved, outdir
check("test grid is sandboxed", outdir("m", "c", "t").startswith(_sandbox),
      outdir("m", "c", "t"))

CFG = {"name": "no-checklist", "envboot": False, "max_steps": 0}


def write_cell(d, rows, summary=True, protocol=None, run=None,
               model="qwen3.8:27b", config=None, claim=None):
    """A cell as run.py writes one: one file per episode, summary derived.

    These fixtures used to write summary.json alone, which is exactly the
    shape of the defect: the summary was the record, and the episodes beside
    it were never read back. `claim` writes a summary that disagrees with the
    episodes on purpose.
    """
    os.makedirs(d, exist_ok=True)
    for r in rows:
        ep = {"model": model, "config": dict(config or CFG), "task": r["task"],
              "row": r, "verify_output": "", "events": []}
        if protocol is not None:
            ep["protocol"] = protocol
        if run is not None:
            ep["run"] = run
        with open(os.path.join(d, f"{r['task']}.rep{r['rep']}.json"), "w") as f:
            json.dump(ep, f)
    if summary:
        body = claim if claim is not None else rows
        p = sum(1 for r in body if r.get("passed"))
        with open(os.path.join(d, "summary.json"), "w") as f:
            json.dump({"model": model, "config": dict(config or CFG),
                       "n": len(body), "passed": p, "rows": body}, f)
    return d


def cell(tag, model="qwen3.8:27b", config="no-checklist"):
    return outdir(model, config, tag)


try:
    starved_rows = [{"task": "a", "rep": i, "steps": 1, "output_tokens": 0,
                     "stop_reason": "error:ModelError",
                     "errors": ["ModelError: TimeoutError: timed out"]}
                    for i in range(40)]
    tag = "unittest-starve"
    write_cell(cell(tag), starved_rows)
    check("starved n=40 is not complete",
          complete("qwen3.8:27b", "no-checklist", tag, 8, 5) is False)
    check("starved detected on disk",
          cell_has_starved("qwen3.8:27b", "no-checklist", tag) is True)

    # A complete 8-task x 5-repeat cell: 5 reps holding 8 episodes each.
    clean_rows = [{"task": f"t{t}", "rep": rep, "steps": 8, "output_tokens": 100,
                   "stop_reason": "finished"}
                  for rep in range(5) for t in range(8)]
    tag = "unittest-clean"
    write_cell(cell(tag), clean_rows)
    check("clean 5x8 is complete",
          complete("qwen3.8:27b", "no-checklist", tag, 8, 5) is True)

    # Same episode count, wrong shape: 40 repeats of one task. Counting rows
    # alone called this complete, which is how a ragged cell passes for a full
    # one -- the miscount shape that put three globmatch episodes under a
    # navigate tag.
    ragged = [{"task": "a", "rep": i, "steps": 8, "output_tokens": 100,
               "stop_reason": "finished"} for i in range(40)]
    tag = "unittest-ragged"
    write_cell(cell(tag), ragged)
    check("40 rows of the wrong shape is not complete",
          complete("qwen3.8:27b", "no-checklist", tag, 8, 5) is False)

    # An extension is complete on its own repeat window, not the frozen one.
    ext = [{"task": f"t{t}", "rep": rep, "steps": 8, "output_tokens": 100,
            "stop_reason": "finished"}
           for rep in range(5, 20) for t in range(8)]
    tag = "unittest-ext"
    write_cell(cell(tag), ext)
    check("extension complete on reps 5-19",
          complete("qwen3.8:27b", "no-checklist", tag, 8, 15, rep_offset=5) is True)
    check("extension is not complete on reps 0-14",
          complete("qwen3.8:27b", "no-checklist", tag, 8, 15, rep_offset=0) is False)
    check("frozen window alone is not the extension",
          complete("qwen3.8:27b", "no-checklist", tag, 8, 5) is False)

    # A1, at the skip check. The summary said n=1 while nine episodes sat
    # beside it; the grid asked the summary and skipped nothing, or ran a cell
    # it should have skipped. It has to count the episodes.
    tag = "unittest-liar"
    one_task = [{"task": "t0", "rep": 0, "steps": 8, "output_tokens": 100,
                 "stop_reason": "finished"}]
    write_cell(cell(tag), clean_rows, claim=one_task)
    check("a summary claiming n=1 does not un-complete 40 episodes",
          complete("qwen3.8:27b", "no-checklist", tag, 8, 5) is True)

    tag = "unittest-liar-starved"
    write_cell(cell(tag), starved_rows, claim=one_task)
    check("starvation the summary omits is still found on disk",
          cell_has_starved("qwen3.8:27b", "no-checklist", tag) is True)

    # A cell whose episodes are all present but whose summary never got
    # written (crash, or a starve-abort that died first) is not complete: the
    # summary is what compare.py reads.
    tag = "unittest-nosummary"
    write_cell(cell(tag), clean_rows, summary=False)
    check("episodes without a summary are not complete",
          complete("qwen3.8:27b", "no-checklist", tag, 8, 5) is False)

    # An episode that will not parse is not a cell with one fewer episode.
    tag = "unittest-corrupt"
    d = write_cell(cell(tag), clean_rows)
    with open(os.path.join(d, "t3.rep2.json"), "w") as f:
        f.write("{ truncated")
    check("a corrupt episode is not complete",
          complete("qwen3.8:27b", "no-checklist", tag, 8, 5) is False)
finally:
    shutil.rmtree(_sandbox, ignore_errors=True)
    os.environ.pop("MH_RESULTS", None)

print("sole tenant (Mac / default)")
check("alone ok", blocking_residents(qwen, [qwen]) == [])
check("peer blocks", blocking_residents(qwen, [qwen, ornith]) == [ornith])
check("empty resident", blocking_residents(qwen, []) == [])

print("share-gpu (GH200, one peer)")
check("qwen + ornith allowed",
      blocking_residents(qwen, [qwen, ornith], share_gpu=True) == [])
check("ornith joining qwen allowed",
      blocking_residents(ornith, [qwen], share_gpu=True) == [])
third = blocking_residents(gemma, [qwen, ornith], share_gpu=True)
check("third model blocked",
      len(third) == 1 and third[0] in (qwen, ornith), str(third))
check("share alone ok", blocking_residents(qwen, [qwen], share_gpu=True) == [])

print("rep-offset extension")
import tempfile
from mh.runtime import (CellError, extension_guard, extension_reps,
                        protocol_block, protocol_diff, resume_conflicts,
                        sibling_cells, sibling_overlap, write_episode,
                        write_json_atomic, write_summary)
from run import seed_for_repeat

ENV = {"node": "testhost", "gpu": None, "platform": "test/arm64",
       "ollama_version": None}
PROTO_A = protocol_block(max_steps=0, max_wall=1800, share_gpu=False,
                         num_ctx=32768, num_predict=4096, temperature=0.6,
                         think=True, env=ENV)
PROTO_B = protocol_block(max_steps=40, max_wall=1800, share_gpu=False,
                         num_ctx=32768, num_predict=4096, temperature=0.6,
                         think=True, env=ENV)
RUN_A = {"tag": "ext", "seed_base": 0, "repeat": 15, "rep_offset": 5,
         "started": "2026-08-24T00:00:00Z"}


def ep_payload(task, rep, protocol=PROTO_A, run=RUN_A, passed=True,
               model="m", config=None, **row):
    r = {"task": task, "rep": rep, "seed": rep, "passed": passed, "steps": 6,
         "output_tokens": 100, "wall_s": 12.0, "stop_reason": "finished"}
    r.update(row)
    ep = {"model": model, "config": config or {"name": "full", "envboot": True},
          "task": task, "row": r, "verify_output": "", "events": []}
    if protocol is not None:
        ep["protocol"] = protocol
    if run is not None:
        ep["run"] = run
    return ep


def fill(d, tasks, reps, **kw):
    os.makedirs(d, exist_ok=True)
    for rep in reps:
        for t in tasks:
            write_episode(d, t, rep, ep_payload(t, rep, **kw))
    return d


check("default offset is the old range", extension_reps(0, 5) == [0, 1, 2, 3, 4])
check("offset 5 repeat 15 covers 5..19",
      extension_reps(5, 15) == list(range(5, 20)))

# The frozen cells ran unseeded at repeat=5, so seed == rep index. An
# extension must keep that, or a paired delta compares different seeds.
frozen = [seed_for_repeat(None, r, 5) for r in extension_reps(0, 5)]
ext = [seed_for_repeat(None, r, 15 + 5) for r in extension_reps(5, 15)]
check("frozen convention is seed == rep", frozen == [0, 1, 2, 3, 4])
check("extension keeps seed == rep", ext == list(range(5, 20)))
check("extension seeds never revisit frozen ones",
      not (set(frozen) & set(ext)))

TASKS = ["nfa_match", "json_patch"]
with tempfile.TemporaryDirectory() as tmp:
    # Each fixture gets its own root: two cells under one root are SIBLINGS
    # for the same (model, config), which is its own refusal below.
    def root(name):
        p = os.path.join(tmp, name)
        os.makedirs(p, exist_ok=True)
        return p

    fresh = os.path.join(root("r-fresh"), "m__full__fresh")
    check("offset 0 is unguarded",
          extension_guard(fresh, TASKS, extension_reps(0, 5), 0, False) is None)
    check("negative offset refused",
          "must be >= 0" in (extension_guard(fresh, TASKS, [], -1, False) or ""))
    check("clean extension allowed",
          extension_guard(fresh, TASKS, extension_reps(5, 15), 5, False) is None)
    check("force refused with offset",
          "--force" in (extension_guard(fresh, TASKS, extension_reps(5, 15),
                                        5, True) or ""))
    # The cell is (model, config); the tag is a directory name. A directory
    # that does not name one cannot be checked against its siblings, and a
    # check that cannot run must not pass.
    check("unparseable cell directory refused",
          "slug__config__tag" in (extension_guard(
              os.path.join(tmp, "loose"), TASKS, extension_reps(5, 15), 5,
              False) or ""))

    # Another run's episodes, with no provenance to say otherwise.
    cell = fill(os.path.join(root("r-cell"), "m__full__cell"), TASKS, range(5),
                protocol=None, run=None)
    with open(os.path.join(cell, "summary.json"), "w") as f:
        f.write("{}")

    r = extension_guard(cell, TASKS, extension_reps(5, 15), 5, False,
                        protocol=PROTO_A)
    check("refuses to rewrite an existing summary",
          r is not None and "summary.json" in r, str(r))

    os.remove(os.path.join(cell, "summary.json"))
    # Previously allowed: the guard only looked at the requested window, so an
    # extension could settle into a directory holding another run's repeats
    # and grow that cell's summary from 5 repeats to 20. The window being
    # clear is not enough.
    r = extension_guard(cell, TASKS, extension_reps(5, 15), 5, False,
                        protocol=PROTO_A)
    check("refuses to land beside another run's episodes",
          r is not None and "from another run" in r, str(r))

    r = extension_guard(cell, TASKS, extension_reps(3, 15), 3, False,
                        protocol=PROTO_A)
    check("refuses to overwrite an existing rep",
          r is not None and "nfa_match.rep3.json" in r, str(r))
    check("refusal counts every collision",
          r is not None and "lands on 4 existing" in r, str(r))

    # A5: our own interrupted extension. Same protocol stamp, so these
    # episodes are ours to resume -- which is what "clean offset past the last
    # rep is allowed" has always meant. Refusing them left a crashed or
    # starve-aborted extension unresumable by any flag.
    mine = fill(os.path.join(root("r-mine"), "m__full__mine"), TASKS, range(5, 8))
    r = extension_guard(mine, TASKS, extension_reps(5, 15), 5, False,
                        protocol=PROTO_A)
    check("clean offset past the last rep is allowed", r is None, str(r))
    with open(os.path.join(mine, "summary.json"), "w") as f:
        f.write("{}")
    r = extension_guard(mine, TASKS, extension_reps(5, 15), 5, False,
                        protocol=PROTO_A)
    check("a starve-aborted extension can be resumed", r is None, str(r))
    r = extension_guard(mine, TASKS, extension_reps(5, 15), 5, False,
                        protocol=PROTO_B)
    check("resuming under a different protocol is not a resume",
          r is not None and "nfa_match.rep5.json" in r, str(r))

    # A summary with nothing left to re-derive it from.
    orphan = os.path.join(root("r-orphan"), "m__full__orphan")
    os.makedirs(orphan)
    with open(os.path.join(orphan, "summary.json"), "w") as f:
        f.write("{}")
    r = extension_guard(orphan, TASKS, extension_reps(5, 15), 5, False,
                        protocol=PROTO_A)
    check("refuses a summary with no episodes beside it",
          r is not None and "no episodes beside it" in r, str(r))

print("sibling cells (a cell is model x config, not a tag)")
with tempfile.TemporaryDirectory() as tmp:
    frozen_cell = fill(os.path.join(tmp, "m__full__hard"), TASKS, range(5))
    fresh_cell = os.path.join(tmp, "m__full__hard-ext")
    other_cfg = fill(os.path.join(tmp, "m__baseline__hard"), TASKS, range(5))
    other_model = fill(os.path.join(tmp, "n__full__hard"), TASKS, range(5))
    sibs = [os.path.basename(p) for p in sibling_cells(fresh_cell)]
    check("siblings are the same (model, config) under another tag",
          sibs == ["m__full__hard"], str(sibs))
    check("another config is not a sibling", "m__baseline__hard" not in sibs)
    check("another model is not a sibling", "n__full__hard" not in sibs)

    ov = sibling_overlap(fresh_cell, TASKS, extension_reps(3, 5))
    check("sibling overlap names the frozen episodes",
          [os.path.basename(k) for k in ov] == ["m__full__hard"] and
          "nfa_match.rep3.json" in ov[frozen_cell], str(ov))
    check("no overlap past the frozen window",
          sibling_overlap(fresh_cell, TASKS, extension_reps(5, 15)) == {})

    # A3: the guard used to inspect only the new tag's own directory, so this
    # was allowed against an empty directory and silently re-ran repeats 3 and
    # 4 that the (model, config) already had.
    r = extension_guard(fresh_cell, TASKS, extension_reps(3, 5), 3, False,
                        protocol=PROTO_A)
    check("extension into a fresh tag sees the frozen cell",
          r is not None and "under another tag" in r, str(r))
    check("the refusal names the sibling",
          r is not None and "m__full__hard" in r, str(r))
    check("an extension past the frozen window is still allowed",
          extension_guard(fresh_cell, TASKS, extension_reps(5, 15), 5, False,
                          protocol=PROTO_A) is None)
    # A plain re-run at offset 0 is how every v1/v2/rep tag in the tree was
    # made. It is not refused -- it is reported, and run.py records it.
    check("offset 0 into a fresh tag is reported, not refused",
          extension_guard(fresh_cell, TASKS, extension_reps(0, 5), 0, False,
                          protocol=PROTO_A) is None)
    check("offset 0 overlap is still visible to the caller",
          len(sibling_overlap(fresh_cell, TASKS, extension_reps(0, 5))) == 1)


print("summary.json is derived from the cell, not from one invocation's rows")
FULL = {"name": "full", "envboot": True, "max_steps": 0, "wall_s": 1800}
with tempfile.TemporaryDirectory() as tmp:
    # The published shape: `--tasks globmatch` ran in a directory that already
    # held nine episodes, and the summary was rebuilt from the one task's rows.
    d = fill(os.path.join(tmp, "gemma4_e4b__full__grid"),
             ["ast_transformer", "globmatch", "navigate"], [0],
             model="gemma4:e4b", config=FULL)
    stale = {"model": "gemma4:e4b", "config": FULL, "n": 1, "passed": 1,
             "rows": [{"task": "globmatch", "rep": 0, "passed": True,
                       "wall_s": 12.0, "output_tokens": 100}]}
    with open(os.path.join(d, "summary.json"), "w") as f:
        json.dump(stale, f)
    s = write_summary(d, "gemma4:e4b", FULL, protocol=PROTO_A)
    check("summary counts every episode on disk, not the ones in --tasks",
          s["n"] == 3, f"n={s['n']}")
    check("every task on disk is in the rebuilt summary",
          sorted(r["task"] for r in s["rows"]) ==
          ["ast_transformer", "globmatch", "navigate"])
    check("derived totals cover the whole cell",
          s["output_tokens"] == 300 and s["wall_s"] == 36.0,
          f"{s['output_tokens']} {s['wall_s']}")
    check("the rebuilt summary is what is on disk",
          json.load(open(os.path.join(d, "summary.json")))["n"] == 3)
    check("rows are ordered by (rep, task)",
          [r["task"] for r in s["rows"]] ==
          ["ast_transformer", "globmatch", "navigate"])
    check("derived_from records the episode count",
          s["derived_from"]["episodes"] == 3)

    # Adding a repeat re-derives the whole cell, not just the new repeat.
    fill(d, ["ast_transformer", "globmatch", "navigate"], [1],
         model="gemma4:e4b", config=FULL)
    s = write_summary(d, "gemma4:e4b", FULL, protocol=PROTO_A)
    check("a second repeat extends the derived summary", s["n"] == 6)
    check("reps span what is on disk", s["reps"] == [0, 1], str(s["reps"]))

    # Never shrink a cell: if the summary claims a row with no episode behind
    # it, the episodes are not the whole record and rebuilding would delete it.
    ghost = json.load(open(os.path.join(d, "summary.json")))
    ghost["rows"].append({"task": "vanished", "rep": 0, "passed": True})
    with open(os.path.join(d, "summary.json"), "w") as f:
        json.dump(ghost, f)
    try:
        write_summary(d, "gemma4:e4b", FULL, protocol=PROTO_A)
        check("refuses to rebuild a summary over a missing episode", False,
              "no CellError")
    except CellError as e:
        check("refuses to rebuild a summary over a missing episode",
              "vanished.rep0" in str(e), str(e))
        check("the summary that would have shrunk is left alone",
              len(json.load(open(os.path.join(d, "summary.json")))["rows"]) == 7)

with tempfile.TemporaryDirectory() as tmp:
    d = fill(os.path.join(tmp, "m__full__t"), ["a", "b"], [0], config=FULL)
    with open(os.path.join(d, "a.rep0.json"), "w") as f:
        f.write('{"row": {"task": "a", "rep": 0}, "trunc')
    try:
        write_summary(d, "m", FULL, protocol=PROTO_A)
        check("an unreadable episode refuses the whole cell", False, "no CellError")
    except CellError as e:
        check("an unreadable episode refuses the whole cell",
              "a.rep0.json" in str(e), str(e))
    check("no summary was written over the refusal",
          not os.path.isfile(os.path.join(d, "summary.json")))

    # A filename and a row that disagree is a cell nobody can count.
    d2 = fill(os.path.join(tmp, "m__full__u"), ["a"], [0], config=FULL)
    ep = json.load(open(os.path.join(d2, "a.rep0.json")))
    ep["row"]["rep"] = 4
    with open(os.path.join(d2, "a.rep0.json"), "w") as f:
        json.dump(ep, f)
    try:
        write_summary(d2, "m", FULL, protocol=PROTO_A)
        check("filename and row must agree on the repeat", False, "no CellError")
    except CellError as e:
        check("filename and row must agree on the repeat",
              "rep 4" in str(e), str(e))

    d3 = fill(os.path.join(tmp, "m__full__v"), ["a"], [0], config=FULL)
    try:
        write_summary(d3, "other-model", FULL, protocol=PROTO_A)
        check("a cell will not answer for another model's episodes", False,
              "no CellError")
    except CellError as e:
        check("a cell will not answer for another model's episodes",
              "'m'" in str(e), str(e))

print("per-episode provenance: one run cannot claim another run's episodes")
with tempfile.TemporaryDirectory() as tmp:
    d = os.path.join(tmp, "m__full__mixed")
    fill(d, ["a"], [0], protocol=PROTO_A, config=FULL)
    fill(d, ["a"], [1], protocol=PROTO_B, config=FULL)
    s = write_summary(d, "m", FULL, protocol=PROTO_A)
    check("a cell that ran under two protocols records neither as its own",
          s["protocol"] is None)
    variants = s.get("protocol_variants") or []
    check("both protocols are recorded, with the episodes that ran under them",
          len(variants) == 2 and
          {e for v in variants for e in v["episodes"]} ==
          {"a.rep0.json", "a.rep1.json"}, str(variants))
    check("the variant carries the max_steps each episode really used",
          sorted((v["protocol"] or {}).get("max_steps") for v in variants)
          == [0, 40], str(variants))

    single = os.path.join(tmp, "m__full__single")
    fill(single, ["a"], [0, 1], protocol=PROTO_A, config=FULL)
    s = write_summary(single, "m", FULL, protocol=PROTO_A)
    check("a cell that ran under one protocol records it", s["protocol"] == PROTO_A)
    check("no variants when there is nothing to disagree about",
          "protocol_variants" not in s)

    # Legacy episodes carry no stamp. The run that rebuilds the summary must
    # not lend them its own.
    legacy = os.path.join(tmp, "m__full__legacy")
    fill(legacy, ["a"], [0], protocol=None, run=None, config=FULL)
    s = write_summary(legacy, "m", FULL, protocol=PROTO_A)
    check("an unstamped episode does not inherit this run's protocol",
          s["protocol"] is None)
    check("unstamped episodes are named in the variants",
          s.get("protocol_variants") == [{"protocol": None,
                                          "episodes": ["a.rep0.json"]}],
          str(s.get("protocol_variants")))

    # Same for the config the episodes actually ran under.
    cfgd = os.path.join(tmp, "m__full__cfg")
    fill(cfgd, ["a"], [0], config=dict(FULL, max_steps=0))
    fill(cfgd, ["a"], [1], config=dict(FULL, max_steps=40))
    s = write_summary(cfgd, "m", FULL, protocol=PROTO_A)
    check("a config flag two episodes disagree on is recorded as unknown",
          s["config"]["max_steps"] is None and s["config"]["name"] == "full",
          str(s["config"]))
    check("config variants name the episodes",
          len(s.get("config_variants") or []) == 2)

    twoname = os.path.join(tmp, "m__full__twoname")
    fill(twoname, ["a"], [0], config=FULL)
    fill(twoname, ["a"], [1], config={"name": "baseline"})
    try:
        write_summary(twoname, "m", FULL, protocol=PROTO_A)
        check("two config names is not one cell", False, "no CellError")
    except CellError as e:
        check("two config names is not one cell", "2 configs" in str(e), str(e))

print("resume_conflicts: refuse before running, not after writing")
with tempfile.TemporaryDirectory() as tmp:
    d = fill(os.path.join(tmp, "m__full__resume"), ["a", "b"], [0],
             protocol=PROTO_A, config=FULL)
    conf, unattr = resume_conflicts(d, ["a", "b"], [0], PROTO_A)
    check("resuming under the same protocol is no conflict", conf == [] and unattr == [])
    conf, unattr = resume_conflicts(d, ["a", "b"], [0], PROTO_B)
    check("resuming with different flags is a conflict", len(conf) == 2, str(conf))
    check("the conflict names the setting that moved",
          bool(conf) and conf[0][1] == ["max_steps"], str(conf))
    conf, _ = resume_conflicts(d, ["a", "b"], [0], PROTO_B, force=True)
    check("--force re-runs them, so there is nothing to conflict with", conf == [])

    # A starved episode is re-run, not kept, so it cannot conflict either.
    fill(d, ["c"], [0], protocol=PROTO_B, config=FULL, passed=False, steps=1,
         output_tokens=0, stop_reason="wall_timeout")
    conf, _ = resume_conflicts(d, ["c"], [0], PROTO_A)
    check("an episode that will be re-run is not a conflict", conf == [], str(conf))

    legacy = fill(os.path.join(tmp, "m__full__unstamped"), ["a"], [0],
                  protocol=None, run=None, config=FULL)
    conf, unattr = resume_conflicts(legacy, ["a"], [0], PROTO_A)
    check("an unstamped episode is unattributed, not a conflict",
          conf == [] and unattr == ["a.rep0.json"], str(unattr))
    check("protocol_diff calls an absent stamp a disagreement",
          protocol_diff(None, PROTO_A) == ["<unrecorded>"])

print("atomic writes")
with tempfile.TemporaryDirectory() as tmp:
    p = os.path.join(tmp, "x.json")
    write_json_atomic(p, {"a": 1})
    check("writes what it was given", json.load(open(p)) == {"a": 1})
    write_json_atomic(p, {"a": 2})
    check("replaces in place", json.load(open(p)) == {"a": 2})
    check("leaves no temp files behind", os.listdir(tmp) == ["x.json"],
          str(os.listdir(tmp)))
    # open(path, "w") truncates before the replacement exists. A payload that
    # fails to encode halfway through used to leave the frozen episode empty.
    class Unserialisable:
        pass
    def readback(path):
        try:
            with open(path) as f:
                return json.load(f)
        except (OSError, ValueError) as e:
            return f"unreadable: {e}"
    try:
        write_json_atomic(p, {"a": 3, "bad": Unserialisable()})
        check("a failed write is refused, not half-applied", False, "no error")
    except TypeError:
        check("a failed write is refused, not half-applied",
              readback(p) == {"a": 2}, str(readback(p)))
    check("a failed write leaves no temp file", os.listdir(tmp) == ["x.json"],
          str(os.listdir(tmp)))

print("is_api_model has one owner")
from mh.runtime import is_api_model
import mh.runtime as _runtime
import mh.model as _model
check("runtime owns the predicate under both names",
      getattr(_runtime, "is_gemini_model", None) is is_api_model)
CASES = ["gemini-3.7-flash", "models/gemini-3.7-flash", "live-gemini",
         "qwen3.8:27b", "hf.co/ornith-ai/Ornith-1.5-35B-A3B-GGUF:Q8_0",
         "gemma4:e4b", "not-gemini-really", "cerebras:gpt-oss-120b",
         "cerebras:gemma-4-31b", "gpt-oss-120b", "gemma-4-31b"]
check("mh.model's copy still agrees with the owner",
      all(_model.is_gemini_model(m) == is_api_model(m) for m in CASES),
      "mh/model.py must import is_gemini_model from mh.runtime")
from mh.runtime import api_model_id, api_provider
check("api_provider routes each name to exactly one provider",
      [api_provider(m) for m in CASES]
      == ["gemini", "gemini", "gemini", None, None, None, None,
          "cerebras", "cerebras", None, None],
      str([api_provider(m) for m in CASES]))
# `gpt-oss-120b` and `gemma-4-31b` are also local ollama tags. A bare name that
# means the hosted model on one host and a local quantised build on another is
# the confusion the protocol stamp exists to prevent, so the prefix is required.
check("a bare hosted-model name stays local",
      not is_api_model("gpt-oss-120b") and not is_api_model("gemma-4-31b"))
check("api_model_id strips the routing prefix and nothing else",
      [api_model_id(m) for m in CASES]
      == ["gemini-3.7-flash", "models/gemini-3.7-flash", "live-gemini",
          "qwen3.8:27b", "hf.co/ornith-ai/Ornith-1.5-35B-A3B-GGUF:Q8_0",
          "gemma4:e4b", "not-gemini-really", "gpt-oss-120b", "gemma-4-31b",
          "gpt-oss-120b", "gemma-4-31b"],
      str([api_model_id(m) for m in CASES]))
check("an API model is never asked to evict a local one",
      _runtime.ensure_sole_tenant("cerebras:gpt-oss-120b") == [])


print("an API arm does not claim this box as its serving hardware")
from mh.runtime import serving_env
from mh.pool import PROTOCOL_KEYS, protocol_drift
from mh.runtime import keep_existing_episode
api_env = serving_env(model="cerebras:gpt-oss-120b")
check("the provider and endpoint are what is recorded",
      api_env["api_provider"] == "cerebras"
      and api_env["api_endpoint"].startswith("https://api.cerebras.ai/"),
      str(api_env))
check("no local GPU, node or ollama version is stamped on a remote episode",
      (api_env["gpu"], api_env["node"], api_env["ollama_version"],
       api_env["platform"]) == (None, None, None, None), str(api_env))
check("the box that ran the loop is still recorded as provenance",
      "client_node" in api_env)
check("but client_node is not a pooling key",
      "env_client_node" not in PROTOCOL_KEYS, str(PROTOCOL_KEYS))
# Two identical remote cells launched from different laptops must still pool;
# two cells against different providers must not.
LAP_A = dict(api_env, client_node="laptop-a")
LAP_B = dict(api_env, client_node="laptop-b")
P_A = protocol_block(max_steps=0, max_wall=1800, share_gpu=False, num_ctx=65536,
                     num_predict=16384, temperature=0.6, think=True, env=LAP_A,
                     top_p=0.95, reasoning_effort="medium")
P_B = protocol_block(max_steps=0, max_wall=1800, share_gpu=False, num_ctx=65536,
                     num_predict=16384, temperature=0.6, think=True, env=LAP_B,
                     top_p=0.95, reasoning_effort="medium")
check("the same remote cell run from two machines still pools",
      protocol_drift(P_A, P_B) == [], str(protocol_drift(P_A, P_B)))
P_C = protocol_block(max_steps=0, max_wall=1800, share_gpu=False, num_ctx=65536,
                     num_predict=16384, temperature=0.6, think=True, env=LAP_A,
                     top_p=0.95, reasoning_effort="high")
check("two reasoning efforts are not one experiment",
      protocol_drift(P_A, P_C) == ["reasoning_effort"],
      str(protocol_drift(P_A, P_C)))
check("a stamp written before these keys existed reads as agreeing with itself",
      protocol_drift(PROTO_A, dict(PROTO_A)) == [])

# protocol_diff (denylist over every key present) and mh.pool.protocol_drift
# (allowlist) answer the same question and must not disagree. They did: a cell
# resumed from a second laptop pooled cleanly by one and published
# protocol_variants by the other, purely on the hostname.
from mh.runtime import PROVENANCE_KEYS, protocol_diff, resume_conflicts


def _resume_says_conflict(written_stamp, running_stamp):
    """Does resume_conflicts refuse to resume a cell stamped `written_stamp`?"""
    import tempfile as _tf
    with _tf.TemporaryDirectory() as d:
        write_episode(d, "alpha", 0, {"model": "m", "config": {}, "task": "alpha",
                                      "row": {"task": "alpha", "rep": 0,
                                              "passed": True, "steps": 5,
                                              "output_tokens": 900,
                                              "stop_reason": "finished"},
                                      "protocol": written_stamp, "run": RUN_A})
        return bool(resume_conflicts(d, ["alpha"], [0], running_stamp)[0])


def resume_conflicts_probe():
    """Resume a one-episode cell whose stamp differs only in provenance."""
    import tempfile as _tf
    with _tf.TemporaryDirectory() as d:
        stamp = dict(P_A, env_client_node="host-that-wrote-it")
        now = dict(P_A, env_client_node="host-running-now")
        write_episode(d, "alpha", 0, {"model": "m", "config": {}, "task": "alpha",
                                      "row": {"task": "alpha", "rep": 0,
                                              "passed": True, "steps": 5,
                                              "output_tokens": 900,
                                              "stop_reason": "finished"},
                                      "protocol": stamp, "run": RUN_A})
        conflicts, _ = resume_conflicts(d, ["alpha"], [0], now)
        assert not conflicts, f"provenance must not block a resume: {conflicts}"
        # and a genuine difference still conflicts, with a name attached
        other = dict(now, reasoning_effort="high")
        return resume_conflicts(d, ["alpha"], [0], other)[0]
check("provenance keys are excluded from protocol_diff too",
      protocol_diff(P_A, LAP_HOST_B := dict(P_A, env_client_node="other-host")) == [],
      str(protocol_diff(P_A, LAP_HOST_B)))
check("the allowlist and the denylist agree about every provenance key",
      all(k not in PROTOCOL_KEYS for k in PROVENANCE_KEYS),
      f"{PROVENANCE_KEYS} vs {PROTOCOL_KEYS}")
# resume_conflicts decided with `stamp != protocol` while reporting with
# protocol_diff. Once provenance was excluded from the diff the two disagreed:
# 90 keepable episodes were refused with an empty "differs on " list, and the
# cell could not be resumed from a second machine at all.
check("a conflict is never reported with nothing to name",
      all(d for _n, d in resume_conflicts_probe()),
      "resume_conflicts must decide with protocol_diff")
# _merge_protocols grouped by the whole stamp, so a cell resumed from a second
# machine published protocol_variants over two groups and the runner declared it
# would not pool -- while mh.pool and protocol_diff both said the episodes
# agreed. Fourth place with its own answer to "same protocol?".
from mh.runtime import _merge_protocols, protocol_identity
CELL_TWO_HOSTS = [
    ("a.rep0.json", {"protocol": dict(P_A, env_client_node="host-a")}),
    ("b.rep1.json", {"protocol": dict(P_A, env_client_node="host-b")}),
]
merged, variants = _merge_protocols(CELL_TWO_HOSTS)
check("two hosts, one experiment: the cell has a single protocol",
      merged is not None and not variants, str(variants)[:120])
check("and it does not claim either host for all of them",
      merged is not None and merged.get("env_client_node") is None,
      str(merged and merged.get("env_client_node")))
check("a genuine protocol split still produces variants",
      _merge_protocols([("a.rep0.json", {"protocol": P_A}),
                        ("b.rep1.json", {"protocol": P_C})])[0] is None)
check("one host is still recorded when every episode agrees",
      _merge_protocols([("a.rep0.json", {"protocol": dict(P_A, env_client_node="h")}),
                        ("b.rep1.json", {"protocol": dict(P_A, env_client_node="h")})]
                       )[0]["env_client_node"] == "h")
check("protocol_identity drops exactly the provenance keys",
      set(P_A) - set(protocol_identity(P_A)) == set(PROVENANCE_KEYS),
      str(set(P_A) - set(protocol_identity(P_A))))
check("a real protocol difference is still caught by both",
      protocol_diff(P_A, P_C) == ["reasoning_effort"]
      and protocol_drift(P_A, P_C) == ["reasoning_effort"],
      f"{protocol_diff(P_A, P_C)} / {protocol_drift(P_A, P_C)}")

# Four places answer "are these two stamps the same experiment?", and each one
# grew its own answer. Every one of them was wrong at least once this week:
#   mh.pool.protocol_drift   allowlist   - said they agreed
#   runtime.protocol_diff    denylist    - said they disagreed
#   resume_conflicts         dict !=     - refused a resume, naming nothing
#   _merge_protocols         dict group  - published two protocols for one cell
# The bug was never in any single one; it was that there were four. This asserts
# they agree on the case that separates them, so a fifth caller that reaches for
# `==` fails here rather than in a published number.
print("every protocol comparison agrees on what counts as the same experiment")
PROV_ONLY_A = dict(P_A, env_client_node="host-one")
PROV_ONLY_B = dict(P_A, env_client_node="host-two")
REAL_A, REAL_B = PROV_ONLY_A, dict(PROV_ONLY_A, num_ctx=4096)
verdicts_same = {
    "pool.protocol_drift": not protocol_drift(PROV_ONLY_A, PROV_ONLY_B),
    "runtime.protocol_diff": not protocol_diff(PROV_ONLY_A, PROV_ONLY_B),
    "resume_conflicts": not _resume_says_conflict(PROV_ONLY_A, PROV_ONLY_B),
    "_merge_protocols": _merge_protocols(
        [("a.rep0.json", {"protocol": PROV_ONLY_A}),
         ("b.rep1.json", {"protocol": PROV_ONLY_B})])[0] is not None,
}
check("all four call provenance-only differences the SAME experiment",
      all(verdicts_same.values()),
      str({k: v for k, v in verdicts_same.items() if not v}))
verdicts_diff = {
    "pool.protocol_drift": bool(protocol_drift(REAL_A, REAL_B)),
    "runtime.protocol_diff": bool(protocol_diff(REAL_A, REAL_B)),
    "resume_conflicts": _resume_says_conflict(REAL_A, REAL_B),
    "_merge_protocols": _merge_protocols(
        [("a.rep0.json", {"protocol": REAL_A}),
         ("b.rep1.json", {"protocol": REAL_B})])[0] is None,
}
check("and all four call a real num_ctx difference DIFFERENT experiments",
      all(verdicts_diff.values()),
      str({k: v for k, v in verdicts_diff.items() if not v}))


print("concurrent runners never evict each other's model")
# Parallelising the grid is the obvious way to use a rented GH200, and it was
# unsafe in a way nothing reported: every runner calls unload_all(keep=its own
# model), which evicts a sibling's weights mid-episode. The victim's next
# /api/chat returns 0 tokens and times out, and lands as a scored failure with
# nothing in the row naming the cause.
from mh.runtime import (active_runners, conflicting_runners,
                        ensure_sole_tenant, register_runner, release_runner)
with tempfile.TemporaryDirectory() as tmp:
    mine = register_runner(tmp, "qwen3.8:27b", pid=os.getpid())
    check("a runner announces the model it holds",
          [r["model"] for r in active_runners(tmp)] == ["qwen3.8:27b"])
    check("a sibling on the SAME model is not a conflict",
          conflicting_runners(tmp, "qwen3.8:27b", pid=-1) == [])
    check("a sibling on a DIFFERENT model is",
          [c["model"] for c in conflicting_runners(tmp, "ornith:35b", pid=-1)]
          == ["qwen3.8:27b"])
    try:
        ensure_sole_tenant("ornith:35b", results_root=tmp, pid=-1)
        check("ensure_sole_tenant refuses to evict a live sibling", False,
              "it did not refuse")
    except RuntimeError as e:
        check("ensure_sole_tenant refuses to evict a live sibling",
              "another runner on this box holds" in str(e), str(e))
        check("and the refusal names the model and pid it would have killed",
              "qwen3.8:27b" in str(e) and str(os.getpid()) in str(e), str(e))
    # A crashed runner must not lock the GPU against every later one.
    register_runner(tmp, "ghost:1b", pid=2 ** 30)
    check("a lease whose process is gone is discarded, not believed",
          [r["model"] for r in active_runners(tmp)] == ["qwen3.8:27b"])
    check("the stale lease file is removed",
          sorted(os.listdir(os.path.join(tmp, ".runners")))
          == [f"{os.getpid()}.json"],
          str(os.listdir(os.path.join(tmp, ".runners"))))
    release_runner(mine)
    check("releasing clears the lease", active_runners(tmp) == [])
    check("an API model needs no tenancy at all",
          ensure_sole_tenant("cerebras:gpt-oss-120b", results_root=tmp) == [])

print("the real decode-slot count, not the declared ceiling")
# OLLAMA_NUM_PARALLEL is documented as a MAXIMUM. On ollama 0.33.0 it read 4
# and the runner launched with -np 1: four concurrent requests all landed on
# slot 0 and aggregate throughput was 1.01x serial. A preflight that checks the
# variable is checking an intention, and would have waved through a grid whose
# extra cells queue while their wall clock runs into --max-wall.
import mh.runtime as _rtm
_real_run = _rtm.subprocess.run


class _PS:
    def __init__(self, out): self.returncode, self.stdout = 0, out


try:
    _rtm.subprocess.run = lambda *a, **k: _PS(
        "/usr/local/lib/ollama/llama-server --model /x -c 32768 -np 1 "
        "--flash-attn auto\n")
    check("a single-slot runner is reported as 1",
          _rtm.observed_parallel_slots() == 1,
          str(_rtm.observed_parallel_slots()))
    _rtm.subprocess.run = lambda *a, **k: _PS(
        "/usr/local/lib/ollama/llama-server --model /x -c 131072 -np 4\n")
    check("a four-slot runner is reported as 4",
          _rtm.observed_parallel_slots() == 4)
    _rtm.subprocess.run = lambda *a, **k: _PS("bash\nsshd\n")
    check("no runner loaded reads as unknown, not as agreement",
          _rtm.observed_parallel_slots() is None)
    def _boom(*a, **k):
        raise OSError("ps unavailable")
    _rtm.subprocess.run = _boom
    check("a platform that cannot answer reads as unknown too",
          _rtm.observed_parallel_slots() is None)
finally:
    _rtm.subprocess.run = _real_run


print("concurrency is part of the protocol")
P_SOLO = protocol_block(max_steps=0, max_wall=1800, share_gpu=False,
                        num_ctx=32768, num_predict=4096, temperature=0.6,
                        think=True, env=ENV, concurrency=1)
P_PAR = protocol_block(max_steps=0, max_wall=1800, share_gpu=False,
                       num_ctx=32768, num_predict=4096, temperature=0.6,
                       think=True, env=ENV, concurrency=4)
check("a cell run four-up is not the same experiment as one run alone",
      protocol_drift(P_SOLO, P_PAR) == ["concurrency"],
      str(protocol_drift(P_SOLO, P_PAR)))
check("and protocol_diff agrees",
      protocol_diff(P_SOLO, P_PAR) == ["concurrency"],
      str(protocol_diff(P_SOLO, P_PAR)))
check("concurrency is a pooling key",
      "concurrency" in PROTOCOL_KEYS, str(PROTOCOL_KEYS))
check("an older stamp without it still agrees with itself",
      protocol_drift(PROTO_A, dict(PROTO_A)) == [])


print("a dry run touches nothing")
import grid as _grid_mod
_TOUCHED = []
_real_est = _grid_mod.ensure_sole_tenant
_grid_mod.ensure_sole_tenant = lambda *a, **k: _TOUCHED.append(a) or []
_argv = sys.argv[:]
try:
    sys.argv = ["grid.py", "--models", "qwen3.8:27b", "--hard", "--parallel",
                "4", "--tag", "dryrun-probe", "--dry-run"]
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        try:
            _grid_mod.main()
        except SystemExit:
            pass
finally:
    sys.argv = _argv
    _grid_mod.ensure_sole_tenant = _real_est
# --parallel evicts once up front so the cells can carry --keep-resident. That
# pass ran before the dry-run check, so `--dry-run` unloaded the resident model
# and then printed that it intended to run nothing.
check("--dry-run with --parallel evicts nothing", _TOUCHED == [], str(_TOUCHED))
check("and it still prints a plan",
      "[grid] done" in buf.getvalue(), buf.getvalue()[-200:])


print("an account refusal is never scored as a model failure")
from mh.model import AccountRefused, ModelError
from mh.runtime import is_starved_episode, is_unserved_episode, unserved_count
REFUSED_OLD = {"task": "t", "rep": 0, "steps": 1, "output_tokens": 0,
               "passed": False, "stop_reason": "error:ModelError",
               "errors": ['ModelError: HTTP 402: {"message":"Payment required"}']}
REFUSED_NEW = {"task": "t", "rep": 1, "steps": 1, "output_tokens": 0,
               "passed": False, "stop_reason": "error:AccountRefused",
               "errors": ["AccountRefused: HTTP 402: quota"]}
REAL_FAIL = {"task": "t", "rep": 2, "steps": 9, "output_tokens": 900,
             "passed": False, "stop_reason": "error:ModelError",
             "errors": ["ModelError: HTTP 500: high demand"]}
check("AccountRefused is a ModelError, so existing handlers still catch it",
      issubclass(AccountRefused, ModelError))
check("a legacy 402 row is recognised", is_unserved_episode(REFUSED_OLD))
check("a post-fix refusal row is recognised", is_unserved_episode(REFUSED_NEW))
check("an ordinary 500 serving failure is NOT", not is_unserved_episode(REAL_FAIL))
check("nor is a normal row", not is_unserved_episode({"stop_reason": "finished"}))
check("unserved_count totals them", unserved_count(
      [REFUSED_OLD, REFUSED_NEW, REAL_FAIL]) == 2)
# The registered exclusion rule is timeouts only; this must not quietly widen it.
check("an unserved episode is NOT silently dropped from denominators",
      not is_starved_episode(REFUSED_OLD) and not is_starved_episode(REFUSED_NEW))
check("but it is re-run rather than kept on resume",
      not keep_existing_episode(REFUSED_OLD)
      and not keep_existing_episode(REFUSED_NEW))
# The episode-level and cell-level checks have to agree. They did not: run.py
# would have replaced a 402 row, but grid.complete() decided the cell holding
# 160 of them was finished and never invoked run.py, printing "skip complete"
# over a 0.0% pass rate.
import grid as _grid
check("a cell holding unserved episodes is not complete",
      _grid.unusable(REFUSED_OLD) and _grid.unusable(REFUSED_NEW),
      "grid.complete() must refuse a cell of account refusals")
check("a starved episode still counts as unusable too",
      _grid.unusable({"steps": 1, "output_tokens": 0,
                      "stop_reason": "wall_timeout"}))
check("a real failure does not make a cell incomplete",
      not _grid.unusable(REAL_FAIL) and not _grid.unusable(
          {"stop_reason": "finished", "passed": True}))


print("run.py end to end (no server, no model, no GPU, throwaway tree)")
import contextlib, io, types
import run as runner
import mh.runtime as _rt


class FakeTask:
    def __init__(self, name):
        self.name = name

    def materialise(self, sandbox):
        os.makedirs(sandbox, exist_ok=True)


class FakeSampler:
    def __init__(self, interval=2.0):
        pass

    def start(self):
        return self

    def stop(self):
        return None


class FakeRes:
    def __init__(self, **kw):
        d = dict(passed=True, finished=True, stop_reason="finished", steps=6,
                 tool_calls=4, wall_s=12.0, model_latency_s=10.0,
                 prompt_tokens=1000, output_tokens=100, errors=[],
                 peak_prompt_tokens=1000, eval_duration_ns=10 ** 9,
                 prompt_eval_duration_ns=10 ** 9, verify_output="ok", events=[])
        d.update(kw)
        self.__dict__.update(d)


def starved_res():
    return FakeRes(passed=False, finished=False, stop_reason="wall_timeout",
                   steps=1, tool_calls=0, output_tokens=0,
                   errors=["ModelError: TimeoutError: timed out"])


class FakeHarness:
    plan = {}
    ran = []

    def __init__(self, client, cfg, sandbox, task, log_dir=None):
        self.task = task

    def run(self):
        FakeHarness.ran.append(self.task.name)
        r = FakeHarness.plan.get(self.task.name)
        if r is None:
            return FakeRes()
        return r() if callable(r) else r


EVICTED = []
runner.Client = lambda *a, **k: types.SimpleNamespace(options={})
runner.Sampler = FakeSampler
runner.Harness = FakeHarness
runner.unstick_server = lambda *a, **k: None
runner.unload_all = lambda *a, **k: []
# Signature tracks run.py's call site: results_root/pid arrived with the runner
# lease that stops parallel cells evicting each other's weights.
runner.ensure_sole_tenant = (lambda model, evict=True, share_gpu=False,
                             results_root=None, pid=None:
                             EVICTED.append(model) or [])
runner.protocol_block = lambda **kw: _rt.protocol_block(env=ENV, **kw)


def run_main(argv, results, tasks=("alpha",), plan=None):
    """Drive run.main(). Returns (exit code or 0, stdout)."""
    FakeHarness.plan = dict(plan or {})
    FakeHarness.ran = []
    del EVICTED[:]
    runner.RESULTS = results
    runner.WORK = os.path.join(results, ".work")
    runner.load_tasks = lambda names: [FakeTask(n) for n in (names or tasks)]
    out = io.StringIO()
    saved = sys.argv
    sys.argv = ["run.py"] + list(argv)
    try:
        with contextlib.redirect_stdout(out):
            runner.main()
        return 0, out.getvalue()
    except SystemExit as e:
        return (e.code if e.code is not None else 0), out.getvalue()
    except Exception as e:
        # A crash is a failure to report, not a reason to stop the suite.
        return f"CRASH {type(e).__name__}: {e}", out.getvalue()
    finally:
        sys.argv = saved


def summary_of(results, tag, model="m", config="full"):
    with open(os.path.join(results, f"{model}__{config}__{tag}",
                           "summary.json")) as f:
        return json.load(f)


with tempfile.TemporaryDirectory() as tmp:
    # A9: --repeat 0 died on an IndexError in the banner, after makedirs and
    # after ensure_sole_tenant had already evicted the resident model.
    code, _ = run_main(["--model", "m", "--tag", "bad", "--repeat", "0"], tmp)
    check("--repeat 0 is refused",
          isinstance(code, str) and "--repeat must be >= 1" in code, str(code))
    check("--repeat 0 evicts nothing", EVICTED == [], str(EVICTED))
    check("--repeat 0 creates no cell directory",
          not os.path.exists(os.path.join(tmp, "m__full__bad")))
    code, _ = run_main(["--model", "m", "--tag", "bad", "--repeat", "-2"], tmp)
    check("negative --repeat is refused", isinstance(code, str), str(code))
    code, _ = run_main(["--model", "m", "--tag", "bad", "--starve-abort", "-1"], tmp)
    check("negative --starve-abort is refused",
          isinstance(code, str) and "--starve-abort" in code, str(code))

with tempfile.TemporaryDirectory() as tmp:
    # A1, end to end: the second invocation is handed one task in a directory
    # that already holds two. The summary must describe the cell.
    code, _ = run_main(["--model", "m", "--tag", "grid", "--tasks", "alpha,beta"], tmp)
    check("first run writes its episodes", code == 0, str(code))
    s = summary_of(tmp, "grid")
    check("summary of the first run", s["n"] == 2 and s["passed"] == 2)
    code, _ = run_main(["--model", "m", "--tag", "grid", "--tasks", "gamma"], tmp)
    check("adding one task does not rewrite the cell as one task",
          summary_of(tmp, "grid")["n"] == 3, str(summary_of(tmp, "grid")["n"]))
    check("the tasks already on disk survive the rebuild",
          sorted(r["task"] for r in summary_of(tmp, "grid")["rows"]) ==
          ["alpha", "beta", "gamma"])
    check("only the new task was actually run", FakeHarness.ran == ["gamma"],
          str(FakeHarness.ran))

    # A4: episodes and summary are written whole, with nothing left behind.
    d = os.path.join(tmp, "m__full__grid")
    leftovers = [n for n in os.listdir(d) if n.startswith(".tmp-")]
    check("no temp files left in the cell", leftovers == [], str(leftovers))
    ep = json.load(open(os.path.join(d, "alpha.rep0.json")))
    stamp = ep.get("protocol") or {}
    check("every episode carries the protocol it ran under",
          stamp.get("max_steps") == 0 and stamp.get("num_ctx") == 32768,
          str(ep.get("protocol")))
    check("every episode carries its run's identity",
          (ep.get("run") or {}).get("tag") == "grid" and
          (ep.get("run") or {}).get("rep_offset") == 0, str(ep.get("run")))

    # A2 + A8: resuming with a different ceiling would have kept those three
    # episodes and then stamped max_steps=40 over the lot.
    code, _ = run_main(["--model", "m", "--tag", "grid", "--tasks", "alpha,beta",
                        "--max-steps", "40"], tmp)
    check("resuming under a different protocol is refused",
          isinstance(code, str) and "different protocol" in code, str(code))
    check("the refusal names the setting that moved",
          isinstance(code, str) and "max_steps" in code, str(code))
    check("the refusal happens before anything runs", FakeHarness.ran == [],
          str(FakeHarness.ran))
    check("the summary it would have relabelled is untouched",
          (summary_of(tmp, "grid")["protocol"] or {}).get("max_steps") == 0,
          str(summary_of(tmp, "grid")["protocol"]))

    code, out = run_main(["--model", "m", "--tag", "grid", "--tasks",
                          "alpha,beta,delta", "--max-steps", "40",
                          "--force-starved"], tmp)
    check("--force-starved is read, and lets the resume through", code == 0,
          str(code))
    s = summary_of(tmp, "grid")
    check("the mixed cell claims no single protocol", s["protocol"] is None)
    check("it records both protocols and whose episodes are whose",
          sorted(len(v["episodes"])
                 for v in (s.get("protocol_variants") or [])) == [1, 3],
          str(s.get("protocol_variants")))
    check("the runner says so out loud", "no single protocol" in out, out[-300:])
    outc = s.get("outcomes") or {}
    check("outcomes record the flag that was used",
          outc.get("force_starved") is True and
          outc.get("episodes_written") == 1, str(outc))

with tempfile.TemporaryDirectory() as tmp:
    # A10: --starve-abort counts CONSECUTIVE starvation. A skipped real
    # episode breaks the run; leaving the streak standing aborted a resume on
    # starvation that was never consecutive.
    code, _ = run_main(["--model", "m", "--tag", "s", "--tasks", "beta"], tmp)
    check("the kept episode is on disk", code == 0, str(code))
    code, out = run_main(["--model", "m", "--tag", "s", "--tasks",
                          "alpha,beta,gamma", "--starve-abort", "2"], tmp,
                         plan={"alpha": starved_res, "gamma": starved_res})
    check("a skipped real episode resets the starve streak", code == 0,
          f"code={code} {out[-200:]}")
    check("both starved episodes ran", FakeHarness.ran.count("gamma") >= 1,
          str(FakeHarness.ran))
    # Genuinely consecutive starvation still aborts.
    code, out = run_main(["--model", "m", "--tag", "s2", "--tasks",
                          "alpha,gamma", "--starve-abort", "2"], tmp,
                         plan={"alpha": starved_res, "gamma": starved_res})
    check("consecutive starvation still aborts the cell", code == 2, str(code))
    check("the aborted cell still gets a derived summary",
          (summary_of(tmp, "s2").get("outcomes") or {}).get("starved_abort")
          is True)

with tempfile.TemporaryDirectory() as tmp:
    # A3, end to end: a fresh tag re-running repeats the (model, config)
    # already has is a duplicate sample. It is allowed -- every v1/v2/rep tag
    # in the tree is one -- but it is never silent.
    run_main(["--model", "m", "--tag", "hard", "--tasks", "alpha"], tmp)
    code, out = run_main(["--model", "m", "--tag", "hard-again", "--tasks",
                          "alpha"], tmp)
    check("a duplicate repeat under a new tag is reported", code == 0, str(code))
    check("the warning names the sibling cell",
          "WARNING" in out and "m__full__hard" in out, out[-300:])
    outc = summary_of(tmp, "hard-again").get("outcomes") or {}
    check("the duplication is recorded in the summary, not just printed",
          outc.get("sibling_overlap") == {"m__full__hard": 1},
          str(outc.get("sibling_overlap")))
    # An extension is refused when the repeats it would add already exist for
    # this (model, config) under ANY tag -- the check the fresh directory used
    # to hide.
    code, _ = run_main(["--model", "m", "--tag", "hard-ext", "--tasks", "alpha",
                        "--rep-offset", "1", "--repeat", "2"], tmp)
    check("an extension past the frozen repeats is allowed", code == 0, str(code))
    check("it wrote the repeats it said it would",
          summary_of(tmp, "hard-ext")["reps"] == [1, 2])
    code, _ = run_main(["--model", "m", "--tag", "hard-ext2", "--tasks", "alpha",
                        "--rep-offset", "1", "--repeat", "2"], tmp)
    check("a second extension onto the same repeats is refused",
          isinstance(code, str) and "under another tag" in code, str(code))

    # A5: an extension that died partway through resumes into its own
    # directory. The guard used to refuse its own output, and neither --force
    # nor --force-starved could get past it.
    os.remove(os.path.join(tmp, "m__full__hard-ext", "alpha.rep2.json"))
    code, _ = run_main(["--model", "m", "--tag", "hard-ext", "--tasks", "alpha",
                        "--rep-offset", "1", "--repeat", "2"], tmp)
    check("an interrupted extension resumes into its own cell", code == 0,
          str(code))
    check("the resume re-ran only the episode that was missing",
          FakeHarness.ran == ["alpha"], str(FakeHarness.ran))
    check("the resumed cell holds both repeats",
          summary_of(tmp, "hard-ext")["n"] == 2)

print()
print(f"{len(PASS)} passed, {len(FAIL)} failed")
if FAIL:
    sys.exit(1)
