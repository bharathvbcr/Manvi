#!/usr/bin/env python3
"""Render paper figures from a compare.py --json-out file. Stdlib only.

Every figure here is derived from the stats JSON, so a regenerated figure can
never disagree with the tables. Hand-drawn SVGs with literal numbers in them
went stale once already: the old repeat_deltas.svg carried protocol-1 values
long after the frozen re-run replaced them.

That guarantee only holds for numbers actually read out of the report, and it
did not hold for the captions. Two of them were literals -- "Every interval
crosses zero" and "40 episodes per cell (8 tasks x 5 pinned seeds)" -- and both
survived injecting a detectable interaction into the data and re-rendering. A
caption is a claim about the figure; it is derived here, or it is not written.
"""
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "paper", "figures")

FONT = 'font-family="Helvetica, Arial, sans-serif"'
QWEN, ORNITH = "#2f6fed", "#c45c26"
# A third arm needs a third colour. Everything not Qwen used to render in the
# Ornith brown, so a three-arm chart drew two different models identically.
THIRD = "#2e8b57"
PALETTE = (QWEN, ORNITH, THIRD, "#7d3c98", "#b8860b")

# Serving-provider prefixes, mirroring mh.runtime.API_PREFIXES. Kept as a
# literal so figures.py stays importable without the harness package.
_PROVIDERS = ("cerebras:",)


def _short(model):
    """Display name for an arm: the model, never the provider.

    `cerebras:gpt-oss-120b` used to render as "cerebras" -- the split on ":"
    that strips an ollama quantisation tag also strips an API provider prefix,
    leaving the vendor where the model should be.
    """
    name = model.split("/")[-1]
    for pref in _PROVIDERS:
        if name.lower().startswith(pref):
            return name[len(pref):][:18]
    return name.split(":")[0][:18]


def _colour_for(model, order=()):
    """Stable colour per arm.

    `order` is the arms present in this figure; a model in it takes its
    palette slot, so three arms get three colours. Falls back to the original
    two-colour rule when the caller does not pass an ordering.
    """
    if order:
        try:
            return PALETTE[list(order).index(model) % len(PALETTE)]
        except ValueError:
            pass
    return QWEN if "qwen" in model.lower() else ORNITH


