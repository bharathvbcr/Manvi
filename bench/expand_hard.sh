#!/bin/bash
# Qwen ‖ Ornith on the GH200. Pass/fail and tokens are the metrics; wall-clock
# is not. --share-gpu keeps both weights resident (one peer). Each arm is a
# separate grid.py so two decode streams can overlap. Skip-existing, so a
# mid-ladder restart does not redo finished episodes.
set -euo pipefail
cd "$(dirname "$0")"
PYTHON="${PYTHON:-python3}"
TAG="${TAG:-hard}"
LOG="results/expand-${TAG}.log"
QWEN="qwen3.8:27b"
ORNITH="hf.co/ornith-ai/Ornith-1.5-35B-A3B-GGUF:Q8_0"
ONE_OFF="no-verifygate,no-envboot,no-checklist,no-loopbreak,no-outcap,no-groundfs,no-nativetools"
# One-shot: Qwen cells whose episodes are the 300s HTTP-timeout artefact.
QWEN_FORCE_CONFIGS="${QWEN_FORCE_CONFIGS:-}"
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
  echo "[expand] $(date -u +%Y-%m-%dT%H:%M:%SZ) arm=$model configs=$configs $*" | tee -a "$LOG" "$slog"
  $PYTHON grid.py --tag "$TAG" --hard --max-steps 0 --max-wall 1800 --share-gpu \
    --models "$model" --configs "$configs" --repeats 5 --seed 0 "$@" \
    2>&1 | tee -a "$slog" | tee -a "$LOG"
}

echo "[expand] $(date -u +%Y-%m-%dT%H:%M:%SZ) PARALLEL share-gpu qwen || ornith tag=$TAG" | tee -a "$LOG"

run_arm "$ORNITH" "$ONE_OFF" &
pid_o=$!
if [ -n "$QWEN_FORCE_CONFIGS" ]; then
  echo "[expand] forcing Qwen configs: $QWEN_FORCE_CONFIGS" | tee -a "$LOG"
  run_arm "$QWEN" "$QWEN_FORCE_CONFIGS" --force
fi
run_arm "$QWEN" "$ONE_OFF" &
pid_q=$!
fail=0
wait "$pid_q" || fail=1
wait "$pid_o" || fail=1
if [ "$fail" -ne 0 ]; then
  echo "[expand] one-off grid failed" | tee -a "$LOG"
  exit 1
fi

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

run_arm "$QWEN" "full,baseline" --force &
pid_q=$!
run_arm "$ORNITH" "full,baseline" --force &
pid_o=$!
fail=0
wait "$pid_q" || fail=1
wait "$pid_o" || fail=1
if [ "$fail" -ne 0 ]; then
  echo "[expand] forced full/baseline failed" | tee -a "$LOG"
  exit 1
fi

$PYTHON compare.py --tag "$TAG" --json-out "results/stats-${TAG}.json" 2>&1 | tee -a "$LOG"
$PYTHON figures.py "results/stats-${TAG}.json" 2>&1 | tee -a "$LOG"
echo "[expand] done $(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$LOG"
