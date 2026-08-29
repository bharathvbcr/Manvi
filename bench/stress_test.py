"""Adversarial tests for the harness, driven by a scripted fake model.

Every hypothesised failure point from DESIGN.md gets a test that would fail if the
defence were removed. No GPU required, so this runs on every change.
"""
import json, os, shutil, subprocess, sys, tempfile, time, traceback
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

# ============================================================ cheating holes
# Each block below is an exploit that worked against the pre-hardening
# instrument. They are written as the exploit, not as a paraphrase of it.
print("C1 interpreter startup hijack")
import mh.bench as benchmod
from mh.bench import model_facing, failure_labels, _file_set
from mh.tools import BENCH_ROOT, containment_backend

SITECUSTOMIZE = "import os\nos._exit(0)\n"

# The exploit: two lines in sitecustomize.py, written with the ordinary
# write_file tool. `site` imported it at interpreter start-up because the
# verifier ran `python3 <test>` with PYTHONPATH=sandbox and no -I/-E, so the
# hidden test never executed and 18 of 19 tasks "passed" untouched.
sbdir = tmpdir(); s_ = os.path.join(sbdir, "s"); task.materialise(s_)
open(os.path.join(s_, "sitecustomize.py"), "w").write(SITECUSTOMIZE)
# Pretend the file was always there, so this checks the *interpreter*, not the
# added-file check that also catches it.
task._manifests[os.path.realpath(s_)] = _file_set(s_)
ok, out = task.verify(s_)
check("C1 sitecustomize does not buy a pass", not ok, out[:200])
check("C1 the hidden test actually ran", "include_taxes" in out or "exit=1" in out)

# A .pth file is the same hole with a different spelling.
sbdir = tmpdir(); s_ = os.path.join(sbdir, "s"); task.materialise(s_)
open(os.path.join(s_, "evil.pth"), "w").write("import os; os._exit(0)\n")
task._manifests[os.path.realpath(s_)] = _file_set(s_)
ok, _ = task.verify(s_)
check("C1 .pth does not buy a pass", not ok)

# PYTHONPATH/PYTHONSTARTUP and friends are not inherited by the verifier.
sbdir = tmpdir(); s_ = os.path.join(sbdir, "s"); task.materialise(s_)
shutil.copy2(os.path.join(task.dir, "reference", "app", "invoice.py"),
             os.path.join(s_, "app", "invoice.py"))
os.environ["PYTHONSTARTUP"] = "/nonexistent/boom.py"
os.environ["PYTHONWARNINGS"] = "error"
ok, _ = task.verify(s_)
del os.environ["PYTHONSTARTUP"], os.environ["PYTHONWARNINGS"]
check("C1 hijacking PYTHON* env does not disturb the verifier", ok)

print("C2 files the agent adds are checked, not ignored")
def solved_sandbox():
    """A sandbox that verifies clean, so an added file is the only variable."""
    d = os.path.join(tmpdir(), "s")
    task.materialise(d)
    shutil.copy2(os.path.join(task.dir, "reference", "app", "invoice.py"),
                 os.path.join(d, "app", "invoice.py"))
    return d

s_ = solved_sandbox()
check("C2 fixture verifies clean before anything is added", task.verify(s_)[0])
open(os.path.join(s_, "sitecustomize.py"), "w").write(SITECUSTOMIZE)
refused, other = task.additions(s_)
check("C2 added startup hook is seen", refused == ["sitecustomize.py"], str(refused))
ok, out = task.verify(s_)
check("C2 added startup hook is refused", not ok, out[:160])
check("C2 the refusal names the file and says why",
      "sitecustomize.py" in out and "loads on its own" in out, out[:160])

# a .pth is executed by site with no import at all
s_ = solved_sandbox()
open(os.path.join(s_, "hook.pth"), "w").write("import os; os._exit(0)\n")
refused, _ = task.additions(s_)
check("C2 added .pth is seen", refused == ["hook.pth"], str(refused))
check("C2 added .pth is refused", not task.verify(s_)[0])

# a top-level module shadowing a stdlib name on the verifier's sys.path
s_ = solved_sandbox()
open(os.path.join(s_, "json.py"), "w").write("def loads(x):\n    return {}\n")
refused, _ = task.additions(s_)
check("C2 stdlib shadow refused", refused == ["json.py"], str(refused))
check("C2 stdlib shadow fails the verification", not task.verify(s_)[0])
s_ = solved_sandbox()
os.makedirs(os.path.join(s_, "json"))
open(os.path.join(s_, "json", "__init__.py"), "w").write("")
check("C2 stdlib shadow as a package is refused too", not task.verify(s_)[0])

