// The invariant every gate in verify.sh owes: a gate that could not run is never
// reported the way a gate that ran and passed is. For each gate this file makes
// the tool unable to run (missing from PATH, exiting 127, a `go list` that
// fails, a package that does not load, a run that produced nothing) and checks
// that verify.sh either FAILS or names the gate under "did not run".
//
// It runs the real step bodies, not a copy of them: each segment is cut out of
// verify.sh between two `step` markers and executed in a scratch tree against
// stub tools on a restricted PATH. A segment boundary that moved is a failure
// here, so the test cannot silently stop covering a step.
import { test } from "node:test";
import assert from "node:assert/strict";
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const VERIFY = readFileSync(path.join(HERE, "..", "verify.sh"), "utf8").split("\n");
const SCRIPTS = HERE;

// The leg region wrappers verify.sh puts around groups of steps. They select
// which leg a step belongs to and carry no behaviour of their own, and a segment
// that spans a region boundary would otherwise contain half a wrapper — an `fi`
// for an `if` it never saw. Only these exact lines are dropped, so the step
// bodies this test exists to run are executed as written; scripts/verify-lib.test.mjs
// is what checks the wrappers themselves.
const REGION_WRAPPER = /^(if leg_runs [a-z-]+; then|fi # end leg [a-z-]+)$/;

/** Lines from the `step` titled `from` up to (not including) the one titled `to`. */
function segment(from, to) {
  const find = (title) => {
    const i = VERIFY.findIndex((l) => l.startsWith(`step "${title}`));
    assert.ok(i >= 0, `verify.sh no longer has a step titled "${title}"; update this test's segments`);
    return i;
  };
  return VERIFY.slice(find(from), find(to)).filter((l) => !REGION_WRAPPER.test(l)).join("\n");
}
const PRELUDE_END = VERIFY.findIndex((l) => l.startsWith("in_manvi()"));
assert.ok(PRELUDE_END > 0, "verify.sh no longer defines in_manvi()");
const PRELUDE = VERIFY.slice(0, PRELUDE_END + 1).join("\n");
const VERDICT = VERIFY.slice(VERIFY.findIndex((l) => l.startsWith("VERDICT_REACHED=1"))).join("\n");

const SEGMENTS = {
  test: segment("Go — test", "Go — module files are tidy"),
  surface: segment("Go — module files are tidy", "Rust — format"),
  rust: segment("Rust — format", "Rust — DevCouncil host"),
  commandgate: segment("Command gate — verdicts match what the shell actually writes", "Docs — mermaid blocks parse with the real grammar"),
  bunaudit: segment("Docs tooling — supply chain", "Brand — the published mark is the drawn mark"),
  consumers: segment("Consumers — Jarvis builds and vets against this checkout", "Provider wire contracts"),
  bench: segment("Bench — instrument, statistics and cell assembly", "Bench — Python lint"),
};

// ---------------------------------------------------------------- stubs

const STUBS = {
  // Behaviour is chosen per scenario by environment variables, so one stub
  // serves the healthy control and every way of failing.
  go: String.raw`
case "$1" in
  list)
    [ "\${GO_LIST:-ok}" = 127 ] && exit 127
    [ "\${GO_LIST:-ok}" = fail ] && { echo "go: updates to go.mod needed" >&2; exit 1; }
    [ "\${GO_LIST:-ok}" = empty ] && exit 0
    printf '%s\n' fmt github.com/bharathvbcr/Manvi/manvi/llm github.com/awnumar/memguard golang.org/x/sys/unix github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/gate
    [ "\${CGO_ENABLED:-0}" = 1 ] && [ "\${GO_LIST_NO_GUSSET:-}" != 1 ] && echo github.com/bharathvbcr/gusset
    exit 0 ;;
  mod)
    [ "\${GO_TIDY:-ok}" = 127 ] && exit 127
    [ "\${GO_TIDY:-ok}" = diff ] && { echo "-	golang.org/x/sys v0.1.0"; exit 1; }
    exit 0 ;;
  tool) echo "total:	(statements)	91.0%"; exit \${GO_COVER_STATUS:-0} ;;
  build|vet) [ "\${GO_BUILD:-ok}" = fail ] && { echo "undefined: workflow.Removed" >&2; exit 1; }; exit 0 ;;
  test)
    all="$*"
    if [[ "$all" == *-json* ]]; then
      [ "\${GO_TEST:-ok}" = 127 ] && exit 127
      [ "\${GO_TEST:-ok}" = fail ] && { echo '{"Action":"fail","Package":"x/manvi/llm","Test":"TestA"}'; echo '{"Action":"fail","Package":"x/manvi/llm"}'; exit 1; }
      [ "\${GO_TEST:-ok}" = silent ] && exit 0
      for a in "$@"; do case "$a" in -coverprofile=*) printf 'mode: set\nx.go:1.1,2.2 1 1\n' > "\${a#-coverprofile=}" ;; esac; done
      for p in devcouncil serve; do for i in 1 2 3 4 5 6; do echo '{"Action":"pass","Package":"x/manvi/'$p'","Test":"T'$i'"}'; done; done
      exit 0
    fi
    case "$all" in
      *TestGeneratedCommands*)
        [ "\${GO_GATE:-ok}" = 127 ] && exit 127
        [ "\${GO_GATE:-ok}" = fail ] && { echo "--- FAIL: TestGenerated"; exit 1; }
        echo "--- PASS: TestGeneratedCommandsNeverOutrunTheirOwnWriteVerdict"; echo "400 generated command lines: 0 refused"; exit 0 ;;
      *TestCommandVerdict*)
        [ "\${GO_GATE:-ok}" = 127 ] && exit 127
        for i in $(seq 1 70); do echo "    --- PASS: Case/c$i"; done
        [ "\${GO_GATE:-ok}" = fail ] && { echo "    --- FAIL: Case/broken"; exit 1; }
        [ "\${GO_GATE:-ok}" = few ] && { :; }
        exit 0 ;;
      *TestParityCounts*)
        [ "\${GO_DOCS:-ok}" = 127 ] && exit 127
        n=8; [ "\${GO_DOCS:-ok}" = few ] && n=3
        for i in $(seq 1 $n); do echo "--- PASS: TestDoc$i"; done
        [ "\${GO_DOCS:-ok}" = fail ] && { echo "--- FAIL: TestDocX"; exit 1; }
        exit 0 ;;
    esac
    exit 0 ;;
esac
exit 0
`,
  golangci: String.raw`
debt=0; for a in "$@"; do [[ "$a" == *golangci-debt.yml ]] && debt=1; done
mode="\${LINT_ENFORCED:-ok}"; [ $debt = 1 ] && mode="\${LINT_DEBT:-ok}"
out=""; for a in "$@"; do case "$a" in --output.json.path=*) out="\${a#--output.json.path=}" ;; esac; done
case "$mode" in
  127) exit 127 ;;
  1) echo "findings" >&2; exit 1 ;;
  typecheck)
    [ -n "$out" ] && echo '{"Issues":[{"FromLinter":"typecheck","Text":"could not import cmp (export data version 5)"}],"Report":{}}' > "$out"
    exit 3 ;;
  typecheck-exit1)
    [ -n "$out" ] && echo '{"Issues":[{"FromLinter":"typecheck","Text":"could not import cmp"}],"Report":{}}' > "$out"
    exit 1 ;;
  empty) exit 0 ;;
  regress)
    [ -n "$out" ] && python3 -c 'import json,sys; print(json.dumps({"Issues":[{"FromLinter":"gosec"}]*9999,"Report":{}}))' > "$out"
    exit 1 ;;
  ok) [ -n "$out" ] && echo '{"Issues":[],"Report":{}}' > "$out"; exit 0 ;;
esac
`,
  govulncheck: String.raw`
[ "\${VULN:-ok}" = 127 ] && exit 127
[ "\${VULN:-ok}" = fail ] && { echo "Vulnerability #1" >&2; exit 3; }
exit 0
`,
  nilaway: String.raw`
case "\${NILAWAY:-ok}" in
  127) exit 127 ;;
  empty) exit 0 ;;
  crash) echo "fatal error" >&2; exit 2 ;;
  partial)
    printf 'a.go:3:4: \033[31merror:\033[0m Potential nil panic detected. Observed nil flow\n\tfrom x\n'
    echo 'b/b.go:1:1: analysis skipped due to errors in package' ; exit 3 ;;
  over)
    for i in $(seq 1 99); do printf 'a.go:%s:4: \033[31merror:\033[0m Potential nil panic detected. Observed nil flow\n\tfrom x\n' $i; done; exit 3 ;;
  ok) for i in 1 2 3; do printf 'a.go:%s:4: \033[31merror:\033[0m Potential nil panic detected. Observed nil flow\n\tfrom x\n' $i; done; exit 3 ;;
esac
`,
  cargo: String.raw`
[ "\${CARGO:-ok}" = 127 ] && exit 127
case "$1" in
  fmt) [ "\${CARGO:-ok}" = fmt ] && { echo "Diff in platform.rs"; exit 1; }; exit 0 ;;
  clippy) [ "\${CARGO:-ok}" = clippy ] && exit 101; exit 0 ;;
  test)
    n=40; [ "\${CARGO:-ok}" = zero ] && n=0
    echo "test result: ok. $n passed; 0 failed; 0 ignored"; [ "\${CARGO:-ok}" = testfail ] && exit 101; exit 0 ;;
  audit)
    [ "\${AUDIT:-ok}" = fail ] && exit 1
    [ "\${AUDIT:-ok}" = native ] && [ "\${PWD##*/}" = native ] && exit 1
    [ "\${AUDIT:-ok}" = unsound ] && [[ " $* " == *" --deny unsound "* ]] && { echo "Warning: unsound" >&2; exit 1; }
    exit 0 ;;
esac
exit 0
`,
  "cargo-audit": String.raw`exit 0`,
  bun: String.raw`[ "\${BUN:-ok}" = fail ] && exit 1; exit 0`,
  // Every bench suite: a summary line with its count, and, when asked, the line
  // a suite prints for a claim it could not check on this host.
  python3: String.raw`
[ "\${BENCH_GAP:-}" = 1 ] && [ "$1" = stress_test.py ] && echo "NOT COVERED: C6 fork denial — no filter here"
echo "100 passed, 0 failed"
exit 0
`,
};

const ENGINE_STUB = "#!/usr/bin/env bash\necho 'export CGO_ENABLED=1'\n";

/**
 * Runs `segments` of the real verify.sh, then its verdict block, in a scratch
 * tree, with `tools` (names from STUBS) on a PATH that otherwise holds only
 * system utilities. Returns the exit status and combined output.
 */
function runGate(segments, tools, env = {}) {
  const base = mkdtempSync(path.join(tmpdir(), "gate-e2e-"));
  const root = path.join(base, "Manvi");
  mkdirSync(path.join(root, "manvi"), { recursive: true });
  mkdirSync(path.join(root, "crates"), { recursive: true });
  mkdirSync(path.join(root, "native"), { recursive: true });
  mkdirSync(path.join(root, "bench", "paper"), { recursive: true });
  mkdirSync(path.join(base, "DevCouncil", "backend", "go_orchestrator", "gate"), { recursive: true });
  symlinkSync(SCRIPTS, path.join(root, "scripts"));
  writeFileSync(path.join(root, "manvi", ".golangci-debt.counts"), "gosec 255\nnoctx 21\n");
  mkdirSync(path.join(base, "DevCouncil", "rust", "gusset-engine"), { recursive: true });
  const engine = path.join(base, "DevCouncil", "rust", "gusset-engine", "cgo-env.sh");
  writeFileSync(engine, ENGINE_STUB);
  chmodSync(engine, 0o755);

  const bin = path.join(base, "bin");
  mkdirSync(bin);
  const names = { go: "go", golangci: "golangci-lint", govulncheck: "govulncheck", nilaway: "nilaway", cargo: "cargo", "cargo-audit": "cargo-audit", bun: "bun", python3: "python3" };
  for (const tool of tools) {
    const file = path.join(bin, names[tool]);
    writeFileSync(file, `#!/usr/bin/env bash\n${STUBS[tool].replaceAll("\\${", "${")}\n`);
    chmodSync(file, 0o755);
  }
  const script = [PRELUDE, ...segments, VERDICT].join("\n");
  writeFileSync(path.join(root, "verify.sh"), script);
  const run = spawnSync("bash", [path.join(root, "verify.sh")], {
    encoding: "utf8",
    cwd: root,
    env: { PATH: `${bin}:/usr/bin:/bin`, HOME: base, TMPDIR: base, ...env },
    timeout: 60000,
  });
  const out = `${run.stdout}${run.stderr}`.replace(/\x1b\[[0-9;]*m/g, "");
  return { status: run.status, out };
}

/** A gate that could not run is a failure, or it is named under "did not run". */
function assertNotAPass(run, why, { named } = {}) {
  // GATE_E2E_SHOW=1 prints the reason each injected fault was refused, so a
  // failure that is the harness's own fault cannot pass for the gate's.
  if (process.env.GATE_E2E_SHOW === "1") {
    const lines = run.out.split("\n").filter((l) => /FAIL|NOT COVERED|INCOMPLETE|could not|exited|did not/.test(l));
    console.log(`### ${why}\n${lines.slice(0, 4).join("\n")}`);
  }
  assert.doesNotMatch(run.out, /PASS all gates/, `${why}: reported a clean pass\n${run.out}`);
  if (run.status === 0) {
    assert.match(run.out, /PASS with gates that did not run:/, `${why}: exited 0 without naming a gap\n${run.out}`);
    if (named) assert.match(run.out, named, `${why}: the gap is not named\n${run.out}`);
  } else {
    assert.match(run.out, /FAIL|INCOMPLETE/, `${why}: nonzero exit with no explanation\n${run.out}`);
  }
}

const ALL = ["go", "golangci", "govulncheck", "nilaway"];

// ---------------------------------------------------------------- controls

test("control: with every tool healthy the surface steps pass, so a failure below is the injected fault", () => {
  const run = runGate([SEGMENTS.surface], ALL);
  assert.equal(run.status, 0, run.out);
  assert.match(run.out, /PASS all gates/);
});

test("control: the test and command-gate segments pass against a healthy stub", () => {
  assert.equal(runGate([SEGMENTS.test], ["go"]).status, 0);
  const run = runGate([SEGMENTS.commandgate], ["go"]);
  assert.equal(run.status, 0, run.out);
});

test("control: the rust segment passes against a healthy cargo", () => {
  const run = runGate([SEGMENTS.rust], ["cargo", "cargo-audit"]);
  assert.equal(run.status, 0, run.out);
});

// ---------------------------------------------------------------- go list, tidy

for (const [mode, desc] of [
  ["fail", "go list exits 1"],
  ["127", "go list exits 127"],
  ["empty", "go list exits 0 and lists nothing"],
]) {
  test(`dependency surface: ${desc}`, () => {
    assertNotAPass(runGate([SEGMENTS.surface], ALL, { GO_LIST: mode }), desc);
    assert.notEqual(runGate([SEGMENTS.surface], ALL, { GO_LIST: mode }).status, 0);
  });
}

test("dependency surface: a cgo graph without the gusset engine is not the shipped configuration", () => {
  const run = runGate([SEGMENTS.surface], ALL, { GO_LIST_NO_GUSSET: "1" });
  assert.notEqual(run.status, 0, run.out);
});

for (const mode of ["diff", "127"]) {
  test(`go mod tidy -diff: ${mode}`, () => {
    const run = runGate([SEGMENTS.surface], ALL, { GO_TIDY: mode });
    assert.notEqual(run.status, 0, run.out);
    assertNotAPass(run, `go mod tidy ${mode}`);
  });
}

// ---------------------------------------------------------------- lint, vuln, nilaway

for (const [env, desc] of [
  [{ LINT_ENFORCED: "127" }, "golangci-lint (enforced set) exits 127"],
  [{ LINT_ENFORCED: "1" }, "golangci-lint (enforced set) reports findings"],
  [{ LINT_DEBT: "127" }, "golangci-lint (debt) exits 127"],
  [{ LINT_DEBT: "typecheck" }, "golangci-lint (debt) can load no package and exits 3 with a typecheck report"],
  [{ LINT_DEBT: "typecheck-exit1" }, "golangci-lint (debt) exits 1 with only a typecheck report"],
  [{ LINT_DEBT: "empty" }, "golangci-lint (debt) writes no report"],
  [{ LINT_DEBT: "regress" }, "golangci-lint (debt) regressed"],
  [{ VULN: "127" }, "govulncheck exits 127"],
  [{ VULN: "fail" }, "govulncheck finds a vulnerability"],
  [{ NILAWAY: "127" }, "nilaway exits 127"],
  [{ NILAWAY: "empty" }, "nilaway prints nothing"],
  [{ NILAWAY: "crash" }, "nilaway crashes"],
  [{ NILAWAY: "partial" }, "nilaway loses a package beside a real finding"],
  [{ NILAWAY: "over" }, "nilaway is above its ceiling"],
]) {
  test(`analysis: ${desc}`, () => {
    const run = runGate([SEGMENTS.surface], ALL, env);
    assert.notEqual(run.status, 0, `${desc}\n${run.out}`);
    assertNotAPass(run, desc);
  });
}

for (const [tool, gate, named] of [
  ["golangci", "lint", /golangci-lint is not installed/],
  ["govulncheck", "vulnerabilities", /govulncheck is not installed/],
  ["nilaway", "nil analysis", /nilaway is not installed/],
]) {
  test(`analysis: ${tool} missing from PATH is named, never a clean pass`, () => {
    const run = runGate([SEGMENTS.surface], ALL.filter((t) => t !== tool));
    assertNotAPass(run, `${tool} missing`, { named });
    assert.match(run.out, named);
    // And CI's guard refuses that log outright.
    const log = path.join(mkdtempSync(path.join(tmpdir(), "gate-log-")), "verify.log");
    writeFileSync(log, run.out);
    const guard = spawnSync(path.join(HERE, "check-verify-log.sh"), [log, "ubuntu-latest", "default"], { encoding: "utf8" });
    assert.equal(guard.status, 1, `${gate}: the CI guard accepted a log whose gate did not run\n${guard.stdout}${guard.stderr}`);
  });
}

// ---------------------------------------------------------------- go test

for (const [mode, desc] of [
  ["127", "go test exits 127"],
  ["fail", "a test fails"],
  ["silent", "go test exits 0 having written no events and no coverage profile"],
]) {
  test(`go test: ${desc}`, () => {
    const run = runGate([SEGMENTS.test], ["go"], { GO_TEST: mode });
    assert.notEqual(run.status, 0, run.out);
    assertNotAPass(run, desc);
  });
}

test("go tool cover failing is not a coverage figure", () => {
  const run = runGate([SEGMENTS.test], ["go"], { GO_COVER_STATUS: "2" });
  assert.notEqual(run.status, 0, run.out);
});

// ---------------------------------------------------------------- command gate and docs counts

for (const [env, desc] of [
  [{ GO_GATE: "127" }, "the command-gate differential cannot run (go exits 127)"],
  [{ GO_GATE: "fail" }, "a command-gate subtest fails after 70 others passed"],
  [{ GO_DOCS: "127" }, "the documentation contract cannot run"],
  [{ GO_DOCS: "fail" }, "a documentation contract test fails"],
  [{ GO_DOCS: "few" }, "the documentation contract runs fewer checks than it declares"],
]) {
  test(`counted gate: ${desc}`, () => {
    const run = runGate([SEGMENTS.commandgate], ["go"], env);
    assert.notEqual(run.status, 0, run.out);
    assertNotAPass(run, desc);
  });
}

// ---------------------------------------------------------------- rust, bun

for (const [env, desc] of [
  [{ CARGO: "127" }, "cargo exits 127"],
  [{ CARGO: "fmt" }, "native/ is unformatted"],
  [{ CARGO: "clippy" }, "clippy fails"],
  [{ CARGO: "zero" }, "the suites compile in and run zero tests"],
  [{ CARGO: "testfail" }, "cargo test fails"],
  [{ AUDIT: "fail" }, "cargo audit finds a vulnerable crate"],
  // native/ is its own workspace with its own Cargo.lock; auditing crates/ alone
  // left the desktop broker's tree unchecked.
  [{ AUDIT: "native" }, "cargo audit finds a vulnerable crate in native/ only"],
  // cargo audit exits 0 on an unsound advisory unless asked to deny it.
  [{ AUDIT: "unsound" }, "cargo audit finds an unsound crate"],
]) {
  test(`rust: ${desc}`, () => {
    const run = runGate([SEGMENTS.rust], ["cargo", "cargo-audit"], env);
    assert.notEqual(run.status, 0, run.out);
    assertNotAPass(run, desc);
  });
}

test("rust: cargo missing from PATH stops the gate", () => {
  const run = runGate([SEGMENTS.rust], []);
  assert.notEqual(run.status, 0, run.out);
});

test("rust: cargo-audit missing is named", () => {
  const run = runGate([SEGMENTS.rust], ["cargo"]);
  assertNotAPass(run, "cargo-audit missing", { named: /cargo-audit is not installed/ });
});

test("bun audit: a failing audit fails and a missing bun is named", () => {
  assert.notEqual(runGate([SEGMENTS.bunaudit], ["bun"], { BUN: "fail" }).status, 0);
  assertNotAPass(runGate([SEGMENTS.bunaudit], []), "bun missing", { named: /bun is not installed/ });
});

// ---------------------------------------------------------------- consumers

// The step runs the real scripts/check-consumers.sh (runGate links scripts/),
// so these exercise its exit statuses rather than a stand-in for them.
function fakeJarvis() {
  const dir = mkdtempSync(path.join(tmpdir(), "gate-jarvis-"));
  writeFileSync(path.join(dir, "go.mod"), "module github.com/bharathvbcr/Jarvis\n\ngo 1.27.4\n");
  return dir;
}

test("consumers: no Jarvis checkout is a gate that did not run, named, never a pass", () => {
  assertNotAPass(runGate([SEGMENTS.consumers], ["go"]), "Jarvis missing", { named: /no Jarvis checkout beside this repository/ });
});

test("consumers: a Jarvis that no longer builds against this checkout fails the gate", () => {
  const run = runGate([SEGMENTS.consumers], ["go"], { MANVI_CONSUMER_JARVIS: fakeJarvis(), GO_BUILD: "fail" });
  assert.notEqual(run.status, 0, run.out);
  assert.match(run.out, /Jarvis no longer builds or vets against this checkout/);
});

test("consumers: control — a Jarvis that builds and vets passes and says what it covered", () => {
  const run = runGate([SEGMENTS.consumers], ["go"], { MANVI_CONSUMER_JARVIS: fakeJarvis() });
  assert.equal(run.status, 0, run.out);
  assert.match(run.out, /covered: Jarvis builds and vets/);
  assert.match(run.out, /PASS all gates/);
});

// verify.sh discards a bench suite's output when it passes, so a claim the suite
// could not check (stress_test.py's C6 fork denial under bwrap) would vanish
// unless the step carries the suite's NOT COVERED line into the gate list.
test("bench: a suite's NOT COVERED line is named as a gate that did not run, never a pass", () => {
  assertNotAPass(runGate([SEGMENTS.bench], ["python3"], { BENCH_GAP: "1" }), "bench suite gap",
    { named: /bench\/stress_test\.py: C6 fork denial — no filter here/ });
});

test("bench: control — suites that name no gap pass", () => {
  const run = runGate([SEGMENTS.bench], ["python3"]);
  assert.equal(run.status, 0, run.out);
  // The fake python3 reports 100 per suite, and the bench step runs every suite
  // in verify.sh's loop; the count is read from there so adding a suite does not
  // leave this pinned to the old number.
  const loop = VERIFY.join("\n").match(/bench_ran=0\nfor t in ([\s\S]*?); do/);
  assert.ok(loop, "verify.sh no longer has the bench suite loop this test counts");
  const suites = loop[1].replace(/\\\n/g, " ").split(/\s+/).filter(Boolean).length;
  assert.ok(suites >= 5, `read only ${suites} bench suites`);
  assert.match(run.out, new RegExp(`covered: ${suites * 100} checks`));
  assert.match(run.out, /PASS all gates/);
});

// ---------------------------------------------------------------- legs

// The zero-step guard sits between the last step and the verdict, so no segment
// above runs it. It is cut out of verify.sh by its own `if` line and run here
// with a leg selected: a leg that entered no step must fail, not print PASS.
const ZERO_STEP_START = VERIFY.findIndex((l) => l.startsWith('if [[ -n "$LEG" ]] && (( STEPS_RUN == 0 ))'));
assert.ok(ZERO_STEP_START > 0, "verify.sh no longer has the zero-step guard before its verdict");
const ZERO_STEP_GUARD = VERIFY.slice(ZERO_STEP_START, ZERO_STEP_START + VERIFY.slice(ZERO_STEP_START).findIndex((l) => l === "fi") + 1).join("\n");

test("legs: a selected leg that entered no step fails and never prints a pass", () => {
  const run = runGate(["LEG=bench", ZERO_STEP_GUARD], []);
  assert.notEqual(run.status, 0, run.out);
  assert.match(run.out, /leg bench entered no step/);
  assert.match(run.out, /INCOMPLETE/);
  assert.doesNotMatch(run.out, /PASS/, "a leg that ran nothing must not print a verdict");
});

test("legs: control — the same leg with one step entered passes and says which leg and how many steps", () => {
  const run = runGate(["LEG=bench", 'step "a step"', ZERO_STEP_GUARD], []);
  assert.equal(run.status, 0, run.out);
  assert.match(run.out, /leg: bench \(1 step\(s\) ran\)/);
  assert.match(run.out, /PASS all gates/);
});

test("legs: the guard applies to a selected leg only, not to a whole-script run", () => {
  const run = runGate([ZERO_STEP_GUARD], []);
  assert.equal(run.status, 0, run.out);
  assert.match(run.out, /leg: all \(0 step\(s\) ran\)/);
});
