import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { compare, parseVerdict } from "./check-verify-gaps.mjs";

const EXPECTED = JSON.parse(readFileSync(new URL("./verify-expected-gaps.json", import.meta.url), "utf8"));

// The verdict verify.sh printed on ubuntu-latest at d6c956f, colour codes and all.
const UBUNTU = [
  "    covered: 874 checks",
  "",
  "\x1b[33mPASS\x1b[0m with gates that did not run:",
  "    - 8 of 3005 test(s) skipped, so their assertions did not run: " +
    EXPECTED.skips.filter((s) => s.os.includes("ubuntu-latest")).map((s) => s.test).join(", "),
  "    - devmap not found — repo navigation is unverified here",
  "    - local — this gate makes no request. The harness reports:",
  "    - anthropic, gemini and xai are verified against scripted servers only.",
  "",
].join("\n");

test("the ubuntu verdict CI produced is fully expected", () => {
  const found = parseVerdict(UBUNTU);
  assert.equal(found.skips.length, 8);
  assert.equal(found.gaps.length, 3);
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
  // spelling tests, so five skips rather than eight.
  const skips = [
    "github.com/bharathvbcr/Manvi/manvi/codingagent.TestInstalledCodexHandshakeWithoutModelTurn",
    "github.com/bharathvbcr/Manvi/manvi/codingagent.TestInstalledCodexReadOnlyTurn",
    "github.com/bharathvbcr/Manvi/manvi/devcouncil.TestEvidenceRustCompatibility",
    "github.com/bharathvbcr/Manvi/manvi/ui/tui.TestEveryUntrustedEventFieldIsCleanedOnTheWayIntoTheFrame/agent",
    "github.com/bharathvbcr/Manvi/manvi/ui/tui.TestEveryUntrustedEventFieldIsCleanedOnTheWayIntoTheFrame/path",
  ];
  const log = [
    "PASS with gates that did not run:",
    `    - 5 of 3005 test(s) skipped, so their assertions did not run: ${skips.join(", ")}`,
    "    - devmap not found — repo navigation is unverified here",
    "    - local — this gate makes no request. The harness reports:",
    "    - anthropic, gemini and xai are verified against scripted servers only.",
  ].join("\n");
  assert.deepEqual(compare(parseVerdict(log), EXPECTED, "macos-latest"), { unexpectedGaps: [], unexpectedSkips: [], stale: [] });
});
