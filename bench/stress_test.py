"""Adversarial tests for the harness, driven by a scripted fake model.

Every hypothesised failure point from DESIGN.md gets a test that would fail if the
defence were removed. No GPU required, so this runs on every change.
"""
import json, os, shutil, sys, tempfile, time, traceback
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from mh import tools as toolmod
from mh.tools import Sandbox, ToolError, cap_output
from mh.model import Reply, _coerce_args, _extract_reasoning, _extract_tool_calls
from mh.harness import Config, Harness, gather_env_snapshot
from mh.bench import load_tasks

PASS, FAIL = [], []


def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print(f"  {'ok  ' if cond else 'FAIL'} {name}" + (f"  {detail}" if not cond and detail else ""))


class FakeClient:
    """Replays a scripted list of assistant turns."""

    def __init__(self, turns, options=None):
        self.turns = list(turns)
        self.calls = 0
        self.seen_messages = []
        self.options = options or {}

    def chat(self, messages, tools=None, retries=2):
        self.seen_messages.append(list(messages))
        self.calls += 1
        if self.turns:
            spec = self.turns.pop(0)
        else:
            spec = {"content": "I have nothing further."}
        return Reply(
            content=spec.get("content", ""),
            reasoning=spec.get("reasoning", ""),
            tool_calls=spec.get("tool_calls", []),
            raw={}, prompt_tokens=spec.get("prompt_tokens", 10),
            output_tokens=10, latency_s=0.0,
            done_reason=spec.get("done_reason", "stop"))


def call(name, **args):
    return {"id": "c", "name": name, "args": args}


def tmpdir():
    return tempfile.mkdtemp(prefix="mhstress-")


# ---------------------------------------------------------------- F8 arg shapes
print("F8 malformed tool arguments")
check("dict passthrough", _coerce_args({"a": 1}) == {"a": 1})
check("json string", _coerce_args('{"a": 1}') == {"a": 1})
check("double encoded", _coerce_args('"{\\"a\\": 1}"') == {"a": 1})
check("empty string", _coerce_args("") == {})
check("bare value kept", _coerce_args("ls -la") == {"_raw": "ls -la"})
check("none", _coerce_args(None) == {"_raw": None})
check("list rejected to _raw", _coerce_args([1, 2]) == {"_raw": [1, 2]})

# ------------------------------------------------------------- F7 reasoning
print("F7 reasoning channel spellings")
check("thinking", _extract_reasoning({"thinking": "t"}) == "t")
check("reasoning_content", _extract_reasoning({"reasoning_content": "r"}) == "r")
check("reasoning", _extract_reasoning({"reasoning": "x"}) == "x")
check("blank ignored", _extract_reasoning({"thinking": "   ", "reasoning": "y"}) == "y")
check("absent", _extract_reasoning({}) == "")
check("content never treated as reasoning", _extract_reasoning({"content": "answer"}) == "")

print("F2 tool call extraction")
tc = _extract_tool_calls({"tool_calls": [
    {"function": {"name": "run_shell", "arguments": '{"cmd":"ls"}'}},
    {"function": {"name": "", "arguments": {}}},
]})
check("string args parsed", len(tc) == 1 and tc[0]["args"] == {"cmd": "ls"})
check("nameless call dropped", len(tc) == 1)
check("no tool_calls key", _extract_tool_calls({}) == [])

# ------------------------------------------------------------- F5 output cap
print("F5 output capping")
body, trunc = cap_output("x" * 100_000, 1000)
check("capped to about the limit", len(body) < 1400, f"len={len(body)}")
check("flagged truncated", trunc)
check("keeps head", body.startswith("x" * 100))
check("keeps tail", body.rstrip().endswith("x" * 100))
check("announces elision", "elided by the harness" in body)
small, t2 = cap_output("short", 1000)
check("short untouched", small == "short" and not t2)
check("zero limit disables", cap_output("abc", 0)[0] == "abc")

