import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

// Attacks on verify.sh's on_exit: no way out of the script before a verdict may
// exit 0, and a non-zero status it was handed is kept.

const ROOT = path.join(path.dirname(fileURLToPath(import.meta.url)), "..");

/**
 * Runs verify.sh's real on_exit and trap line (cut out of the file, not copied)
 * in a scratch script whose remaining work is body.
 * @param {string} body
 * @param {string} shell
 * @param {{ prelude?: string }} [opts]
 */
function runOnExit(body, shell, opts = {}) {
  const lines = readFileSync(process.env.ZADV_VERIFY_SH ?? path.join(ROOT, "verify.sh"), "utf8").split("\n");
  const start = lines.findIndex((l) => l.startsWith("on_exit() {"));
  assert.ok(start >= 0, "verify.sh no longer defines on_exit()");
  const end = lines.findIndex((l, i) => i > start && l === "}");
  const trap = lines.findIndex((l, i) => i > end && l === "trap on_exit EXIT");
  assert.ok(end > start && trap > end, "on_exit() or its trap line moved; update this test");
  const dir = mkdtempSync(path.join(tmpdir(), "zadv-on-exit-"));
  const script = [
    "set -euo pipefail",
    "VERDICT_REACHED=0",
    "STEP_NAME=''",
    "step_mark() { :; }",
    opts.prelude ?? "",
    ...lines.slice(start, trap + 1),
    body.replaceAll("@DIR@", dir),
  ].join("\n");
  const file = path.join(dir, "fixture.sh");
  writeFileSync(file, script);
  const run = spawnSync(shell, [file], { encoding: "utf8", timeout: 15000 });
  return { status: run.status, signal: run.signal, err: run.stderr };
}

for (const shell of ["/bin/bash", "bash"]) {
  const never0 = (name, body, opts) =>
    test("zadv_w24va_onexit " + name + " (" + shell + ")", () => {
      const r = runOnExit(body, shell, opts);
      assert.equal(r.signal, null, "killed by " + r.signal);
      assert.notEqual(r.status, 0, "exited 0 before a verdict:\n" + r.err);
    });

  never0("set -u abort in a subshell", "( echo \"$nope\" )\necho after");
  never0("set -u abort in a command substitution", "x=\"$(echo \"$nope\")\"\necho after");
  never0("set -u abort in a function", "f() { echo \"$nope\"; }\nf\necho after");
  never0("set -u abort inside (( ))", "(( nope + 1 ))\necho after");
  never0("set -u abort inside $(( ))", "x=$(( nope + 1 ))\necho after");
  never0("set -u abort inside let", "let 'x = nope + 1'\necho after");
  never0("set -u abort inside a function's (( ))", "f() { (( nope > 0 )) || true; }\nf\necho after");
  never0("an ERR trap that exits 0", "trap 'exit 0' ERR\nfalse\necho after");
  never0("an ERR trap that only logs", "trap 'echo err >&2' ERR\nfalse\necho after");
  never0("falling off the end with no verdict", "true");
  never0("exit 0 from a sourced lib after VERDICT_REACHED was set and reset",
    "printf 'VERDICT_REACHED=1\\nVERDICT_REACHED=0\\nexit 0\\n' >@DIR@/lib.sh\nsource @DIR@/lib.sh");
  never0("VERDICT_REACHED unset, then exit 0", "unset VERDICT_REACHED\nexit 0");
  never0("VERDICT_REACHED holding a non-number, then exit 0", "VERDICT_REACHED=oops\nexit 0");
  never0("exit 0 while the trap's own cleanup fails", "mkdir @DIR@/prof\nGO_COVER_PROFILE=@DIR@/prof\nexit 0");

  for (const code of [1, 2, 7, 42, 126, 127, 130, 143, 255]) {
    test("zadv_w24va_onexit keeps status " + code + " (" + shell + ")", () => {
      const r = runOnExit("exit " + code, shell);
      assert.equal(r.status, code, r.err);
    });
  }

  test("zadv_w24va_onexit a status set by a failing command under set -e is kept (" + shell + ")", () => {
    const r = runOnExit("bash -c 'exit 9'\necho after", shell);
    assert.equal(r.status, 9, r.err);
  });

  for (const [sig, expected] of [["INT", 130], ["TERM", 143], ["HUP", 129]]) {
    test("zadv_w24va_onexit SIG" + sig + " before a verdict exits non-zero (" + shell + ")", () => {
      const r = runOnExit("kill -" + sig + " $$\nsleep 1\necho after", shell);
      // Either the 128+n status or a re-raised signal: both are a non-zero end.
      assert.ok(r.signal === "SIG" + sig || r.status === expected, "signal " + r.signal + " exit " + r.status + ":\n" + r.err);
      assert.match(r.err, /INCOMPLETE/, "the trap did not run");
    });
  }

  test("zadv_w24va_onexit a signal while a child runs ends non-zero and runs the trap (" + shell + ")", () => {
    const r = runOnExit("sleep 1 >/dev/null 2>&1 &\nkill -TERM $$\nwait", shell);
    assert.ok(r.signal === "SIGTERM" || (r.status !== 0 && r.status !== null), "signal " + r.signal + " exit " + r.status + ":\n" + r.err);
    assert.match(r.err, /INCOMPLETE/, "the trap did not run");
  });

  test("zadv_w24va_onexit a reached verdict with a non-zero status keeps it (" + shell + ")", () => {
    assert.equal(runOnExit("VERDICT_REACHED=1\nexit 3", shell).status, 3);
  });
}
