"""The harness: everything that decides what the model sees at each step.

Components are individually switchable (see Config) so their effect can be measured
rather than assumed. That is the whole point -- the paper's result is that harness
structure dominates for weak models, and a component nobody A/B'd is a guess.

NOTE: this file is the measuring instrument. The frozen grid under bench/results/
was collected and analysed with the pre-hardening harness and is NOT to be re-run
or re-scored; everything here applies to future runs only.
"""
import hashlib
import json
import os
import time

from . import tools as toolmod
from .bench import model_facing
from .model import AccountRefused
from .tools import Sandbox, ToolError, cap_output, containment_backend

ENVBOOT_TIMEOUT_S = 15
WALL_S_DEFAULT = 1800          # episode fail line; 30 minutes
HTTP_TIMEOUT_S = 1800          # per-request cap; harness shrinks this to remaining wall
FIRST_TURN_TIMEOUT_S = 600     # wedged llama-server never returns; real first turn is 1–3 min
LOOP_WINDOW = 8                # tool calls remembered for loop detection
MAX_TRUNCATED_NUDGES = 2       # consecutive output-limit turns before giving up


class Config:
    """One harness configuration. Defaults are the full harness."""

    FLAGS = ("envboot", "nativetools", "outcap", "checklist",
             "verifygate", "loopbreak", "groundfs")

    def __init__(self, name="full", envboot=True, nativetools=True, outcap=True,
                 checklist=True, verifygate=True, loopbreak=True, groundfs=True,
                 max_steps=0, wall_s=WALL_S_DEFAULT,
                 output_cap=toolmod.MAX_OUTPUT_BYTES):
        self.name = name
        self.envboot = envboot
        self.nativetools = nativetools
        self.outcap = outcap
        self.checklist = checklist
        self.verifygate = verifygate
        self.loopbreak = loopbreak
        self.groundfs = groundfs
        self.max_steps = max_steps
        # 0 disables. Default 1800s: a hung Ollama call is not a 62-minute task.
        self.wall_s = wall_s
        self.output_cap = output_cap if outcap else 10 ** 9

    def as_dict(self):
        d = {f: getattr(self, f) for f in self.FLAGS}
        d.update(name=self.name, max_steps=self.max_steps, wall_s=self.wall_s)
        return d

    @classmethod
    def baseline(cls):
        """Every discovered component off. This is the thing to beat."""
        return cls(name="baseline", envboot=False, checklist=False, verifygate=False,
                   loopbreak=False, groundfs=False, outcap=False)


# --- the discovered component: environment bootstrap -------------------------
#
# Meta-Harness §B.3: one compound shell command before the first LLM turn, injected
# as an [Environment Snapshot] block, guarded by a 15s timeout, failing silently.
# It removes the 2-4 turns an agent otherwise spends discovering what is installed.

ENVBOOT_SCRIPT = r"""
echo "cwd: $(pwd)"
echo "files:"
ls -Ap 2>/dev/null | head -20
n=$(ls -A 2>/dev/null | wc -l | tr -d ' ')
[ "$n" -gt 20 ] && echo "  ... ($n entries total)"
echo "languages:"
for c in python3 python gcc g++ node java rustc go cargo make; do
  v=$(command -v $c 2>/dev/null) || continue
  ver=$($c --version 2>&1 | head -1)
  echo "  $c: $ver"
done
echo "package managers:"
for c in pip3 pip npm cargo apt-get brew; do
  command -v $c >/dev/null 2>&1 && echo "  $c: $(command -v $c)"
done
echo "test runners:"
for c in pytest go; do
  command -v $c >/dev/null 2>&1 && echo "  $c available"
done
python3 -c 'import sys;print("  python packages:", ", ".join(sorted({m.split(".")[0] for m in ("pytest","numpy","json") if __import__("importlib.util",fromlist=["x"]).find_spec(m)})))' 2>/dev/null
echo "memory:"
if command -v vm_stat >/dev/null 2>&1; then
  echo "  $(vm_stat | head -2 | tr '\n' ' ')"
elif [ -r /proc/meminfo ]; then
  echo "  $(grep MemAvailable /proc/meminfo)"
fi
"""


