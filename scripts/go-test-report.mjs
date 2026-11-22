#!/usr/bin/env node
// Reads `go test -json` and answers the two questions a CI log cannot.
//
//   go-test-report.mjs text            stdin -> stdout, the ordinary test output
//   go-test-report.mjs report <file>   markdown: what never finished, what was slow
//
// Why this exists. `go test ./...` prints a package's output only when that
// package finishes, and in package order, so one wedged package leaves a log
// that simply stops: the Windows job ran 46 minutes against a 45 minute timeout
// and the log ended 45 minutes before the step did, naming nothing. -json
// streams a `run` event when a test starts and a terminal event when it ends,
// so the tests and packages that started and never ended are exactly the
// suspects, and no timeout dump is needed to find them.
//
// The report is a diagnostic, not a gate. A run that produced no events is said
// to have produced none, which is not the same sentence as "nothing hung".
import { createReadStream } from "node:fs";
import { createInterface } from "node:readline";
import { pathToFileURL } from "node:url";

const TERMINAL = new Set(["pass", "fail", "skip"]);
const TIMED_OUT = /^panic: test timed out after (\S+)/;

/** One `go test -json` line as an event, or null for anything else (build output, blank). */
export function parseLine(line) {
  try {
    const e = JSON.parse(line);
    return e !== null && typeof e === "object" && typeof e.Action === "string" ? e : null;
  } catch {
    return null;
  }
}

// `go test -json` carries the verbose stream, so every test would otherwise put
// a `=== RUN` and a `--- PASS` line in the log: thousands of lines that say
// nothing, burying the failures a reader opened the log for. Only progress
// markers and passes are dropped. A failure, a skip (this repository treats a
// skip as a finding), a package result and everything a test printed are kept,
// and the dropped lines are all still in the JSON the job uploads.
const PROGRESS = /^(?:=== (?:RUN|PAUSE|CONT|NAME)\b.*|\s*--- PASS: .*)\n?$/;

/** What a plain, non-verbose `go test` run would have printed for this line. */
export function renderText(line) {
  const e = parseLine(line);
  if (e === null) return `${line}\n`;
  if (typeof e.Output !== "string" || PROGRESS.test(e.Output)) return "";
  return e.Output;
}

/** Folds the event stream into per-package and per-test records. */
export function summarize(lines, top = 15) {
  const packages = new Map();
  const tests = new Map();
  const timedOut = new Map();
  let events = 0;
  let unparsed = 0;

  const record = (map, key, init) => {
    if (!map.has(key)) map.set(key, { ...init, done: false, status: "", elapsed: 0 });
    return map.get(key);
  };

  for (const line of lines) {
    if (line.trim() === "") continue;
    const e = parseLine(line);
    if (e === null) {
      unparsed++;
      continue;
    }
    events++;
    if (!e.Package) continue;
    // go test kills a package binary that outlives -timeout and prints this
    // line first. It is a finding of its own, not an assertion failure.
    if (e.Action === "output" && typeof e.Output === "string") {
      const m = TIMED_OUT.exec(e.Output);
      if (m) timedOut.set(e.Package, m[1]);
    }
    const isPackageEvent = e.Test === undefined || e.Test === "";
    const target = isPackageEvent
      ? record(packages, e.Package, { pkg: e.Package })
      : record(tests, `${e.Package}\0${e.Test}`, { pkg: e.Package, test: e.Test });
    if (TERMINAL.has(e.Action)) {
      target.done = true;
      target.status = e.Action;
      target.elapsed = typeof e.Elapsed === "number" ? e.Elapsed : 0;
    }
  }

  const all = (map) => [...map.values()];
  const slowest = (items) => items.filter((i) => i.done).sort((a, b) => b.elapsed - a.elapsed).slice(0, top);
  return {
    events,
    unparsed,
    packages: packages.size,
    tests: tests.size,
    failedTests: all(tests).filter((t) => t.status === "fail").length,
    timedOutPackages: [...timedOut].map(([pkg, after]) => ({ pkg, after })),
    unfinishedPackages: all(packages).filter((p) => !p.done),
    unfinishedTests: all(tests).filter((t) => !t.done),
    slowestPackages: slowest(all(packages)),
    slowestTests: slowest(all(tests)),
  };
}

const short = (pkg) => pkg.replace(/^github\.com\/[^/]+\/[^/]+\//, "");
const secs = (n) => `${n.toFixed(1)}s`;

/** The summary as markdown, for $GITHUB_STEP_SUMMARY. */
export function renderReport(s, title = "go test") {
  const out = [`### ${title}`, ""];
  if (s.events === 0) {
    out.push("The run produced no `go test -json` events, so nothing here says whether anything hung.");
    if (s.unparsed > 0) out.push("", `${s.unparsed} line(s) were not JSON events (a build failure prints here).`);
    return `${out.join("\n")}\n`;
  }
  out.push(`${s.packages} packages, ${s.tests} tests, ${s.failedTests} failed (${s.events} events, ${s.unparsed} non-event lines).`, "");

  if (s.timedOutPackages.length > 0) {
    out.push("**Timed out, not an assertion failure** — go test killed the package binary at its -timeout (a hang, or a host too loaded to finish):", "");
    for (const p of s.timedOutPackages) out.push(`- package \`${short(p.pkg)}\` after ${p.after}`);
    out.push("");
  }

  if (s.unfinishedPackages.length === 0 && s.unfinishedTests.length === 0) {
    out.push("Every package and every test that started also finished.", "");
  } else {
    out.push("**Started and never finished** — the suspects for a hang or a timeout:", "");
    for (const p of s.unfinishedPackages) out.push(`- package \`${short(p.pkg)}\``);
    for (const t of s.unfinishedTests) out.push(`- \`${short(t.pkg)}\` \`${t.test}\``);
    out.push("");
  }

  const table = (heading, rows, name) => {
    if (rows.length === 0) return;
    out.push(`**${heading}**`, "", "| seconds | |", "| ---: | --- |");
    for (const r of rows) out.push(`| ${secs(r.elapsed)} | ${name(r)} |`);
    out.push("");
  };
  table("Slowest packages", s.slowestPackages, (r) => `\`${short(r.pkg)}\` (${r.status})`);
  table("Slowest tests", s.slowestTests, (r) => `\`${short(r.pkg)}\` \`${r.test}\` (${r.status})`);
  return `${out.join("\n")}\n`;
}

async function* linesOf(stream) {
  for await (const line of createInterface({ input: stream, crlfDelay: Infinity })) yield line;
}

async function main(argv) {
  const [mode, file] = argv;
  // A reader that closes early (`| head`, a killed tee) is not a failure of
  // this script, and an unhandled EPIPE would print a stack trace into a log
  // whose whole job is to be read.
  process.stdout.on("error", (err) => {
    if (err.code === "EPIPE") process.exit(0);
    throw err;
  });
  if (mode === "text") {
    for await (const line of linesOf(process.stdin)) process.stdout.write(renderText(line));
    return 0;
  }
  if (mode === "report" && file) {
    const lines = [];
    for await (const line of linesOf(createReadStream(file))) lines.push(line);
    process.stdout.write(renderReport(summarize(lines), process.env.GO_TEST_REPORT_TITLE || "go test"));
    return 0;
  }
  process.stderr.write("usage: go-test-report.mjs text | report <go-test.json>\n");
  return 2;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exitCode = await main(process.argv.slice(2));
}
