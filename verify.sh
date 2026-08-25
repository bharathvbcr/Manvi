#!/usr/bin/env bash
# One gate for both planes. Every phase in the strategy ends in a command that
# can fail; this is that command.
#
#   ./verify.sh          format check, vet/clippy, and both test suites
#   ./verify.sh --fix    rewrite formatting in place first
#   ./verify.sh --race   the Go suite again under the race detector
set -euo pipefail

cd "$(dirname "$0")"
FIX=0
RACE=0
case "${1:-}" in
  --fix)  FIX=1 ;;
  --race) RACE=1 ;;
  "")     ;;
  *)      printf 'usage: %s [--fix|--race]\n' "$0" >&2; exit 2 ;;
esac

# The Go plane is built and tested with cgo off, because "no cgo" is a claim
# this repository makes in four source comments and in its own architecture
# diagram, and a claim no build asserts is a claim that stops being true
# quietly. The process boundary to Rust exists so that it can stay true: a
# store linked in-process would forfeit CGO_ENABLED=0, cross-compilation, and
# the single static binary in one step. Setting it here rather than on the
# individual builds means vet, the test suites, and every binary this gate
# produces are all the configuration that ships.
export CGO_ENABLED=0

fail() { printf '\n\033[31mFAIL\033[0m %s\n' "$1" >&2; exit 1; }
step() { printf '\n\033[36m==>\033[0m %s\n' "$1"; }

step "Go — format"
if (( FIX )); then
  gofmt -w manvi
else
  unformatted="$(gofmt -l manvi)"
  [[ -z "$unformatted" ]] || fail "gofmt: $unformatted (run ./verify.sh --fix)"
fi

step "Go — vet"
(cd manvi && go vet ./...) || fail "go vet"

# Tests that drive a real binary across the Go/Rust boundary fail rather than
# skip when the toolchain is missing, unless an operator opts out explicitly.
# This gate exists because they did once skip: the store package located the
# Rust workspace with a hand-counted relative path, the path was wrong by one
# level, and `go test ./...` printed "ok" for a package that executed nothing.
step "Go — test"
if [[ -n "${MANVI_TEST_ALLOW_SKIP:-}" ]]; then
  fail "MANVI_TEST_ALLOW_SKIP is set; this gate will not certify a run that is permitted to skip seams"
fi
(cd manvi && go test ./...) || fail "go test"

# A package whose tests all skip still prints "ok". Count what actually ran in
# the packages that cross the process boundary, so a silent skip cannot pass.
# The race detector needs cgo, which the default gate deliberately turns off:
# CGO_ENABLED=0 is a claim this repository makes, so the default run has to be
# the shipped configuration. That left `go test -race ./...` — the first thing a
# reviewer of a concurrency-heavy Go project runs — outside every gate, and it
# was not clean: TestConcurrentQueriesDoNotInterfere wedged a full run for ten
# minutes and failed 2 of 5 isolated runs, because forking a shell two dozen
# times over does not survive the race runtime on macOS. Opt-in, but gated.
#
# -p 1 is load-bearing, not tidiness. Package binaries otherwise run one per
# CPU (18 here), each carrying the race runtime and several of them forking
# shells and stub servers. That saturation is the same failure the paragraph
# above describes, scaled up: a tree-wide parallel run wedged for 90 minutes on
# a test that takes 0.99s, while its own package run clean in 78s and the whole
# tree serialised finishes in 4.4 minutes. The detector is measuring the
# machine at that point, not the code, and a gate that reports the machine is
# not a gate. Serialised is well inside the timeout below; if that stops being
# true, raise -p before raising the timeout, because the timeout only buys a
# longer wait for the same answer.
if (( RACE )); then
  step "Go — race detector"
  (cd manvi && CGO_ENABLED=1 go test -race -count=1 -p 1 -timeout 900s ./...) || fail "go test -race"
  printf '    covered: every package under the race detector, with cgo on\n'
fi

step "Go — cross-boundary coverage"
for pkg in ./dc/store ./devcouncil; do
  ran="$( (cd manvi && go test -count=1 -v "$pkg" 2>/dev/null) | grep -c '^--- PASS' || true )"
  (( ran >= 5 )) || fail "$pkg ran only ${ran} tests; the process boundary is not being exercised"
  printf '    %s: %s tests against the real binaries\n' "$pkg" "$ran"
done

step "Rust — format"
if (( FIX )); then
  (cd crates && cargo fmt --all)
