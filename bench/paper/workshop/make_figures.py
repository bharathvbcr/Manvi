#!/usr/bin/env python3
"""Emit pgfplots figure bodies for the workshop paper from stats-hard.json.

Generated, not drawn. A hand-drawn figure goes stale the moment the data moves;
this repository already had that happen once (repeat_deltas.svg carried
protocol-1 values next to frozen tables).
"""
import json, os, sys

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = sys.argv[1] if len(sys.argv) > 1 else os.path.join(HERE, "..", "stats-hard.json")
Q, O = "qwen3.8:27b", "hf.co/ornith-ai/Ornith-1.5-35B-A3B-GGUF:Q8_0"
ORDER = ["full", "baseline", "no-verifygate", "no-envboot", "no-checklist",
         "no-loopbreak", "no-outcap", "no-groundfs", "no-nativetools"]
SHORT = {"full": "full", "baseline": "base", "no-verifygate": "no-vg",
         "no-envboot": "no-eb", "no-checklist": "no-cl", "no-loopbreak": "no-lb",
         "no-outcap": "no-oc", "no-groundfs": "no-gf", "no-nativetools": "no-nt"}
d = json.load(open(SRC))
cells, deltas, inter = d["cells"], d["deltas"], d["interaction"]


def ladder(path):
    """Cell pass rate with bootstrap CI, both models, ordered ladder."""
    rows = []
    for i, c in enumerate(ORDER):
        for m, off in ((Q, -0.18), (O, +0.18)):
            v = cells[f"{m}|{c}"]
            rows.append((i + off, 100 * v["mean"], 100 * (v["mean"] - v["lo"]),
                         100 * (v["hi"] - v["mean"]), m))
    def series(m):
        return "\n".join(f"({x:.2f},{y:.1f}) +- (0,{up:.1f}) -= (0,{lo:.1f})"
                         for x, y, lo, up, mm in rows if mm == m)
    ticks = ",".join(str(i) for i in range(len(ORDER)))
    labels = ",".join(SHORT[c] for c in ORDER)
    open(path, "w").write(f"""\
\\begin{{tikzpicture}}
\\begin{{axis}}[
  width=\\linewidth, height=5.0cm,
  ymin=40, ymax=105, ylabel={{pass rate (\\%)}},
  xtick={{{ticks}}}, xticklabels={{{labels}}},
  x tick label style={{font=\\scriptsize,rotate=35,anchor=east}},
  y tick label style={{font=\\scriptsize}},
  ylabel style={{font=\\scriptsize}},
  legend style={{font=\\scriptsize,at={{(0.5,1.02)}},anchor=south,legend columns=2,draw=none}},
  grid=major, grid style={{gray!20}},
  error bars/y dir=both, error bars/y explicit,
]
\\addplot[only marks,mark=*,mark size=1.6pt,color=blue!70!black] coordinates {{
{series(Q)}
}};
\\addlegendentry{{Qwen 3.8 27B}}
\\addplot[only marks,mark=square*,mark size=1.6pt,color=orange!80!black] coordinates {{
{series(O)}
}};
\\addlegendentry{{Ornith-1.5 35B-A3B}}
\\end{{axis}}
\\end{{tikzpicture}}
""")
    print("wrote", path)


def interaction(path):
    """Interaction statistic per ablation, sorted, with a zero line."""
    rows = sorted(((k, v) for k, v in inter.items() if not k.startswith("_")),
                  key=lambda kv: kv[1]["delta_weak_minus_strong"])
    pts, ticks, labels = [], [], []
    for i, (k, v) in enumerate(rows):
        mid = 100 * v["delta_weak_minus_strong"]
        pts.append(f"({mid:.1f},{i}) +- ({100*v['hi']-mid:.1f},0) -= ({mid-100*v['lo']:.1f},0)")
        ticks.append(str(i)); labels.append(SHORT.get(k, k))
    open(path, "w").write(f"""\
\\begin{{tikzpicture}}
\\begin{{axis}}[
  width=\\linewidth, height=4.4cm,
  xmin=-40, xmax=45, xlabel={{$\\Delta_{{\\mathrm{{weaker}}}}-\\Delta_{{\\mathrm{{stronger}}}}$ (points)}},
  ytick={{{",".join(ticks)}}}, yticklabels={{{",".join(labels)}}},
  y tick label style={{font=\\scriptsize}}, x tick label style={{font=\\scriptsize}},
  xlabel style={{font=\\scriptsize}},
  ymin=-0.7, ymax={len(rows)-0.3},
  grid=major, grid style={{gray!20}},
  error bars/x dir=both, error bars/x explicit,
]
\\addplot[only marks,mark=*,mark size=1.8pt,color=black] coordinates {{
{chr(10).join(pts)}
}};
\\draw[dashed,gray] (axis cs:0,-0.7) -- (axis cs:0,{len(rows)-0.3});
\\end{{axis}}
\\end{{tikzpicture}}
""")
    print("wrote", path)


ladder(os.path.join(HERE, "fig_ladder.tex"))
interaction(os.path.join(HERE, "fig_interaction.tex"))
