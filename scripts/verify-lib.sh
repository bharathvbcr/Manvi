# Gate helpers that verify.sh sources. They live here, and not inline, so that
# scripts/verify-lib.test.mjs can run each one against a stub `go`,
# `golangci-lint` or `nilaway` and watch it refuse the outputs a real tool
# produces when it could not do its job.
#
# The shape every helper shares: a tool that failed, produced nothing, or
# produced a report of its own failure is an error (status 2, reason on stderr),
# never a clean count. A count read off a pipeline whose producer's exit status
# was masked with `|| true` is how this gate once passed a failing differential,
# a failing `go list`, and a lint run that never loaded a package. Do not write
# that pipeline over a command that can fail: run the command to a file, check
# its status, then count the file. scripts/verify-lib.test.mjs scans for it.
#
# Callers define `fail`. The helpers return rather than exit so a caller's
# `|| fail "…"` decides the message, and so a `$(…)` capture cannot swallow an
# exit.

# gate_count_passes <module-dir> <grep-pattern> <go test args…>
#
# Runs `go test -count=1 -v <args>` in the module, and prints how many output
# lines match the pattern. A failing run is status 2 with its output on stderr:
# a failed subtest must never be one fewer PASS line in a count that is still
# over its floor.
gate_count_passes() {
  local dir="$1" pattern="$2" out status=0 n
  shift 2
  out="$(mktemp)"
  (cd "$dir" && go test -count=1 -v "$@") >"$out" 2>&1 || status=$?
  if (( status != 0 )); then
    printf 'go test %s exited %d:\n' "$*" "$status" >&2
    tail -n 200 "$out" >&2
    rm -f "$out"
    return 2
  fi
  n="$(grep -c "$pattern" "$out")" || n=0
  rm -f "$out"
  printf '%s' "$n"
}

# gate_go_test_json <module-dir> <out.json> <go test args…>
#
# One instrumented run of the whole suite, as JSON events, so the pass verdict,
# the skip report and the cross-boundary counts all read the same execution.
# A failing run prints the output of every failed package, bounded, and is
# status 2.
gate_go_test_json() {
  local dir="$1" json="$2" status=0 err
  shift 2
  err="$(mktemp)"
  (cd "$dir" && go test -json "$@") >"$json" 2>"$err" || status=$?
  if (( status != 0 )); then
    python3 - "$json" <<'PY' >&2
import json, sys
failed, tests, out = set(), [], []
for line in open(sys.argv[1], errors="replace"):
    line = line.strip()
    if not line.startswith("{"):
        continue
    try:
        e = json.loads(line)
    except ValueError:
        continue
    if e.get("Action") == "fail" and not e.get("Test"):
        failed.add(e.get("Package"))
    if e.get("Action") == "fail" and e.get("Test"):
        tests.append(e["Package"] + "." + e["Test"])
    if e.get("Action") == "output":
        out.append((e.get("Package"), e.get("Output", "")))
shown = [text for pkg, text in out if pkg in failed]
print("".join(shown[-400:]), end="")
if tests:
    print("failed tests:")
    print("\n".join("  " + t for t in tests[:50]))
PY
    tail -n 50 "$err" >&2
    printf 'go test -json exited %d\n' "$status" >&2
    rm -f "$err"
    return 2
  fi
  rm -f "$err"
}

# gate_go_json_report <events.json> <package-path-suffix…>
#
# Reads a `go test -json` stream and prints:
#   observed <n>        test results seen (pass, fail or skip)
#   ran <suffix> <n>    top-level tests that passed in the package ending so
#   skip <pkg>.<test>   one line per skipped test
gate_go_json_report() {
  python3 - "$@" <<'PY'
import json, sys
path, suffixes = sys.argv[1], sys.argv[2:]
observed = 0
skips = []
ran = {s: 0 for s in suffixes}
for line in open(path, errors="replace"):
    line = line.strip()
    if not line.startswith("{"):
        continue
    try:
        e = json.loads(line)
    except ValueError:
        continue
    test, action, pkg = e.get("Test"), e.get("Action"), e.get("Package", "")
    if not test:
        continue
    if action in ("pass", "fail", "skip"):
        observed += 1
    if action == "skip":
        skips.append(pkg + "." + test)
    if action == "pass" and "/" not in test:
        for s in suffixes:
            if pkg.endswith(s):
                ran[s] += 1
print("observed", observed)
for s in suffixes:
    print("ran", s, ran[s])
for name in skips:
    print("skip", name)
PY
}

# gate_deps_list <module-dir> <outfile>
#
# `go list -deps ./...` for the environment the caller has set. Status 2 when
# it fails or lists nothing: an empty list passes every allowlist.
gate_deps_list() {
  local dir="$1" out="$2" err status=0
  err="$(mktemp)"
  (cd "$dir" && go list -deps ./...) >"$out" 2>"$err" || status=$?
  if (( status != 0 )); then
    printf 'go list -deps exited %d:\n' "$status" >&2
    tail -n 50 "$err" >&2
    rm -f "$err"
    return 2
  fi
  rm -f "$err"
  if [[ ! -s "$out" ]]; then
    printf 'go list -deps listed no packages\n' >&2
    return 2
  fi
}

