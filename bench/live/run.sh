#!/usr/bin/env bash
#
# bench/live/run.sh — the heavy multi-agent benchmark against the live Gemini
# API, with the wire recorded.
#
# It needs a Gemini credential, from either of two places:
#
#   1. exported in the shell that runs it
#        export GEMINI_API_KEY=...
#        ./bench/live/run.sh
#
#   2. or in .env.local at the repository root, which this script sources and
#      .gitignore already covers
#        printf 'GEMINI_API_KEY=%s\n' "$KEY" > .env.local && chmod 600 .env.local
#        ./bench/live/run.sh
#
# The shell wins when both are set, so a one-off key can override the file
# without editing it. The key is never printed, never copied anywhere else, and
# never leaves loopback: the recording proxy forwards the caller's own header
# verbatim to Google and records only request bodies and response streams,
# never headers.
#
# Options, as environment variables:
#   MODEL        model to drive          (default gemini-3.7-flash)
#   FANOUT       max concurrent children (default 6)
#   MAX_STEPS    step ceiling per turn   (default 60)
#   WORKSPACE    where the agent works   (default a fresh scratch repo)
#   NO_PROXY_CAPTURE=1                   talk to Google directly, record nothing
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODEL=${MODEL:-gemini-3.7-flash}
FANOUT=${FANOUT:-6}
MAX_STEPS=${MAX_STEPS:-60}
OUT=${OUT:-$REPO_ROOT/bench/results/live-gemini}
mkdir -p "$OUT"

# The file is read only for names the harness's own credential resolver accepts,
# and only when the shell has not already supplied one. Sourcing it wholesale
# would let an unrelated line in it change how this benchmark runs.
ENV_FILE="${ENV_FILE:-$REPO_ROOT/.env.local}"
if [ -f "$ENV_FILE" ]; then
  # Parsed in the shell rather than with sed, because BSD sed on macOS has no
  # \+ or \? and silently matched nothing -- which reads exactly like a file
  # with no key in it. A credential reader that fails quietly is the worst kind.
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line#"${line%%[![:space:]]*}"}"          # strip leading space
    case "$line" in ''|'#'*) continue;; esac
    line="${line#export }"
    name="${line%%=*}"
    value="${line#*=}"
    case "$name" in
      GEMINI_API_KEY|GOOGLE_API_KEY) ;;
      *) continue;;                                   # only names the resolver accepts
    esac
    value="${value%\"}"; value="${value#\"}"          # optional surrounding quotes
    value="${value%\'}"; value="${value#\'}"
    [ -z "$value" ] && continue
    # The shell wins, so a one-off key overrides the file without editing it.
    [ -n "$(eval "printf %s \"\${$name:-}\"")" ] && continue
    export "$name=$value"
    from_file=1
  done < "$ENV_FILE"
  # Said out loud, because "which key did that run use" is the first question
  # asked of a result that looks wrong.
  [ -n "${from_file:-}" ] && echo "== credential read from $ENV_FILE =="
fi

if [ -z "${GEMINI_API_KEY:-}" ] && [ -z "${GOOGLE_API_KEY:-}" ]; then
  cat >&2 <<MSG
No Gemini credential found. Set one of GEMINI_API_KEY or GOOGLE_API_KEY, either:

  export GEMINI_API_KEY=...                      # this shell only
  printf 'GEMINI_API_KEY=%s\n' "\$KEY" > $ENV_FILE && chmod 600 $ENV_FILE

The repository's .gitignore already covers .env.local. Get a key from
https://console.cloud.google.com/apis/credentials (Generative Language API).
MSG
  exit 1
fi

echo "== building manvi and the recording proxy =="
go -C "$REPO_ROOT/manvi" build -o "$OUT/manvi" ./cmd/manvi || exit 1
go -C "$REPO_ROOT/bench/live" build -o "$OUT/geminiproxy" . || exit 1

