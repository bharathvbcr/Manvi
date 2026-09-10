"""Sandbox tool dispatch is a named map, not getattr."""
import os
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mh.tools import Sandbox, TOOL_NAMES

PASS, FAIL = [], []


def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print(f"  {'ok  ' if cond else 'FAIL'} {name}" + (f"  {detail}" if not cond and detail else ""))


print("sandbox handlers")
root = tempfile.mkdtemp(prefix="mh-tools-")
sb = Sandbox(root)
handlers = sb.handlers()
check("finish is a named handler, not a getattr", handlers["finish"] == sb.finish)
check("every advertised tool has a handler", set(handlers) == set(TOOL_NAMES))
check("finish returns the summary", sb.finish(summary="done") == "done")
os.rmdir(root)

print()
print(f"{len(PASS)} passed, {len(FAIL)} failed")
if FAIL:
    sys.exit(1)