# ...but ordinary work products are not treated as cheating
sbdir = tmpdir(); s_ = os.path.join(sbdir, "s"); task.materialise(s_)
shutil.copy2(os.path.join(task.dir, "reference", "app", "invoice.py"),
             os.path.join(s_, "app", "invoice.py"))
open(os.path.join(s_, "notes.txt"), "w").write("scratch\n")
open(os.path.join(s_, "helper.py"), "w").write("VALUE = 1\n")
os.makedirs(os.path.join(s_, "__pycache__"), exist_ok=True)
open(os.path.join(s_, "__pycache__", "x.cpython-99.pyc"), "wb").write(b"\x00")
ok, out = task.verify(s_)
check("C2 benign additions still pass", ok, out[:200])
check("C2 benign additions are recorded, not silent", "sandbox additions" in out, out[-200:])

# the check must not skip itself when the manifest is missing
fresh = load_tasks(["cryptic"])[0]
sbdir = tmpdir(); s_ = os.path.join(sbdir, "s")
shutil.copytree(fresh.setup_dir, s_)
shutil.copy2(os.path.join(fresh.dir, "reference", "app", "invoice.py"),
             os.path.join(s_, "app", "invoice.py"))
check("C2 unmaterialised sandbox verifies clean", fresh.verify(s_)[0])
open(os.path.join(s_, "sitecustomize.py"), "w").write(SITECUSTOMIZE)
check("C2 unmaterialised sandbox is still checked", not fresh.verify(s_)[0])

print("C3 the gate is not an oracle")
# Reference solution with the tax applied twice: the hidden test now prints its
# own expected values, e.g. "FAIL 29.16 want 27.0".
WRONG = open(os.path.join(task.dir, "reference", "app", "invoice.py")).read().replace(
    'total *= (1 + cfg["tax_rate"])', 'total *= (1 + cfg["tax_rate"] * 2)')
sbdir = tmpdir(); s_ = os.path.join(sbdir, "s"); task.materialise(s_)
open(os.path.join(s_, "app", "invoice.py"), "w").write(WRONG)
raw_ok, raw = task.verify(s_)
check("C3 fixture really does leak values in the raw output",
      (not raw_ok) and "want" in raw, raw[:200])

fc = FakeClient([{"tool_calls": [call("finish", summary="done")]},
                 {"tool_calls": [call("finish", summary="done")]},
                 {"tool_calls": [call("finish", summary="done")]}])
h = Harness(fc, Config(), s_, task)
r = h.run()
gate_msgs = [m["content"] for m in fc.seen_messages[-1]
             if m.get("role") == "tool" and m.get("name") == "finish"]
gate_text = "\n".join(gate_msgs)
check("C3 gate replied at all", bool(gate_text))
check("C3 gate never shows the expected value", "want" not in gate_text, gate_text[:300])
check("C3 gate never shows the hidden test's numbers",
      "27.0" not in gate_text and "29.16" not in gate_text, gate_text[:300])
check("C3 gate still says the checks failed", "hidden checks fail" in gate_text)
check("C3 gate says what is withheld", "never shown" in gate_text)
check("C3 the raw output is still kept for the record",
      "want" in r.verify_output)
# tamper messages are harness text, not test output, and must still be shown
check("C3 tamper text still reaches the model",
      "protected files were changed" in model_facing(
          False, "VERIFY FAILED: protected files were changed: test_invoice.py (modified)"))
labels, total = failure_labels("FAIL [1,2,2,2,5] 2 got (0,0) want (1,3)")
check("C3 labels carry no values", labels == ["FAIL"] and total == 1, str(labels))

