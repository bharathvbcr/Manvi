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


class BuildError(Exception):
    """A body this script cannot prepare safely. An Exception, not SystemExit,
    so a caller testing the checks sees a failure rather than an exit."""
HASH = "82e453a"
WITHHELD = "a single fixed commit (identifier withheld for review)"

FIGURE = re.compile(r"^!\[[^\]\n]*\]\(([^)\s]+)\)\n\n(\*\*Figure \d+\.\*\*[^\n]*)$", re.M)
TABLE = re.compile(r"^(\*\*Table \d+\.\*\*[^\n]*)\n\n(?=\|)", re.M)


def attach_captions(text):
    text, n_fig = FIGURE.subn(lambda m: f"![{m.group(2)}]({m.group(1)})", text)
    text, n_tab = TABLE.subn(lambda m: f"Table: {m.group(1)}\n\n", text)
    left = re.findall(r"^\*\*(?:Figure|Table) \d+\.\*\*", text, re.M)
    if left:
        raise BuildError(f"captions not attached to a float: {left}")
    return text, n_fig, n_tab


def lift_abstract(text):
    m = re.search(r"^## Abstract\n\n(.*?)\n(?=## )", text, re.S | re.M)
    if not m:
        raise BuildError("no '## Abstract' section to lift into the TMLR abstract")
    block = "".join("  " + line + "\n" for line in m.group(1).strip().splitlines())
    return "---\nabstract: |\n" + block + "---\n\n" + text[:m.start()] + text[m.end():]


def withhold_hash(text):
    text = text.replace(f"commit `{HASH}`", WITHHELD)
    if HASH in text:
        raise BuildError(f"{HASH} survives outside 'commit `{HASH}`'; anonymisation incomplete")
    return text


def anonymise(text):
    """What the review copy must not carry besides the hash.

    TMLR's template: acknowledgments are added "only ... once your submission
    is accepted and deanonymized", so the section is dropped. The code is
    pointed at through the supplementary material, not "this repository".
    """
    text = withhold_hash(text)
    m = re.search(r"^## Acknowledgments\n.*?(?=^## )", text, re.S | re.M)
    if not m:
        raise BuildError("no '## Acknowledgments' section to withhold")
    text = text[:m.start()] + text[m.end():]
    old = "live in the `bench/` directory of this repository"
    if text.count(old) != 1:
        raise BuildError(f"expected one '{old}' to redirect to the supplementary material")
    text = text.replace(old, "are in the supplementary material, under `bench/`")
    if "this repository" in text:
        raise BuildError("'this repository' survives in the review copy")
    return text


def prepare(text, mode):
    if mode not in MODES:
        raise BuildError(f"unknown mode {mode}")
    text, _, _ = attach_captions(text)
    if mode.startswith("tmlr"):
        text = lift_abstract(text)
    if mode == "tmlr-review":
        text = anonymise(text)
    return text


if __name__ == "__main__":
    path, mode = sys.argv[1], sys.argv[2]
    with open(path) as f:
        try:
            out = prepare(f.read(), mode)
        except BuildError as e:
            raise SystemExit(f"prepare_body: {e}")
    with open(path, "w") as f:
        f.write(out)