# ------------------------------------------------------- containment / escapes
print("containment: path escapes")
root = tmpdir()
os.makedirs(os.path.join(root, "sub"))
open(os.path.join(root, "sub", "f.txt"), "w").write("hello")
outside = tmpdir()
open(os.path.join(outside, "secret.txt"), "w").write("SECRET")
sb = Sandbox(root)

def refuses(path):
    try:
        sb.resolve(path)
        return False
    except ToolError:
        return True

check("relative escape", refuses("../../etc/passwd"))
check("absolute escape", refuses("/etc/passwd"))
check("absolute to other tmp", refuses(os.path.join(outside, "secret.txt")))
check("dotdot midpath", refuses("sub/../../x"))
check("empty path", refuses(""))
check("non-string path", refuses(None))
check("inside allowed", sb.resolve("sub/f.txt").endswith("sub/f.txt"))
check("root itself allowed", sb.resolve(".") == os.path.realpath(root))
# a symlink pointing out must not be a way through
os.symlink(outside, os.path.join(root, "link"))
check("symlink escape refused", refuses("link/secret.txt"))
# prefix-collision: /root-evil must not count as inside /root
sibling = root + "-evil"
os.makedirs(sibling, exist_ok=True)
check("sibling prefix refused", refuses(sibling))

print("containment: shell and tools")
out = sb.run_shell(cmd="pwd")
check("shell runs in sandbox", os.path.realpath(root) in out)
check("shell reports exit code", "exit=" in sb.run_shell(cmd="exit 3") and
      "exit=3" in sb.run_shell(cmd="exit 3"))
check("shell captures stderr", "[stderr]" in sb.run_shell(cmd="echo boom >&2"))
try:
    sb.run_shell(cmd="")
    check("empty cmd refused", False)
except ToolError:
    check("empty cmd refused", True)
big = sb.run_shell(cmd="yes abcdefgh | head -c 200000")
check("shell output capped", len(big) < 40_000, f"len={len(big)}")

print("tool error messages are actionable")
def err(fn, **kw):
    try:
        fn(**kw); return ""
    except ToolError as e:
        return str(e)
check("read missing file", "does not exist" in err(sb.read_file, path="nope.txt"))
with open(os.path.join(root, "e.txt"), "w") as _f:
    _f.write("aaa")
check("edit missing text", "was not found" in
      err(sb.edit_file, path="e.txt", old="zzz", new="y"))
with open(os.path.join(root, "dup.txt"), "w") as _f:
    _f.write("x\nx\n")
check("edit ambiguous", "exactly once" in err(sb.edit_file, path="dup.txt", old="x", new="y"))
check("edit empty old", "non-empty" in err(sb.edit_file, path="e.txt", old="", new="y"))
check("write needs content", "requires 'content'" in err(sb.write_file, path="w.txt"))
check("read dir lists", "directory containing" in sb.read_file(path="sub"))
sb.write_file(path="deep/nested/x.txt", content="v")
check("write creates parents", os.path.exists(os.path.join(root, "deep/nested/x.txt")))
check("write coerces non-str", "wrote" in sb.write_file(path="n.txt", content=123))

# ------------------------------------------------------------- envboot
print("F1 environment bootstrap")
snap = gather_env_snapshot(root)
check("snapshot produced", snap.startswith("[Environment Snapshot]"))
check("snapshot has cwd", "cwd:" in snap)
check("snapshot has languages", "python3" in snap)
check("snapshot bounded", len(snap) < 6000, f"len={len(snap)}")
check("snapshot on missing dir fails silently",
      gather_env_snapshot("/nonexistent-dir-xyz") == "")
check("snapshot honours timeout", gather_env_snapshot(root, timeout=0) == "")

