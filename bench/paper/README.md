# Paper draft

Manuscript: [`harness_architecture.md`](harness_architecture.md) — **revision 2, 22 August 2026**.

Three protocol documents sit beside it and are not part of the manuscript:
[`preregistration.md`](preregistration.md), the registered v2 grid (two locally
served models); [`DEVIATIONS.md`](DEVIATIONS.md), which records every departure
from it under §12 along with what had been observed when each was decided; and
[`extension-cerebras.md`](extension-cerebras.md), a separately
registered extension adding API-served `gpt-oss-120b` and `gemma-4-31b` arms.
The extension runs under its own tag and does not touch the frozen grid or the
registered design; it carries its own pilot gates and its own suitability
assessment, including what an arm with no sole tenancy and a best-effort seed
costs the analysis.

Figures are in `figures/`. The draft is NeurIPS-style IMRAD. Headline metrics are the **frozen**
720-episode hard grid (Tables 3–8), re-derived for this revision from `stats-hard.json` and the
per-episode `summary.json` rows. Protocol-1's 160-episode full-versus-baseline contrast
(Qwen 75%→97.5%, \(\Delta +22.5\) pp) is archived and noted, not the headline. The draft does
**not** claim \(\Delta_{\text{weak}} > \Delta_{\text{strong}}\); all eight interaction intervals
include zero (Table 7).

## Revision 2

**Corrected.** §5.7's verify-gate contrast previously read 12/12 vs 7/12 (\(p=0.037\)) and
13/13 vs 7/13 (\(p=0.015\)). Both counted `qwen3.8_27b-mlx__full__rep` — which holds three
`globmatch` episodes, not `navigate` — as a gate-on `navigate` tag. Corrected: **9/9 vs 5/9,
\(p=0.082\)** on matched tags and **10/10 vs 7/13, \(p=0.019\)** pooled. The conservative
reading no longer reaches \(\alpha=0.05\). Contract-breaking finishes: Qwen 6 of 13 (was 5 of
12). Table 5 medians are midpoints (7,512.5 / 16,340.5, was 7,513 / 16,341). §8 tabulates every
change.

**Added.** Tamper detection and the three non-cheating mechanisms (§3.2); per-task
characterization with `protect` sets (Table 2); instrument validation counts — selftest 19/19,
stress 106/106, stats 84 (§4.2); an explicit interaction table and figure (Table 7, Figure 5);
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