else
  (cd crates && cargo fmt --all -- --check) || fail "cargo fmt (run ./verify.sh --fix)"
fi

step "Rust — clippy"
(cd crates && cargo clippy --all-targets -- -D warnings) || fail "cargo clippy"

step "Rust — test"
(cd crates && cargo test) || fail "cargo test"

# The parity fixtures are the contracts the ports are held to. If one is missing
# or truncated the suites would still pass while checking nothing, so assert
# their shape here rather than trusting the tests that read them.
step "Parity fixtures"
glob_cases="$(grep -cv '^#' testdata/fnmatch-parity.tsv || true)"
(( glob_cases >= 500 )) || fail "glob fixture has only ${glob_cases} cases; regenerate with scripts/gen-fnmatch-parity.py"
printf '    %s glob cases shared by Go and Rust\n' "$glob_cases"

cmd_cases="$(grep -cv '^#' testdata/command-parity.tsv || true)"
(( cmd_cases >= 200 )) || fail "command fixture has only ${cmd_cases} cases; regenerate with scripts/gen-command-parity.py"
printf '    %s command cases against the Python engine\n' "$cmd_cases"

# Python interop needs DevCouncil's virtualenv, which is not present on a CI
# runner. The portable half of the claim — that the schema Rust writes is the
# schema a plain sqlite3 reads — is checked unconditionally below; only the
# half that needs the incumbent's own code is conditional, and its absence is
# reported rather than passed over.
step "Cross-language — store schema"
tmpdb="$(mktemp -d)/state.sqlite"
(cd crates && cargo build -q -p dc-store --bin dcstore) || fail "building dcstore"

# `health` must not manufacture the store it is asked about. This gate used to
# run it first against a path that did not exist yet, and it passed — because
# health opened with SQLITE_OPEN_CREATE, made an empty database, and reported it
# healthy. A typo in --db was therefore indistinguishable from a working store,
# which is the same class as every other "a check that could not run answered
# like one that passed" defect in this file. The order below is now load-bearing
# rather than incidental: a writing command creates, and health only reads.
if crates/target/debug/dcstore --db "$(mktemp -d)/absent.sqlite" health >/dev/null 2>&1; then
  fail "dcstore health reported a database that does not exist as healthy"
fi
printf '    covered: health refuses a store that does not exist rather than creating one\n'

crates/target/debug/dcstore --db "$tmpdb" acquire --task VERIFY-1 --owner gate --ttl-seconds 60 >/dev/null \
  || fail "dcstore acquire"
crates/target/debug/dcstore --db "$tmpdb" health >/dev/null || fail "dcstore health"
if command -v sqlite3 >/dev/null; then
  held="$(sqlite3 "$tmpdb" "SELECT task_id FROM task_leases WHERE status='active';")"
  [[ "$held" == "VERIFY-1" ]] || fail "an independent reader saw '$held', not VERIFY-1"
  index="$(sqlite3 "$tmpdb" "SELECT count(*) FROM sqlite_master WHERE type='index' AND name='ux_task_leases_active';")"
  (( index == 1 )) || fail "the partial unique index is missing; mutual exclusion is not enforced by the schema"
  printf '    covered: an independent sqlite3 reader agrees, and the exclusion index exists\n'
else
  printf '\033[33m    NOT COVERED\033[0m: sqlite3 not on PATH — schema readability is unverified here\n'
fi

step "Cross-language — Python interop"
if [[ -x ../DevCouncil/.venv/bin/python && -d ../DevCouncil/src ]]; then
  printf '    covered: Rust and Python drive one state.sqlite\n'
else
  printf '\033[33m    NOT COVERED\033[0m: ../DevCouncil/.venv not found — lease interop against the incumbent is unverified here\n'
fi

# The verifier's content gates are the ones whose absence used to be reported as
# a degradation. Assert they actually fire, from the shell, against the built
# binary — a gate that is only proven by its own unit tests is a gate that can
# be disconnected from the harness without anything going red.
step "Verifier — rigor gates fire"
(cd crates && cargo build -q -p dc-verify --bin dcverify) || fail "building dcverify"
rigor_out="$(printf 'diff --git a/src/a.go b/src/a.go\n--- a/src/a.go\n+++ b/src/a.go\n@@ -1,1 +1,2 @@\n package a\n+const k = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA"\n' \
  | crates/target/debug/dcverify check --planned 'src/a.go')"
