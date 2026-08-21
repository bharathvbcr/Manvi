#!/bin/bash
# Start the updated expander only after the in-flight expand_hard.sh exits.
# Does not kill the Qwen no-verifygate cell.
set -euo pipefail
cd "$(dirname "$0")"
LOG="results/expand-hard.log"
echo "[wait] $(date -u +%Y-%m-%dT%H:%M:%SZ) waiting for current expand_hard.sh" | tee -a "$LOG"
while pgrep -f "/bin/bash ./expand_hard.sh" >/dev/null 2>&1; do
  sleep 30
done
echo "[wait] $(date -u +%Y-%m-%dT%H:%M:%SZ) prior expander exited; starting updated script" | tee -a "$LOG"
exec ./expand_hard.sh
