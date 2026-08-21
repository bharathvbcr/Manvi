#!/usr/bin/env bash
# One gate for both planes. Every phase in the strategy ends in a command that
# can fail; this is that command.
#
#   ./verify.sh          format check, vet/clippy, and both test suites
#   ./verify.sh --fix    rewrite formatting in place first
set -euo pipefail

cd "$(dirname "$0")"
FIX=0
[[ "${1:-}" == "--fix" ]] && FIX=1

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
crates/target/debug/dcstore --db "$tmpdb" health >/dev/null || fail "dcstore health"
crates/target/debug/dcstore --db "$tmpdb" acquire --task VERIFY-1 --owner gate --ttl-seconds 60 >/dev/null \
  || fail "dcstore acquire"
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

printf '\n\033[32mPASS\033[0m all gates\n'