print("F1 filesystem grounding")
tasks = {t.name: t for t in load_tasks()}
task = tasks["cryptic"]
sbdir = tmpdir()
task.materialise(os.path.join(sbdir, "s"))
h_on = Harness(FakeClient([]), Config(), os.path.join(sbdir, "s"), task)
h_off = Harness(FakeClient([]), Config(name="no-groundfs", groundfs=False),
                os.path.join(sbdir, "s"), task)
check("groundfs sentence present when on",
      "Everything you need is already in the task directory" in
      h_on.initial_messages()[1]["content"])
check("groundfs sentence absent when off",
      "Everything you need is already in the task directory" not in
      h_off.initial_messages()[1]["content"])

print("nativetools ablation is live")
from mh.tools import schemas_for, FILE_TOOLS
check("full surface five tools", len(schemas_for(True)) == 5)
check("shell surface two tools",
      {s["function"]["name"] for s in schemas_for(False)} == {"run_shell", "finish"})
h_shell = Harness(FakeClient([]), Config(name="no-nativetools", nativetools=False),
                  os.path.join(sbdir, "s"), task)
sys_txt = h_shell.initial_messages()[0]["content"]
check("shell prompt offers run_shell and finish",
      "You have these tools: run_shell, finish" in sys_txt)
check("five-tool list absent from shell prompt",
      "run_shell, read_file, write_file, edit_file, finish" not in sys_txt)
try:
    h_shell.dispatch(call("read_file", path="app/invoice.py"))
    refused = False
except ToolError as e:
    refused = "not available" in str(e)
check("file tool refused when nativetools off", refused)

# ------------------------------------------------------------- harness loop
print("harness loop behaviour")
tasks = {t.name: t for t in load_tasks()}
task = tasks["cryptic"]

def run_with(turns, cfg=None, task=task):
    sbdir = tmpdir()
    task.materialise(os.path.join(sbdir, "s"))
    fc = FakeClient(turns)
    h = Harness(fc, cfg or Config(), os.path.join(sbdir, "s"), task)
    return h.run(), fc, h

FIX = call("edit_file", path="app/invoice.py",
           old='cfg["include_taxes"]', new='cfg["include_tax"]')

# F9: a model that just says "done" without doing anything must not pass
res, fc, _ = run_with([{"tool_calls": [call("finish", summary="all done")]}] * 6)
check("F9 empty claim does not pass", not res.passed)
check("F9 verifier ran", res.verify_runs >= 1)

# the real fix does pass, and the gate confirms it
res, fc, _ = run_with([
    {"tool_calls": [FIX]},
    {"tool_calls": [call("finish", summary="fixed")]},
    {"tool_calls": [call("finish", summary="fixed")]},
])
check("real fix passes", res.passed)
check("checklist fired once", any(e["t"] == "checklist" for e in res.events))
check("verifygate fired", any(e["t"] == "verifygate" and e["ok"] for e in res.events))

# F4: with verifygate on, a premature finish is refused and work continues
res, fc, _ = run_with([
    {"tool_calls": [call("finish", summary="done")]},
    {"tool_calls": [call("finish", summary="done")]},
    {"tool_calls": [FIX]},
    {"tool_calls": [call("finish", summary="really done")]},
])
check("F4 premature finish refused then recovered", res.passed)

# F3: loop breaking
loop_turns = [{"tool_calls": [call("run_shell", cmd="echo same")]} for _ in range(10)]
res, fc, _ = run_with(loop_turns, Config(name="lb"))
check("F3 loopbreak fired", any(e["t"] == "loopbreak" for e in res.events))
res2, _, _ = run_with(loop_turns, Config(name="nolb", loopbreak=False))
check("F3 loopbreak is what caused it", not any(e["t"] == "loopbreak" for e in res2.events))

# F2: hallucinated tool name is reported, not fatal
res, fc, _ = run_with([
    {"tool_calls": [call("summon_daemon", x=1)]},
    {"tool_calls": [FIX]},
    {"tool_calls": [call("finish", summary="ok")]},
    {"tool_calls": [call("finish", summary="ok")]},
])
check("F2 unknown tool survived", res.passed)
check("F2 unknown tool named the real ones",
      any("there is no tool named" in e for e in res.errors))

