"""Tool surface for the harness.

Five tools, deliberately. Small models degrade as the tool count grows, and every
tool here earns its place by removing a class of failure: read/write/edit exist so
the model never has to get shell quoting right to touch a file.

Everything is confined to one sandbox directory. The agent is a 27-35B model running
real shell on a real machine, so containment is enforced on our side, by resolved
path, not by asking the model nicely.
"""
import os
import subprocess

MAX_OUTPUT_BYTES = 30_000       # Terminus-KIRA's cap, inherited by the paper's harness
SHELL_TIMEOUT_S = 120


class ToolError(Exception):
    """Recoverable: the message goes back to the model as the tool result."""


def cap_output(text, limit=MAX_OUTPUT_BYTES):
    """Head+tail truncation.

    Never drop the middle silently: the tail of a build log holds the error and the
    head holds the command, and a model shown a quietly-truncated log will confidently
    reason about output it never saw.
    """
    if limit <= 0 or len(text) <= limit:
        return text, False
    keep = limit // 2
    head, tail = text[:keep], text[-keep:]
    dropped = len(text) - 2 * keep
    return (f"{head}\n\n... [{dropped} bytes elided by the harness; "
            f"{keep} head + {keep} tail bytes shown] ...\n\n{tail}"), True


class Sandbox:
    def __init__(self, root, output_cap=MAX_OUTPUT_BYTES):
        self.root = os.path.realpath(root)
        self.output_cap = output_cap

    def resolve(self, path):
        """Resolve a model-supplied path inside the sandbox, or refuse."""
        if not isinstance(path, str) or not path.strip():
            raise ToolError("path must be a non-empty string")
        p = path.strip()
        full = p if os.path.isabs(p) else os.path.join(self.root, p)
        real = os.path.realpath(full)
        if real != self.root and not real.startswith(self.root + os.sep):
            raise ToolError(
                f"path {path!r} resolves outside the task directory and was refused. "
                f"Work only inside {self.root}.")
        return real

    # ---- tool implementations ------------------------------------------------

    def run_shell(self, cmd=None, **_):
        if not isinstance(cmd, str) or not cmd.strip():
            raise ToolError("run_shell requires a non-empty 'cmd' string")
        try:
            proc = subprocess.run(
                ["/bin/bash", "-lc", cmd], cwd=self.root, capture_output=True,
                text=True, errors="replace", timeout=SHELL_TIMEOUT_S)
        except subprocess.TimeoutExpired:
            raise ToolError(
                f"command timed out after {SHELL_TIMEOUT_S}s and was killed. "
                f"Long-running or interactive commands will not work here.")
        out = proc.stdout or ""
        err = proc.stderr or ""
        body = out
        if err.strip():
            body += ("\n" if body and not body.endswith("\n") else "") + "[stderr]\n" + err
        body, _ = cap_output(body, self.output_cap)
        return f"exit={proc.returncode}\n{body}" if body.strip() else f"exit={proc.returncode}\n(no output)"

    def read_file(self, path=None, **_):
        real = self.resolve(path)
        if not os.path.exists(real):
            raise ToolError(f"{path!r} does not exist. List the directory before reading.")
        if os.path.isdir(real):
            entries = sorted(os.listdir(real))
            return f"{path} is a directory containing:\n" + "\n".join(entries[:200])
        try:
            with open(real, "r", errors="replace") as f:
                text = f.read()
        except OSError as e:
            raise ToolError(f"could not read {path!r}: {e}")
        numbered = "\n".join(f"{i:>5}\t{line}"
                             for i, line in enumerate(text.splitlines(), 1))
        body, _ = cap_output(numbered, self.output_cap)
        return body if body.strip() else "(empty file)"

    def write_file(self, path=None, content=None, **_):
        real = self.resolve(path)
        if content is None:
            raise ToolError("write_file requires 'content'")
        if not isinstance(content, str):
            content = str(content)
        os.makedirs(os.path.dirname(real) or self.root, exist_ok=True)
        try:
            with open(real, "w") as f:
                f.write(content)
        except OSError as e:
            raise ToolError(f"could not write {path!r}: {e}")
        return f"wrote {len(content)} bytes to {path}"

    def edit_file(self, path=None, old=None, new=None, **_):
        real = self.resolve(path)
        if not os.path.exists(real):
            raise ToolError(f"{path!r} does not exist; use write_file to create it.")
        if not isinstance(old, str) or old == "":
            raise ToolError("edit_file requires a non-empty 'old' string to replace")
        if new is None:
            new = ""
        with open(real, "r", errors="replace") as f:
            text = f.read()
        count = text.count(old)
        if count == 0:
            raise ToolError(
                f"the 'old' text was not found in {path!r}. Read the file again and "
                f"copy the exact text, including indentation.")
        if count > 1:
            raise ToolError(
                f"the 'old' text appears {count} times in {path!r}; it must match "
                f"exactly once. Include more surrounding context to disambiguate.")
        with open(real, "w") as f:
            f.write(text.replace(old, new, 1))
        return f"replaced 1 occurrence in {path}"

    def finish(self, summary=None, **_):
        return summary or ""


SCHEMAS = [
    {"type": "function", "function": {
        "name": "run_shell",
        "description": "Run a bash command in the task directory and return its exit "
                       "code and combined output. Use this to explore, build and test.",
        "parameters": {"type": "object", "properties": {
            "cmd": {"type": "string", "description": "The bash command to run."}},
            "required": ["cmd"]}}},
    {"type": "function", "function": {
        "name": "read_file",
        "description": "Read a file and return it with line numbers. If given a "
                       "directory, lists its entries.",
        "parameters": {"type": "object", "properties": {
            "path": {"type": "string", "description": "Path relative to the task directory."}},
            "required": ["path"]}}},
    {"type": "function", "function": {
        "name": "write_file",
        "description": "Create or overwrite a file with the exact content given.",
        "parameters": {"type": "object", "properties": {
            "path": {"type": "string", "description": "Path relative to the task directory."},
            "content": {"type": "string", "description": "Full new content of the file."}},
            "required": ["path", "content"]}}},
    {"type": "function", "function": {
        "name": "edit_file",
        "description": "Replace one exact unique occurrence of 'old' with 'new' in a "
                       "file. Preferred over write_file for small changes.",
        "parameters": {"type": "object", "properties": {
            "path": {"type": "string", "description": "Path relative to the task directory."},
            "old": {"type": "string", "description": "Exact text to replace; must occur exactly once."},
            "new": {"type": "string", "description": "Replacement text."}},
            "required": ["path", "old", "new"]}}},
    {"type": "function", "function": {
        "name": "finish",
        "description": "Declare the task complete. Only call this after you have run "
                       "the tests and seen them pass.",
        "parameters": {"type": "object", "properties": {
            "summary": {"type": "string", "description": "What you changed and how you verified it."}},
            "required": ["summary"]}}},
]

TOOL_NAMES = [s["function"]["name"] for s in SCHEMAS]
FILE_TOOLS = ("read_file", "write_file", "edit_file")


def schemas_for(nativetools=True):
    """Full five-tool surface, or shell+finish only.

    Native file tools exist so a small model never has to quote a pipeline to
    touch a file. Turning them off is the ablation: the model must go through
    run_shell. The flag was a no-op until this function was the single caller.
    """
    if nativetools:
        return SCHEMAS
    return [s for s in SCHEMAS if s["function"]["name"] not in FILE_TOOLS]