# --- the workspace the agent actually works in -------------------------------
# A fresh tree by default. --yolo turns every gate off, including the repository
# boundary, so pointing this at a tree you care about is a deliberate choice.
if [ -n "${WORKSPACE:-}" ]; then
  WORK="$WORKSPACE"
else
  WORK="$OUT/workspace"
  rm -rf "$WORK"; mkdir -p "$WORK/src"
  cat > "$WORK/go.mod" <<'EOF'
module benchwork

go 1.24
EOF
  cat > "$WORK/src/calc.go" <<'EOF'
package src

// Search returns the index of target in a sorted slice, or -1.
func Search(xs []int, target int) int {
	lo, hi := 0, len(xs)
	for lo < hi {
		mid := (lo + hi) / 2
		if xs[mid] == target {
			return mid
		}
		if xs[mid] < target {
			lo = mid
		} else {
			hi = mid
		}
	}
	return -1
}
EOF
  cat > "$WORK/src/calc_test.go" <<'EOF'
package src

import "testing"

func TestSearch(t *testing.T) {
	xs := []int{1, 3, 5, 7, 9}
	for i, x := range xs {
		if got := Search(xs, x); got != i {
			t.Errorf("Search(%d) = %d, want %d", x, got, i)
		}
	}
	if got := Search(xs, 4); got != -1 {
		t.Errorf("Search(4) = %d, want -1", got)
	}
}
EOF
  cat > "$WORK/README.md" <<'EOF'
# benchwork

A deliberately broken binary search: the loop never narrows on the low side,
so a miss spins forever rather than returning -1.
EOF
  (cd "$WORK" && git init -q && git add -A && \
     git -c user.email=b@b -c user.name=b commit -qm seed) >/dev/null 2>&1
fi
echo "== workspace: $WORK =="

# --- the recording proxy ------------------------------------------------------
BASE_URL="${UPSTREAM:-https://generativelanguage.googleapis.com}/v1beta"
PROXY_PID=""
if [ "${NO_PROXY_CAPTURE:-0}" != "1" ]; then
  PROXY_CAPTURE="$OUT/gemini-wire.log" PROXY_ADDR=127.0.0.1:8899 \
    PROXY_UPSTREAM="${UPSTREAM:-https://generativelanguage.googleapis.com}" \
    "$OUT/geminiproxy" > "$OUT/proxy.log" 2>&1 &
  PROXY_PID=$!
  sleep 1
  BASE_URL="http://127.0.0.1:8899/v1beta"
fi
cleanup() { [ -n "$PROXY_PID" ] && kill "$PROXY_PID" 2>/dev/null; }
trap cleanup EXIT

export MANVI_LLM_PROVIDER_DEFAULT=gemini
export MANVI_MODEL="$MODEL"
export MANVI_LLM_GEMINI_BASE_URL="$BASE_URL"
export MANVI_LLM_EFFORT=high
export MANVI_AGENTS_MAX_FANOUT="$FANOUT"
export MANVI_AGENTS_MAX_SPAWN_DEPTH=2
export MANVI_SUBAGENTS_DYNAMIC_ENABLED=true

echo
echo "== live wire contract =="
(cd "$WORK" && "$OUT/manvi" probe gemini --model "$MODEL" 2>&1) | tee "$OUT/probe.txt"

# Each scenario names a feature of the harness and a prompt that forces it.
# The fan-out prompts are the interesting ones: a spawn call carrying six
# labelled prompts is a large argument object, which is the payload most likely
# to be streamed as fragments rather than as one inline object.
run_case() {
  local name="$1"; shift
  local prompt="$1"; shift
  echo
  echo "== $name =="
  local start=$SECONDS
  (cd "$WORK" && "$OUT/manvi" run --yolo -p "$prompt" \
      --max-steps "$MAX_STEPS" --timeout 15m 2>&1) | tee "$OUT/$name.txt"
  local code=${PIPESTATUS[0]}
  echo "-- $name exit=$code elapsed=$((SECONDS-start))s" | tee -a "$OUT/summary.txt"
}

: > "$OUT/summary.txt"

