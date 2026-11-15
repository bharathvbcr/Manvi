#!/usr/bin/env node
/**
 * Fail a CI run whose verify.sh verdict names a gate that did not run, or a
 * test that skipped, which scripts/verify-expected-gaps.json does not list for
 * this runner.
 *
 * verify.sh degrades a gate it cannot run to NOT COVERED and still passes,
 * which is right on a laptop. On CI the only check was for a missing analysis
 * tool, so a gate that began skipping for any other reason — a new t.Skip, a
 * tool that stopped being found — rode along under a green check.
 *
 * CI runs one verify.sh leg per job, so a log covers only that leg's gates. An
 * entry in the expected-gaps file names the legs it belongs to, and a log is
 * compared only against those: a gap listed for one leg cannot excuse the same
 * text appearing in another. The log must also say which leg it was and that the
 * leg ran at least one step, so a job given the wrong leg, or one that ran
 * nothing, is refused rather than compared against an empty set of gaps.
 *
 *   usage: check-verify-gaps.mjs <verify.log> <runner os label> [default|race|fuzz] [leg|all]
 *   exit:  0 every gap is expected · 1 an unexpected gap · 2 could not check
 */
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const EXPECTED = path.join(path.dirname(fileURLToPath(import.meta.url)), "verify-expected-gaps.json");
export const MODES = ["default", "race", "fuzz"];
// The same list as ALL_LEGS in verify.sh and the matrix in verify.yml;
// scripts/verify-lib.test.mjs fails when any of the three differs.
export const LEGS = ["go-test", "rust", "checks", "bench"];
const SKIPPED = /^(\d+) of (\d+) test\(s\) skipped, so their assertions did not run: (.*)$/;

/**
 * The leg line verify.sh prints before its verdict: `leg: <name> (<n> step(s) ran)`.
 * @param {string} log
 * @returns {{ leg: string, steps: number } | null}
 */
export function parseLeg(log) {
  const match = /^leg: (\S+) \((\d+) step\(s\) ran\)$/m.exec(log.replace(/\x1b\[[0-9;]*m/g, ""));
  return match ? { leg: match[1], steps: Number(match[2]) } : null;
}

/**
 * The gaps a verdict reports: plain gate texts, and skipped test names.
 * @param {string} log
 * @returns {{ verdict: boolean, gaps: string[], skips: string[] }}
 */
export function parseVerdict(log) {
  const lines = log.replace(/\x1b\[[0-9;]*m/g, "").split("\n");
  const start = lines.findIndex((line) => line.includes("with gates that did not run:"));
  if (start === -1) {
    const passed = lines.some((line) => /^PASS\b/.test(line.trim()));
    return { verdict: passed, gaps: [], skips: [] };
  }
  /** @type {string[]} */
  const gaps = [];
  /** @type {string[]} */
  const skips = [];
  for (const line of lines.slice(start + 1)) {
    const match = /^\s{2,}- (.*)$/.exec(line);
    if (!match) break;
    const text = match[1].trim();
    const skipped = SKIPPED.exec(text);
    if (skipped) skips.push(...skipped[3].split(", ").map((name) => name.trim()).filter(Boolean));
    else gaps.push(text);
  }
  return { verdict: true, gaps, skips };
}

/**
 * @param {{ gaps: string[], skips: string[] }} found
 * @param {{ gaps: {text: string, os: string[], modes?: string[], legs?: string[]}[], skips: {test: string, os: string[], modes?: string[], legs?: string[]}[] }} expected
 * @param {string} os
 * @param {string} [mode] which verify.sh invocation produced the log; an entry with no `modes` holds in every one
 * @param {string} [leg] which leg produced it; "all" is a whole-script run, which every entry applies to. For one
 *   leg an entry applies only if it names it: an entry with no `legs` excuses nothing there, so a missing field
 *   cannot widen what a leg is allowed to skip.
 */
export function compare(found, expected, os, mode = "default", leg = "all") {
  const here = (/** @type {{os: string[], modes?: string[], legs?: string[]}} */ e) =>
    e.os.includes(os) && (e.modes ?? MODES).includes(mode) && (leg === "all" || (e.legs ?? []).includes(leg));
  const gapsHere = new Set(expected.gaps.filter(here).map((g) => g.text));
  const skipsHere = new Set(expected.skips.filter(here).map((s) => s.test));
  return {
    unexpectedGaps: found.gaps.filter((g) => !gapsHere.has(g)),
    unexpectedSkips: found.skips.filter((s) => !skipsHere.has(s)),
    stale: [
      ...[...gapsHere].filter((g) => !found.gaps.includes(g)),
      ...[...skipsHere].filter((s) => !found.skips.includes(s)),
    ],
  };
}

function main() {
  const [logPath, os, mode = "default", leg = "all"] = process.argv.slice(2);
  if (!logPath || !os || !MODES.includes(mode) || (leg !== "all" && !LEGS.includes(leg))) {
    console.error("usage: check-verify-gaps.mjs <verify.log> <runner os label> [default|race|fuzz] [leg|all]");
    process.exit(2);
  }
  const text = readFileSync(logPath, "utf8");
  const found = parseVerdict(text);
  if (!found.verdict) {
    console.error("verify.log has no verdict, so which gates ran is unknown.");
    process.exit(2);
  }
  if (leg !== "all") {
    const said = parseLeg(text);
    if (!said) {
      console.error(`verify.log does not say which leg it ran, so it cannot be checked as the ${leg} leg.`);
      process.exit(2);
    }
    if (said.leg !== leg) {
      console.error(`this job was asked to run the ${leg} leg and its log is the ${said.leg} leg's.`);
      process.exit(2);
    }
    if (said.steps === 0) {
      console.error(`the ${leg} leg ran no step, so its gates were not run.`);
      process.exit(2);
    }
  }
  // A whole-script run keeps the label it always had; a leg run adds the leg.
  const label = leg === "all" ? `${os} (${mode})` : `${os} (${mode}, ${leg})`;
  const result = compare(found, JSON.parse(readFileSync(EXPECTED, "utf8")), os, mode, leg);
  for (const entry of result.stale) console.log(`::notice title=expected gap did not occur on ${label}::${entry}`);
  const unexpected = [...result.unexpectedGaps, ...result.unexpectedSkips.map((s) => `skipped: ${s}`)];
  if (unexpected.length) {
    for (const entry of unexpected) console.log(`::error title=unexpected NOT COVERED on ${label}::${entry}`);
    console.error(`${unexpected.length} gate(s) did not run that ${path.basename(EXPECTED)} does not expect on ${label}.`);
    console.error("Make the gate run, or list it there with the reason it cannot.");
    process.exit(1);
  }
  console.log(`Every gap on ${label} is expected (${found.gaps.length} gate(s), ${found.skips.length} skip(s)).`);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) main();
