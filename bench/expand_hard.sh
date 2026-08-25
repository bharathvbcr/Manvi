#!/bin/bash
# Sole-tenant expansion. Qwen+Ornith share-gpu starved Qwen (0-token 30 min
# timeouts). Ornith one-offs already on disk are kept. Qwen starved cells are
# re-run with --force-starved and no --share-gpu. Frozen full/baseline is
# sequential, one model at a time.
set -euo pipefail
cd "$(dirname "$0")"
PYTHON="${PYTHON:-python3}"
TAG="${TAG:-hard}"
# grid.py honours MH_RESULTS; this script wrote bare `results/` regardless, so
# a redirected run archived and force-overwrote the wrong tree.
RESULTS="${MH_RESULTS:-results}"
LOG="${RESULTS}/expand-${TAG}.log"
QWEN="qwen3.8:27b"
ORNITH="hf.co/ornith-ai/Ornith-1.5-35B-A3B-GGUF:Q8_0"
ONE_OFF="no-verifygate,no-envboot,no-checklist,no-loopbreak,no-outcap,no-groundfs,no-nativetools"
mkdir -p "$RESULTS"

# Exclusive lock. Two expanders over one results tree put both models on the
# GPU at once -- the share-gpu starvation this script exists to undo. mkdir is
# atomic on every filesystem we care about; flock(1) is not on macOS.
LOCK="${RESULTS}/.expand-${TAG}.lock"
acquire_lock() {
  if mkdir "$LOCK" 2>/dev/null; then
    echo $$ > "$LOCK/pid"
    trap 'rm -rf "$LOCK"' EXIT INT TERM
    return 0
  fi
  local holder
  holder="$(cat "$LOCK/pid" 2>/dev/null || true)"
  if [ -n "$holder" ] && kill -0 "$holder" 2>/dev/null; then
    return 1
  fi
  # The holder is gone. A crashed expander must not wedge the tree forever,
  # but a stale lock is still a fact worth printing rather than swallowing.
  echo "[expand] clearing stale lock from pid ${holder:-unknown}" >&2
  rm -rf "$LOCK"
  acquire_lock
}
if ! acquire_lock; then
  echo "[expand] another expander holds ${LOCK} (pid $(cat "$LOCK/pid" 2>/dev/null)); refusing to start a second one" >&2
  exit 3
fi

arm_log() {
  local model="$1"
  echo "${RESULTS}/expand-${TAG}-$(echo "$model" | tr '/:' '__').log"
}

run_arm() {
  local model="$1"
  local configs="$2"
  shift 2
  local slog
  slog="$(arm_log "$model")"
  echo "[expand] $(date -u +%Y-%m-%dT%H:%M:%SZ) SOLE_TENANT arm=$model configs=$configs $*" | tee -a "$LOG" "$slog"
  $PYTHON grid.py --tag "$TAG" --hard --max-steps 0 --max-wall 1800 \
    --models "$model" --configs "$configs" --repeats 5 --seed 0 "$@" \
    2>&1 | tee -a "$slog" | tee -a "$LOG"
}

echo "[expand] $(date -u +%Y-%m-%dT%H:%M:%SZ) sole-tenant recovery tag=$TAG" | tee -a "$LOG"

# Ornith one-offs: skip complete clean cells.
run_arm "$ORNITH" "$ONE_OFF"

# Qwen: rewrite only 0-token timeout artefacts; keep verifygate/envboot.
run_arm "$QWEN" "$ONE_OFF" --force-starved

# Archive the cells THIS tag is about to --force over. The list used to be
# hardcoded to __hard while --force targeted --tag "$TAG", so `TAG=hard2`
# archived one tree and destroyed another.
for model in "$QWEN" "$ORNITH"; do
  slug="$(echo "$model" | tr '/:' '__')"
  for cfg in full baseline; do
    d="${slug}__${cfg}__${TAG}"
    src="${RESULTS}/$d"
    dst="${RESULTS}/${d}-protocol1"
    [ -d "$src" ] || continue
    [ -e "$dst" ] && continue
    # cp -a then rename: an interrupted copy used to leave a partial
    # `-protocol1` that satisfied the existence test on the next run, so the
    # archive was silently skipped and --force destroyed the only copy.
    tmp="${dst}.partial.$$"
    rm -rf "$tmp"
    echo "[expand] archive ${src} -> ${dst}" | tee -a "$LOG"
    cp -a "$src" "$tmp"
    mv "$tmp" "$dst"
  done
done

# Sequential so Qwen is never on the GPU with Ornith resident.
echo "[expand] $(date -u +%Y-%m-%dT%H:%M:%SZ) restarting ollama before frozen full/baseline" | tee -a "$LOG"
sudo -n systemctl restart ollama || true
sleep 8
run_arm "$ORNITH" "full,baseline" --force
run_arm "$QWEN" "full,baseline" --force

$PYTHON compare.py --tag "$TAG" --json-out "${RESULTS}/stats-${TAG}.json" 2>&1 | tee -a "$LOG"
# Explicit output directory. figures.py defaults to paper/figures, so an
# expansion of any tag would otherwise overwrite the paper's committed
# figures with that tag's data. Figures belong beside the stats they were
# rendered from; regenerating the paper's is a deliberate, separate act.
$PYTHON figures.py "${RESULTS}/stats-${TAG}.json" "${RESULTS}/figures-${TAG}" 2>&1 | tee -a "$LOG"
echo "[expand] done $(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$LOG"
