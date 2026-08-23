# Workshop submission

`harness_ablation.tex` — 5 pages + references, anonymised, compiles standalone.

    pdflatex harness_ablation.tex && pdflatex harness_ablation.tex

The long-form manuscript stays at `../harness_architecture.md`. This is not a summary of it:
it is reframed around a different claim (see below) and drops the material that only supports
the long version.

## Before submitting — things I could not verify

1. **Style file.** This compiles with plain `article` so it can be checked offline. Swap in
   the host workshop's current `.sty` and delete the `geometry`/`times` block. Workshop style
   files are reissued annually; take the one linked from the workshop's own call.
2. **Page limit.** Workshop limits differ from the main conference (commonly 4–9 pages) and
   are set per workshop. 5 pages single-column will reflow when the style file is applied.
3. **Deadlines and which workshops exist this cycle.** Not checkable from here — my training
   data predates the current cycle and I will not guess at dates. Confirm from the call.
4. **Anonymisation.** Author block is anonymised. The repository URL is deliberately omitted;
   add an anonymised artifact link if the venue wants one, and check that the archived run
   directories do not carry identifying paths.

## Why the reframing

The long manuscript leads with the full-versus-baseline contrast. That contrast is null
(Δ +5.0, CI [0.0, +10.0]), and a workshop paper whose headline is "we measured a thing and the
interval included zero" is weak regardless of how carefully it was measured.

The findings that actually survive their intervals are more interesting than the headline:

- **Neither model's best configuration is `full`.** Qwen peaks at `no-loopbreak`, Ornith at
  `no-outcap`. Shipping the bundle because each component has a plausible story means shipping
  components that cost you episodes.
- **The only ladder interval excluding zero says a component hurt** — removing the output cap
  raises the weaker model by 10.0 points, CI [-17.5, -2.5].
- **The interaction everyone assumes is undetectable** at n=5, with point estimates disagreeing
  in sign.
- **Two ways this measurement silently goes wrong**, both of which we hit: a mid-grid protocol
  change inflating the same contrast to +22.5 (CI excluding zero, four times the real effect),
  and difficulty-correlated censoring in an API arm.

So the paper is pitched as *the instrument and what it costs to use honestly*, not as an
effect size. The negative results are the contribution rather than an apology.

## Venue fit

Ranked by fit to the reframed claim, as venue *classes* — I cannot confirm which specific
workshops are running:

1. **Negative-results / reproducibility workshops** (the ICBINB lineage at NeurIPS is the
   canonical example). Best fit by a distance: a preregistered-style null with clean positive
   controls and two documented measurement failures is exactly their remit, and nowhere else
   rewards §6.
2. **LLM-agent / agentic-AI workshops** at NeurIPS or ICLR. Most topically direct, largest
   audience, but such venues skew toward positive results and a null headline competes poorly
   unless §5's "bundle is not optimal" finding leads.
3. **Evaluation / benchmarking workshops.** The methodological content (censoring, denominator
   rules, protocol discipline, unrun≠passed) is arguably the stronger contribution and travels
   further than the specific harness.
4. **MLSys-style systems venues.** Only if expanded — the GH200 envelope and local-serving
   angle is currently one paragraph.

My recommendation is (1) if such a workshop is running this cycle, else (3). Both reward the
thing this work actually did well, which is measure carefully and report what it found.

## What was cut, and where it still lives

Per-task matrix, stop-reason table, full architecture detail, instrument-validation counts,
the corrections table, citation verification, and the full third-arm investigation are all in
`../harness_architecture.md`. If the venue allows an appendix, the per-task matrix and the
stop-reason table are the two most worth restoring — they are what let a reader see where the
bundle paid and where the weaker model simply never finished.