# gate_deps_unexpected <deps-file> <allowed-regex>
#
# The external packages in a `go list -deps` listing that the allowlist does
# not name. The module itself is a dotted path, so it is excluded explicitly;
# the allowlist is for external packages. `grep` exits 1 for "nothing left",
# which here is the good answer, and is the only status it is allowed to hide.
gate_deps_unexpected() {
  local file="$1" allowed="$2"
  grep -E '^[a-z0-9-]+\.[a-z]+/' "$file" \
    | grep -v '^crypto/internal' \
    | grep -v '^github\.com/bharathvbcr/Manvi/manvi\(/\|$\)' \
    | grep -Ev "$allowed" || true
}

# gate_debt_ratchet <module-dir> <golangci config> <counts-file>
#
# Runs the debt lint set and compares its per-linter counts to the recorded
# ones. Prints `REGRESSED <linter: a -> b …>` and `IMPROVED <…>` lines. Status 2
# when the run could not be trusted: golangci-lint exits 0 (clean) or 1
# (findings) and anything else is a failure to run, and a `typecheck` finding
# is the linter reporting that it could not load a package, in which case every
# real linter's count is zero by omission and reads as debt paid off.
gate_debt_ratchet() {
  local dir="$1" config="$2" counts="$3" json err status=0 totals linter recorded now regressed="" improved=""
  json="$(mktemp)"
  err="$(mktemp)"
  (cd "$dir" && golangci-lint run --config "$config" \
      --output.json.path="$json" --output.text.path= ./...) >/dev/null 2>"$err" || status=$?
  if (( status > 1 )); then
    printf 'golangci-lint exited %d, which is a failure to run rather than a finding:\n' "$status" >&2
    tail -n 30 "$err" >&2
    rm -f "$json" "$err"
    return 2
  fi
  rm -f "$err"
  if [[ ! -s "$json" ]]; then
    printf 'the debt lint run produced no report; the ratchet has nothing to compare\n' >&2
    rm -f "$json"
    return 2
  fi
  totals="$(python3 - "$json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
rep_err = (d.get("Report") or {}).get("Error")
if rep_err:
    print("error", rep_err)
counts = {}
for i in d.get("Issues") or []:
    counts[i["FromLinter"]] = counts.get(i["FromLinter"], 0) + 1
for k, v in sorted(counts.items()):
    print(k, v)
PY
)" || { rm -f "$json"; printf 'the debt lint report is not parseable JSON\n' >&2; return 2; }
  rm -f "$json"
  if grep -qE '^(typecheck|error) ' <<<"$totals"; then
    printf 'the debt lint run could not load the packages it was asked about, so its counts mean nothing:\n' >&2
    grep -E '^(typecheck|error) ' <<<"$totals" >&2
    return 2
  fi
  while read -r linter recorded; do
    [[ -n "$linter" ]] || continue
    now="$(awk -v l="$linter" '$1 == l { print $2 }' <<<"$totals")"
    now="${now:-0}"
    if (( now > recorded )); then
      regressed="${regressed} ${linter}: ${recorded} -> ${now}"
    elif (( now < recorded )); then
      improved="${improved} ${linter}: ${recorded} -> ${now}"
    fi
  done < <(grep -E '^[a-z]+ [0-9]+$' "$counts")
  [[ -z "$regressed" ]] || printf 'REGRESSED%s\n' "$regressed"
  [[ -z "$improved" ]] || printf 'IMPROVED%s\n' "$improved"
}

# gate_nilaway_count <module-dir> <include-pkgs>
#
# Prints how many potential nil panics nilaway found. Status 2 when its output
# says it did not analyse everything. nilaway exits non-zero for findings and
# for load failures alike, so the status cannot tell them apart; the output can.
# A finding is one column-0 line naming "Potential nil panic detected" with
# tab-indented detail under it, and a finding that shares its nil source with
# others is followed by a "(Same nil source could also cause …)" summary line.
# Any other column-0 line is a compiler error, an
# "analysis skipped due to errors in package", or a "failed prerequisites" —
# a package that dropped out of the run, whose findings are then missing from a
# count that is only ever compared with `<=`.
gate_nilaway_count() {
  local dir="$1" include="$2" raw out status=0 stray found tab=$'\t'
  raw="$(mktemp)"
  out="$(mktemp)"
  (cd "$dir" && nilaway -include-pkgs="$include" ./...) >"$raw" 2>&1 || status=$?
  # 0 is clean, 1 and 3 are findings or load failures; the output below tells
  # those apart. Anything else is nilaway not running.
  if (( status != 0 && status != 1 && status != 3 )); then
    printf 'nilaway exited %d, which is a failure to run:\n' "$status" >&2
    tail -n 20 "$raw" >&2
    rm -f "$raw" "$out"
    return 2
  fi
  sed -E 's/\x1b\[[0-9;]*m//g' "$raw" >"$out"
  rm -f "$raw"
  if [[ ! -s "$out" ]]; then
    printf 'nilaway produced no output at all; the ceiling has nothing to compare\n' >&2
    rm -f "$out"
    return 2
  fi
  stray="$(grep -vE "^(${tab}| *\$)|Potential nil panic detected|^\(Same nil source could also cause" "$out" | head -n 5 || true)"
  if [[ -n "$stray" ]]; then
    printf 'nilaway did not analyse everything; the count would be a lower bound:\n%s\n' "$stray" >&2
    rm -f "$out"
    return 2
  fi
  found="$(grep -c 'Potential nil panic detected' "$out")" || found=0
  rm -f "$out"
  if (( found == 0 )); then
    printf 'nilaway output matched no known shape; the count would be meaningless\n' >&2
    return 2
  fi
  printf '%s' "$found"
}
