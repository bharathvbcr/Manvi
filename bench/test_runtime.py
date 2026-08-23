"""Residency policy. No ollama required."""
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

print("complete() refuses starved cells")
import json, shutil, tempfile
from grid import complete, cell_has_starved, outdir
tag = "unittest-starve"
d = outdir("qwen3.8:27b", "no-checklist", tag)
os.makedirs(d, exist_ok=True)
try:
    starved_rows = [{"task": "a", "rep": i, "steps": 1, "output_tokens": 0,
                     "stop_reason": "error:ModelError"} for i in range(40)]
    json.dump({"n": 40, "rows": starved_rows},
              open(os.path.join(d, "summary.json"), "w"))
    check("starved n=40 is not complete",
          complete("qwen3.8:27b", "no-checklist", tag, 8, 5) is False)
    check("starved detected on disk",
          cell_has_starved("qwen3.8:27b", "no-checklist", tag) is True)
    clean_rows = [{"task": "a", "rep": i, "steps": 8, "output_tokens": 100,
                   "stop_reason": "finished"} for i in range(40)]
    json.dump({"n": 40, "rows": clean_rows},
              open(os.path.join(d, "summary.json"), "w"))
    check("clean n=40 is complete",
          complete("qwen3.8:27b", "no-checklist", tag, 8, 5) is True)
finally:
    shutil.rmtree(d, ignore_errors=True)

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

print()
print(f"{len(PASS)} passed, {len(FAIL)} failed")
if FAIL:
    sys.exit(1)