def _esc(s):
    return (str(s).replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;"))


def _write(path, parts):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write("\n".join(parts) + "\n")
    print("wrote", path)


def _plural(n, one, many=None):
    return one if n == 1 else (many or one + "s")


def _units(span, one, many=None):
    """Plural for a `_span` string: "1" is singular, "6-8" and "8" are not."""
    return _plural(1 if span == "1" else 2, one, many)


def _span(values, fmt="{:g}"):
    """"8" for a constant, "6-8" for a range, "?" for nothing.

    A caption that says "8 tasks" when the cells hold 6 and 8 is a false
    statement about the figure it labels, and a caption that says it
    unconditionally is a false statement waiting to happen.
    """
    vals = sorted({v for v in values if v is not None})
    if not vals:
        return "?"
    if len(vals) == 1:
        return fmt.format(vals[0])
    return f"{fmt.format(vals[0])}–{fmt.format(vals[-1])}"


def episode_caption(report):
    """"40 episodes per cell (8 tasks x 5 pinned seeds)", read off the report.

    Every number here comes from the cells and per_task blocks, so a grid with
    a different shape -- or a ragged one -- relabels itself instead of
    inheriting the shape this study happened to run.
    """
    cells = report.get("cells", {})
    per_task = report.get("per_task", {})
    if not cells:
        return "No cells in this report."
    n_ep = _span(c.get("n") for c in cells.values())
    n_rep = _span(c.get("n_repeats") for c in cells.values())
    n_task = _span(len(per_task[k]) for k in cells if k in per_task)
    starved = sum(c.get("n_starved") or 0 for c in cells.values())
    tail = (f" {starved} starved {_plural(starved, 'episode')} excluded."
            if starved else "")
    return (f"{n_ep} {_units(n_ep, 'episode')} per cell "
            f"({n_task} {_units(n_task, 'task')} &#215; "
            f"{n_rep} pinned {_units(n_rep, 'seed')}).{tail}")


def interaction_caption(report, rows):
    """Say what the plotted intervals do, counted from the plotted intervals.

    `rows` is the (name, entry) list actually being drawn, so the caption
    cannot describe a different set than the one on the page.
    """
    n = len(rows)
    above = sum(1 for _, v in rows if v["lo"] > 0)
    below = sum(1 for _, v in rows if v["hi"] < 0)
    excl = above + below
    if not n:
        return "No interaction contrasts in this report."
    if excl == 0:
        crossing = (f"All {n} intervals cross zero." if n > 1
                    else "The interval crosses zero.")
    else:
        parts = []
        if above:
            parts.append(f"{above} entirely above")
        if below:
            parts.append(f"{below} entirely below")
        crossing = (f"{excl} of {n} {_plural(n, 'interval')} "
                    f"{_plural(excl, 'excludes', 'exclude')} zero "
                    f"({', '.join(parts)}); {n - excl} "
                    f"{_plural(n - excl, 'crosses', 'cross')} it.")
    mult = report.get("multiplicity") or {}
    fwer = mult.get("fwer_at_measured_coverage",
                    mult.get("fwer_at_nominal_coverage"))
    tail = ""
    if excl and fwer is not None and mult.get("n_intervals"):
        tail = (f" Across the {mult['n_intervals']}-interval ladder, "
                f"P(&#8805;1 excludes zero | global null) = "
                f"{100 * fwer:.0f}%.")
    return ("Positive is the direction the weak-vs-strong hypothesis predicts. "
            + crossing + tail)


def _source_note(source):
    return ("Generated from " + _esc(source) + "."
            if source else "Generated from the stats report.")


# --- Figure 3: pass rate per cell -------------------------------------------

def _bar_y(pct, top=90, bottom=300, full=100.0):
    return bottom - (pct / full) * (bottom - top)


def pass_rates_svg(cells, path, subtitle=None, degenerate=()):
    """cells: list of (model, config, mean, lo, hi). Grouped by model, coloured by model.

    `subtitle` is the derived shape caption; `degenerate` names the (model,
    config) cells whose interval carries no width, which are drawn as an open
    marker rather than as a confident zero-length error bar.
    """
    degenerate = set(degenerate)
    subtitle = subtitle or "Shape not available from this report."
    # Width follows the cell count. It was fixed at 760 for the eighteen cells
    # of a two-arm grid; a three-arm pool has twenty-seven and overran the
    # canvas, and the rasteriser refused the truncated result rather than
    # writing a chart with its first bars sliced off.
    n_cells = max(len(cells), 1)
    top, bot, axis_l = 90, 300, 80
    span = max(640, int(36 * n_cells))
    axis_r = axis_l + span
    w, h = axis_r + 40, 380
    mid = w // 2
    parts = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{w}" height="{h}" '
        f'viewBox="0 0 {w} {h}" {FONT}>',
        f'<rect width="{w}" height="{h}" fill="#fff"/>',
        f'<text x="{mid}" y="26" text-anchor="middle" font-size="14" font-weight="600" '
        f'fill="#111">Pass rate by (model, configuration), bootstrap 95% CI</text>',
        f'<text x="{mid}" y="44" text-anchor="middle" font-size="11" fill="#555">'
        f'{subtitle}</text>',
    ]
    for t in (0, 25, 50, 75, 100):
        yy = _bar_y(t, top, bot)
        parts.append(f'<line x1="{axis_l}" y1="{yy:.1f}" x2="{axis_r}" y2="{yy:.1f}" stroke="#eee"/>')
        parts.append(f'<text x="{axis_l-8}" y="{yy+4:.1f}" text-anchor="end" font-size="10" fill="#444">{t}</text>')
    parts.append(f'<text x="30" y="{(top+bot)//2}" transform="rotate(-90 30 {(top+bot)//2})" font-size="11" fill="#333">pass rate (%)</text>')
    parts.append(f'<line x1="{axis_l}" y1="{bot}" x2="{axis_r}" y2="{bot}" stroke="#222"/>')
    parts.append(f'<line x1="{axis_l}" y1="{top}" x2="{axis_l}" y2="{bot}" stroke="#222"/>')

    n = n_cells
    gap = (span - 20) / n
    arm_order = []
    for _m, *_r in cells:
        if _m not in arm_order:
            arm_order.append(_m)
    width = max(6.0, gap * 0.66)
    prev_model = None
    for i, (model, cfg, mean, lo, hi) in enumerate(cells):
        x = axis_l + 12 + i * gap
        cx = x + width / 2
        if prev_model is not None and model != prev_model:
            sep = x - gap * 0.18
            parts.append(f'<line x1="{sep:.1f}" y1="{top}" x2="{sep:.1f}" y2="{bot+26}" stroke="#bbb" stroke-dasharray="2,3"/>')
        prev_model = model
        colour = _colour_for(model, arm_order)
        y, ylo, yhi = _bar_y(100*mean, top, bot), _bar_y(100*lo, top, bot), _bar_y(100*hi, top, bot)
        opacity = "1" if cfg in ("full", "baseline") else "0.55"
        parts.append(f'<rect x="{x:.1f}" y="{y:.1f}" width="{width:.1f}" height="{bot-y:.1f}" '
                     f'fill="{colour}" fill-opacity="{opacity}"/>')
        if f"{model}|{cfg}" in degenerate:
            # No width to draw. A zero-length error bar reads as certainty;
            # this reads as an estimator that ran out of signal.
            parts.append(f'<circle cx="{cx:.1f}" cy="{y:.1f}" r="4.5" fill="#fff" '
                         f'stroke="#111" stroke-width="1.4"/>')
            parts.append(f'<text x="{cx:.1f}" y="{y-9:.1f}" text-anchor="middle" '
                         f'font-size="9" fill="#111">{100*mean:.1f}*</text>')
        else:
            parts.append(f'<line x1="{cx:.1f}" y1="{ylo:.1f}" x2="{cx:.1f}" y2="{yhi:.1f}" stroke="#111" stroke-width="1.4"/>')
            parts.append(f'<line x1="{cx-3:.1f}" y1="{ylo:.1f}" x2="{cx+3:.1f}" y2="{ylo:.1f}" stroke="#111" stroke-width="1.4"/>')
            parts.append(f'<line x1="{cx-3:.1f}" y1="{yhi:.1f}" x2="{cx+3:.1f}" y2="{yhi:.1f}" stroke="#111" stroke-width="1.4"/>')
            parts.append(f'<text x="{cx:.1f}" y="{yhi-5:.1f}" text-anchor="middle" font-size="9" fill="#111">{100*mean:.1f}</text>')
        parts.append(f'<text x="{cx:.1f}" y="{bot+8:.1f}" text-anchor="end" font-size="9" fill="#333" '
                     f'transform="rotate(-45 {cx:.1f} {bot+8:.1f})">{_esc(cfg)}</text>')

    seen = []
    for model, *_ in cells:
        if model not in seen:
            seen.append(model)
    stride = max(150, (span - 20) // max(len(seen), 1))
    for j, model in enumerate(seen):
        parts.append(f'<rect x="{axis_l+10+j*stride}" y="62" width="10" height="10" fill="{_colour_for(model, seen)}"/>')
        parts.append(f'<text x="{axis_l+26+j*stride}" y="71" font-size="10" fill="#333">{_esc(_short(model))}</text>')
    n_deg = sum(1 for m, c, *_ in cells if f"{m}|{c}" in degenerate)
    if n_deg:
        # Footer rather than legend: the note has to fit, and a cell with no
        # spread to resample is a caveat on the reading, not a series.
        parts.append(f'<circle cx="{axis_l+4}" cy="{h-12}" r="4" fill="#fff" '
                     f'stroke="#111" stroke-width="1.4"/>')
        parts.append(f'<text x="{axis_l+14}" y="{h-8}" font-size="9" fill="#555">'
                     f'* {n_deg} {_plural(n_deg, "cell")} with no observed '
                     f'variance across repeats: the bootstrap has no spread to '
                     f'resample, so no interval is drawn.</text>')
    parts.append("</svg>")
    _write(path, parts)


# --- Figure 4: paired delta per repeat --------------------------------------

def repeat_deltas_svg(report, path, ablation="baseline", source=None):
    """Paired Delta = full - <ablation>, per pinned seed, straight from cell rates."""
    cells = report.get("cells", {})
    models = sorted({k.split("|", 1)[0] for k in cells}, key=lambda m: "qwen" not in m.lower())
    series = []
    for m in models:
        full = cells.get(f"{m}|full")
        base = cells.get(f"{m}|{ablation}")
        if not full or not base:
            continue
        reps = sorted(full["rates"], key=int)
        d = [full["rates"][r] - base["rates"][r] for r in reps]
        series.append((_short(m), d, _colour_for(m, models)))
    if not series:
        return
    nrep = max(len(d) for _, d, _ in series)

    top, zero, bot, left, right = 70, 190, 300, 70, 680
    lim = 0.5
    def y(d):
        return zero - (d / lim) * (zero - top)
    xs = [left + 70 + i * ((right - left - 110) / max(nrep - 1, 1)) for i in range(nrep)]

    parts = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="720" height="340" viewBox="0 0 720 340" {FONT}>',
        '<rect width="720" height="340" fill="#fff"/>',
        f'<text x="360" y="26" text-anchor="middle" font-size="14" font-weight="600" fill="#111">'
        f'Paired &#916; = full &#8722; {_esc(ablation)}, by repeat</text>',
        f'<text x="360" y="42" text-anchor="middle" font-size="11" fill="#555">'
        f'Each point is one pinned seed. {_source_note(source)}</text>',
        f'<line x1="{left}" y1="{zero}" x2="{right}" y2="{zero}" stroke="#222"/>',
        f'<line x1="{left}" y1="{top}" x2="{left}" y2="{bot}" stroke="#222"/>',
        f'<text x="28" y="185" transform="rotate(-90 28 185)" font-size="11" fill="#333">&#916; pass rate</text>',
        '<g font-size="10" fill="#444">',
    ]
    for t in (0.5, 0.25, 0.0, -0.25):
        yy = y(t)
        parts.append(f'<text x="{left-6}" y="{yy+4:.1f}" text-anchor="end">{t:+.2f}</text>'
                     if t else f'<text x="{left-6}" y="{yy+4:.1f}" text-anchor="end">0</text>')
        if t:
            parts.append(f'<line x1="{left}" y1="{yy:.1f}" x2="{right}" y2="{yy:.1f}" stroke="#f0f0f0"/>')
    parts.append("</g>")

    for name, d, colour in series:
        pts = " ".join(f"{xs[i]:.1f},{y(v):.1f}" for i, v in enumerate(d))
        parts.append(f'<polyline fill="none" stroke="{colour}" stroke-width="2" points="{pts}"/>')
        parts.append(f'<g fill="{colour}">')
        for i, v in enumerate(d):
            parts.append(f'<circle cx="{xs[i]:.1f}" cy="{y(v):.1f}" r="5"/>')
        parts.append("</g>")

    parts.append('<g font-size="10" fill="#333">')
    for i in range(nrep):
        parts.append(f'<text x="{xs[i]:.1f}" y="318" text-anchor="middle">{i}</text>')
    parts.append(f'<text x="{(left+right)/2:.0f}" y="334" text-anchor="middle" fill="#555">repeat (pinned seed)</text>')
    parts.append("</g>")
    # Legend sits in the clear band between the subtitle and the plot top, laid
    # out horizontally and centred. It used to be pinned inside the plot at
    # (right-150, 62), which collided with any series reaching the top-right --
    # at 20 repeats the Ornith line runs straight through the label.
    entries = [(name, colour) for name, _, colour in series]
    widths = [16 + int(len(name) * 5.6) for name, _ in entries]
    total = sum(widths) + 18 * (len(entries) - 1)
    x = (left + right) / 2 - total / 2
    for name, colour in entries:
        parts.append(f'<rect x="{x:.0f}" y="52" width="10" height="10" fill="{colour}"/>')
        parts.append(f'<text x="{x+16:.0f}" y="61" font-size="10" fill="#333">{_esc(name)}</text>')
        x += 16 + len(name) * 5.6 + 18
    parts.append("</svg>")
    _write(path, parts)