# a tool that raises an unexpected error must not kill the run
class Exploding(Harness):
    def dispatch(self, call_):
        if call_["name"] == "run_shell":
            raise RuntimeError("tool exploded")
        return super().dispatch(call_)

sbdir = tmpdir(); task.materialise(os.path.join(sbdir, "s"))
h = Exploding(FakeClient([
    {"tool_calls": [call("run_shell", cmd="x")]},
    {"tool_calls": [FIX]},
    {"tool_calls": [call("finish", summary="ok")]},
    {"tool_calls": [call("finish", summary="ok")]},
]), Config(), os.path.join(sbdir, "s"), task)
r = h.run()
check("unexpected tool exception contained", r.passed)

# model that never calls a tool stops instead of spinning
res, fc, _ = run_with([{"content": "Here is my plan."}] * 10)
check("no-tool-call terminates", res.stop_reason == "no_tool_call")
check("no-tool-call bounded", fc.calls <= 4, f"calls={fc.calls}")

# step ceiling holds when one is set
res, fc, _ = run_with([{"tool_calls": [call("run_shell", cmd=f"echo {i}")]}
                       for i in range(200)], Config(name="cap", max_steps=5))
check("max_steps enforced", res.steps == 5 and res.stop_reason == "max_steps")

# 0 means no ceiling — 45 unique shells would have died at the old default of 40
uncap_turns = [{"tool_calls": [call("run_shell", cmd=f"echo {i}")]}
               for i in range(45)]
uncap_turns += [
    {"tool_calls": [FIX]},
    {"tool_calls": [call("finish", summary="ok")]},
    {"tool_calls": [call("finish", summary="ok")]},
]
res, fc, _ = run_with(uncap_turns, Config(name="uncapped", max_steps=0))
check("max_steps 0 is not a 40-turn ceiling",
      res.steps > 40 and res.stop_reason == "finished" and res.passed,
      f"steps={res.steps} stop={res.stop_reason}")

print("episode wall-clock timeout")
from mh.harness import WALL_S_DEFAULT, HTTP_TIMEOUT_S, FIRST_TURN_TIMEOUT_S
import mh.harness as harness_mod
check("default wall is 30 minutes", WALL_S_DEFAULT == 1800)
check("HTTP cap matches the episode wall", HTTP_TIMEOUT_S == 1800)
check("first-turn cap is 10 minutes", FIRST_TURN_TIMEOUT_S == 600)
check("Config default matches", Config().wall_s == 1800)

class ProbeTimeouts:
    def __init__(self):
        self.timeout = 1800
        self.http_timeout = 1800
        self.first = None
        self.later = None
        self.n = 0
    def chat(self, messages, tools=None, retries=2):
        self.n += 1
        if self.n == 1:
            self.first = self.timeout
            return Reply(content="", reasoning="", tool_calls=[FIX],
                         raw={}, prompt_tokens=10, output_tokens=10,
                         latency_s=0.0, done_reason="stop")
        self.later = self.timeout
        return Reply(content="", reasoning="",
                     tool_calls=[call("finish", summary="ok")],
                     raw={}, prompt_tokens=10, output_tokens=10,
                     latency_s=0.0, done_reason="stop")

old_first = harness_mod.FIRST_TURN_TIMEOUT_S
harness_mod.FIRST_TURN_TIMEOUT_S = 7
sbdir = tmpdir()
task.materialise(os.path.join(sbdir, "probe"))
probe = ProbeTimeouts()
Harness(probe, Config(name="first-turn", wall_s=1800, max_steps=0),
        os.path.join(sbdir, "probe"), task).run()
