import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { LEGS, compare, parseLeg, parseVerdict } from "./check-verify-gaps.mjs";

const EXPECTED = JSON.parse(readFileSync(new URL("./verify-expected-gaps.json", import.meta.url), "utf8"));

// The verdict verify.sh printed on ubuntu-latest at d6c956f, colour codes and all,
// with its skip list taken from the expected file, so an entry added since (the
// approve-fd re-exec helper) is in it as CI now prints it.
const UBUNTU_SKIPS = EXPECTED.skips.filter((s) => s.os.includes("ubuntu-latest")).map((s) => s.test);
const UBUNTU = [
  "    covered: 874 checks",
  "",
  "\x1b[33mPASS\x1b[0m with gates that did not run:",
  `    - ${UBUNTU_SKIPS.length} of 3005 test(s) skipped, so their assertions did not run: ` + UBUNTU_SKIPS.join(", "),
  "    - devmap not found — repo navigation is unverified here",
  "    - local — this gate makes no request. The harness reports:",
  "    - anthropic, gemini and xai are verified against scripted servers only.",
  // The bench leg's C6 fork denial, which bwrap cannot give (since 2026-11-19).
  "    - bench/stress_test.py: C6 fork denial — the bwrap profile has no seccomp filter, so a Linux worker can fork (the macOS sandbox-exec profile denies it)",
  "",
].join("\n");

test("the ubuntu verdict CI produced is fully expected", () => {
  const found = parseVerdict(UBUNTU);
  assert.equal(found.skips.length, UBUNTU_SKIPS.length);
  assert.equal(found.gaps.length, 4);
  assert.deepEqual(compare(found, EXPECTED, "ubuntu-latest"), { unexpectedGaps: [], unexpectedSkips: [], stale: [] });
});

test("a new NOT COVERED line fails", () => {
  const found = parseVerdict(UBUNTU.replace("    - devmap not found", "    - sqlite3 not on PATH — schema readability is unverified here\n    - devmap not found"));
  assert.deepEqual(compare(found, EXPECTED, "ubuntu-latest").unexpectedGaps, [
    "sqlite3 not on PATH — schema readability is unverified here",
  ]);
});

test("a new skipped test fails, and an OS-specific skip is not expected elsewhere", () => {
  const found = parseVerdict(UBUNTU.replace("did not run: ", "did not run: example.com/pkg.TestNew, "));
  assert.deepEqual(compare(found, EXPECTED, "ubuntu-latest").unexpectedSkips, ["example.com/pkg.TestNew"]);
  const mac = compare(parseVerdict(UBUNTU), EXPECTED, "macos-latest");
  assert.ok(mac.unexpectedSkips.some((s) => s.endsWith("TestSpellingVariantOfACreatedDirectoryCannotEscape")));
});

test("a clean PASS has no gaps, and a log with no verdict is not a pass", () => {
  assert.deepEqual(parseVerdict("\x1b[32mPASS\x1b[0m all gates\n"), { verdict: true, gaps: [], skips: [] });
  assert.equal(parseVerdict("FAIL go test\n").verdict, false);
});

test("every expected entry says why", () => {
  for (const entry of [...EXPECTED.gaps, ...EXPECTED.skips]) {
    assert.ok(entry.why && entry.why.length > 10, JSON.stringify(entry));
    assert.ok(entry.os.length > 0, JSON.stringify(entry));
  }
});

