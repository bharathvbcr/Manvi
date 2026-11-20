import { test } from "node:test";
import assert from "node:assert/strict";
import { chmodSync, existsSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { LEGS, compare, parseVerdict } from "./check-verify-gaps.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.join(HERE, "..");
const LIB = path.join(HERE, "verify-lib.sh");

/**
 * Runs a snippet with the gate helpers sourced and `stubs` first on PATH. A stub
 * is the body of an executable shell script: the point of these tests is what a
 * helper does when the real tool misbehaves in the ways it has.
 * @param {string} snippet
 * @param {Record<string, string>} stubs
 */
function gate(snippet, stubs = {}) {
  const bin = mkdtempSync(path.join(tmpdir(), "gate-bin-"));
  for (const [name, body] of Object.entries(stubs)) {
    const file = path.join(bin, name);
    writeFileSync(file, `#!/usr/bin/env bash\n${body}\n`);
    chmodSync(file, 0o755);
  }
  const work = mkdtempSync(path.join(tmpdir(), "gate-work-"));
  const run = spawnSync("bash", ["-c", `set -euo pipefail\ncd "$1"\nsource "$2"\n${snippet}`, "gate", work, LIB], {
    encoding: "utf8",
    env: { ...process.env, PATH: `${bin}:${process.env.PATH}` },
  });
  return { status: run.status, out: run.stdout, err: run.stderr, work };
}

/** `status=$?` after a helper, so a nonzero return is data rather than an abort. */
const capture = (call) => `set +e\nout="$(${call})"\nstatus=$?\nset -e\nprintf 'status=%s\\n' "$status"\nprintf '%s\\n' "$out"`;

// ---------------------------------------------------------------- go test counts

const GO_WITH_ONE_FAILURE = [
  ...Array.from({ length: 70 }, (_, i) => `echo '    --- PASS: Case/c${i} (0.00s)'`),
  `echo '    --- FAIL: Case/broken (0.00s)'`,
  `echo FAIL`,
  `exit 1`,
].join("\n");

test("a go test that exited 1 is not a count, however many cases passed first", () => {
  const r = gate(capture(`gate_count_passes . '^    --- PASS' ./gate/`), { go: GO_WITH_ONE_FAILURE });
  assert.match(r.out, /^status=2$/m, r.out + r.err);
  assert.match(r.err, /--- FAIL: Case\/broken/);
});

test("a passing go test is counted from its output", () => {
  const body = Array.from({ length: 12 }, (_, i) => `echo '    --- PASS: Case/c${i} (0.00s)'`).join("\n");
  const r = gate(capture(`gate_count_passes . '^    --- PASS' ./gate/`), { go: body });
  assert.match(r.out, /^status=0$/m, r.out + r.err);
  assert.match(r.out, /^12$/m);
});

test("a passing go test that printed no matching line counts zero rather than failing the shell", () => {
  const r = gate(capture(`gate_count_passes . '^--- PASS' ./x/`), { go: "echo ok" });
  assert.match(r.out, /^status=0$/m, r.out + r.err);
  assert.match(r.out, /^0$/m);
});

test("the suite run reports its failing packages and returns 2", () => {
  const events = [
    `{"Action":"output","Package":"p/bad","Output":"--- FAIL: TestX (0.00s)\\n"}`,
    `{"Action":"fail","Package":"p/bad"}`,
    `{"Action":"output","Package":"p/good","Output":"ok  p/good\\n"}`,
    `{"Action":"pass","Package":"p/good"}`,
  ];
  const r = gate(capture(`gate_go_test_json . "$PWD/events.json" ./...`), {
    go: `${events.map((e) => `echo '${e}'`).join("\n")}\nexit 1`,
  });
  assert.match(r.out, /^status=2$/m, r.out + r.err);
  assert.match(r.err, /--- FAIL: TestX/);
  assert.doesNotMatch(r.err, /ok {2}p\/good/);
});

test("a failed test's own output survives a later test that floods the package output", () => {
  const events = [
    { Action: "output", Package: "p/bad", Test: "TestX", Output: "    x_test.go:9: the real reason\n" },
    { Action: "fail", Package: "p/bad", Test: "TestX" },
    ...Array.from({ length: 500 }, (_, i) => ({ Action: "output", Package: "p/bad", Test: "TestLater", Output: `noise ${i}\n` })),
    { Action: "pass", Package: "p/bad", Test: "TestLater" },
    { Action: "fail", Package: "p/bad" },
  ].map((e) => JSON.stringify(e));
  const r = gate(capture(`gate_go_test_json . "$PWD/events.json" ./...`), {
    go: `${events.map((e) => `echo '${e}'`).join("\n")}\nexit 1`,
  });
  assert.match(r.out, /^status=2$/m, r.out + r.err);
  assert.match(r.err, /the real reason/);
  assert.match(r.err, /failed tests:\n {2}p\/bad\.TestX/);
  assert.doesNotMatch(r.err, /noise 499/);
});

test("failed-test output is bounded to 60 lines per test and 20 tests", () => {
  const events = [];
  for (let t = 0; t < 25; t++) {
    for (let i = 0; i < 100; i++) events.push({ Action: "output", Package: "p/bad", Test: `T${t}`, Output: `T${t} line ${i}\n` });
    events.push({ Action: "fail", Package: "p/bad", Test: `T${t}` });
  }
  events.push({ Action: "fail", Package: "p/bad" });
  const lines = events.map((e) => JSON.stringify(e));
  const r = gate(capture(`gate_go_test_json . "$PWD/events.json" ./...`), {
    go: `${lines.map((e) => `echo '${e}'`).join("\n")}\nexit 1`,
  });
  assert.match(r.err, /T0 line 99/);
  assert.doesNotMatch(r.err, /T0 line 39\b/);
  assert.match(r.err, /T0 line 40\b/);
  assert.match(r.err, /5 more failed tests not shown/);
  assert.doesNotMatch(r.err, /T20 line 99/);
});

test("the event report counts top-level passes per package, skips and observed results", () => {
  const ev = [
    ["pass", "m/manvi/devcouncil", "TestA"],
    ["pass", "m/manvi/devcouncil", "TestA/sub"],
    ["pass", "m/manvi/devcouncil", "TestB"],
    ["skip", "m/manvi/serve", "TestS"],
    ["pass", "m/manvi/serve", "TestT"],
    ["fail", "m/manvi/other", "TestF"],
  ].map(([Action, Package, Test]) => JSON.stringify({ Action, Package, Test }));
  const r = gate(
    `printf '%s\\n' '${ev.join("\n")}' 'not json' > events.json\ngate_go_json_report events.json /manvi/devcouncil /manvi/serve`,
  );
  assert.equal(r.status, 0, r.err);
  assert.deepEqual(r.out.trim().split("\n"), [
    "observed 6",
    "ran /manvi/devcouncil 2",
    "ran /manvi/serve 1",
    "skip m/manvi/serve.TestS",
  ]);
});

// ---------------------------------------------------------------- dependency surface

test("a failing go list is not an empty, passing dependency surface", () => {
  const r = gate(capture(`gate_deps_list . "$PWD/deps.txt"`), { go: `echo 'go: cannot find main module' >&2\nexit 1` });
  assert.match(r.out, /^status=2$/m, r.out + r.err);
  assert.match(r.err, /cannot find main module/);
});

test("a go list that lists nothing is not a passing dependency surface", () => {
  const r = gate(capture(`gate_deps_list . "$PWD/deps.txt"`), { go: "exit 0" });
  assert.match(r.out, /^status=2$/m, r.out + r.err);
});

test("the allowlist names what is outside it and nothing else", () => {
  const list = [
    "fmt",
    "github.com/bharathvbcr/Manvi/manvi/agent",
    "github.com/awnumar/memguard",
    "github.com/bharathvbcr/gusset",
    "github.com/bharathvbcr/gusset/internal/ffi",
    "github.com/evil/pkg",
    "crypto/internal/boring",
  ].join("\n");
  const base = String.raw`github\.com/awnumar/(memguard|memcall)`;
  const r = gate(
    `printf '%s\\n' '${list}' > deps.txt\n` +
      `echo cgo-off; gate_deps_unexpected deps.txt '^(${base})(/|$)'\n` +
      `echo cgo; gate_deps_unexpected deps.txt '^(${base}|github\\.com/bharathvbcr/gusset|github\\.com/evil)(/|$)'`,
  );
  assert.equal(r.status, 0, r.err);
  assert.deepEqual(r.out.trim().split("\n"), [
    "cgo-off",
    "github.com/bharathvbcr/gusset",
    "github.com/bharathvbcr/gusset/internal/ffi",
    "github.com/evil/pkg",
    "cgo",
  ]);
});

// ---------------------------------------------------------------- lint debt ratchet

const COUNTS = "gosec 3\nnoctx 1\n";
const reportStub = (json, exit) =>
  `for a in "$@"; do case "$a" in --output.json.path=*) out="\${a#*=}" ;; esac; done\ncat > "$out" <<'JSON'\n${json}\nJSON\nexit ${exit}`;

test("a typecheck-only report is not debt paid off, even when golangci-lint exits 1", () => {
  const json = JSON.stringify({
    Issues: [{ FromLinter: "typecheck", Text: "could not import cmp (export data version 5 is greater than maximum supported version 4)" }],
  });
  for (const exit of [1, 3]) {
    const r = gate(`printf 'gosec 3\\nnoctx 1\\n' > counts\n${capture(`gate_debt_ratchet . .golangci-debt.yml "$PWD/counts"`)}`, {
      "golangci-lint": reportStub(json, exit),
    });
    assert.match(r.out, /^status=2$/m, `exit ${exit}: ${r.out}${r.err}`);
    assert.doesNotMatch(r.out, /IMPROVED/);
    assert.match(r.err, /typecheck|failure to run/);
  }
});

test("a golangci-lint that failed to run is an error even with a plausible report", () => {
  const json = JSON.stringify({ Issues: [{ FromLinter: "gosec" }] });
  const r = gate(`printf '${COUNTS}' > counts\n${capture(`gate_debt_ratchet . .golangci-debt.yml "$PWD/counts"`)}`, {
    "golangci-lint": reportStub(json, 7),
  });
  assert.match(r.out, /^status=2$/m, r.out + r.err);
});

test("a Report.Error is an error", () => {
  const json = JSON.stringify({ Issues: [], Report: { Error: "context loading failed" } });
  const r = gate(`printf '${COUNTS}' > counts\n${capture(`gate_debt_ratchet . .golangci-debt.yml "$PWD/counts"`)}`, {
    "golangci-lint": reportStub(json, 0),
  });
  assert.match(r.out, /^status=2$/m, r.out + r.err);
});

test("regressions and improvements are reported against the recorded counts", () => {
  const issues = [...Array(5).fill({ FromLinter: "gosec" })];
  const r = gate(`printf '${COUNTS}' > counts\n${capture(`gate_debt_ratchet . .golangci-debt.yml "$PWD/counts"`)}`, {
    "golangci-lint": reportStub(JSON.stringify({ Issues: issues }), 1),
  });
  assert.match(r.out, /^status=0$/m, r.out + r.err);
  assert.match(r.out, /^REGRESSED gosec: 3 -> 5$/m);
  assert.match(r.out, /^IMPROVED noctx: 1 -> 0$/m);
});

// ---------------------------------------------------------------- nilaway

const FINDING =
  "/x/a/a.go:6:9: \x1b[31merror: \x1b[0mPotential nil panic detected. Observed nil flow from source to dereference point:\n\t- a/a.go:6:9: deep read from parameter `m`\n";
const nilaway = (text, exit) => `printf '%s' '${text.replace(/'/g, "'\\''")}'\nexit ${exit}`;

test("nilaway counts findings, tolerating colour codes and its shared-source summary lines", () => {
  const shared = "(Same nil source could also cause potential nil panic(s) at 1 other place(s): anthropic/adapter.go:671:11.)\n";
  const r = gate(capture(`gate_nilaway_count . example.com/n`), { nilaway: nilaway(FINDING + shared + "\n" + FINDING, 3) });
  assert.match(r.out, /^status=0$/m, r.out + r.err);
  assert.match(r.out, /^2$/m);
});

test("one package that failed to load beside real findings is an error, not a lower count", () => {
  const output =
    "/x/b/b.go:3:23: undefined: undefinedThing\n" +
    "nilaway_config: analysis skipped due to errors in package\n" +
    "nilaway_struct_field_analyzer: failed prerequisites: nilaway_config@example.com/n/b\n" +
    FINDING;
  const r = gate(capture(`gate_nilaway_count . example.com/n`), { nilaway: nilaway(output, 1) });
  assert.match(r.out, /^status=2$/m, r.out + r.err);
  assert.match(r.err, /analysis skipped|failed prerequisites|undefined/);
});

test("nilaway producing nothing, or exiting abnormally, is an error", () => {
  const empty = gate(capture(`gate_nilaway_count . example.com/n`), { nilaway: "exit 0" });
  assert.match(empty.out, /^status=2$/m, empty.out + empty.err);
  const crashed = gate(capture(`gate_nilaway_count . example.com/n`), { nilaway: nilaway(FINDING, 2) });
  assert.match(crashed.out, /^status=2$/m, crashed.out + crashed.err);
});

// ---------------------------------------------------------------- repo-level rules

/**
 * Statements in a shell file that count lines of a test or analysis command's
 * output with `grep -c` and then discard that command's exit status with
 * `|| true`. That is the shape that passed a failing differential.
 * @param {string} text
 */
export function maskedCounts(text) {
  const lines = text.split("\n");
  const found = [];
  lines.forEach((line, i) => {
    if (!/\|\s*grep -[a-zA-Z]*c/.test(line)) return;
    const window = lines.slice(Math.max(0, i - 3), i + 2).join("\n");
    if (/\bgo (test|list|vet)\b|\bcargo\b|nilaway|golangci-lint|govulncheck/.test(window) && /\|\|\s*true/.test(window)) {
      found.push(`${i + 1}: ${line.trim()}`);
    }
  });
  return found;
}

test("the masked-count scanner finds the shapes this gate once passed on", () => {
  const old = [
    `diff_ran="$( (cd "$gate_mod" && go test -count=1 -v ./gate/ \\`,
    `  -run 'TestX' 2>/dev/null) \\`,
    `  | grep -c '^    --- PASS' || true )"`,
  ].join("\n");
  assert.equal(maskedCounts(old).length, 1);
  assert.equal(maskedCounts(`ran="$( (cd manvi && go test -count=1 -v "$pkg" 2>/dev/null) | grep -c '^--- PASS' || true )"`).length, 1);
  assert.equal(maskedCounts(`direct="$( (cd manvi && go list -deps ./... 2>/dev/null) | grep -cE '^x' || true )"`).length, 1);
  // A count over a file the status of whose producer was already checked is fine.
  assert.equal(maskedCounts(`n="$(grep -c "$pattern" "$out")" || n=0`).length, 0);
});

test("verify.sh and its helpers carry no masked count over a test or analysis command", () => {
  for (const file of ["../verify.sh", "verify-lib.sh"]) {
    const found = maskedCounts(readFileSync(path.join(HERE, file), "utf8"));
    assert.deepEqual(found, [], `${file}: capture the command's status, then count`);
  }
});

// ---------------------------------------------------------------- legs
//
// verify.sh runs as one script locally and as one leg per job in CI. The split is
// only safe if no step can fall between legs, so these tests read verify.sh's
// region wrappers (`if leg_runs <name>; then … fi # end leg <name>`) and hold
// them to the list of legs the guard, the script and the workflow all name.

const VERIFY_LINES = readFileSync(path.join(ROOT, "verify.sh"), "utf8").split("\n");

/** Every leg region in verify.sh: its leg, and the steps inside it. Throws on a malformed region. */
function legRegions() {
  const regions = [];
  let open = null;
  VERIFY_LINES.forEach((line, i) => {
    const opens = /^if leg_runs ([a-z-]+); then$/.exec(line);
    const closes = /^fi # end leg ([a-z-]+)$/.exec(line);
    if (opens) {
      assert.equal(open, null, `verify.sh:${i + 1}: region ${opens[1]} opens inside ${open?.leg}`);
      open = { leg: opens[1], line: i + 1, steps: [] };
    } else if (closes) {
      assert.ok(open, `verify.sh:${i + 1}: \`fi # end leg ${closes[1]}\` closes nothing`);
      assert.equal(closes[1], open.leg, `verify.sh:${i + 1}: closes ${closes[1]} but ${open.leg} is open`);
      regions.push(open);
      open = null;
    } else if (/^\s*step "/.test(line)) {
      assert.ok(open, `verify.sh:${i + 1}: step outside every leg region: ${line.trim()}`);
      open.steps.push(line.trim());
    }
  });
  assert.equal(open, null, `verify.sh: region ${open?.leg} is never closed`);
  return regions;
}

/** verify.sh's own answer to which legs exist. */
function listLegs(shell = "bash") {
  const run = spawnSync(shell, [path.join(ROOT, "verify.sh"), "--list-legs"], { encoding: "utf8" });
  assert.equal(run.status, 0, run.stderr);
  return run.stdout.split("\n").filter(Boolean);
}

test("every step is inside exactly one leg region, and every leg has steps", () => {
  const regions = legRegions();
  const stepLines = VERIFY_LINES.filter((l) => /^\s*step "/.test(l)).length;
  assert.equal(regions.reduce((n, r) => n + r.steps.length, 0), stepLines, "a step is counted twice or not at all");
  for (const leg of listLegs()) {
    const steps = regions.filter((r) => r.leg === leg).flatMap((r) => r.steps);
    assert.ok(steps.length > 0, `leg ${leg} has no step: a job running it would run nothing`);
  }
  for (const r of regions) assert.ok(listLegs().includes(r.leg), `verify.sh:${r.line}: region names an unknown leg ${r.leg}`);
});

test("step titles are unique, so a leg's log can be told from another's by what ran", () => {
  const titles = VERIFY_LINES.filter((l) => /^\s*step "/.test(l)).map((l) => /step "([^"]*)"/.exec(l)[1]);
  assert.equal(new Set(titles).size, titles.length);
});

test("the legs are one list: verify.sh, the guard, and the workflow matrix", () => {
  assert.deepEqual(listLegs(), LEGS, "verify.sh --list-legs and LEGS in check-verify-gaps.mjs differ");
  const matrix = /^\s+leg: \[([^\]]+)\]$/m.exec(readFileSync(path.join(ROOT, ".github/workflows/verify.yml"), "utf8"));
  assert.ok(matrix, "the gate job's matrix names no legs");
  assert.deepEqual(matrix[1].split(",").map((s) => s.trim()), LEGS, "the gate matrix and verify.sh --list-legs differ");
});

test("verify.sh refuses an unknown leg and an option its leg does not run, and --list-legs starts nothing", () => {
  const run = (...args) => spawnSync("bash", [path.join(ROOT, "verify.sh"), ...args], { encoding: "utf8", timeout: 20000 });
  const unknown = run("--leg", "nonsense");
  assert.equal(unknown.status, 2);
  assert.match(unknown.stderr, /unknown leg: nonsense/);
  assert.doesNotMatch(unknown.stdout + unknown.stderr, /==>/, "an unknown leg must not start any step");
  for (const [args, why] of [
    [["--leg", "bench", "--race"], /--race is a step of the go-test leg/],
    [["--leg", "go-test", "--fuzz"], /--fuzz is a step of the checks leg/],
    [["--race", "--fuzz"], /usage:/],
    [["--leg"], /usage:/],
  ]) {
    const r = run(...args);
    assert.equal(r.status, 2, args.join(" "));
    assert.match(r.stderr, why, args.join(" "));
    assert.doesNotMatch(r.stdout + r.stderr, /==>/, `${args.join(" ")} must not start any step`);
  }
});

test("verify.sh parses its arguments under the bash macOS ships", { skip: !existsSync("/bin/bash") }, () => {
  // /bin/bash is 3.2 on macOS, where an empty array under `set -u` is an unbound
  // variable; the parsing is plain words and must keep working there.
  assert.deepEqual(listLegs("/bin/bash"), LEGS);
  const r = spawnSync("/bin/bash", [path.join(ROOT, "verify.sh"), "--leg", "nonsense"], { encoding: "utf8" });
  assert.equal(r.status, 2);
});

// ---------------------------------------------------------------- workflow

const WORKFLOW_FILE = readFileSync(path.join(ROOT, ".github/workflows/verify.yml"), "utf8");
// Comments explain the pins; only what a runner executes is checked.
const WORKFLOW = WORKFLOW_FILE.split("\n").filter((l) => !/^\s*#/.test(l)).join("\n");
const VERSIONS = Object.fromEntries(
  readFileSync(path.join(HERE, "tool-versions.env"), "utf8")
    .split("\n")
    .filter((l) => /^[A-Z_]+=/.test(l))
    .map((l) => l.split("=")),
);

test("the workflow installs no tool from a floating reference", () => {
  assert.doesNotMatch(WORKFLOW, /@latest\b/);
  assert.doesNotMatch(WORKFLOW, /toolchain install stable|default stable/);
  for (const tool of ["golangci-lint", "govulncheck", "nilaway"]) {
    for (const m of WORKFLOW.matchAll(new RegExp(`go install [^\\n]*${tool}@(\\S+)`, "g"))) {
      assert.match(m[1], /^"?\$[A-Z_]+_VERSION"?$/, `${tool} must install the pinned version: ${m[0]}`);
    }
  }
  for (const m of WORKFLOW.matchAll(/cargo install cargo-audit[^\n]*/g)) {
    assert.match(m[0], /--version "\$CARGO_AUDIT_VERSION"/, m[0]);
  }
});

test("the job that runs the bench leg installs ruff at the pinned version", () => {
  // verify.sh's Python lint is a step of the bench leg and reports "ruff is not
  // installed" without it; check-verify-log.sh rejects that as a provisioning
  // failure, so a bench leg without ruff cannot pass. No other leg runs ruff, so
  // the one install is gated to the leg that does, and the deep jobs, which run
  // the go-test and checks legs, install none.
  assert.equal([...WORKFLOW.matchAll(/pipx install "ruff==\$RUFF_VERSION"/g)].length, 1);
  assert.match(WORKFLOW, /name: Python lint tool\n\s+if: matrix\.leg == 'bench'/);
  assert.doesNotMatch(WORKFLOW, /ruff(==|@)(?!\$RUFF_VERSION)/, "ruff must be installed from the pin, not a literal or a float");
  assert.match(VERSIONS.RUFF_VERSION, /^\d+\.\d+\.\d+$/);
});

test("the pinned values are real versions, and node matches the workflow", () => {
  assert.match(VERSIONS.GOLANGCI_LINT_VERSION, /^v\d+\.\d+\.\d+$/);
  assert.match(VERSIONS.GOVULNCHECK_VERSION, /^v\d+\.\d+\.\d+$/);
  assert.match(VERSIONS.NILAWAY_VERSION, /^v\d+\.\d+\.\d+-\d{14}-[0-9a-f]{12}$|^v\d+\.\d+\.\d+$/);
  assert.match(VERSIONS.CARGO_AUDIT_VERSION, /^\d+\.\d+\.\d+$/);
  assert.match(VERSIONS.RUST_TOOLCHAIN, /^\d+\.\d+\.\d+$/);
  const nodes = [...WORKFLOW.matchAll(/node-version: (\S+)/g)].map((m) => m[1]);
  assert.ok(nodes.length >= 2);
  for (const n of nodes) assert.equal(n, VERSIONS.NODE_VERSION);
});

test("every job that runs verify.sh runs the log guards, and the long jobs are bounded", () => {
  const jobs = WORKFLOW.slice(WORKFLOW.indexOf("\njobs:\n")).split(/^  (?=[a-z]+:\n)/m).filter((j) => /^[a-z]+:\n/.test(j));
  const names = jobs.map((j) => j.split(":")[0]);
  assert.deepEqual(names.sort(), ["deep", "gate", "windows"]);
  for (const job of jobs) {
    const name = job.split(":")[0];
    if (/\.\/verify\.sh/.test(job)) {
      assert.match(job, /tee verify\.log/, `${name} keeps no log`);
      assert.match(job, /scripts\/check-verify-log\.sh verify\.log/, `${name} runs no guard`);
    }
    assert.match(job, /timeout-minutes: \d+/, `${name} has no timeout`);
  }
});

test("every job hands its leg to verify.sh and to the guard, and a deep job runs the leg that owns its mode", () => {
  // The gate runs one leg per job. If the matrix leg reached verify.sh but not the
  // guard, the guard would judge a leg's log against the whole script's entries.
  assert.match(WORKFLOW, /\.\/verify\.sh --leg \$\{\{ matrix\.leg \}\} 2>&1/);
  assert.match(WORKFLOW, /check-verify-log\.sh verify\.log \$\{\{ matrix\.os \}\} default \$\{\{ matrix\.leg \}\}/);
  assert.match(WORKFLOW, /\.\/verify\.sh --leg \$\{\{ matrix\.leg \}\} --\$\{\{ matrix\.mode \}\}/);
  assert.match(WORKFLOW, /check-verify-log\.sh verify\.log ubuntu-latest \$\{\{ matrix\.mode \}\} \$\{\{ matrix\.leg \}\}/);
  // verify.sh refuses --race outside go-test and --fuzz outside checks, so a
  // wrong pairing would fail at run time; this fails it here, in review.
  const owner = { race: "go-test", fuzz: "checks" };
  const deep = WORKFLOW.slice(WORKFLOW.indexOf("\n  deep:\n"));
  const pairs = [...deep.matchAll(/- mode: (\w+)\n\s+leg: ([a-z-]+)/g)].map((m) => [m[1], m[2]]);
  assert.deepEqual(pairs.map(([mode]) => mode).sort(), ["fuzz", "race"], "a deep mode lost its job");
  for (const [mode, leg] of pairs) assert.equal(leg, owner[mode], `--${mode} is a step of ${owner[mode]}, not ${leg}`);
});

test("the deep jobs are guarded with their own mode, and an unlisted gap there fails", () => {
  assert.match(WORKFLOW, /check-verify-log\.sh verify\.log ubuntu-latest \$\{\{ matrix\.mode \}\}/);
  const expected = JSON.parse(readFileSync(path.join(HERE, "verify-expected-gaps.json"), "utf8"));
  const fuzzLog = [
    "\x1b[33mPASS\x1b[0m with gates that did not run:",
    // verify.sh names each starved target on its own line; one that has no
    // floor in fuzz_floors is an unexpected gap in every mode.
    "    - fuzz target ./nowhere:FuzzUnlisted was sampled, not explored: 12 of 1000000 inputs in 120s",
    "",
  ].join("\n");
  for (const mode of ["race", "fuzz"]) {
    const result = compare(parseVerdict(fuzzLog), expected, "ubuntu-latest", mode);
    assert.equal(result.unexpectedGaps.length, 1, mode);
  }
});

test("an entry restricted to one mode holds only there", () => {
  const expected = {
    gaps: [{ text: "x", os: ["ubuntu-latest"], modes: ["fuzz"] }],
    skips: [],
  };
  const found = { gaps: ["x"], skips: [] };
  assert.deepEqual(compare(found, expected, "ubuntu-latest", "fuzz").unexpectedGaps, []);
  assert.deepEqual(compare(found, expected, "ubuntu-latest", "race").unexpectedGaps, ["x"]);
  assert.deepEqual(compare(found, expected, "ubuntu-latest", "default").unexpectedGaps, ["x"]);
  // No `modes` means all of them.
  const anywhere = { gaps: [{ text: "x", os: ["ubuntu-latest"] }], skips: [] };
  for (const mode of ["default", "race", "fuzz"]) assert.deepEqual(compare(found, anywhere, "ubuntu-latest", mode).unexpectedGaps, []);
});

test("the guard script fails a deep-mode log with an unlisted NOT COVERED line", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "gate-log-"));
  const log = path.join(dir, "verify.log");
  writeFileSync(log, "\x1b[33mPASS\x1b[0m with gates that did not run:\n    - bwrap not found — containment is unverified here\n\n");
  for (const mode of ["race", "fuzz"]) {
    const run = spawnSync(path.join(HERE, "check-verify-log.sh"), [log, "ubuntu-latest", mode], { encoding: "utf8" });
    assert.equal(run.status, 1, `${mode}: ${run.stdout}${run.stderr}`);
    assert.match(run.stdout, /unexpected NOT COVERED on ubuntu-latest \(\w+\)::bwrap not found/);
  }
  writeFileSync(log, "    - golangci-lint is not installed — the enforced lint set did not run\n");
  const missing = spawnSync(path.join(HERE, "check-verify-log.sh"), [log, "ubuntu-latest", "race"], { encoding: "utf8" });
  assert.equal(missing.status, 1);
});

// ---------------------------------------------------------------- release pins

const NOTCOVERED_DEF = `NOT_COVERED=""\nnotcovered() { NOT_COVERED="\${NOT_COVERED}\n    - $1"; }`;
const DRIFT = "DevCouncil is at 0123456789ab, pinned fedcba987654 (3 ahead, 0 behind), clean. The release builds the pin.";

test("a sibling off its pin is carried into NOT_COVERED, not left in mid-run stderr", () => {
  const r = gate(`${NOTCOVERED_DEF}\ngate_release_check\nprintf 'NC=[%s]\\n' "$NOT_COVERED"`, {
    node: `echo 'check-release: NOT COVERED: ${DRIFT}' >&2\nexit 0`,
  });
  assert.equal(r.status, 0, r.err);
  assert.match(r.out, /NC=\[\n {4}- DevCouncil is at 0123456789ab, pinned fedcba987654 \(3 ahead, 0 behind\), clean\./, r.out);
  // The line is still shown where it happened.
  assert.match(r.err, /check-release: NOT COVERED: DevCouncil is at/);
});

test("siblings at their pins leave NOT_COVERED empty", () => {
  const r = gate(`${NOTCOVERED_DEF}\ngate_release_check\nprintf 'NC=[%s]\\n' "$NOT_COVERED"`, {
    node: `echo 'check-release: ok' >&2\nexit 0`,
  });
  assert.equal(r.status, 0, r.err);
  assert.match(r.out, /^NC=\[\]$/m, r.out);
});

test("a check-release failure still fails, and its NOT COVERED lines are still recorded", () => {
  const r = gate(
    `${NOTCOVERED_DEF}\nset +e\ngate_release_check --require-pins\nst=$?\nset -e\nprintf 'st=%s NC=[%s]\\n' "$st" "$NOT_COVERED"`,
    { node: `echo 'check-release: NOT COVERED: ${DRIFT}' >&2\necho 'check-release: release notes missing' >&2\nexit 1` },
  );
  assert.match(r.out, /^st=1 NC=\[\n {4}- DevCouncil is at/m, r.out);
  assert.match(r.err, /release notes missing/);
});

test("arguments reach check-release unchanged", () => {
  const r = gate(`${NOTCOVERED_DEF}\ngate_release_check --require-pins`, { node: `printf 'args=%s\\n' "$*" >&2` });
  assert.match(r.err, /args=scripts\/check-release\.mjs --require-pins/);
});

// ---------------------------------------------------------------- store replies

const REFUSED = `{"ok":false,"code":"no_such_task","task_id":"VERIFY-1","error":"task VERIFY-1 is not a workbench item of this repository"}`;
const storeStub = (reply, exit = 0) => `printf '%s\\n' '${reply}'\nexit ${exit}`;

test("a store reply of ok:false is a refusal even when the binary exited 0", () => {
  const r = gate(capture(`gate_store_ok dcstore --db x acquire --task VERIFY-1`), { dcstore: storeStub(REFUSED) });
  assert.match(r.out, /^status=2$/m, r.out + r.err);
  assert.doesNotMatch(r.out, /no_such_task/, "a refused reply is not output the caller can mistake for a lease");
  assert.match(r.err, /is not a workbench item of this repository/);
});

test("a store reply that is not a JSON object with ok:true is a refusal", () => {
  for (const reply of ["not json at all", "[1,2]", `"ok"`, `{"ok":"true"}`, `{"lease":{}}`]) {
    const r = gate(capture(`gate_store_ok dcstore acquire`), { dcstore: storeStub(reply) });
    assert.match(r.out, /^status=2$/m, `${reply}: ${r.out}${r.err}`);
  }
  const empty = gate(capture(`gate_store_ok dcstore acquire`), { dcstore: "exit 0" });
  assert.match(empty.out, /^status=2$/m, empty.out + empty.err);
});

test("a store that exited non-zero is a refusal even if its reply says ok", () => {
  const r = gate(capture(`gate_store_ok dcstore acquire`), { dcstore: storeStub(`{"ok":true}`, 3) });
  assert.match(r.out, /^status=[1-9]/m, r.out + r.err);
  assert.doesNotMatch(r.out, /^status=0$/m);
});

test("an ok:true store reply is passed through unchanged", () => {
  const reply = `{"ok":true,"lease":{"token":"abc"}}`;
  const r = gate(capture(`gate_store_ok dcstore acquire`), { dcstore: storeStub(reply) });
  assert.match(r.out, /^status=0$/m, r.out + r.err);
  assert.match(r.out, /^\{"ok":true,"lease":\{"token":"abc"\}\}$/m);
});

test("a seeded task is the local repository, then a ready workbench item of it", () => {
  const r = gate(`gate_seed_task dcstore "$PWD/s.sqlite" VERIFY-1 >/dev/null\ncat calls.log`, {
    dcstore: `printf '%s\\n' "$*" >> calls.log\nprintf '%s\\n' '{"ok":true}'`,
  });
  assert.equal(r.status, 0, r.err);
  const [first, second, ...rest] = r.out.trim().split("\n");
  assert.deepEqual(rest, []);
  assert.match(first, /^--db \S+\/s\.sqlite local-repository$/);
  assert.match(second, /^--db \S+\/s\.sqlite work --method items\.put --input /);
  const input = JSON.parse(second.slice(second.indexOf("--input ") + "--input ".length));
  assert.deepEqual(input, {
    request_id: "seed-VERIFY-1",
    id: "VERIFY-1",
    expected_revision: 0,
    title: "VERIFY-1",
    status: "ready",
    repository_ids: ["this-repo"],
    primary_repository_id: "this-repo",
    scope: {},
  });
});

test("seeding a task the store refuses is an error", () => {
  const r = gate(capture(`gate_seed_task dcstore "$PWD/s.sqlite" VERIFY-1`), {
    dcstore: `if [ "$3" = work ]; then printf '%s\\n' '${REFUSED}'; else printf '%s\\n' '{"ok":true}'; fi`,
  });
  assert.match(r.out, /^status=2$/m, r.out + r.err);
});

// ---------------------------------------------------------------- on_exit

/**
 * Runs verify.sh's real on_exit and its trap line in a scratch script, with the
 * variables the trap reads and `body` as the script's remaining work. The trap
 * is cut out of verify.sh rather than copied here, so a change to it is what
 * this runs.
 * @param {string} body
 * @param {string} shell
 */
function runOnExit(body, shell) {
  const lines = readFileSync(path.join(ROOT, "verify.sh"), "utf8").split("\n");
  const start = lines.findIndex((l) => l.startsWith("on_exit() {"));
  assert.ok(start >= 0, "verify.sh no longer defines on_exit()");
  const end = lines.findIndex((l, i) => i > start && l === "}");
  const trap = lines.findIndex((l, i) => i > end && l === "trap on_exit EXIT");
  assert.ok(end > start && trap > end, "on_exit() or its trap line moved; update this test");
  const script = [
    "set -euo pipefail",
    "VERDICT_REACHED=0",
    "STEP_NAME=''",
    "step_mark() { :; }",
    ...lines.slice(start, trap + 1),
    body,
  ].join("\n");
  const dir = mkdtempSync(path.join(tmpdir(), "on-exit-"));
  const file = path.join(dir, "fixture.sh");
  writeFileSync(file, script);
  const run = spawnSync(shell, [file], { encoding: "utf8" });
  return { status: run.status, err: run.stderr };
}

for (const shell of ["/bin/bash", "bash"]) {
  test(`on_exit never exits 0 before a verdict: an unbound variable in arithmetic (${shell})`, () => {
    const run = runOnExit('echo before\nx=$(( not_a_variable + 1 ))\necho after', shell);
    assert.notEqual(run.status, 0, `the script aborted and exited 0:\n${run.err}`);
    assert.match(run.err, /INCOMPLETE/);
  });

  test(`on_exit turns a plain exit 0 before the verdict into a failure (${shell})`, () => {
    assert.equal(runOnExit("exit 0", shell).status, 1);
  });

  test(`on_exit keeps a non-zero status it was handed (${shell})`, () => {
    assert.equal(runOnExit("exit 7", shell).status, 7);
  });

  test(`on_exit leaves a reached verdict's success alone (${shell})`, () => {
    assert.equal(runOnExit("VERDICT_REACHED=1\nexit 0", shell).status, 0);
  });
}
