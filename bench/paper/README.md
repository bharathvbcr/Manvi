# Paper draft

Manuscript: [`harness_architecture.md`](harness_architecture.md) — **revision 3, 29 August 2026**.

Three protocol documents sit beside it and are not part of the manuscript:
[`preregistration.md`](preregistration.md), the registered v2 grid (two locally
served models); [`DEVIATIONS.md`](DEVIATIONS.md), which records every departure
from it under §12 along with what had been observed when each was decided; and
[`extension-cerebras.md`](extension-cerebras.md), a separately registered
extension adding the API-served `gpt-oss-120b` arm. [`run_v2.sh`](run_v2.sh) is
the six-phase driver that ran the grid, committed verbatim.

Headline metrics are the **registered 1,440-episode v2 grid** (tag `v2`), drawn
from [`stats-v2.json`](stats-v2.json). The third arm, API-served
`gpt-oss-120b`, is a further 1,440 episodes under tag `ext-cerebras`
([`stats-ext-cerebras.json`](stats-ext-cerebras.json)) and is reported in §5.7.
The frozen 720-episode v1 grid that revision 2 headlined is **superseded** and
appears only in §5.9 as an instrument-failure record; none of the three are
pooled for pass rates.

Build the PDF and TeX with [`build_pdf.sh`](build_pdf.sh) (pandoc + xelatex).
The markdown is the source of truth; the script works on a copy.

## Revision 3

**The headline is replaced, not corrected.** An audit found five defects in the
v1 instrument — most consequentially a verify gate that returned hidden-test
output including expected values on a failed `finish`, in gate-on cells only
(77 of 760 episodes), and five hard tasks that accepted demonstrably wrong
solutions. An audit of all 760 v1 episodes and 3,879 file-writing tool calls
found **zero** exploiting episodes, so v1's numbers were not produced by
cheating — but they were measured on an instrument that could not have detected
it. The response is a new registered experiment, not a corrected table.

**What the v2 grid shows.** H1 is **supported on both arms** at the Šidák-corrected
98.7% level: Qwen 79.4% → 95.0% (Δ +0.156, [+0.069, +0.244]) and Ornith
40.0% → 60.6% (Δ +0.206, [+0.113, +0.300]). H2 is **not supported** — removing
the output cap leaves the weaker arm at [−0.006, +0.156], reversing the sign of
v1's Δ −0.100 whose interval had excluded zero. All eight interaction intervals
include zero again, under both pairing schemes.

**Where the component-level evidence lives.** Deviation D2 left the *local*
exploratory cells at n=5, where measured bootstrap coverage is **82.3%**; four
of sixteen intervals exclude zero against a null expectation of 2.83, so Table 7
is reported as uninformative except for Qwen `no-verifygate`. The third arm kept
n=20 on all nine cells: there four of eight exclude zero against a null
expectation of **0.47** at 94.1% coverage, and `no-nativetools` (+0.144) and
`no-verifygate` (+0.100) survive a Šidák correction across its whole ladder
(Table 10). The same four exclusions mean opposite things on the two ladders.

**§5.8** gives a post-hoc mechanism for the `no-checklist` result: the checklist
moves 31 of 160 episodes out of `finish` and into a no-tool-call stall, and
those buckets pass at ~100% and ~67% respectively, which accounts for the whole
delta. It buys nothing because `verifygate` already prevents the failure it
targets.

**Known gap.** Figures 3–5 have **not** been regenerated against `stats-v2.json`
and still render the superseded v1 grid. No table in revision 3 cites them.

## Revision 2 (superseded)

**Corrected.** §5.7's verify-gate contrast previously read 12/12 vs 7/12 (\(p=0.037\)) and
13/13 vs 7/13 (\(p=0.015\)). Both counted `qwen3.8_27b-mlx__full__rep` — which holds three
`globmatch` episodes, not `navigate` — as a gate-on `navigate` tag. Corrected: **9/9 vs 5/9,
\(p=0.082\)** on matched tags and **10/10 vs 7/13, \(p=0.019\)** pooled. The conservative
reading no longer reaches \(\alpha=0.05\). Contract-breaking finishes: Qwen 6 of 13 (was 5 of
12). Table 5 medians are midpoints (7,512.5 / 16,340.5, was 7,513 / 16,341). §8 tabulates every
change.

**Added.** Tamper detection and the three non-cheating mechanisms (§3.2); per-task
characterization with `protect` sets (Table 2); instrument validation counts — selftest 19/19,
stress 106/106, stats 84 (§4.2, superseded by revision 3's 189 and 166); an explicit interaction table and figure (Table 7, Figure 5);
a GH200 compute envelope from 78,027 samples over 43.8 h (Table 8); an explicit limitations
section (§7); a corrections section (§8); citation verification status (Appendix B).

**Figures are now generated.** `figures.py` renders Figures 3, 4 and 5 from `stats-hard.json`
as `figures/*.generated.svg`; they reproduce byte-identically and cannot disagree with the
tables. The old `repeat_deltas.svg` was a hand-written SVG that still plotted **protocol-1**
deltas (Qwen 0.5, 0.25, 0.125, 0.25, 0) next to frozen tables; the frozen values are
0.125, 0, 0.125, 0, 0. `repeat_deltas.svg` and `pass_rates.svg` are kept only as a record.

```bash
python3 figures.py results/stats-hard.json
```

**Literature.** Expanded from 5 references to 19 via the ScholarLM MCP server (arXiv + OpenAlex).
§2 is restructured into six themed subsections: harness search [1], [2], [5]; scaffolds vs.
pipelines [3], [4], [8]–[11], [19]; verification and self-repair [16], [17]; benchmark gaming
[12], [13], [14]; context handling [7]; statistics and reproducibility [6], [15], [18].

18 of 19 references are machine-verified, including **[1] Meta-Harness and [2] Terminal-Bench,
which revision 1 could not resolve** — both now confirmed on title, full author list, year and
DOI. Only [5] Terminus-KIRA remains unresolved; it has no independent identifier and is cited as
it appears in [1]. It is load-bearing for the 30 kB cap in §3.1. See Appendix B, which also notes
that this provider returns some OpenAlex records with mismatched abstracts — every reference was
accepted only on agreement of title, authors, and year.