run_case "01-fanout-read" \
  "Fan out $FANOUT read-only sub-agents with devcouncil_spawn_subagents, one per area of this \
repository, each with a distinct multi-sentence prompt describing exactly what to inspect and \
what to report back. Wait for all of them, then write one consolidated summary of what they found."

run_case "02-dynamic-roles" \
  "Define two sub-agent types with devcouncil_define_subagent: a read-only 'auditor' that may not \
reach MCP tools, and a writing 'fixer'. Then invoke three auditors concurrently with \
devcouncil_invoke_subagent to review src/calc.go, and report what each one said."

run_case "03-fix-with-verification" \
  "src/calc.go has a bug: Search never narrows the low bound, so a miss loops forever. \
Reproduce it, fix it, and run 'go test ./...' to prove the fix. Use the native tools."

run_case "04-navigation-and-graph" \
  "Build the navigation index if it is not built, then use devcouncil_graph_query and \
devcouncil_graph_context to describe what Search depends on and what depends on it. \
Delegate the graph reads to two sub-agents and combine their answers."

run_case "05-lease-and-scope" \
  "List the tasks that are ready with devcouncil_next_task. If there are none, say so plainly \
rather than inventing one, then run devcouncil_verify_task and report exactly what it said."

echo
echo "===================== SUMMARY ====================="
cat "$OUT/summary.txt"

if [ -f "$OUT/gemini-wire.log" ]; then
  echo
  echo "===================== WIRE FACTS ====================="
  python3 - "$OUT/gemini-wire.log" <<'PY'
import json, re, sys, collections
raw = open(sys.argv[1], errors="replace").read()

# The one question the harness cannot answer about itself: which shape the live
# server uses for a streamed tool call's arguments.
inline = fragments = 0
for m in re.finditer(r'"arguments"\s*:\s*(.)', raw):
    if m.group(1) == '"': fragments += 1
    elif m.group(1) == '{': inline += 1
print(f"tool-call argument frames: {inline} inline object(s), {fragments} JSON-string fragment(s)")
if fragments:
    print("  -> the server DOES stream arguments as string fragments; the adapter must reassemble them.")
if inline and not fragments:
    print("  -> every call arrived as one inline object at the sizes this run produced.")

# The capture interleaves this proxy's own "===== ... =====" markers into the
# byte stream, and a marker can land mid-frame. Only names that look like an
# actual event name are counted, so the proxy's own bookkeeping is not
# reported back as an event the adapter has never seen.
events = collections.Counter(
    n for n in re.findall(r'^event:\s*([A-Za-z][\w.]*)\s*$', raw, re.M))
print("SSE events seen:", dict(events))

unknown = set(events) - {
    "interaction.created","interaction.in_progress","interaction.completed",
    "interaction.requires_action","interaction.status_update","error",
    "step.start","step.delta","step.stop","done"}
print("events the adapter has no case for:", sorted(unknown) or "none")

reqs = [json.loads(b) for b in re.findall(r'=====\n(\{.*?\})\n===== RESPONSE', raw, re.S)] if False else []
stores = re.findall(r'"store":(true|false)', raw)
# store must be true: this API rejects every function_result when it is false,
# so a run with store=false cannot get past its first tool call. What must stay
# absent is any back-reference to a stored interaction.
print("store=true:", stores.count("true"), "| store=false (must be 0):", stores.count("false"))
resumed = raw.count("previous_interaction_id")
print("requests resuming a stored interaction (must be 0):", resumed)
levels = collections.Counter(re.findall(r'"thinking_level":"(\w+)"', raw))
print("thinking_level sent:", dict(levels))
statuses = collections.Counter(re.findall(r'"status":"(\w+)"', raw))
print("interaction statuses:", dict(statuses))
unmapped = set(statuses) - {"completed","requires_action","incomplete","max_tokens","failed","in_progress"}
print("statuses the adapter does not map:", sorted(unmapped) or "none")
PY
  echo
  echo "full wire capture: $OUT/gemini-wire.log"
fi
echo "all output under: $OUT"