grep -q '"gate":"secret_scan"' <<<"$rigor_out" || fail "the secret scanner did not fire on a planted credential"
grep -q '"severity":"blocking"' <<<"$rigor_out" || fail "a planted credential did not produce a blocking finding"
grep -q 'AAAAAAAAAAAAAAAAAAAAAAAAAAAA' <<<"$rigor_out" && fail "the finding reproduced the credential it found"
printf '    covered: a planted credential blocks, and the finding does not quote it\n'

# A malformed diff must be an error, never an empty clean result.
if printf 'this is not a diff\n' | crates/target/debug/dcverify check --planned 'src/a.go' >/dev/null 2>&1; then
  fail "input that is not a diff parsed as a clean empty result"
fi
printf '    covered: unparseable input is an error, not an empty pass\n'

# The coverage gate is the one that changed meaning most recently: it used to
# report every changed file as unmeasured because nothing fed it measurements.
# Assert from the shell that a real profile changes the answer, so the wiring
# cannot rot back to "unmeasured" without going red.
step "Verifier — coverage changes the answer"
covdiff="$(printf 'diff --git a/gate/gate.go b/gate/gate.go\n--- a/gate/gate.go\n+++ b/gate/gate.go\n@@ -9,1 +9,3 @@\n context\n+a := 1\n+unreached()\n')"
tmpcov="$(mktemp)"
printf 'mode: set\nmanvi/gate/gate.go:10.1,10.10 1 1\n' > "$tmpcov"

blind="$(printf '%s' "$covdiff" | crates/target/debug/dcverify check --planned 'gate/gate.go')"
grep -q '"coverage_unmeasured":\["gate/gate.go"\]' <<<"$blind" \
  || fail "with no profile, the changed file must be reported unmeasured: $blind"

measured="$(printf '%s' "$covdiff" | crates/target/debug/dcverify check --planned 'gate/gate.go' --coverage "$tmpcov")"
grep -q '"coverage_unmeasured":\[\]' <<<"$measured" \
  || fail "with a profile, the file must no longer be unmeasured: $measured"
grep -q '"uncovered_lines":\[11\]' <<<"$measured" \
  || fail "the unexecuted added line must be reported as a gap: $measured"
printf '    covered: no profile reports unmeasured; a real profile distinguishes covered from uncovered\n'

# A broken profile must degrade the gate rather than report every line as
# unexercised, which would turn a pipeline fault into a wall of false gaps.
printf 'not a coverage file\n' > "$tmpcov"
if printf '%s' "$covdiff" | crates/target/debug/dcverify check --planned 'gate/gate.go' --coverage "$tmpcov" >/dev/null 2>&1; then
  fail "an unreadable coverage file was accepted as a measurement of zero"
fi
rm -f "$tmpcov"
printf '    covered: an unreadable profile errors rather than reporting zero coverage\n'

# Repo navigation. The index is optional — a machine without devmap can still
# run everything else — so its absence is reported rather than failing the gate.
#
# What this step must not do is infer the answer from the artifact's existence,
# which is what it used to do: it printed "covered" whenever the graph file was
# present. On this machine that file was present while the devmap on PATH could
# not open the index at all (`unsupported future schema version 11`), and every
# path in the graph still named the pre-rename `harness/` tree, so the neighbour
# rule resolved no area for any file it was asked about. The gate reported a
# check that had not run as one that had passed — the exact failure this
# repository exists to prevent — so both halves are executed here instead.
#
# `devmap status` is the probe rather than `manvi map status`: the latter prints
# the unavailability and returns nil, so its exit status is 0 whether the index
# opened or not, and gating on it would rebuild the same lie in a new place.
step "Repo navigation"
mapbin="${MANVI_MAP_BINARY:-devmap}"
graph="${MANVI_GRAPH:-.devcouncil/code_graph.json}"
if ! command -v "$mapbin" >/dev/null && [[ ! -x "$mapbin" ]]; then
  printf '\033[33m    NOT COVERED\033[0m: devmap not found — repo navigation is unverified here\n'
elif ! mapout="$("$mapbin" status 2>&1)"; then
  printf '\033[33m    NOT COVERED\033[0m: `%s status` failed — the navigation tools cannot read the index: %s\n' \
    "$mapbin" "$(printf '%s' "$mapout" | tr '\n' ' ')"