test("the macos verdict CI produced is fully expected", () => {
  // verify.sh (macos-latest) at d6c956f, attempt 2: APFS runs the three
  // spelling tests, so four skips rather than seven; plus the approve-fd re-exec
  // helper, which skips on both runners since it was added, as do the TUI pty
  // and killed-save re-exec helpers. Since 2026-11-09 (ft-7a1354a5) it also
  // skips the ESC-[ boundary test (every platform) and the XDG-in-repo test
  // (darwin's user config dir ignores XDG), both measured on macOS.
  const skips = [
    "github.com/bharathvbcr/Manvi/manvi/ui/input.TestAdvReadBoundaryInsideStartMarkerWithGapPastEscapeDelay",
    "github.com/bharathvbcr/Manvi/manvi/cmd/manvi.TestZAdvUserConfigViaXDGInsideRepo",
    "github.com/bharathvbcr/Manvi/manvi/codingagent.TestInstalledCodexHandshakeWithoutModelTurn",
    "github.com/bharathvbcr/Manvi/manvi/codingagent.TestInstalledCodexReadOnlyTurn",
    "github.com/bharathvbcr/Manvi/manvi/cmd/manvi.TestApproveFDBareApproverHelper",
    "github.com/bharathvbcr/Manvi/manvi/cmd/manvi.TestTUIPtyHelper",
    "github.com/bharathvbcr/Manvi/manvi/session.TestSaveKillChild",
    "github.com/bharathvbcr/Manvi/manvi/session.TestAdvFirstSaveKillChild",
    "github.com/bharathvbcr/Manvi/manvi/ui/tui.TestEveryUntrustedEventFieldIsCleanedOnTheWayIntoTheFrame/agent",
    "github.com/bharathvbcr/Manvi/manvi/ui/tui.TestEveryUntrustedEventFieldIsCleanedOnTheWayIntoTheFrame/path",
    // Since 2026-11-19: three tests that need an installed agy, Claude Code or
    // devmap binary, which no runner has, measured on macOS.
    "github.com/bharathvbcr/Manvi/manvi/codingagent.TestAgyRealBinaryACPHandshakeAndCrossProcessResume",
    "github.com/bharathvbcr/Manvi/manvi/codingagent.TestRealClaudeBinaryStreamJSONHandshake",
    "github.com/bharathvbcr/Manvi/manvi/devcouncil.TestGitWorktreeCoordinatorRealDevmapBinaryStackedSeed",
  ];
  const log = [
    "PASS with gates that did not run:",
    `    - ${skips.length} of 3005 test(s) skipped, so their assertions did not run: ${skips.join(", ")}`,
    "    - devmap not found — repo navigation is unverified here",
    "    - local — this gate makes no request. The harness reports:",
    "    - anthropic, gemini and xai are verified against scripted servers only.",
  ].join("\n");
  assert.deepEqual(compare(parseVerdict(log), EXPECTED, "macos-latest"), { unexpectedGaps: [], unexpectedSkips: [], stale: [] });
});

test("every expected skip names a Go test that still exists", () => {
  // verify.sh only reports a skip that happened, so an entry whose test was
  // deleted (TestEvidenceRustCompatibility, in cab5166) is never contradicted
  // by a run and would sit here certifying nothing. Entries are
  // <import path>.<Test>[/<subtest>]; the test must be declared in that package.
  const module = "github.com/bharathvbcr/Manvi/manvi/";
  for (const { test: name } of EXPECTED.skips) {
    assert.ok(name.startsWith(module), `${name} is outside ${module}`);
    const parsed = /^(.*?)\.(Test\w+)(\/.*)?$/.exec(name.slice(module.length));
    assert.ok(parsed, `${name} is not <package>.<Test>[/<subtest>]`);
    const [, pkg, top] = parsed;
    const dir = new URL(`../manvi/${pkg}/`, import.meta.url);
    assert.ok(existsSync(dir), `${name}: package directory ${pkg} does not exist`);
    const declared = readdirSync(dir)
      .filter((file) => file.endsWith("_test.go"))
      .some((file) => new RegExp(`^func ${top}\\(`, "m").test(readFileSync(new URL(file, dir), "utf8")));
    assert.ok(declared, `${name}: no func ${top} in ${pkg}; delete the stale entry`);
  }
});

// ---------------------------------------------------------------- fuzz floors
//
// A --fuzz target that cannot reach MANVI_FUZZMIN inputs is named on a line of
// its own with its count. A listed target is accepted only at or above its own
// floor; an unlisted one, or one that fell under its floor, still fails.
const STARVED_LINE = (target, n) => `    - fuzz target ${target} was sampled, not explored: ${n} of 1000000 inputs in 120s`;
const fuzzLog = (...lines) => ["PASS with gates that did not run:", ...lines, ""].join("\n");
const SLOWEST = EXPECTED.fuzz_floors[0];

test("fuzz floors: a listed target at or above its floor is expected", () => {
  const found = parseVerdict(fuzzLog(STARVED_LINE(SLOWEST.target, SLOWEST.floor)));
  assert.deepEqual(compare(found, EXPECTED, "ubuntu-latest", "fuzz", "checks").unexpectedGaps, []);
});