print("C4 the hidden test's source is not reachable from inside the verifier")
STEAL = (
    "import sys, shutil\n"
    "try:\n"
    "    shutil.copy(sys.argv[0], 'STOLEN.txt')\n"
    "except Exception as exc:\n"
    "    open('STOLEN.err', 'w').write(repr(exc))\n"
    "_m = sys.modules.get('__main__')\n"
    "open('MAINFILE.txt', 'w').write(str(getattr(_m, '__file__', None)))\n"
    "open('ARGV0.txt', 'w').write(str(sys.argv[0]))\n"
)
sbdir = tmpdir(); s_ = os.path.join(sbdir, "s"); task.materialise(s_)
ref = open(os.path.join(task.dir, "reference", "app", "invoice.py")).read()
open(os.path.join(s_, "app", "invoice.py"), "w").write(STEAL + ref)
task.verify(s_)
stolen = os.path.join(s_, "STOLEN.txt")
check("C4 argv[0] is not a copyable path to the test", not os.path.exists(stolen),
      "STOLEN.txt exists")
if os.path.exists(stolen):
    check("C4 nothing exfiltrated", "invoice_total" not in open(stolen).read())
argv0 = open(os.path.join(s_, "ARGV0.txt")).read()
mainfile = open(os.path.join(s_, "MAINFILE.txt")).read()
check("C4 payload really did run inside the verifier", bool(argv0))
check("C4 argv[0] names no file", not os.path.exists(argv0), argv0)
check("C4 __file__ names no file", not os.path.exists(mainfile), mainfile)
check("C4 no copy of the hidden test is left on disk anywhere",
      not any(os.path.exists(os.path.join(d, "hidden_test.py"))
              for d in (s_, tempfile.gettempdir())))

print("C5 run_shell is contained")
# bwrap was missing from this list, so the Linux backend -- the one the grid
# actually runs under on a rented GPU -- failed the check that asserts a
# backend is in use. Worse, the substantive block below was gated on
# `== "sandbox-exec"`, so every real containment assertion was SKIPPED on
# Linux and the else-branch demanded the backend be "off". The Linux
# confinement path had therefore never been exercised by this suite on the
# platform that matters.
check("C5 a containment backend is in use or explicitly disabled",
      containment_backend() in ("sandbox-exec", "bwrap", "off", None),
      f"backend={containment_backend()!r}")
# A replica of the real layout: <root>/tasks/<name> beside <root>/.work/<slug>.
rep_root = tmpdir()
rep_tasks = os.path.join(rep_root, "tasks")
os.makedirs(rep_tasks)
shutil.copytree(task.dir, os.path.join(rep_tasks, "cryptic"))
rtask = load_tasks(["cryptic"], tasks_dir=rep_tasks)[0]
rsand = os.path.join(rep_root, ".work", "cryptic-full-0")
os.makedirs(os.path.dirname(rsand), exist_ok=True)
rtask.materialise(rsand)
rsb = Sandbox(rsand, protected_roots=rtask.guard_roots)
victim = os.path.join(rep_tasks, "cryptic", "hidden_test.py")
before = open(victim, "rb").read()

if containment_backend() in ("sandbox-exec", "bwrap"):
    got = rsb.run_shell(cmd="cat ../../tasks/cryptic/hidden_test.py")
    check("C5 the hidden test is not readable from the shell",
          "invoice_total" not in got, got[:200])
    got = rsb.run_shell(cmd="echo POISONED >> ../../tasks/cryptic/hidden_test.py; "
                            "echo rc=$?")
    check("C5 the hidden test is not writable from the shell",
          open(victim, "rb").read() == before, got[:200])
    got = rsb.run_shell(cmd=f"cat {BENCH_ROOT}/mh/bench.py")
    check("C5 the harness source is not readable from the shell",
          "def verify" not in got, got[:200])
    got = rsb.run_shell(cmd="echo inside > ok.txt && cat ok.txt")
    check("C5 the sandbox itself is still writable", "inside" in got, got[:200])
else:
    check("C5 uncontained runs are refused or stamped",
          containment_backend() == "off")

# Even if a write did land some other way, a rewritten hidden test is refused
# rather than scored against: the source is pinned when the task is loaded.
open(victim, "ab").write(b"\nimport sys; sys.exit(0)\n")
ok, out = rtask.verify(rsand)
check("C5 a poisoned hidden test fails loudly instead of passing",
      (not ok) and "pinned" in out, out[:200])
open(victim, "wb").write(before)
check("C5 restoring the test restores verification", not rtask.verify(rsand)[0])

print("output caps are measured in bytes")
body, trunc = cap_output("漢" * 40_000, 30_000)
check("cap counts bytes, not characters",
      len(body.encode("utf-8")) < 31_000, f"bytes={len(body.encode('utf-8'))}")