harness_mod.FIRST_TURN_TIMEOUT_S = old_first
check("first turn uses first-turn cap", probe.first == 7, str(probe.first))
check("later turns use remaining wall", probe.later is not None and probe.later > 7,
      str(probe.later))

class SlowClient(FakeClient):
    def chat(self, messages, tools=None, retries=2):
        time.sleep(0.05)
        return super().chat(messages, tools, retries)

sbdir = tmpdir(); task.materialise(os.path.join(sbdir, "s"))
slow_turns = [{"tool_calls": [FIX]}]
slow_turns += [{"tool_calls": [call("run_shell", cmd=f"echo {i}")]} for i in range(40)]
slow_turns += [
    {"tool_calls": [call("finish", summary="ok")]},
    {"tool_calls": [call("finish", summary="ok")]},
]
h = Harness(SlowClient(slow_turns), Config(name="wall", wall_s=0.12, max_steps=0),
            os.path.join(sbdir, "s"), task)
res = h.run()
check("wall_timeout stop reason", res.stop_reason == "wall_timeout", res.stop_reason)
check("wall_timeout is a fail even if the fix landed", not res.passed)
check("wall_timeout still ran the verifier", res.verify_runs >= 1)
check("wall_timeout did not spin to max_steps", res.steps < 40, f"steps={res.steps}")
check("wall event recorded", any(e.get("t") == "wall_timeout" for e in res.events))

# a client that raises does not lose the run
class Boom:
    def chat(self, *a, **k):
        raise RuntimeError("server down")
sbdir = tmpdir(); task.materialise(os.path.join(sbdir, "s"))
r = Harness(Boom(), Config(), os.path.join(sbdir, "s"), task).run()
check("client error contained", r.stop_reason.startswith("error:") and not r.passed)
check("client error still verified", r.verify_runs >= 1)

# truncated assistant output is handled, not mistaken for silence
res, fc, _ = run_with([
    {"content": "x" * 100, "done_reason": "length"},
    {"tool_calls": [FIX]},
    {"tool_calls": [call("finish", summary="ok")]},
    {"tool_calls": [call("finish", summary="ok")]},
])
check("F6 truncation recovered", res.passed)

# ------------------------------------------------- non-cheating invariant
print("non-cheating invariant")
sbdir = tmpdir(); s = os.path.join(sbdir, "s"); task.materialise(s)
# delete the failing assertion instead of fixing the bug
open(os.path.join(s, "test_invoice.py"), "w").write("print('ok')\n")
ok, out = task.verify(s)
check("gutting the visible test fails", not ok)
check("tamper is named", "protected files were changed" in out)
# deleting it entirely
os.remove(os.path.join(s, "test_invoice.py"))
ok, out = task.verify(s)
check("deleting the test fails", not ok and "deleted" in out)
# a verifier that cannot run is not a pass
class Broken(type(task)):
    pass
bt = load_tasks(["cryptic"])[0]
bt.hidden = "/nonexistent/hidden.py"
ok, out = bt.verify(s)
check("unrunnable verifier is not a pass", not ok)

# the agent cannot shadow the hidden test from inside the sandbox
sbdir = tmpdir(); s = os.path.join(sbdir, "s"); task.materialise(s)
shutil.copy2(os.path.join(task.dir, "reference", "app", "invoice.py"),
             os.path.join(s, "app", "invoice.py"))
open(os.path.join(s, "hidden_test.py"), "w").write("import sys; sys.exit(0)\n")
ok, _ = task.verify(s)
check("shadowing hidden_test still verifies honestly", ok)

# ------------------------------------------------- F6 context exhaustion
print("F6 context exhaustion is loud, not silent")
sbdir = tmpdir(); task.materialise(os.path.join(sbdir, "s"))
fc = FakeClient([{"tool_calls": [call("run_shell", cmd="echo hi")],
                  "prompt_tokens": 31000}], options={"num_ctx": 32768})
