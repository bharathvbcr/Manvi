#!/bin/bash
# Registered v2 grid, seeds allocated by hypothesis rather than uniformly.
#
#   full, baseline   n=20  -- H1, the primary confirmatory test, both arms.
#                            Ornith's full-vs-baseline is the weakest contrast
#                            in the design (§9: 60% power even at n=20), so it
#                            gets the full allocation rather than a reduced one.
#   no-outcap        n=20  -- H2 is confirmatory (§2) and shares the Sidak
#                            alpha with H1. Not cut to 5 with the H3 family.
#   the other six    n=5   -- H3, which §5 already labels exploratory.
#
# Decided blind: qwen's baseline cell had not finished when this was set, so no
# delta had been seen for either arm. A §12 deviation, not a data-driven one.
# Phases are chained so the GPU never idles between them, and every phase
# resumes over episodes already on disk.
set -u
cd /home/ubuntu/manvi-bench
export PYTHONUNBUFFERED=1
ORNITH="hf.co/ornith-ai/Ornith-1.5-35B-A3B-GGUF:Q8_0"
SIX="no-envboot,no-verifygate,no-checklist,no-loopbreak,no-groundfs,no-nativetools"

echo "=== P1 qwen full+baseline n=20  $(date -u) ==="
python3 grid.py --models qwen3.8:27b --hard --repeats 20 --seed 0 --tag v2 \
        --configs full,baseline
echo "=== P1 exit=$? ==="

echo "=== P2 qwen no-outcap n=20  $(date -u) ==="
python3 grid.py --models qwen3.8:27b --hard --repeats 20 --seed 0 --tag v2 \
        --configs no-outcap
echo "=== P2 exit=$? ==="

echo "=== P3 qwen six ablations n=5  $(date -u) ==="
python3 grid.py --models qwen3.8:27b --hard --repeats 5 --seed 0 --tag v2 \
        --configs "$SIX"
echo "=== P3 exit=$? ==="

echo "=== P4 ornith full+baseline n=20  $(date -u) ==="
python3 grid.py --models "$ORNITH" --hard --repeats 20 --seed 0 --tag v2 \
        --configs full,baseline
echo "=== P4 exit=$? ==="

echo "=== P5 ornith no-outcap n=20  $(date -u) ==="
python3 grid.py --models "$ORNITH" --hard --repeats 20 --seed 0 --tag v2 \
        --configs no-outcap
echo "=== P5 exit=$? ==="

echo "=== P6 ornith six ablations n=5  $(date -u) ==="
python3 grid.py --models "$ORNITH" --hard --repeats 5 --seed 0 --tag v2 \
        --configs "$SIX"
echo "=== P6 exit=$? ==="
echo "=== ALL RUNS DONE $(date -u) ==="