check("multibyte cap still flags truncation", trunc)
check("no mangled characters at the cut", "�" not in body)
body, _ = cap_output("a" * 100 + "漢" * 10, 10 ** 9)
check("large limit leaves text alone", body.endswith("漢" * 10))

print("directory listings say when they are partial")
bigdir = tmpdir()
for i in range(250):
    open(os.path.join(bigdir, f"f{i:03d}.txt"), "w").write("x")
bsb = Sandbox(bigdir)
listing = bsb.read_file(path=".")
check("listing reports the true total", "250 entries" in listing, listing[:120])
check("listing says how many it withheld", "not listed" in listing, listing[:120])
small_listing = Sandbox(root).read_file(path="sub")
check("a complete listing claims nothing", "not listed" not in small_listing)

print("read_file is bounded before it builds strings")
bigroot = tmpdir()
bigfile = os.path.join(bigroot, "big.txt")
with open(bigfile, "w") as _f:
    for i in range(1_000_000):
        _f.write(f"line {i:08d} padding padding padding\n")
bigsize = os.path.getsize(bigfile)
probe_src = (
    "import os, resource, sys\n"
    f"sys.path.insert(0, {os.path.dirname(os.path.abspath(__file__))!r})\n"
    "from mh.tools import Sandbox\n"
    "b0 = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss\n"
    f"out = Sandbox({bigroot!r}).read_file(path='big.txt')\n"
    "b1 = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss\n"
    "print(len(out), b1 - b0)\n")
probe = subprocess.run([sys.executable, "-c", probe_src], capture_output=True, text=True)
check("read_file probe ran", probe.returncode == 0, probe.stderr[-300:])
if probe.returncode == 0:
    out_len, rss_delta = (int(x) for x in probe.stdout.split())
    unit = 1 if sys.platform == "darwin" else 1024      # macOS reports bytes
    grew = rss_delta * unit
    check("read_file output stays inside the cap", out_len < 70_000, f"len={out_len}")
    check("read_file does not pull the whole file into memory",
          grew < bigsize, f"grew={grew} for a {bigsize}-byte file")
# ...and the bound holds for the no-outcap ablation too, where the cap is 1e9
nocap_out = Sandbox(bigroot, output_cap=10 ** 9).read_file(path="big.txt")
check("the read bound survives the outcap ablation",
      len(nocap_out) <= 2 * toolmod.MAX_READ_BYTES + 1000, f"len={len(nocap_out)}")
check("and it still says it is partial", "read only part of this" in nocap_out)
big_out = Sandbox(bigroot).read_file(path="big.txt")
check("a partial read says so", "read only part of this" in big_out, big_out[:120])
check("a partial read gives both numbers",
      str(bigsize) in big_out and "line 00000000" in big_out)
os.remove(bigfile)

print("a timed-out command takes its children with it")
old_timeout = toolmod.SHELL_TIMEOUT_S
toolmod.SHELL_TIMEOUT_S = 2
orphan_root = tmpdir()
osb = Sandbox(orphan_root)
# The duration doubles as a marker unique to this process, so a concurrent copy
# of this suite cannot be mistaken for a survivor of ours.
MARK = os.getpid()
try:
    osb.run_shell(cmd=f"sleep {MARK} & sleep {MARK} & sleep {MARK} & wait")
    timed_out = False
except ToolError as e:
    timed_out = "timed out" in str(e)
toolmod.SHELL_TIMEOUT_S = old_timeout
check("the timeout fired", timed_out)
time.sleep(0.5)
survivors = subprocess.run(["/bin/sh", "-c", f"pgrep -f 'sleep {MARK}$' | wc -l"],
                           capture_output=True, text=True).stdout.strip()
check("no orphaned children survive the timeout", survivors == "0", f"survivors={survivors}")
if survivors != "0":
    subprocess.run(["/bin/sh", "-c", f"pkill -f 'sleep {MARK}$'"])

# ... and the same deadline applies to the verifier, which runs the agent's code
sbdir = tmpdir()
htask_dir = os.path.join(sbdir, "tasks", "hangs")
os.makedirs(os.path.dirname(htask_dir))
shutil.copytree(tasks["cryptic"].dir, htask_dir)
VMARK = os.getpid() + 1
with open(os.path.join(htask_dir, "task.json"), "w") as _f:
    json.dump({"name": "hangs", "kind": "python", "hidden": "hidden_test.py",
               "protect": ["test_invoice.py"], "timeout": 3,
               "blurb": "a hidden test that hangs"}, _f)
