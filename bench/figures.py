#!/usr/bin/env python3
"""Render paper figures from a compare.py --json-out file. Stdlib only."""
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "paper", "figures")


def _bar_y(pct, top=80, bottom=300, full=100.0):
    return bottom - (pct / full) * (bottom - top)


def pass_rates_svg(cells, path):
    # cells: list of (label, mean, lo, hi, color)
    w, h = 720, 360
    parts = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{w}" height="{h}" '
        f'viewBox="0 0 {w} {h}" font-family="Helvetica, Arial, sans-serif">',
        '<rect width="720" height="360" fill="#fff"/>',
        '<text x="360" y="28" text-anchor="middle" font-size="14" font-weight="600" '
        'fill="#111">Pass rate (n repeats, bootstrap 95% CI)</text>',
        '<line x1="80" y1="300" x2="680" y2="300" stroke="#222"/>',
        '<line x1="80" y1="80" x2="80" y2="300" stroke="#222"/>',
    ]
    n = max(len(cells), 1)
    gap = 520 / n
    for i, (label, mean, lo, hi, color) in enumerate(cells):
        x = 110 + i * gap
        y = _bar_y(100 * mean)
        ylo = _bar_y(100 * lo)
        yhi = _bar_y(100 * hi)
        height = 300 - y
        parts.append(f'<rect x="{x}" y="{y:.1f}" width="54" height="{height:.1f}" fill="{color}"/>')
        cx = x + 27
        parts.append(f'<line x1="{cx}" y1="{ylo:.1f}" x2="{cx}" y2="{yhi:.1f}" stroke="#111" stroke-width="1.5"/>')
        parts.append(f'<text x="{cx}" y="318" text-anchor="middle" font-size="9" fill="#333">{label}</text>')
        parts.append(f'<text x="{cx}" y="{y-6:.1f}" text-anchor="middle" font-size="10" fill="#111">{100*mean:.1f}</text>')
    parts.append("</svg>")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write("\n".join(parts) + "\n")


def from_report(report, outdir=OUT):
    os.makedirs(outdir, exist_ok=True)
    colors = ["#2f6fed", "#9bb8f5", "#c45c26", "#e0b89a", "#3a7d44", "#8fbf88"]
    cells = []
    for i, (key, c) in enumerate(sorted(report.get("cells", {}).items())):
        model, cfg = key.split("|", 1)
        short = model.split("/")[-1].split(":")[0][:12]
        cells.append((f"{short}\n{cfg}"[:22].replace("\n", " "),
                      c["mean"], c["lo"], c["hi"], colors[i % len(colors)]))
    if cells:
        pass_rates_svg(cells, os.path.join(outdir, "pass_rates.generated.svg"))
        print("wrote", os.path.join(outdir, "pass_rates.generated.svg"))


def main():
    src = sys.argv[1] if len(sys.argv) > 1 else os.path.join(HERE, "paper", "stats-hard.json")
    with open(src) as f:
        report = json.load(f)
    from_report(report)


if __name__ == "__main__":
    main()