def gather_env_snapshot(root, timeout=ENVBOOT_TIMEOUT_S):
    """Return the snapshot block, or "" if anything at all went wrong.

    Fails silently by contract: in an unusual environment a broken bootstrap must
    degrade to the baseline harness, never break the run.
    """
    try:
        _rc, out, _err = toolmod.run_bounded(["/bin/bash", "-lc", ENVBOOT_SCRIPT],
                                             cwd=root, timeout=timeout)
        out = out.strip()
    except Exception:
        return ""
    if not out:
        return ""
    body, _ = cap_output(out, 4000)
    return f"[Environment Snapshot]\n{body}\n[End Environment Snapshot]"


SYSTEM_PROMPT = """You are a software engineer working autonomously in a terminal.

You have these tools: run_shell, read_file, write_file, edit_file, finish.

Rules that matter:
- Investigate before you edit. Read a file before changing it; never guess at its
  contents, at a path, or at an API you have not seen in this session.
- Make the smallest change that fixes the actual cause. Do not rewrite whole files
  when an edit will do.
- Run the tests yourself and read the real output. A test you did not run is not a
  test that passed.
- Never edit or delete the tests to make them pass. Fix the code.
- When you are done, call finish with a summary of what you changed and the command
  output that shows it works.

Work in small steps: one tool call, look at the result, then decide the next step."""

SYSTEM_PROMPT_SHELL = """You are a software engineer working autonomously in a terminal.

You have these tools: run_shell, finish. There is no read_file, write_file, or edit_file.
Inspect and change files only through run_shell (cat, tee, python, sed).

Rules that matter:
- Investigate before you edit. Read a file before changing it; never guess at its
  contents, at a path, or at an API you have not seen in this session.
- Make the smallest change that fixes the actual cause. Do not rewrite whole files
  when an edit will do.
- Run the tests yourself and read the real output. A test you did not run is not a
  test that passed.
- Never edit or delete the tests to make them pass. Fix the code.
- When you are done, call finish with a summary of what you changed and the command
  output that shows it works.

Work in small steps: one tool call, look at the result, then decide the next step."""

CHECKLIST = """Before calling finish, check each of these and state your answer to each:
1. Did I run the project's tests, in this session, and see them pass in real output?
2. Does my change fix the underlying cause, or only the one input in the example?
3. Did I leave any test, assertion or file weakened, deleted or stubbed?
4. Did I change anything the task did not ask me to change?
If any answer is unsatisfactory, keep working instead of calling finish."""


class Result:
    def __init__(self):
        self.steps = 0
        self.tool_calls = 0
        self.finished = False           # model called finish
        self.passed = False             # verifier said so
        self.stop_reason = ""
        self.wall_s = 0.0
        self.prompt_tokens = 0
        self.output_tokens = 0
        self.model_latency_s = 0.0
        self.errors = []
        self.events = []
        self.verify_output = ""
        self.verify_runs = 0
        # Recorded, never assumed: an episode that ran without OS containment,
        # or without a working context guard, must not be indistinguishable in
        # the record from one that had both.
        self.containment = None
        self.ctx_guard_active = False
        self.peak_prompt_tokens = 0
        self.eval_duration_ns = 0
        self.prompt_eval_duration_ns = 0

    def as_dict(self):
        return {k: v for k, v in self.__dict__.items()}


def _call_signature(call):
    """A stable key for "the model made this exact call again".

    Same rule as the event log below: anything derived from model output is
    parsed defensively. A payload json.dumps cannot serialise must degrade to a
    weaker signature, never kill the episode.
    """
    try:
        return json.dumps({"name": call["name"], "args": call["args"]}, sort_keys=True)
    except (TypeError, ValueError):
        return repr((call.get("name"), call.get("args")))