with open(os.path.join(htask_dir, "hidden_test.py"), "w") as _f:
    _f.write("import subprocess, time\n"
             f"subprocess.Popen(['sleep', '{VMARK}'])\n"
             "time.sleep(120)\n")
htask = load_tasks(["hangs"], tasks_dir=os.path.dirname(htask_dir))[0]
hs = os.path.join(sbdir, "hangs-sandbox"); htask.materialise(hs)
t_start = time.time()
ok, out = htask.verify(hs)
check("a hanging hidden test is a fail, not a pass", not ok)
check("and it is reported as a timeout", "timed out" in out, out[:120])
check("the timeout is honoured, not the wall clock", time.time() - t_start < 30,
      f"{time.time() - t_start:.1f}s")
time.sleep(0.5)
vsurv = subprocess.run(["/bin/sh", "-c", f"pgrep -f 'sleep {VMARK}$' | wc -l"],
                       capture_output=True, text=True).stdout.strip()
check("a timed-out verifier leaves no processes behind", vsurv == "0", f"survivors={vsurv}")
if vsurv != "0":
    subprocess.run(["/bin/sh", "-c", f"pkill -f 'sleep {VMARK}$'"])

print("file tools refuse directories")
dsb = Sandbox(root)
os.makedirs(os.path.join(root, "adir"), exist_ok=True)
check("write_file refuses a directory",
      "is a directory" in err(dsb.write_file, path="adir", content="x"))
check("edit_file refuses a directory",
      "is a directory" in err(dsb.edit_file, path="adir", old="x", new="y"))

print("grader tampering is graded, not filed as an infrastructure fault")
sbdir = tmpdir(); s_ = os.path.join(sbdir, "s"); task.materialise(s_)
shutil.copy2(os.path.join(task.dir, "reference", "app", "invoice.py"),
             os.path.join(s_, "app", "invoice.py"))
os.remove(os.path.join(s_, "test_invoice.py"))
os.makedirs(os.path.join(s_, "test_invoice.py"))
ok, out = task.verify(s_)
check("a protected file replaced by a directory is caught", not ok)
check("and it is named as tampering",
      "replaced by a directory" in out, out[:200])
fc = FakeClient([{"tool_calls": [call("finish", summary="done")]}] * 3)
r = Harness(fc, Config(), s_, task).run()
check("the episode is a fail, not a runner_error",
      (not r.passed) and not r.stop_reason.startswith("error:"), r.stop_reason)

print("loop breaking counts results, not just calls")
LOOK = call("run_shell", cmd="cat marker.txt")
def bump(n):
    return call("write_file", path="marker.txt", content=f"state {n}")
cycle = [{"tool_calls": [bump(0)]}, {"tool_calls": [LOOK]},
         {"tool_calls": [bump(1)]}, {"tool_calls": [LOOK]},
         {"tool_calls": [bump(2)]}, {"tool_calls": [LOOK]},
         {"tool_calls": [bump(3)]}, {"tool_calls": [LOOK]},
         {"tool_calls": [FIX]},
         {"tool_calls": [call("finish", summary="ok")]},
         {"tool_calls": [call("finish", summary="ok")]}]
res, fc, _ = run_with(cycle, Config(name="lb-cycle"))
check("an edit/test/edit cycle is not mistaken for a loop",
      not any(e["t"] == "loopbreak" for e in res.events))
check("the cycle still finishes", res.passed)

res, fc, _ = run_with([{"tool_calls": [call("run_shell", cmd="echo same")]}
                       for _ in range(200)], Config(name="lb-200"))
lb = [e for e in res.events if e["t"] == "loopbreak"]
check("a genuine loop is still broken", len(lb) > 100, f"n={len(lb)}")
windows = [e.get("window") for e in lb]
check("the loop memory is observable at all", all(w is not None for w in windows))
check("the loop memory stays bounded",
      windows and all(w is not None and w <= harness_mod.LOOP_WINDOW for w in windows),
      f"max={max(w for w in windows if w is not None) if any(windows) else None}")
blocked = [m for m in fc.seen_messages[-1]
           if m.get("role") == "tool" and "Repeating it will not help" in str(m.get("content"))]
