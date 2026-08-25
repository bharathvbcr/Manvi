#!/bin/bash
# Start the updated expander only after the in-flight expand_hard.sh exits.
#
# This used to detect the running expander with
#     pgrep -f "/bin/bash ./expand_hard.sh"
# which matches one exact argv. `bash expand_hard.sh`, `bash ./expand_hard.sh`,
# an absolute path, nohup, tmux and systemd all miss it -- and on a miss the
# loop fell straight through to `exec ./expand_hard.sh`, putting a SECOND
# expander on the same results tree with two models racing the GPU. That is
# precisely the share-gpu starvation the expander exists to undo, so the
# failure mode of the guard was to cause the thing it guarded against.
#
# It now waits on the expander's own lock, which is the same fact the expander
# itself uses to refuse a second copy. A guard and the thing it guards must
# agree on what "already running" means.
set -euo pipefail
cd "$(dirname "$0")"
TAG="${TAG:-hard}"
RESULTS="${MH_RESULTS:-results}"
LOG="${RESULTS}/expand-${TAG}.log"
LOCK="${RESULTS}/.expand-${TAG}.lock"
mkdir -p "$RESULTS"

held() {
  local pid
  [ -d "$LOCK" ] || return 1
  pid="$(cat "$LOCK/pid" 2>/dev/null || true)"
  # A lock with no live owner is stale, not held. expand_hard.sh clears those
  # itself; reporting one as held here would wait forever on a dead process.
  [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null
}

if held; then
  echo "[wait] $(date -u +%Y-%m-%dT%H:%M:%SZ) waiting for expander pid $(cat "$LOCK/pid")" | tee -a "$LOG"
  while held; do
    sleep 30
  done
  echo "[wait] $(date -u +%Y-%m-%dT%H:%M:%SZ) prior expander exited" | tee -a "$LOG"
else
  echo "[wait] $(date -u +%Y-%m-%dT%H:%M:%SZ) no expander holds ${LOCK}" | tee -a "$LOG"
fi

echo "[wait] $(date -u +%Y-%m-%dT%H:%M:%SZ) starting updated script" | tee -a "$LOG"
exec ./expand_hard.sh