# --- Figure 5: interaction, weaker minus stronger ---------------------------

def interaction_svg(report, path, section="interaction"):
    """The interaction ladder from `section` of the report.

    `section` selects the resampling scheme: "interaction" is the unpaired one
    the paper cites, "interaction_paired" the seed-paired one. They are
    different procedures and produce different widths, so the figure says
    which it drew.
    """
    inter = {k: v for k, v in report.get(section, {}).items() if not k.startswith("_")}
    if not inter:
        return
    rows = sorted(inter.items(), key=lambda kv: -kv[1]["delta_weak_minus_strong"])
    left, right, top = 160, 590, 70
    rowh = 26
    w = 800
    h = top + rowh * len(rows) + 60
    lim = 0.40
    zero = (left + right) / 2
    def x(v):
        return zero + (v / lim) * ((right - left) / 2)

    parts = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{w}" height="{h}" viewBox="0 0 {w} {h}" {FONT}>',
        f'<rect width="{w}" height="{h}" fill="#fff"/>',
        f'<text x="400" y="26" text-anchor="middle" font-size="14" font-weight="600" fill="#111">'
        f'Interaction: &#916;<tspan font-size="10">weaker</tspan> &#8722; &#916;<tspan font-size="10">stronger</tspan> '
        f'(bootstrap 95% CI, {"seed-paired" if section.endswith("paired") else "unpaired"} resampling)</text>',
        f'<text x="400" y="44" text-anchor="middle" font-size="11" fill="#555">'
        f'{interaction_caption(report, rows)}</text>',
        f'<line x1="{zero}" y1="{top-8}" x2="{zero}" y2="{top+rowh*len(rows)}" stroke="#222" stroke-dasharray="3,3"/>',
    ]
    for i, (name, v) in enumerate(rows):
        yy = top + i * rowh + 12
        lo, hi, mid = v["lo"], v["hi"], v["delta_weak_minus_strong"]
        parts.append(f'<text x="{left-10}" y="{yy+4}" text-anchor="end" font-size="11" fill="#333">{_esc(name)}</text>')
        parts.append(f'<line x1="{x(lo):.1f}" y1="{yy}" x2="{x(hi):.1f}" y2="{yy}" stroke="#888" stroke-width="2"/>')
        for e in (lo, hi):
            parts.append(f'<line x1="{x(e):.1f}" y1="{yy-4}" x2="{x(e):.1f}" y2="{yy+4}" stroke="#888" stroke-width="2"/>')
        parts.append(f'<circle cx="{x(mid):.1f}" cy="{yy}" r="4.5" fill="#111"/>')
        parts.append(f'<text x="{right+8}" y="{yy+4}" font-size="9" fill="#666">'
                     f'{100*mid:+.1f} [{100*lo:+.1f}, {100*hi:+.1f}]</text>')
    ybase = top + rowh * len(rows) + 6
    parts.append(f'<line x1="{left}" y1="{ybase}" x2="{right}" y2="{ybase}" stroke="#222"/>')
    for t in (-0.4, -0.2, 0.0, 0.2, 0.4):
        parts.append(f'<text x="{x(t):.1f}" y="{ybase+16}" text-anchor="middle" font-size="10" fill="#444">'
                     f'{100*t:+.0f}</text>')
    parts.append(f'<text x="{zero}" y="{ybase+34}" text-anchor="middle" font-size="10" fill="#555">'
                 'percentage points</text>')
    parts.append("</svg>")
    _write(path, parts)