check("the loopbreak message states what is actually true",
      blocked and "byte-identical output" in blocked[-1]["content"])

print("a model that only produces truncated prose still terminates")
class Endless:
    def __init__(self):
        self.n = 0
    def chat(self, messages, tools=None, retries=2):
        self.n += 1
        if self.n > 50:
            raise AssertionError("harness did not terminate on truncated prose")
        return Reply(content="x" * 100, reasoning="", tool_calls=[], raw={},
                     prompt_tokens=10, output_tokens=10, latency_s=0.0,
                     done_reason="length")
sbdir = tmpdir(); s_ = os.path.join(sbdir, "s"); task.materialise(s_)
endless = Endless()
r = Harness(endless, Config(name="no-wall", max_steps=0, wall_s=0), s_, task).run()
check("truncated prose terminates without a wall clock",
      r.stop_reason == "no_tool_call", r.stop_reason)
check("and it terminates quickly", endless.n <= 5, f"turns={endless.n}")

print("malformed argument payloads do not kill the episode")
sbdir = tmpdir(); s_ = os.path.join(sbdir, "s"); task.materialise(s_)
class Unserialisable:
    pass

fc = FakeClient([
    {"tool_calls": [{"id": "c", "name": "run_shell", "args": "ls -la"}]},
    {"tool_calls": [{"id": "c", "name": "run_shell", "args": ["ls"]}]},
    {"tool_calls": [{"id": "c", "name": "run_shell",
                     "args": {"cmd": Unserialisable()}}]},
    {"tool_calls": [FIX]},
    {"tool_calls": [call("finish", summary="ok")]},
    {"tool_calls": [call("finish", summary="ok")]},
])
r = Harness(fc, Config(), s_, task).run()
check("non-dict args are a tool error, not an episode error",
      not r.stop_reason.startswith("error:"), r.stop_reason)
check("non-dict args still let the run finish", r.passed)
check("non-dict args are reported to the model",
      any("must be an object" in e for e in r.errors))
check("the event log records the raw payload",
      any(e.get("args", {}).get("_raw") for e in r.events if e["t"] == "tool"))

print("a check that could not run is recorded as not run")
sbdir = tmpdir(); s_ = os.path.join(sbdir, "s"); task.materialise(s_)
r = Harness(FakeClient([{"tool_calls": [FIX]},
                        {"tool_calls": [call("finish", summary="ok")]},
                        {"tool_calls": [call("finish", summary="ok")]}]),
            Config(), s_, task).run()
check("context guard reports itself off when num_ctx is unknown",
      r.ctx_guard_active is False)
check("and says so in the event log",
      any(e["t"] == "ctx_guard_disabled" for e in r.events))
sbdir = tmpdir(); s_ = os.path.join(sbdir, "s"); task.materialise(s_)
r = Harness(FakeClient([{"tool_calls": [FIX]},
                        {"tool_calls": [call("finish", summary="ok")]},
                        {"tool_calls": [call("finish", summary="ok")]}],
                       options={"num_ctx": 32768}), Config(), s_, task).run()
check("context guard reports itself on when num_ctx is known", r.ctx_guard_active is True)
check("no spurious disabled event",
      not any(e["t"] == "ctx_guard_disabled" for e in r.events))
check("containment is recorded on every episode",
      r.containment == containment_backend() and
      any(e["t"] == "containment" for e in r.events), str(r.containment))

print()

print("containment proves itself")
from mh.tools import containment_proves_itself, containment_backend
_ok, _backend, _detail = containment_proves_itself()
if _backend == "off":
    check("containment opt-out is not reported as containment", not _ok, _detail)
else:
    # The backend is a claim; this is the check. A profile that silently stops
    # confining -- a wrong path, a platform whose wrapper is a no-op -- would
    # otherwise be indistinguishable from a working one, and every episode
    # would still be recorded as contained.
    check(f"{_backend} actually confines reads and writes", _ok, _detail)
    check("a read of the bench tree is refused under containment",
          "did not stop a read" not in _detail, _detail)
    check("a write into the bench tree is refused under containment",
          "did not stop a write" not in _detail, _detail)

print(f"{len(PASS)} passed, {len(FAIL)} failed")
if FAIL:
    print("FAILURES:")
    for f in FAIL:
        print("  -", f)
sys.exit(1 if FAIL else 0)