r = Harness(fc, Config(), os.path.join(sbdir, "s"), task).run()
check("stops on context exhaustion", r.stop_reason == "context_exhausted")
check("records the event", any(e["t"] == "context_exhausted" for e in r.events))
check("explains itself", any("silently drop context" in e for e in r.errors))
check("does not report a pass", not r.passed)

# under the limit it must not trip
sbdir = tmpdir(); task.materialise(os.path.join(sbdir, "s"))
fc = FakeClient([{"tool_calls": [FIX], "prompt_tokens": 10000},
                 {"tool_calls": [call("finish", summary="ok")], "prompt_tokens": 11000},
                 {"tool_calls": [call("finish", summary="ok")], "prompt_tokens": 12000}],
                options={"num_ctx": 32768})
r = Harness(fc, Config(), os.path.join(sbdir, "s"), task).run()
check("headroom does not trip the guard", r.passed and r.stop_reason == "finished")
check("peak prompt tokens tracked", r.peak_prompt_tokens == 12000, f"{r.peak_prompt_tokens}")

# a client without options (num_ctx unknown) must not crash
sbdir = tmpdir(); task.materialise(os.path.join(sbdir, "s"))
r = Harness(FakeClient([{"tool_calls": [FIX]},
                        {"tool_calls": [call("finish", summary="ok")]},
                        {"tool_calls": [call("finish", summary="ok")]}]),
            Config(), os.path.join(sbdir, "s"), task).run()
check("unknown num_ctx disables guard safely", r.passed)

# ------------------------------------------- multi-call turns leave nothing unanswered
print("multi-call turns")

def count_tool_msgs(fc):
    """tool-role messages the harness appended, from the last transcript it sent"""
    return sum(1 for m in fc.seen_messages[-1] if m.get("role") == "tool")

# work + finish in one turn: the work must still run, and finish still evaluated
sbdir = tmpdir(); s_ = os.path.join(sbdir, "s"); task.materialise(s_)
fc = FakeClient([
    {"tool_calls": [call("run_shell", cmd="echo one"), FIX,
                    call("finish", summary="done")]},
    {"tool_calls": [call("finish", summary="done")]},
])
h = Harness(fc, Config(), s_, task)
r = h.run()
check("work calls in a finish turn still execute",
      sum(1 for e in r.events if e["t"] == "tool") == 2)
check("finish in a mixed turn still reaches the gate",
      any(e["t"] in ("checklist", "verifygate") for e in r.events))
check("mixed turn still passes", r.passed)

# every emitted call gets exactly one reply
sbdir = tmpdir(); s_ = os.path.join(sbdir, "s"); task.materialise(s_)
fc = FakeClient([
    {"tool_calls": [call("run_shell", cmd="echo a"), call("run_shell", cmd="echo b"),
                    call("read_file", path="app/config.py")]},
    {"tool_calls": [FIX]},
    {"tool_calls": [call("finish", summary="ok")]},
    {"tool_calls": [call("finish", summary="ok")]},
])
h = Harness(fc, Config(), s_, task); r = h.run()
first_turn_tools = [e for e in r.events if e["t"] == "tool" and e["step"] == 1]
check("three calls -> three results", len(first_turn_tools) == 3)
check("three-call turn still passes", r.passed)

# two finishes in one turn: the second is answered, not dropped
sbdir = tmpdir(); s_ = os.path.join(sbdir, "s"); task.materialise(s_)
fc = FakeClient([
    {"tool_calls": [FIX]},
    {"tool_calls": [call("finish", summary="a"), call("finish", summary="b")]},
    {"tool_calls": [call("finish", summary="a"), call("finish", summary="b")]},
])
h = Harness(fc, Config(), s_, task); r = h.run()
check("duplicate finish handled", r.passed)

print()
print(f"{len(PASS)} passed, {len(FAIL)} failed")
if FAIL:
    print("FAILURES:")
    for f in FAIL:
        print("  -", f)
sys.exit(1 if FAIL else 0)