test("fuzz floors: a listed target under its floor fails, and says which floor", () => {
  const found = parseVerdict(fuzzLog(STARVED_LINE(SLOWEST.target, SLOWEST.floor - 1)));
  const [gap] = compare(found, EXPECTED, "ubuntu-latest", "fuzz", "checks").unexpectedGaps;
  assert.match(gap, new RegExp(`below its floor of ${SLOWEST.floor}$`));
});

test("fuzz floors: an unlisted starved target fails, and floors hold only in the fuzz job", () => {
  const unlisted = parseVerdict(fuzzLog(STARVED_LINE("./nowhere:FuzzNew", 5)));
  assert.equal(compare(unlisted, EXPECTED, "ubuntu-latest", "fuzz", "checks").unexpectedGaps.length, 1);
  const listed = parseVerdict(fuzzLog(STARVED_LINE(SLOWEST.target, SLOWEST.floor)));
  assert.equal(compare(listed, EXPECTED, "ubuntu-latest", "default", "checks").unexpectedGaps.length, 1);
  assert.equal(compare(listed, EXPECTED, "macos-latest", "fuzz", "checks").unexpectedGaps.length, 1);
});

test("fuzz floors: a listed target that now clears the input floor is reported, not failed", () => {
  const found = parseVerdict(fuzzLog());
  const result = compare(found, EXPECTED, "ubuntu-latest", "fuzz", "checks");
  assert.deepEqual(result.unexpectedGaps, []);
  assert.ok(result.stale.includes(`fuzz target ${SLOWEST.target} reached the input floor`));
});

test("fuzz floors: every entry is a declared target with a positive floor and a reason", () => {
  for (const f of EXPECTED.fuzz_floors) {
    assert.ok(Number.isInteger(f.floor) && f.floor > 0, JSON.stringify(f));
    assert.ok(f.why && f.why.length > 10, JSON.stringify(f));
    assert.deepEqual(f.modes, ["fuzz"], f.target);
    assert.deepEqual(f.legs, ["checks"], f.target);
    const [, pkg, fn] = /^\.\/(.+):(Fuzz\w+)$/.exec(f.target) ?? [];
    assert.ok(pkg, `${f.target} is not ./<package>:<FuzzTarget>`);
    const dir = new URL(`../manvi/${pkg}/`, import.meta.url);
    const declared = existsSync(dir) && readdirSync(dir).filter((file) => file.endsWith("_test.go"))
      .some((file) => new RegExp(`^func ${fn}\\(`, "m").test(readFileSync(new URL(file, dir), "utf8")));
    assert.ok(declared, `${f.target}: no such fuzz target; delete the stale floor`);
  }
});

// A gap whose message wraps onto deeper-indented lines used to end the parse, so
// every gap printed after it went unchecked.
test("a gap that runs over several lines does not hide the gaps after it", () => {
  const found = parseVerdict([
    "PASS with gates that did not run:",
    "    - a long message that wraps:",
    "                  and carries on here",
    "    - sqlite3 not on PATH — schema readability is unverified here",
    "",
  ].join("\n"));
  assert.deepEqual(found.gaps, ["a long message that wraps: and carries on here", "sqlite3 not on PATH — schema readability is unverified here"]);
});

// ---------------------------------------------------------------- legs
//
// CI runs one verify.sh leg per job. What is checked for a leg is that leg's
// gaps and skips, and a log that does not say it was that leg is refused.

const GUARD = fileURLToPath(new URL("./check-verify-gaps.mjs", import.meta.url));

test("every expected entry names known legs, and gaps and skips sit where verify.sh produces them", () => {
  for (const entry of [...EXPECTED.gaps, ...EXPECTED.skips]) {
    const name = entry.text ?? entry.test;
    assert.ok(Array.isArray(entry.legs) && entry.legs.length > 0, `${name} names no leg`);
    for (const leg of entry.legs) assert.ok(LEGS.includes(leg), `${name} names an unknown leg: ${leg}`);
  }
  // A skip is a Go test that skipped, which only the go-test leg runs; the
  // gaps come from the wire and navigation steps, which are in checks, except
  // the ones a bench suite names, which verify.sh prefixes with the suite.
  for (const s of EXPECTED.skips) assert.deepEqual(s.legs, ["go-test"], s.test);
  for (const g of EXPECTED.gaps) assert.deepEqual(g.legs, g.text.startsWith("bench/") ? ["bench"] : ["checks"], g.text);
});

