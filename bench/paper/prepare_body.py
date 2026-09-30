#!/usr/bin/env python3
"""Adapt the pandoc body of harness_architecture.md for LaTeX.

    prepare_body.py BODY.md {article|tmlr-review|tmlr-preprint}

Every mode: the manuscript writes its captions as bold paragraphs
("**Figure 3.** ...", "**Table 4.** ...") beside the image or table. LaTeX
sees those as ordinary text, so a page break could, and did, strand Figure 1's
caption on the page after its figure. Each one is folded into the pandoc
caption of the float it describes, so the two are typeset as one object;
header.tex suppresses LaTeX's own label, since the number is already in the
caption text.

TMLR modes: the abstract moves from its "## Abstract" section into a YAML
metadata block, rendered in the style's own abstract environment.

tmlr-review also withholds the instrument's commit hash. The repository is
public, so the hash resolves to it: it identifies the author without adding
anything a reviewer needs to judge the results. Any occurrence the
substitution does not cover fails the build rather than shipping.
"""
import re
import sys

MODES = ("article", "tmlr-review", "tmlr-preprint")
HASH = "82e453a"
WITHHELD = "a single fixed commit (identifier withheld for review)"

FIGURE = re.compile(r"^!\[[^\]\n]*\]\(([^)\s]+)\)\n\n(\*\*Figure \d+\.\*\*[^\n]*)$", re.M)
TABLE = re.compile(r"^(\*\*Table \d+\.\*\*[^\n]*)\n\n(?=\|)", re.M)


def attach_captions(text):
    text, n_fig = FIGURE.subn(lambda m: f"![{m.group(2)}]({m.group(1)})", text)
    text, n_tab = TABLE.subn(lambda m: f"Table: {m.group(1)}\n\n", text)
    left = re.findall(r"^\*\*(?:Figure|Table) \d+\.\*\*", text, re.M)
    if left:
        raise SystemExit(f"captions not attached to a float: {left}")
    return text, n_fig, n_tab


def lift_abstract(text):
    m = re.search(r"^## Abstract\n\n(.*?)\n(?=## )", text, re.S | re.M)
    if not m:
        raise SystemExit("no '## Abstract' section to lift into the TMLR abstract")
    block = "".join("  " + line + "\n" for line in m.group(1).strip().splitlines())
    return "---\nabstract: |\n" + block + "---\n\n" + text[:m.start()] + text[m.end():]


def withhold_hash(text):
    text = text.replace(f"commit `{HASH}`", WITHHELD)
    if HASH in text:
        raise SystemExit(f"{HASH} survives outside 'commit `{HASH}`'; anonymisation incomplete")
    return text


def prepare(text, mode):
    if mode not in MODES:
        raise SystemExit(f"unknown mode {mode}")
    text, _, _ = attach_captions(text)
    if mode.startswith("tmlr"):
        text = lift_abstract(text)
        # At full width the graphical abstract misses page 1 by ~0.5 in;
        # tmlr.sty's \flushbottom then stretches the abstract to fill the
        # page. The style's layout is not ours to change, so the figure shrinks.
        text, n = re.subn(r"^(!\[\*\*Figure 1\.\*\*[^\n]*\]\([^)]+\))$", r"\1{width=75%}",
                          text, count=1, flags=re.M)
        if n != 1:
            raise SystemExit("graphical abstract (Figure 1) not found")
    if mode == "tmlr-review":
        text = withhold_hash(text)
    return text


if __name__ == "__main__":
    path, mode = sys.argv[1], sys.argv[2]
    with open(path) as f:
        out = prepare(f.read(), mode)
    with open(path, "w") as f:
        f.write(out)
