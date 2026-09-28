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
 *   usage: check-verify-gaps.mjs <verify.log> <runner os label>
 *   exit:  0 every gap is expected · 1 an unexpected gap · 2 could not check
 */
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const EXPECTED = path.join(path.dirname(fileURLToPath(import.meta.url)), "verify-expected-gaps.json");
const SKIPPED = /^(\d+) of (\d+) test\(s\) skipped, so their assertions did not run: (.*)$/;

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
 * @param {{ gaps: {text: string, os: string[]}[], skips: {test: string, os: string[]}[] }} expected
 * @param {string} os
 */
export function compare(found, expected, os) {
  const gapsHere = new Set(expected.gaps.filter((g) => g.os.includes(os)).map((g) => g.text));
  const skipsHere = new Set(expected.skips.filter((s) => s.os.includes(os)).map((s) => s.test));
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
  const [logPath, os] = process.argv.slice(2);
  if (!logPath || !os) {
    console.error("usage: check-verify-gaps.mjs <verify.log> <runner os label>");
    process.exit(2);
  }
  const found = parseVerdict(readFileSync(logPath, "utf8"));
  if (!found.verdict) {
    console.error("verify.log has no verdict, so which gates ran is unknown.");
    process.exit(2);
  }
  const result = compare(found, JSON.parse(readFileSync(EXPECTED, "utf8")), os);
  for (const entry of result.stale) console.log(`::notice title=expected gap did not occur on ${os}::${entry}`);
  const unexpected = [...result.unexpectedGaps, ...result.unexpectedSkips.map((s) => `skipped: ${s}`)];
  if (unexpected.length) {
    for (const entry of unexpected) console.log(`::error title=unexpected NOT COVERED on ${os}::${entry}`);
    console.error(`${unexpected.length} gate(s) did not run that ${path.basename(EXPECTED)} does not expect on ${os}.`);
    console.error("Make the gate run, or list it there with the reason it cannot.");
    process.exit(1);
  }
  console.log(`Every gap on ${os} is expected (${found.gaps.length} gate(s), ${found.skips.length} skip(s)).`);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) main();