elif [[ ! -f "$graph" ]]; then
  printf '\033[33m    NOT COVERED\033[0m: no %s — run `manvi map build`; the neighbour rule will report repo_map.unavailable\n' "$graph"
else
  # A readable index is not the same question as a graph that describes this
  # tree, and only the second one is what the neighbour rule reads. A directory
  # rename leaves a perfectly parseable artifact whose every path is gone; the
  # rule then answers "unknown area" for every file, a degradation that reads
  # from the outside exactly like a clean deny. Both numbers are carried so a
  # single deleted file cannot be mistaken for a wholesale mismatch.
  indexed=0
  stale=0
  while IFS= read -r indexed_path; do
    [[ -n "$indexed_path" ]] || continue
    indexed=$(( indexed + 1 ))
    [[ -e "$indexed_path" ]] || stale=$(( stale + 1 ))
  done < <(grep -o '"path": "[^"]*"' "$graph" | sed 's/.*: "//; s/"$//' | sort -u)
  if (( indexed == 0 )); then
    printf '\033[33m    NOT COVERED\033[0m: %s names no files — rebuild it with `manvi map build`\n' "$graph"
  elif (( stale > 0 )); then
    printf '\033[33m    NOT COVERED\033[0m: %d of %d paths in %s no longer exist — the graph describes an older tree and the neighbour rule cannot place current files; run `manvi map build`\n' \
      "$stale" "$indexed" "$graph"
  else
    # Every path resolving is still not the question. The graph is a separate
    # file from the index, written by a separate command, and a graph built from
    # an older generation of a tree that has only grown has every path resolve
    # perfectly — which is exactly the state this check passed through: the
    # index stood at generation 4 with 4,249 symbols while the artifact carried
    # generation 2 and 2,713, every one of its paths existed, and the scope rung
    # spent every session deciding from a graph missing 112 files.
    #
    # Both sides stamp the generation they came from, so the comparison is exact
    # rather than a heuristic over counts.
    index_gen="$(printf '%s' "$mapout" | grep -o '"generation_id":[[:space:]]*[0-9]*' | head -1 | grep -o '[0-9]*$')"
    graph_gen="$(grep -o '"generation_id":[[:space:]]*[0-9]*' "$graph" | head -1 | grep -o '[0-9]*$')"
    if [[ -z "$graph_gen" ]]; then
      printf '\033[33m    NOT COVERED\033[0m: %s carries no generation stamp, so whether it was written from the index the navigation tools read is unverified\n' "$graph"
    elif [[ -z "$index_gen" ]]; then
      printf '\033[33m    NOT COVERED\033[0m: `%s status` reported no generation, so the graph cannot be checked against it\n' "$mapbin"
    elif [[ "$index_gen" != "$graph_gen" ]]; then
      printf '\033[33m    NOT COVERED\033[0m: %s was written from generation %s and the index holds %s — the scope rung and the navigation tools would answer about different trees; run `manvi map build`\n' \
        "$graph" "$graph_gen" "$index_gen"
    else
      printf '    covered: `%s status` opened the index; all %d paths in %s resolve, and both stand at generation %s\n' \
        "$mapbin" "$indexed" "$graph" "$index_gen"
    fi
  fi
fi

