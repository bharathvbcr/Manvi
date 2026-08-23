#!/bin/bash
# Sole-tenant expansion. Qwen+Ornith share-gpu starved Qwen (0-token 30 min
# timeouts). Ornith one-offs already on disk are kept. Qwen starved cells are
# re-run with --force-starved and no --share-gpu. Frozen full/baseline is
# sequential, one model at a time.
set -euo pipefail
cd "$(dirname "$0")"
PYTHON="${PYTHON:-python3}"
TAG="${TAG:-hard}"
LOG="results/expand-${TAG}.log"
QWEN="qwen3.8:27b"
ORNITH="hf.co/ornith-ai/Ornith-1.5-35B-A3B-GGUF:Q8_0"
ONE_OFF="no-verifygate,no-envboot,no-checklist,no-loopbreak,no-outcap,no-groundfs,no-nativetools"
mkdir -p results

arm_log() {
  local model="$1"
  echo "results/expand-${TAG}-$(echo "$model" | tr '/:' '__').log"
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

for d in \
  qwen3.8_27b__full__hard \
  qwen3.8_27b__baseline__hard \
  hf.co_ornith-ai_Ornith-1.5-35B-A3B-GGUF_Q8_0__full__hard \
  hf.co_ornith-ai_Ornith-1.5-35B-A3B-GGUF_Q8_0__baseline__hard
do
  if [ -d "results/$d" ] && [ ! -e "results/${d}-protocol1" ]; then
    echo "[expand] archive results/${d} -> results/${d}-protocol1" | tee -a "$LOG"
    cp -a "results/$d" "results/${d}-protocol1"
  fi
done

# Sequential so Qwen is never on the GPU with Ornith resident.
echo "[expand] $(date -u +%Y-%m-%dT%H:%M:%SZ) restarting ollama before frozen full/baseline" | tee -a "$LOG"
sudo -n systemctl restart ollama || true
sleep 8
run_arm "$ORNITH" "full,baseline" --force
run_arm "$QWEN" "full,baseline" --force

$PYTHON compare.py --tag "$TAG" --json-out "results/stats-${TAG}.json" 2>&1 | tee -a "$LOG"
$PYTHON figures.py "results/stats-${TAG}.json" 2>&1 | tee -a "$LOG"
echo "[expand] done $(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$LOG"
