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