# The mark in assets/ is generated from the same grid the TUI draws. A hand-edit
# to either would give the published asset and the splash screen two different
# marks, so the asset is regenerated here and compared rather than trusted.
# The documentation is a declaration layer, and it was the one nothing checked.
# Five drifted claims were found in one audit — tool counts of 37 and 23 against
# a registry of 44, a fixture "776 cases" that held 775, and a ladder called
# 5-Tier that the README drew with six rungs. Each was true when written. The
# guard re-reads them against the artifact that decides the answer, so the class
# fails here rather than in front of a reader.
# A fuzz target is only a target if a runner can find it. `go test -fuzz=X ./pkg`
# answers "no fuzz tests to fuzz" and exits 0 when X is not in pkg — so a runner
# pointed at the wrong package reports success while executing nothing, which is
# how this repository's own fuzz sweep once recorded three passes for targets it
# never ran. Enumerate what is declared, and make each one prove it is reachable
# where it lives.
step "Fuzz targets — every declared target is reachable"
fuzz_declared=0
fuzz_missing=""
while IFS= read -r decl; do
  file="${decl%%:*}"
  fn="${decl##*:}"
  pkg="./$(dirname "${file#manvi/}")"
  fuzz_declared=$(( fuzz_declared + 1 ))
  # Capture before matching. Piping `go test` into `grep -q` lets grep exit on
  # the first match, `go test` take SIGPIPE, and `set -o pipefail` report the
  # pipeline as failed — which would mark every reachable target unreachable.
  listing="$( (cd manvi && go test -list "^${fn}\$" "$pkg" 2>/dev/null) || true )"
  if ! printf '%s\n' "$listing" | grep -qx "$fn"; then
    fuzz_missing="${fuzz_missing} ${pkg}:${fn}"
  fi
done < <(grep -rn '^func Fuzz' manvi --include='*_test.go' | sed -E 's/^([^:]+):[0-9]+:func (Fuzz[A-Za-z0-9_]+).*/\1:\2/')
(( fuzz_declared >= 10 )) || fail "only ${fuzz_declared} fuzz targets found; the sweep is not looking at the harness"
[[ -z "$fuzz_missing" ]] || fail "declared but not reachable in their own package:${fuzz_missing}"
printf '    covered: all %s declared fuzz targets are reachable where they are defined\n' "$fuzz_declared"

# The command gate has two verdicts to reconcile — one about the command, one
# about the files its redirections open — and for a long time only the first was
# reliably reached. The tests that prove the second are differential: they run
# each command line under `sh -c` in a throwaway tree and compare the files that
# appeared against the gate's own verdict on those files. That makes them the
# only checks here whose expectation comes from the filesystem rather than from
# something a person wrote down, which is exactly why they found what the
# hand-written fixtures could not.
#
# They are counted rather than trusted to have run. `go test ./gate` prints "ok"
# whether these executed or were renamed out of existence, and a differential
# check that silently stopped running would leave the class it covers looking
# closed.
step "Command gate — verdicts match what the shell actually writes"
diff_ran="$( (cd manvi && go test -count=1 -v ./gate/ \
  -run 'TestCommandVerdictIsNeverLooserThanTheWritesItPerforms|TestHiddenWritesAreRefusedOutrightUnderEveryPosture' 2>/dev/null) \
  | grep -c '^    --- PASS' || true )"
(( diff_ran >= 60 )) || fail "the command/filesystem differential ran only ${diff_ran} cases; the corpus is not being exercised"
printf '    covered: %s command lines executed under sh and reconciled against the gate\n' "$diff_ran"

# The generated half of the same differential. The corpus above covers the
# shapes someone wrote down; this assembles command lines from the shell's own
# operators and checks the same invariant against the filesystem, and it is what
# found the substituted write that escaped the repository root altogether.
#
# The count that matters is not how many lines ran but how many got far enough
# to be checked: the invariant is conditional on the gate having allowed the
# line, so a run where nothing was allowed passes while proving nothing. The
# test fails on its own if that happens; this step surfaces the numbers.
step "Command gate — generated command lines hold the same invariant"
gen_out="$( (cd manvi && MANVI_GATE_SOAK="${MANVI_GATE_SOAK:-400}" go test -count=1 -v ./gate/ \
  -run 'TestGeneratedCommandsNeverOutrunTheirOwnWriteVerdict' 2>&1) )"
grep -q '^--- PASS' <<<"$gen_out" || { printf '%s\n' "$gen_out" >&2; fail "the generated differential did not pass"; }
printf '    %s\n' "$(grep -o '[0-9]* generated command lines: .*' <<<"$gen_out" | head -1)"

step "Docs — every stated count is the measured count"
docs_ran="$( (cd manvi && go test -count=1 -v ./internal/contract/ -run 'TestParityCountsInProseMatchTheFixtures|TestMermaidDiagramsAreWellFormed|TestPolicyLadderRungCountIsConsistent|TestOutcomeStateCountIsConsistent|TestEveryRelativeDocLinkResolves|TestEveryCLISubcommandIsDocumented' 2>/dev/null) | grep -c '^--- PASS' || true )"
(( docs_ran == 6 )) || fail "the documentation contract ran only ${docs_ran} of 6 checks"
printf '    covered: parity counts, mermaid syntax, ladder rungs, outcome states, links and subcommands all agree with the code\n'

# The Go suite's mermaid check is structural: it knows the edge operators and
# nothing else. Two diagrams shipped broken straight past it — a participant
# named `Loop`, which Mermaid's lexer reads as its reserved `loop` keyword
# regardless of case, and a raw `;` inside a CSI escape sequence, which the
# grammar takes for a statement separator. Both rendered as GitHub parse errors
# on the project's front door while every gate stayed green. Only the real
# grammar sees that class, so every fenced block is handed to mermaid.parse
# here. The dependency tree this needs is dev-only (nothing in it reaches a
# shipped binary), pinned by package-lock.json.
step "Docs — mermaid blocks parse with the real grammar"
if command -v node >/dev/null && command -v npm >/dev/null; then
  if [[ ! -d node_modules ]]; then
    npm ci --no-audit --no-fund --silent || fail "npm ci could not provision the mermaid parser gate"
  fi
  node scripts/check-mermaid.mjs || fail "a mermaid diagram does not parse with the real grammar"
else
  printf '\033[33m    NOT COVERED\033[0m: node/npm not on PATH — diagrams are checked structurally only\n'
fi

step "Brand — the published mark is the drawn mark"
logo_bin="$(mktemp -d)/manvi"
(cd manvi && go build -o "$logo_bin" ./cmd/manvi) || fail "building manvi"
# Init off for both harness invocations below. Every command prepares the
# repository it runs in — that is the point of it — and a verification script
# that scaffolds the tree it is checking would be reporting on a tree it just
# changed, and would start a background index build from the TUI step.
if ! diff -q <(MANVI_HARNESS_INIT_ENABLED=false "$logo_bin" logo --svg) assets/manvi-mark.svg >/dev/null; then
  fail "assets/manvi-mark.svg differs from \`manvi logo --svg\` — regenerate it"
fi
printf '    covered: assets/manvi-mark.svg is byte-identical to the generator\n'

# The live wire contracts. Every provider constant was transcribed from
# documentation, and documentation is a claim about an API rather than the API.
# Nothing in this script can close that, so it is reported, never assumed.
# Three of the four adapters can only be checked against the real thing by
# spending money and reaching the public internet, so they stay an operator's
# command. The local adapter is the exception and is treated as one: its
# endpoint is a process on this machine, so when it is up there is no reason to
# certify a run without having actually talked to it. That turns the one
# provider whose live contract is free to check from a claim into a result.
step "Provider wire contracts"
# Which server, and which model, are both asked of the binary rather than
# decided here.
#
# This gate used to curl a hardcoded http://127.0.0.1:8000/v1 and read
# llm.local.model out of `manvi flags --all` with awk. Both were reimplementations
# of decisions the binary makes, and both were wrong in the same way: the port is
# vLLM's, so a machine running Ollama on 11434 was certified as having no local
# server while one was answering, and the setting is only one of three places a
# model id comes from.
#
# `manvi local --resolve` is the binary's own answer to "what would a run use",
# printed as key=value for exactly this caller. It performs the endpoint scan and
# the model resolution a real turn performs, so this gate and the run it certifies
# can no longer disagree about either.
#
# The caution that shaped the old gate is kept, and is now enforced in code
# rather than in this comment: it still never picks a model off the server's
# list. A local server lists its whole weights cache, and the first entry here
# was an audio model whose probe failed the gate with a 400 that said nothing
# about this adapter. Resolution only answers when the server itself leaves
# nothing to choose — exactly one model that reports it can both generate text
# and call tools — and otherwise refuses and names the candidates.
probe_bin="$(mktemp -t manvi-probe)"
(cd manvi && go build -o "$probe_bin" ./cmd/manvi) || fail "building manvi for the local probe"

# stdout is the document, stderr is the reason it could not be produced. Kept
# apart so a partial document can never be parsed as a whole one.
resolve_err="$(mktemp -t manvi-resolve-err)"
if resolved="$(MANVI_HARNESS_INIT_ENABLED=false "$probe_bin" local --resolve 2>"$resolve_err")"; then
  probe_model="$(printf '%s\n' "$resolved" | awk -F= '$1 == "model" { print substr($0, index($0, "=") + 1) }')"
  probe_base="$(printf '%s\n' "$resolved" | awk -F= '$1 == "base_url" { print substr($0, index($0, "=") + 1) }')"
  model_source="$(printf '%s\n' "$resolved" | awk -F= '$1 == "model_source" { print substr($0, index($0, "=") + 1) }')"
  base_source="$(printf '%s\n' "$resolved" | awk -F= '$1 == "base_url_source" { print substr($0, index($0, "=") + 1) }')"

  # A document that parsed to nothing is not a pass. Without this a change to
  # the output shape would silently probe an empty model name.
  [[ -n "$probe_model" && -n "$probe_base" ]] || \
    fail "manvi local --resolve produced no model or address: $resolved"

  # The model is pinned for the probe so this gate reports the model it actually
  # exercised, rather than one the probe resolved a second time.
  if MANVI_HARNESS_INIT_ENABLED=false MANVI_MODEL="$probe_model" \
      "$probe_bin" probe local >/tmp/manvi-local-probe.log 2>&1; then
    printf '    covered: local — one real request to %s (%s) on %s (%s)\n' \
      "$probe_base" "$base_source" "$probe_model" "$model_source"
    printf '             satisfied the wire contract\n'
  else
    cat /tmp/manvi-local-probe.log >&2
    fail "the local adapter's live wire contract does not hold"
  fi
else
  # The binary's own diagnosis, verbatim. It already distinguishes an
  # unreachable server from an ambiguous model and says what to do about each,
  # so restating it here would be a second wording of the same fact — free to
  # drift, and drifting toward whichever one is read less often.
  printf '\033[33m    NOT COVERED\033[0m: local — this gate makes no request. The harness reports:\n'
  sed 's/^/                  /' "$resolve_err"
fi
rm -f "$probe_bin" "$resolve_err"
printf '\033[33m    NOT COVERED\033[0m: anthropic, gemini and xai are verified against scripted servers only.\n'
printf '                  Run `manvi probe anthropic|gemini|xai` with a credential to check a live endpoint.\n'

# The TUI's one non-negotiable property is that it hands the terminal back. A
# unit test cannot establish it — Start refuses anything that is not a tty — so
# this drives the real binary through a pty and asserts the teardown sequences
# are actually written. Without them an operator is left on the alternate screen
# in raw mode, where the shell shows nothing they type.
step "TUI — the terminal is handed back"
tui_pty() {
  # macOS and util-linux spell script's arguments differently.
  if script -q /dev/null true >/dev/null 2>&1; then
    script -q "$1" "${@:2}"
  elif script -q -c true /dev/null >/dev/null 2>&1; then
    script -q -c "${*:2}" "$1"
  else
    return 127
  fi
}
if command -v script >/dev/null; then
  tui_bin="$(mktemp -d)/manvi"
  (cd manvi && go build -o "$tui_bin" ./cmd/manvi) || fail "building manvi"
  tui_log="$(mktemp -d)/tui.out"
  # Ctrl+Q twice, after raw mode is established. Before it, the tty's own flow
  # control eats Ctrl+Q as XON and the keystroke never reaches the program.
  ( sleep 2; printf '\021'; sleep 0.4; printf '\021'; sleep 1 ) \
    | MANVI_HARNESS_INIT_ENABLED=false TERM=xterm-256color tui_pty "$tui_log" "$tui_bin" tui >/dev/null 2>&1 || true
  missing=""
  for seq in '?1049l' '?2004l' '?1006l' '?25h'; do
    grep -q "$(printf '\033')\[${seq}" "$tui_log" || missing="$missing $seq"
  done
  if [[ -n "$missing" ]]; then
    fail "the TUI exited without restoring the terminal (missing:$missing)"
  fi
  printf '    covered: alternate screen, mouse, bracketed paste and the cursor are all restored on exit\n'
else
  printf '\033[33m    NOT COVERED\033[0m: script(1) not available — TUI terminal restoration is unverified here\n'
fi

# The benchmark is the instrument the paper's numbers come from, and it was
# outside every gate: `grep bench verify.sh` returned nothing. Its suites cover
# the bootstrap and the paired deltas, the seed pinning a delta depends on,
# the cell-assembly refusals that keep two protocols out of one cell, and the
# containment the verifier relies on. An instrument nothing checks is an
# instrument nobody can trust, so it is checked here with everything else.
step "Bench — instrument, statistics and cell assembly"
for t in test_stats.py test_pool.py test_runtime.py test_compute.py stress_test.py; do
  (cd bench && python3 "$t" >/dev/null) || fail "bench/$t"
done
(cd bench && python3 selftest.py >/dev/null) || fail "bench/selftest.py"
printf '    covered: bootstrap CIs, paired deltas, seed pinning, cell-assembly refusals,\n'
printf '             sandbox containment, and 19 tasks that start broken and reject tampering\n'

printf '\n\033[32mPASS\033[0m all gates\n'
