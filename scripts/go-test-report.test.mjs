// The report is only worth having if it names a hung test and does not name a
// finished one. Each case below is a shape `go test -json` really emits.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import { parseLine, renderReport, renderText, summarize } from "./go-test-report.mjs";

const SCRIPT = fileURLToPath(new URL("./go-test-report.mjs", import.meta.url));
const ev = (o) => JSON.stringify({ Time: "2026-11-15T01:00:00Z", ...o });
const P = "github.com/bharathvbcr/Manvi/manvi/codingagent";

test("a clean run reports nothing unfinished and ranks the slowest first", () => {
  const s = summarize([
    ev({ Action: "start", Package: P }),
    ev({ Action: "run", Package: P, Test: "TestA" }),
    ev({ Action: "pass", Package: P, Test: "TestA", Elapsed: 0.2 }),
    ev({ Action: "run", Package: P, Test: "TestB" }),
    ev({ Action: "pass", Package: P, Test: "TestB", Elapsed: 4.5 }),
    ev({ Action: "pass", Package: P, Elapsed: 5 }),
  ]);
  assert.equal(s.unfinishedTests.length, 0);
  assert.equal(s.unfinishedPackages.length, 0);
  assert.deepEqual(s.slowestTests.map((t) => t.test), ["TestB", "TestA"]);
  assert.match(renderReport(s), /Every package and every test that started also finished/);
});

test("a test that started and never ended is named, and the finished one beside it is not", () => {
  const s = summarize([
    ev({ Action: "run", Package: P, Test: "TestDone" }),
    ev({ Action: "pass", Package: P, Test: "TestDone", Elapsed: 1 }),
    ev({ Action: "run", Package: P, Test: "TestWedged" }),
    ev({ Action: "run", Package: P, Test: "TestWedged/sub" }),
  ]);
  assert.deepEqual(s.unfinishedTests.map((t) => t.test).sort(), ["TestWedged", "TestWedged/sub"]);
  const md = renderReport(s);
  assert.match(md, /Started and never finished/);
  // The suspects section runs from its heading to the next bold heading. A
  // finished test may appear in the slowest table below it, and must not
  // appear here.
  const suspects = md.slice(md.indexOf("Started and never finished"), md.indexOf("**Slowest"));
  assert.match(suspects, /TestWedged/);
  assert.doesNotMatch(suspects, /TestDone/, "a finished test must not be listed as a suspect");
});

test("a package that started and never finished is named", () => {
  const s = summarize([
    ev({ Action: "start", Package: P }),
    ev({ Action: "start", Package: "github.com/bharathvbcr/Manvi/manvi/agent" }),
    ev({ Action: "pass", Package: "github.com/bharathvbcr/Manvi/manvi/agent", Elapsed: 2 }),
  ]);
  assert.deepEqual(s.unfinishedPackages.map((p) => p.pkg), [P]);
  assert.match(renderReport(s), /package `manvi\/codingagent`/);
});

test("a package that failed to build is finished, not hung", () => {
  const s = summarize([ev({ Action: "start", Package: P }), ev({ Action: "fail", Package: P, Elapsed: 0 })]);
  assert.equal(s.unfinishedPackages.length, 0);
});

test("a run with no events says so instead of saying nothing hung", () => {
  const md = renderReport(summarize([]));
  assert.match(md, /no `go test -json` events/);
  assert.doesNotMatch(md, /Every package and every test/);
});

test("build output that is not JSON is counted, not fatal", () => {
  const s = summarize(["# pkg\n", "./x.go:3: syntax error", ev({ Action: "pass", Package: P, Elapsed: 1 }), ""]);
  assert.equal(s.unparsed, 2);
  assert.equal(s.events, 1);
  const only = renderReport(summarize(["./x.go:3: syntax error"]));
  assert.match(only, /1 line\(s\) were not JSON events/);
});

test("text mode gives back what plain go test prints, and passes non-JSON through", () => {
  assert.equal(renderText(ev({ Action: "output", Package: P, Output: "ok  \tpkg\t1.2s\n" })), "ok  \tpkg\t1.2s\n");
  assert.equal(renderText(ev({ Action: "run", Package: P, Test: "TestA" })), "");
  assert.equal(renderText("# pkg [pkg.test]"), "# pkg [pkg.test]\n");
  assert.equal(parseLine("42"), null, "a bare JSON number is not an event");
  assert.equal(parseLine("{\"Action\":1}"), null);
});

test("text mode drops progress and passes, and keeps failures, skips and what tests printed", () => {
  const out = (Output) => renderText(ev({ Action: "output", Package: P, Test: "TestA", Output }));
  for (const noise of ["=== RUN   TestA\n", "=== PAUSE TestA\n", "=== CONT  TestA\n", "=== NAME  TestA/sub\n", "--- PASS: TestA (0.00s)\n", "    --- PASS: TestA/sub (0.00s)\n"]) {
    assert.equal(out(noise), "", `dropped: ${JSON.stringify(noise)}`);
  }
  for (const kept of [
    "--- FAIL: TestA (0.01s)\n",
    "    --- FAIL: TestA/sub (0.01s)\n",
    "--- SKIP: TestA (0.00s)\n",
    "    a_test.go:12: the reason it skipped\n",
    "    a_test.go:40: want 1, got 2\n",
    "FAIL\n",
    "ok  \tgithub.com/x/y\t1.2s\n",
    "panic: test timed out after 20m0s\n",
  ]) {
    assert.equal(out(kept), kept, `kept: ${JSON.stringify(kept)}`);
  }
});

test("a reader that hangs up early is not an error", () => {
  const many = Array.from({ length: 20000 }, (_, i) => ev({ Action: "output", Package: P, Output: `line ${i}\n` })).join("\n");
  const r = spawnSync("sh", ["-c", 'node "$0" text | head -n 1', SCRIPT], { input: many, encoding: "utf8" });
  assert.equal(r.stdout, "line 0\n");
  assert.equal(r.stderr, "", "an EPIPE stack trace belongs in no log");
});

test("the command line reads a file and exits 0, and refuses a bad invocation", () => {
  const ok = spawnSync(process.execPath, [SCRIPT, "text"], {
    input: `${ev({ Action: "output", Package: P, Output: "hello\n" })}\n`,
    encoding: "utf8",
  });
  assert.equal(ok.status, 0);
  assert.equal(ok.stdout, "hello\n");
  const bad = spawnSync(process.execPath, [SCRIPT, "bogus"], { encoding: "utf8" });
  assert.equal(bad.status, 2);
  assert.match(bad.stderr, /usage:/);
});