test("a leg is compared only against its own entries", () => {
  const found = parseVerdict(UBUNTU);
  // The whole-script verdict holds both legs' gaps, so judged as one leg each
  // half of it is unexpected there.
  const asGoTest = compare(found, EXPECTED, "ubuntu-latest", "default", "go-test");
  assert.equal(asGoTest.unexpectedGaps.length, 4, "checks- and bench-leg gaps must not be excused in the go-test leg");
  assert.equal(asGoTest.unexpectedSkips.length, 0);
  const asChecks = compare(found, EXPECTED, "ubuntu-latest", "default", "checks");
  assert.deepEqual(asChecks.unexpectedGaps.map((g) => g.split(":")[0]), ["bench/stress_test.py"], "the bench leg's gap must not be excused in checks");
  const asBench = compare(found, EXPECTED, "ubuntu-latest", "default", "bench");
  assert.equal(asBench.unexpectedGaps.length, 3, "checks-leg gaps must not be excused in the bench leg");
  assert.equal(asChecks.unexpectedSkips.length, UBUNTU_SKIPS.length, "go-test-leg skips must not be excused in the checks leg");
});

test("an entry that names no leg excuses nothing in a leg run, and everything in a whole run", () => {
  const found = { gaps: ["g"], skips: [] };
  const expected = { gaps: [{ text: "g", os: ["ubuntu-latest"] }], skips: [] };
  assert.deepEqual(compare(found, expected, "ubuntu-latest", "default", "checks").unexpectedGaps, ["g"]);
  assert.deepEqual(compare(found, expected, "ubuntu-latest", "default", "all").unexpectedGaps, []);
  assert.deepEqual(compare(found, expected, "ubuntu-latest").unexpectedGaps, []);
});

test("the leg line is read, and anything that is not that line is not", () => {
  assert.deepEqual(parseLeg("noise\n\x1b[0mleg: go-test (9 step(s) ran)\nPASS\n"), { leg: "go-test", steps: 9 });
  assert.deepEqual(parseLeg("leg: all (39 step(s) ran)"), { leg: "all", steps: 39 });
  assert.equal(parseLeg("PASS all gates"), null);
  assert.equal(parseLeg("  leg: rust (2 step(s) ran)"), null, "only the line verify.sh prints, at the start of a line");
});

/** Runs the guard CLI on a log. */
function guard(log, ...args) {
  const file = path.join(mkdtempSync(path.join(tmpdir(), "leg-log-")), "verify.log");
  writeFileSync(file, log);
  const run = spawnSync(process.execPath, [GUARD, file, ...args], { encoding: "utf8" });
  return { status: run.status, out: `${run.stdout}${run.stderr}` };
}
const PASS = "\x1b[32mPASS\x1b[0m all gates\n";

test("the guard CLI accepts a leg log that names its leg, and refuses the three ways a leg log can be wrong", () => {
  const ok = guard(`leg: bench (2 step(s) ran)\n${PASS}`, "ubuntu-latest", "default", "bench");
  assert.equal(ok.status, 0, ok.out);

  const wrong = guard(`leg: rust (7 step(s) ran)\n${PASS}`, "ubuntu-latest", "default", "bench");
  assert.equal(wrong.status, 2);
  assert.match(wrong.out, /asked to run the bench leg and its log is the rust leg's/);

  const empty = guard(`leg: bench (0 step(s) ran)\n${PASS}`, "ubuntu-latest", "default", "bench");
  assert.equal(empty.status, 2);
  assert.match(empty.out, /ran no step/);

  const unsaid = guard(PASS, "ubuntu-latest", "default", "bench");
  assert.equal(unsaid.status, 2);
  assert.match(unsaid.out, /does not say which leg it ran/);
});

test("the guard CLI keeps judging a whole-script log with no leg, and refuses an unknown leg", () => {
  assert.equal(guard(PASS, "ubuntu-latest", "default").status, 0);
  assert.equal(guard(PASS, "ubuntu-latest", "default", "all").status, 0);
  const unknown = guard(PASS, "ubuntu-latest", "default", "nonsense");
  assert.equal(unknown.status, 2);
  assert.match(unknown.out, /usage:/);
});
