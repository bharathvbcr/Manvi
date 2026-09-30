#!/bin/bash
# Build the manuscript from harness_architecture.md.
#
#   build_pdf.sh [article]      harness_architecture.{tex,pdf}: the repository
#                               copy (pandoc article, xelatex, named author)
#   build_pdf.sh tmlr-review    tmlr/harness_architecture.review.{tex,pdf}:
#                               TMLR style, anonymous, for submission
#   build_pdf.sh tmlr-preprint  tmlr/harness_architecture.preprint.{tex,pdf}:
#                               TMLR style, de-anonymised, for arXiv
#
# The manuscript is the source of truth; this script never edits it. It works
# on a copy in a build directory, rewriting only what LaTeX cannot consume:
# SVG figure references become the PNGs that svg2png.py rasterises, and the
# leading H1 becomes document metadata rather than a section heading.
set -euo pipefail
cd "$(dirname "$0")"

MODE=${1:-article}
case "$MODE" in
  article|tmlr-review|tmlr-preprint) ;;
  *) echo "unknown mode: $MODE (article | tmlr-review | tmlr-preprint)" >&2; exit 2 ;;
esac

SRC=harness_architecture.md
BUILD=.build
mkdir -p "$BUILD"

# Figures are regenerated from the reports they belong to, then rasterised.
# The pass-rate chart spans all three arms (it carries its own not-comparable
# caveat); the graphical abstract shows the REGISTERED two-arm family, because
# its confirmatory level must match Table 6 rather than the six-test pooled
# sensitivity analysis reported in §5.7.
( cd .. && python3 figures.py paper/stats-all3.json paper/figures ) >/dev/null
( cd .. && python3 -c "
import json, figures
r = json.load(open('paper/stats-v2.json'))
figures.graphical_abstract_svg(r, 'paper/figures/graphical_abstract.generated.svg', source='stats-v2.json')
" ) >/dev/null
python3 svg2png.py figures/*.svg >/dev/null

TITLE=$(sed -n '1s/^# //p' "$SRC")
# SVG -> PNG (LaTeX cannot read SVG here), and drop image alt text: the
# manuscript writes its own "**Figure N.**" captions, so pandoc's auto-caption
# would print a second, differently-numbered one under every figure.
tail -n +2 "$SRC" \
  | sed 's|\(figures/[A-Za-z0-9_.-]*\)\.svg|\1.png|g' \
  | sed 's|!\[[^]]*\](|![](|g' > "$BUILD/body.md"

# Captions become real float captions (all modes); TMLR modes also lift the
# abstract, and the review copy withholds the commit hash. See prepare_body.py.
python3 prepare_body.py "$BUILD/body.md" "$MODE"

READ=(--from=markdown+tex_math_single_backslash+pipe_tables+raw_tex
      --resource-path=.:figures)

if [ "$MODE" = article ]; then
  COMMON=(
    "${READ[@]}"
    --metadata title="$TITLE"
    --metadata author="BHARATH CHANDRA VADDARAM"
    --metadata date="29 September 2026"
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
  exit 0
fi

# TMLR: tmlr.sty is used unmodified, through tmlr/template.tex.
if [ "$MODE" = tmlr-review ]; then
  OUT=tmlr/harness_architecture.review
  OPT=()
else
  OUT=tmlr/harness_architecture.preprint
  OPT=(-V tmlr-option=preprint
       -V author="Bharath Chandra Vaddaram"
       -V email="bharath.vbcr@gmail.com"
       -V affiliation="Independent Researcher")
fi
TMLR=(
  "${READ[@]}"
  --template=tmlr/template.tex
  -V title="$TITLE"
  -V month=09 -V year=2026
  "${OPT[@]+"${OPT[@]}"}"
  -H header.tex
)
pandoc "${TMLR[@]}" -s -o "$OUT.tex" "$BUILD/body.md"
TEXINPUTS="$PWD/tmlr:" pandoc "${TMLR[@]}" --pdf-engine=pdflatex -o "$OUT.pdf" "$BUILD/body.md"
echo "wrote $OUT.tex and $OUT.pdf"
