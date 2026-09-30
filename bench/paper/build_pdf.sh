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

# Citations: --natbib turns [@key] into \citep and @key into \citet. pandoc is
# never given --bibliography, so it appends no \bibliography of its own; the one
# \bibliography{references} is the body's, where the References section stands.
# The style belongs to the template that loads natbib: plainnat (author-year)
# from pandoc's default template for the article, tmlr from tmlr/template.tex.
READ=(--from=markdown+tex_math_single_backslash+pipe_tables+raw_tex
      --natbib --resource-path=.:figures)

if [ "$MODE" = article ]; then
  OUT=harness_architecture
  ENGINE=-xelatex
  PANDOC=(
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
    -V biblio-style=plainnat -V natbiboptions=authoryear,round
    -H header.tex
  )
else
  # TMLR: tmlr.sty is used unmodified, through tmlr/template.tex.
  ENGINE=-pdf
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
  PANDOC=(
    "${READ[@]}"
    --template=tmlr/template.tex
    -V title="$TITLE"
    -V month=09 -V year=2026
    "${OPT[@]+"${OPT[@]}"}"
    -H header.tex
  )
fi

fail() { echo "build_pdf.sh ($MODE): $*" >&2; exit 1; }

pandoc "${PANDOC[@]}" -s -o "$OUT.tex" "$BUILD/body.md"
n_bib=$(grep -c '^[^%]*\\bibliography{' "$OUT.tex" || true)
n_sty=$(grep -c '^[^%]*\\bibliographystyle{' "$OUT.tex" || true)
[ "$n_bib" = 1 ] || fail "$OUT.tex has $n_bib \\bibliography commands, expected exactly 1"
[ "$n_sty" = 1 ] || fail "$OUT.tex has $n_sty \\bibliographystyle commands, expected exactly 1"

# pdflatex/xelatex + bibtex + reruns, in a per-mode build directory. The .tex is
# compiled from here so figures/ resolves; bibtex finds references.bib and
# tmlr.bst through BIBINPUTS/BSTINPUTS (it does not read TEXINPUTS).
command -v latexmk >/dev/null || fail "latexmk not found; it runs bibtex, which pandoc does not"
TEXOUT="$BUILD/latex-$MODE"
JOB=$(basename "$OUT")
mkdir -p "$TEXOUT"
TEXINPUTS="$PWD/tmlr:" BIBINPUTS="$PWD:" BSTINPUTS="$PWD/tmlr:" \
  latexmk "$ENGINE" -interaction=nonstopmode -halt-on-error -outdir="$TEXOUT" \
  "$OUT.tex" > "$TEXOUT/latexmk.out" 2>&1 \
  || { tail -n 30 "$TEXOUT/latexmk.out" >&2; fail "latexmk failed; see $TEXOUT/latexmk.out and $TEXOUT/$JOB.log"; }

# Fail closed: a citation BibTeX could not resolve still yields a PDF, with
# "(?)" where the author-year label should be.
LOG="$TEXOUT/$JOB.log"
[ -s "$LOG" ] || fail "no LaTeX log at $LOG"
[ -s "$TEXOUT/$JOB.blg" ] || fail "bibtex did not run (no $TEXOUT/$JOB.blg)"
if grep -E 'Citation .* undefined|There were undefined references' "$LOG" >&2; then
  fail "undefined citations or references (see $LOG)"
fi
if grep -E "I didn't find a database entry|error message" "$TEXOUT/$JOB.blg" >&2; then
  fail "bibtex reported errors (see $TEXOUT/$JOB.blg)"
fi
pdftotext "$TEXOUT/$JOB.pdf" "$TEXOUT/$JOB.txt"
if grep -n -F -e '(?)' -e '[?]' "$TEXOUT/$JOB.txt" >&2; then
  fail "the PDF prints an unresolved citation"
fi
if [ "$MODE" = tmlr-review ] &&
   grep -n -i -E 'vaddaram|bharath|gmail|82e453a|this repository' "$TEXOUT/$JOB.txt" >&2; then
  fail "the review PDF identifies the author"
fi
cp "$TEXOUT/$JOB.pdf" "$OUT.pdf"
echo "wrote $OUT.tex and $OUT.pdf"