class Harness:
    def __init__(self, client, config, sandbox_root, task, log_dir=None):
        self.client = client
        self.cfg = config
        self.task = task
        self.sb = Sandbox(sandbox_root, output_cap=config.output_cap,
                          protected_roots=task.guard_roots)
        self.log_dir = log_dir
        self.res = Result()
        backend = containment_backend()
        self.res.containment = backend
        if backend != "sandbox-exec":
            self.res.events.append(
                {"t": "containment", "backend": backend,
                 "note": "shell commands are not OS-contained in this episode"})
        else:
            self.res.events.append({"t": "containment", "backend": backend})
        # Ollama silently drops the oldest tokens when a prompt exceeds num_ctx.
        # Silent truncation would corrupt a run invisibly -- the model would answer
        # from a context we did not give it -- so we watch the headroom and stop
        # loudly instead of producing a quiet, wrong result.
        self.num_ctx = int(getattr(client, "options", {}).get("num_ctx", 0) or 0)
        self.ctx_limit = int(self.num_ctx * 0.9) if self.num_ctx else 0
        # The guard used to switch itself off in silence when num_ctx was absent,
        # so "guard never fired" and "guard never ran" looked identical in the
        # record. Say which one it was.
        self.res.ctx_guard_active = bool(self.ctx_limit)
        if not self.ctx_limit:
            self.res.events.append(
                {"t": "ctx_guard_disabled",
                 "reason": "client reported no num_ctx; prompt growth is unchecked "
                           "and silent server-side truncation would go unnoticed"})
        self._checked = False

    # -- context construction -------------------------------------------------

    def initial_messages(self):
        prompt = SYSTEM_PROMPT if self.cfg.nativetools else SYSTEM_PROMPT_SHELL
        msgs = [{"role": "system", "content": prompt}]
        parts = []
        if self.cfg.envboot:
            snap = gather_env_snapshot(self.sb.root)
            if snap:
                parts.append(snap)
                self.res.events.append({"t": "envboot", "bytes": len(snap)})
            else:
                self.res.events.append({"t": "envboot_empty"})
        parts.append(f"[Task]\n{self.task.instruction}")
        if self.cfg.groundfs:
            parts.append("Everything you need is already in the task directory. "
                         "Read files before you edit them.")
        msgs.append({"role": "user", "content": "\n\n".join(parts)})
        return msgs

    # -- tool execution -------------------------------------------------------

    def dispatch(self, call):
        name, args = call["name"], call["args"]
        if not self.cfg.nativetools and name in toolmod.FILE_TOOLS:
            raise ToolError(
                f"{name} is not available in this harness. Use run_shell to "
                "read or edit files.")
        if name not in toolmod.TOOL_NAMES:
            raise ToolError(
                f"there is no tool named {name!r}. The available tools are: "
                f"{', '.join(toolmod.TOOL_NAMES)}.")
        fn = getattr(self.sb, name)
        if not isinstance(args, dict):
            raise ToolError(f"arguments for {name} must be an object")
        return fn(**args)

    def run_verifier(self):
        """Run the task's verifier. It lives outside the sandbox and the agent
        cannot read or edit it.

        Returns (ok, raw, for_model). `raw` is the record kept in the episode log
        for the researcher; `for_model` is the redacted verdict -- pass/fail and
        assertion labels -- and is the only thing that may reach the model.
        """
        self.res.verify_runs += 1
        ok, output = self.task.verify(self.sb.root)
        self.res.verify_output = output
        return ok, output, model_facing(ok, output)

    # -- main loop ------------------------------------------------------------

    def run(self):
        t0 = time.time()
        msgs = self.initial_messages()
        recent = []              # (call signature, hash of that call's output)
        nudged_finish = False
        truncated_nudges = 0

        try:
            # 0 / None / negative: no turn ceiling. Stop on finish, no_tool_call,
            # context_exhausted, or the wall-clock budget — not an arbitrary
            # 40-step cap.
            budget = self.cfg.max_steps or 0
            uncapped = budget <= 0
            wall = self.cfg.wall_s or 0
            while uncapped or self.res.steps < budget:
                elapsed = time.time() - t0
                if wall > 0 and elapsed >= wall:
                    self.res.stop_reason = "wall_timeout"
                    self.res.errors.append(
                        f"episode exceeded {wall:.0f}s wall clock and was failed")
                    self.res.events.append(
                        {"t": "wall_timeout", "elapsed_s": round(elapsed, 1),
                         "wall_s": wall})
                    break
                if wall > 0 and hasattr(self.client, "timeout"):
                    remaining = wall - elapsed
                    cap = getattr(self.client, "http_timeout", HTTP_TIMEOUT_S)
                    if self.res.steps == 0:
                        cap = min(int(cap), int(FIRST_TURN_TIMEOUT_S))
                    self.client.timeout = max(1, min(int(cap), int(remaining)))
                self.res.steps += 1
                reply = self.client.chat(msgs, tools=toolmod.schemas_for(self.cfg.nativetools))
                self.res.prompt_tokens += reply.prompt_tokens
                self.res.output_tokens += reply.output_tokens
                self.res.model_latency_s += reply.latency_s
                self.res.eval_duration_ns += reply.eval_duration_ns
                self.res.prompt_eval_duration_ns += reply.prompt_eval_duration_ns

                ev = {"t": "assistant", "step": self.res.steps,
                      "latency_s": round(reply.latency_s, 2),
                      "out_tok": reply.output_tokens,
                      "prompt_tok": reply.prompt_tokens,
                      "n_calls": len(reply.tool_calls),
                      "reasoning_chars": len(reply.reasoning),
                      "content": reply.content[:400]}
                if reply.truncated:
                    ev["truncated"] = True
                self.res.events.append(ev)
                self.res.peak_prompt_tokens = max(self.res.peak_prompt_tokens,
                                                  reply.prompt_tokens)
                if self.ctx_limit and reply.prompt_tokens > self.ctx_limit:
                    self.res.events.append(
                        {"t": "context_exhausted", "step": self.res.steps,
                         "prompt_tokens": reply.prompt_tokens,
                         "num_ctx": self.num_ctx})
                    self.res.errors.append(
                        f"prompt reached {reply.prompt_tokens} tokens against "
                        f"num_ctx={self.num_ctx}; stopping rather than letting the "
                        f"server silently drop context")
                    self.res.stop_reason = "context_exhausted"
                    break

                # Append-only history: we never rewrite earlier turns, so the server's
                # KV prefix stays valid. Rewriting history costs a full re-prefill.
                assistant_msg = {"role": "assistant", "content": reply.content}
                if reply.tool_calls:
                    assistant_msg["tool_calls"] = [
                        {"id": c.get("id") or f"call_{i}",
                         "signature": c.get("signature"),
                         "function": {"name": c["name"], "arguments": c["args"]}}
                        for i, c in enumerate(reply.tool_calls)]
                msgs.append(assistant_msg)

                if not reply.tool_calls:
                    # No tool call and no finish: the model is talking, not working.
                    if reply.truncated:
                        # This branch used to `continue` without touching any
                        # counter: with no step ceiling and no wall clock it ran
                        # 61,406 steps in 3.1s and never terminated.
                        truncated_nudges += 1
                        if truncated_nudges > MAX_TRUNCATED_NUDGES:
                            self.res.stop_reason = "no_tool_call"
                            self.res.errors.append(
                                f"model hit the output limit {truncated_nudges} turns "
                                f"in a row without making a tool call")
                            self.res.events.append(
                                {"t": "truncated_stall", "step": self.res.steps,
                                 "turns": truncated_nudges})
                            break
                        msgs.append({"role": "user", "content":
                                     "Your previous message hit the output limit. Make one "
                                     "small tool call instead of a long explanation."})
                        continue
                    if nudged_finish:
                        self.res.stop_reason = "no_tool_call"
                        break
                    nudged_finish = True
                    msgs.append({"role": "user", "content":
                                 "Continue by making a tool call. If the work is genuinely "
                                 "complete, call finish."})
                    continue

                nudged_finish = False
                truncated_nudges = 0
                stop = False
                # Answer every call the model made. Handling `finish` last means a
                # turn that mixes real work with a finish never leaves a call
                # unanswered -- a transcript with fewer tool results than tool calls
                # is one the model has to reason around.
                work = [c for c in reply.tool_calls if c["name"] != "finish"]
                closing = [c for c in reply.tool_calls if c["name"] == "finish"]

                for call in work:
                    self.res.tool_calls += 1
                    sig = _call_signature(call)
                    # Loop detection keys on the call *and its result*. Counting
                    # the call alone blocked the legitimate edit -> test -> edit
                    # cycle and told the model it "got the same result" when the
                    # file had changed between runs, which was simply false.
                    prior = [h for s_, h in recent if s_ == sig]
                    looping = len(prior) >= 2 and prior[-1] == prior[-2]
                    if self.cfg.loopbreak and looping:
                        self.res.events.append(
                            {"t": "loopbreak", "step": self.res.steps,
                             "call": call["name"], "window": len(recent)})
                        msgs.append({"role": "tool",
                                     "name": call["name"],
                                     "tool_call_id": call.get("id"),
                                     "content":
                                     f"You have already made this exact {call['name']} "
                                     "call twice and got byte-identical output both "
                                     "times. Repeating it will not help. State what you "
                                     "actually know, then try a different approach."})
                        # Record the block like a repeat, and trim: the old code
                        # appended here without the window trim, so 200 identical
                        # calls grew `recent` without bound and the count never
                        # decayed.
                        recent.append((sig, prior[-1]))
                        if len(recent) > LOOP_WINDOW:
                            recent.pop(0)
                        continue
                    try:
                        out = self.dispatch(call)
                    except ToolError as e:
                        out = f"error: {e}"
                        self.res.errors.append(str(e))
                    except Exception as e:  # a tool bug must not kill the run
                        out = f"error: {type(e).__name__}: {e}"
                        self.res.errors.append(f"{type(e).__name__}: {e}")
                    recent.append((sig, hashlib.sha256(
                        out.encode("utf-8", "replace")).hexdigest()))
                    if len(recent) > LOOP_WINDOW:
                        recent.pop(0)
                    args = call.get("args")
                    # dispatch() already defends against a non-dict args payload;
                    # this line used to call .items() on it outside the try and
                    # killed the whole episode instead.
                    logged = ({k: str(v)[:200] for k, v in args.items()}
                              if isinstance(args, dict) else {"_raw": str(args)[:200]})
                    self.res.events.append(
                        {"t": "tool", "step": self.res.steps, "name": call["name"],
                         "args": logged, "out": out[:600], "out_len": len(out)})
                    msgs.append({"role": "tool",
                                 "name": call["name"],
                                 "tool_call_id": call.get("id"),
                                 "content": out})

                for call in closing[:1]:
                    self.res.tool_calls += 1
                    if self.cfg.checklist and not self._checked:
                        self._checked = True
                        self.res.events.append({"t": "checklist"})
                        msgs.append({"role": "tool",
                                     "name": "finish",
                                     "tool_call_id": call.get("id"),
                                     "content": CHECKLIST})
                        break
                    if self.cfg.verifygate:
                        ok, raw, for_model = self.run_verifier()
                        self.res.events.append(
                            {"t": "verifygate", "ok": ok, "step": self.res.steps})
                        if ok:
                            self.res.finished = True
                            self.res.stop_reason = "finished"
                            stop = True
                            break
                        # Only the redacted verdict goes back. The raw text is the
                        # hidden test's own stdout -- inputs and expected values --
                        # and handing it to the model turned the gate into an
                        # oracle in 77 of the 760 frozen episodes.
                        body, _ = cap_output(for_model, 4000)
                        msgs.append({"role": "tool",
                                     "name": "finish",
                                     "tool_call_id": call.get("id"),
                                     "content": body + "\n\nKeep working."})
                        break
                    self.res.finished = True
                    self.res.stop_reason = "finished"
                    stop = True

                # a duplicate finish in the same turn still needs a reply
                for call in closing[1:]:
                    msgs.append({"role": "tool",
                                 "name": "finish",
                                 "tool_call_id": call.get("id"),
                                 "content":
                                 "Only one finish per turn is considered."})

                if stop:
                    break
            else:
                self.res.stop_reason = "max_steps"
        except AccountRefused:
            # Deliberately not scored. The provider refused the account, so this
            # episode is not a measurement of anything and must not become a row
            # that reads like the model failing. The runner stops the cell.
            raise
        except Exception as e:
            elapsed = time.time() - t0
            wall = self.cfg.wall_s or 0
            if wall > 0 and elapsed >= wall:
                self.res.stop_reason = "wall_timeout"
                self.res.errors.append(
                    f"episode exceeded {wall:.0f}s wall clock ({type(e).__name__}: {e})")
                self.res.events.append(
                    {"t": "wall_timeout", "elapsed_s": round(elapsed, 1),
                     "wall_s": wall, "cause": type(e).__name__})
            else:
                self.res.stop_reason = f"error:{type(e).__name__}"
                self.res.errors.append(f"{type(e).__name__}: {e}")

        # Final verification always runs, whatever the model claimed. A run that
        # stopped on max_steps may still have fixed the code; a run that called
        # finish may not have. wall_timeout is the exception: the episode is a
        # fail even if the verifier would pass, so a hung client cannot score.
        ok, out, _ = self.run_verifier()
        self.res.verify_output = out
        if self.res.stop_reason == "wall_timeout":
            self.res.passed = False
            self.res.events.append({"t": "wall_timeout_score", "verify_ok": ok})
        else:
            self.res.passed = ok
        self.res.wall_s = time.time() - t0
        return self.res
