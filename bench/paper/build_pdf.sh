#!/bin/bash
# Build harness_architecture.{tex,pdf} from the markdown manuscript.
#
# The manuscript is the source of truth; this script never edits it. It works
# on a copy in a build directory, rewriting only what LaTeX cannot consume:
# SVG figure references become the PNGs that svg2png.py rasterises, and the
# leading H1 becomes document metadata rather than a section heading.
set -euo pipefail
cd "$(dirname "$0")"

SRC=harness_architecture.md
BUILD=.build
mkdir -p "$BUILD"

python3 svg2png.py figures/*.svg >/dev/null

TITLE=$(sed -n '1s/^# //p' "$SRC")
# SVG -> PNG (LaTeX cannot read SVG here), and drop image alt text: the
# manuscript writes its own "**Figure N.**" captions, so pandoc's auto-caption
# would print a second, differently-numbered one under every figure.
tail -n +2 "$SRC" \
  | sed 's|\(figures/[A-Za-z0-9_.-]*\)\.svg|\1.png|g' \
  | sed 's|!\[[^]]*\](|![](|g' > "$BUILD/body.md"

COMMON=(
  --from=markdown+tex_math_single_backslash+pipe_tables+raw_tex
  --metadata title="$TITLE"
  --metadata author="BHARATH CHANDRA VADDARAM"
  --metadata date="29 August 2026"
  --resource-path=.:figures
  --toc --toc-depth=2
  --number-sections=false
  -V documentclass=article
  -V papersize=a4
  -V geometry:margin=2.2cm
  -V fontsize=10pt
  -V colorlinks=true
  -V linkcolor=black -V urlcolor=blue -V toccolor=black
  -H header.tex
)

pandoc "${COMMON[@]}" -s -o harness_architecture.tex "$BUILD/body.md"
pandoc "${COMMON[@]}" --pdf-engine=xelatex -o harness_architecture.pdf "$BUILD/body.md"
echo "wrote harness_architecture.tex and harness_architecture.pdf"