# --- Figure 1: graphical abstract -------------------------------------------

def graphical_abstract_svg(report, path, source=None):
    """The graphical abstract, drawn from the report rather than by hand.

    The hand-drawn PNG this replaces carried protocol-1 numbers (97.5% versus
    75%, delta +22.5) long after those were archived as not-the-headline -- the
    same failure mode the paper records for the hand-drawn Figure 4. Generating
    it from the same report as every other figure is the only way the front
    page cannot disagree with Table 3.
    """
    cells = report.get("cells", {})
    models = sorted({k.split("|", 1)[0] for k in cells},
                    key=lambda m: "qwen" not in m.lower())
    arms = []
    for m in models:
        full, base = cells.get(f"{m}|full"), cells.get(f"{m}|baseline")
        if full and base:
            arms.append((_short(m), full["mean"] * 100, base["mean"] * 100,
                         _colour_for(m, models)))
    if not arms:
        return

    W, H = 900, 250 + 96 * len(arms)
    FLAGS = ["envboot", "nativetools", "outcap", "checklist",
             "verifygate", "loopbreak", "groundfs"]
    parts = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" '
        f'viewBox="0 0 {W} {H}" {FONT}>',
        f'<rect width="{W}" height="{H}" fill="#fff"/>',
        f'<text x="{W//2}" y="34" text-anchor="middle" font-size="19" '
        f'font-weight="700" fill="#111">Harness architecture as an experimental object</text>',
    ]
    # the pipeline: model -> switchable harness -> hidden verifier -> outcome
    boxes = [("Local coding model", "weights held fixed"),
             ("Switchable harness", "7 independent flags"),
             ("Hidden verifier", "outside the sandbox"),
             ("Pass / fail", "unrun never passes")]
    bw, gap, by, bh = 190, 26, 58, 104
    bx0 = (W - (len(boxes) * bw + (len(boxes) - 1) * gap)) // 2
    for i, (head, sub) in enumerate(boxes):
        x = bx0 + i * (bw + gap)
        parts.append(f'<rect x="{x}" y="{by}" width="{bw}" height="{bh}" rx="8" '
                     f'fill="#f7f9fc" stroke="#c8d2e0"/>')
        parts.append(f'<text x="{x+bw//2}" y="{by+26}" text-anchor="middle" '
                     f'font-size="12" font-weight="600" fill="#111">{_esc(head)}</text>')
        parts.append(f'<text x="{x+bw//2}" y="{by+44}" text-anchor="middle" '
                     f'font-size="10" fill="#666">{_esc(sub)}</text>')
        if i == 1:
            for j, fl in enumerate(FLAGS):
                fx = x + 14 + (j % 2) * 88
                fy = by + 62 + (j // 2) * 13
                parts.append(f'<circle cx="{fx}" cy="{fy-3}" r="3" fill="{QWEN}"/>')
                parts.append(f'<text x="{fx+7}" y="{fy}" font-size="8.5" '
                             f'fill="#444">{_esc(fl)}</text>')
        if i < len(boxes) - 1:
            ax = x + bw + 4
            parts.append(f'<path d="M{ax} {by+bh//2} L{ax+gap-8} {by+bh//2}" '
                         f'stroke="#8a97a8" stroke-width="1.5" fill="none"/>')
            parts.append(f'<path d="M{ax+gap-12} {by+bh//2-4} l5 4 -5 4" '
                         f'stroke="#8a97a8" stroke-width="1.5" fill="none"/>')

    # one paired bar group per arm, full against baseline
    lab_w, bar_l, bar_r = 150, 250, W - 150
    y = by + bh + 46
    for name, full, base, colour in arms:
        for k, (tag, val, fill) in enumerate((("full", full, colour),
                                              ("baseline", base, "#cfd6de"))):
            yy = y + k * 26
            w = (bar_r - bar_l) * (val / 100.0)
            parts.append(f'<text x="{bar_l-10}" y="{yy+13}" text-anchor="end" '
                         f'font-size="11" fill="#333">{_esc(name)} {tag}</text>')
            parts.append(f'<rect x="{bar_l}" y="{yy}" width="{w:.1f}" height="18" '
                         f'rx="3" fill="{fill}"/>')
            parts.append(f'<text x="{bar_l+w+8:.1f}" y="{yy+13}" font-size="11" '
                         f'font-weight="600" fill="#111">{val:.1f}%</text>')
        d = report.get("confirmatory", {})
        key = next((k for k in d if k.startswith("H1|") and _short(k.split("|")[1]) == name), None)
        if key:
            c = d[key]
            verdict = "supported" if c.get("supported") else "not supported"
            parts.append(
                f'<text x="{bar_l}" y="{y+64}" font-size="10.5" fill="#444">'
                f'&#916; = {c["delta"]:+.3f}, {c["level"]:.1f}% CI '
                f'[{c["lo"]:+.3f}, {c["hi"]:+.3f}] &#8212; {verdict}</text>')
        y += 96

    n_ep = sum(c.get("n", 0) for c in cells.values())
    levels = sorted({round(c.get("level", 0), 1)
                     for c in (report.get("confirmatory") or {}).values()})
    lvl = (" Confirmatory level %.1f%% over %d test(s)."
           % (levels[-1], len(report.get("confirmatory") or {}))) if levels else ""
    parts.append(f'<text x="{W//2}" y="{H-10}" text-anchor="middle" font-size="9.5" '
                 f'fill="#777">Preregistered, {n_ep:,} episodes.{lvl} '
                 f'{_source_note(source)}</text>')
    parts.append("</svg>")
    _write(path, parts)


def from_report(report, outdir, source=None):
    """Render every figure into `outdir`. The directory is written in place.

    `outdir` is required. It used to default to the committed paper/figures and
    ran as a side effect of `compare.py --json-out`, so any filtered or
    exploratory run silently replaced the paper's figures with figures of a
    different selection. Choosing where these land is now the caller's
    decision, made explicitly.
    """
    if not outdir:
        raise ValueError("from_report needs an explicit output directory")
    os.makedirs(outdir, exist_ok=True)
    order = ["full", "baseline", "no-envboot", "no-nativetools", "no-outcap",
             "no-checklist", "no-verifygate", "no-loopbreak", "no-groundfs"]
    def sort_key(item):
        model, cfg = item[0].split("|", 1)
        return ("qwen" not in model.lower(), model,
                order.index(cfg) if cfg in order else len(order), cfg)
    cells = []
    for key, c in sorted(report.get("cells", {}).items(), key=sort_key):
        model, cfg = key.split("|", 1)
        cells.append((model, cfg, c["mean"], c["lo"], c["hi"]))
    degenerate = [k.split(":", 1)[1]
                  for k in (report.get("degenerate_intervals") or {})
                  if k.startswith("cells:")]
    # A pass-rate chart spanning arms served under different protocols is a
    # capability comparison the data does not support. compare.py prints that
    # caveat; the figure has to carry it too, or the figure disagrees with the
    # report it was rendered from.
    pairs = report.get("arm_protocol_drift_pairs") or []
    drift_note = ""
    if pairs:
        keys = sorted({k for p_ in pairs for k in p_.get("keys", [])
                       if not k.startswith("env_")})
        drift_note = (" NOT comparable across arms: %d arm pair(s) differ on %s."
                      % (len(pairs), ", ".join(keys) or "protocol"))
    if cells:
        pass_rates_svg(
            cells, os.path.join(outdir, "pass_rates.generated.svg"),
            subtitle=f"{episode_caption(report)} {_source_note(source)}{drift_note}",
            degenerate=degenerate)
    repeat_deltas_svg(report, os.path.join(outdir, "repeat_deltas.generated.svg"),
                      source=source)
    graphical_abstract_svg(report,
                           os.path.join(outdir, "graphical_abstract.generated.svg"),
                           source=source)
    interaction_svg(report, os.path.join(outdir, "interaction.generated.svg"))
    if report.get("interaction_paired"):
        interaction_svg(report,
                        os.path.join(outdir, "interaction_paired.generated.svg"),
                        section="interaction_paired")


def main():
    src = sys.argv[1] if len(sys.argv) > 1 else os.path.join(HERE, "paper", "stats-hard.json")
    outdir = sys.argv[2] if len(sys.argv) > 2 else OUT
    with open(src) as f:
        report = json.load(f)
    print(f"rendering {src} -> {outdir}")
    from_report(report, outdir, source=os.path.basename(src))


if __name__ == "__main__":
    main()
