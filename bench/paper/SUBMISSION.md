# Submission plan

Written 29 September 2026. Every venue fact below was read from the venue's own page on that
date unless it is marked **unverified**. Re-check each one before you submit: calls get amended.

There are two manuscripts. They are not at the same stage.

| | Long paper | Workshop paper |
|---|---|---|
| File | `harness_architecture.md` → `.tex`/`.pdf` via `build_pdf.sh` | `workshop/harness_ablation.tex` |
| Date | Revision 4, 29 Sep 2026 | 23 Aug 2026 |
| Data | Registered v2 grid (`stats-v2.json`) + third arm (`stats-ext-cerebras.json`) | **Superseded v1 grid** (`stats-hard.json`) |
| State | Results are final. The framing is written for the repository, not for a venue. | **Its thesis has been overturned.** It needs a rewrite, not formatting. |

## Status (Route A chosen, 29 Sep 2026)

Done in revision 4:
- §1.3 front matter
- §1.4 reference [5]:
  - the 30 kB cap is anchored on [1, App. B.3], which states it verbatim
  - [5] itself is verified at github.com/krafton-ai/kira
- §1.5 figure provenance
- §1.6
- figures:
  - nothing is clipped (every figure is checked by `figures.overflow`)
  - Figure 3 has one panel per arm
  - each model keeps one colour in every figure
- §5.10 and §5.11 moved to Appendices C and D; the main text is about 15,200 words
- **TMLR build** from the unmodified `tmlr/tmlr.sty` (Apache-2.0, fetched 29 Sep 2026 from
  JmlrOrg/tmlr-style-file, style dated 30 Jun 2023):
  - `build_pdf.sh tmlr-review`: anonymous, "Under review as submission to TMLR", with the
    commit hash withheld (the repository is public, so the hash would identify you) and Acknowledgments dropped, as the TMLR template asks until acceptance. 31 pages.
  - `build_pdf.sh tmlr-preprint`: named, for arXiv.
  - Both outputs go to `tmlr/`, are gitignored and are rebuilt on demand.

Still open:
- **Citation style.** `tmlr.sty` sets natbib author-year. The manuscript uses a hand-written
  numeric list ([1]–[24]), which renders but doesn't match TMLR's house style. Converting to a
  `.bib` file with `\citep` calls is the remaining formatting job.
- **Anonymised supplementary ZIP** (Route A step 5), then the self-tests from that copy (step 6).
- **OpenReview profile and arXiv endorsement** (§2). Only you can do these.

## 1. What's left

### Blocking

1. **The workshop paper argues the opposite of what v2 found.** Its two main claims are
   - "full vs baseline is null, Δ +5.0, CI [0.0, +10.0]" (`workshop/harness_ablation.tex:45`, `:201`)
   - "the only ladder interval excluding zero says `no-outcap` *hurts*, −10.0 [−17.5, −2.5]" (`:223`, `:243`)

   Revision 3 overturns both. H1 is supported on both arms at the Šidák-corrected level: Qwen
   Δ +0.156 [+0.069, +0.244], Ornith Δ +0.206 [+0.113, +0.300]. The `no-outcap` cell reversed
   sign to [−0.006, +0.156]. This is recorded in the `harness_architecture.md` abstract and in
   `README.md` "Revision 3". The reasoning in `workshop/README.md` under "Why the reframing"
   was built on those v1 numbers, so it no longer holds. The power table (`:279`–`:287`) is
   computed at v1's n.

   *Why the rewrite is worth doing:* the "instrument, and what it costs to use it honestly"
   pitch gets stronger with v2. The v1→v2 sign flip on the same cell is the cleanest
   instrument-artefact result in the project. The 82.3% coverage audit is a measured result,
   not a caveat. The third arm now carries real component-level effects: `no-nativetools`
   +0.144 and `no-verifygate` +0.100, both surviving Šidák.

   *Mechanics:* `workshop/make_figures.py` defaults to `../stats-hard.json` (line 12).
   `stats-v2.json` has the same `cells`/`deltas`/`interaction` keys and the same model IDs, so
   regenerating the figures means changing the default path. Figures for the third arm need
   `stats-all3.json` plus a third series.

2. **Don't start the rewrite until you pick a venue.** The page limit (4–10 pp), the template
   (IEEEtran or NeurIPS-style) and the anonymity rules (single- or double-blind) all change
   what fits. See §3.

3. **The long paper has no submittable front matter.** The first two paragraphs are revision
   history ("Draft — 29 August 2026 (revision 3)", "Revision 3 replaces the headline…"). The
   abstract is four paragraphs and 796 words (`sed -n 13,19p | wc -w`); venues cap it at roughly 150–250. §8
   "Corrections to the previous draft" and Appendix B's tooling notes are written for readers
   of the repository. For submission:
   - move the revision history to `README.md`
   - write a single-paragraph abstract
   - fold §8 into a short "instrument history" paragraph, since §5.9 already carries the substance
   - drop the author's name (double-blind venues)
   - move the "Data and code" section, which names repo paths and commits, to an anonymised
     artifact link

   The manuscript is about 19,900 words. TMLR has no hard limit (§3), but it warns that
   unusually long papers delay review. Any conference track means cutting it by more than half.

### Should fix

4. **Reference [5] Terminus-KIRA is unresolved, and it is load-bearing.** It is the source
   for the 30 kB output cap in §3.1 (Appendix B). A web search on 29 Sep 2026 found nothing
   for it. Either find a primary source or re-anchor
   the cap on [1] (Lee et al.), which the paper already cites as the setting it follows. A
   reviewer who checks will find a citation that nobody can locate.
5. **The Data section's figure-provenance sentence is wrong twice**
   (`harness_architecture.md:618`). It says Figures 3–6 "are rendered from `stats-v2.json`"
   and each "carries the source filename".
   - Figures 3–4 are stamped `stats-all3.json`, and Figure 3's caption agrees with the stamp,
     not with the Data section.
   - Figures 5–6 carry no stamp at all: `figures.py:337` (`interaction_svg`) takes no `source`
     argument. The last regeneration was 5d9293a, on the v2 report; that's inferred, not stamped.

   Either stamp Figures 5–6 and correct the sentence, or narrow it.
6. **Stale line in `README.md`:** "Known gap: Figures 3–5 have not been regenerated". That was
   fixed in 5d9293a (see 5). Corrected in this change.
7. **Self-disclosed weaknesses reviewers will press on** (`harness_architecture.md` §7):
   - eight Python tasks written by the harness author
   - the local ladder sits at n=5 (deviation D2, not blind)
   - Ornith's Δ mixes the harness effect with serving failures: 20 `ModelError` and 48
     `context_exhausted` out of 160

   These are honest limitations, not defects, and you can't fix them before a November
   deadline. Lead with them rather than burying them.

## 2. "No correspondence": affiliation and accounts

I've read this as: **no institutional affiliation or institutional email, and no prior
contact with any venue.** If you meant something else, tell me and I'll redo this section.

The second part doesn't matter: none of the venues below needs pre-contact. The first part
creates lead-time blockers:

| Item | Constraint | Source | Action |
|---|---|---|---|
| **OpenReview** (TMLR; any NeurIPS/ICLR workshop) | A Gmail-registered profile goes to moderation and "may take up to two weeks". An institutional email bypasses it. | OpenReview docs | Create the profile **now**, well before any deadline. |
| **TMLR profile** | "All authors must have complete and active OpenReview profiles, including information such as affiliations, conflicts of interest, and publication history." | TMLR author guide | Use "Independent Researcher" as the affiliation. It's common practice, but **unverified** as acceptable to TMLR specifically. |
| **arXiv** | First-time submitters need endorsement per category. Automatic endorsement mostly comes from an institutional email plus prior papers. At least one endorser is needed; endorsers must have recent papers in the category; "inappropriate to email large numbers of potential endorsers". | arXiv endorsement help | Start a cs.SE (or cs.LG) submission to get your endorsement link. Ask **one** author of a paper you cite who shows as an endorser on arXiv. |
| **HotCRP** (ICSE workshops, FORGE) | An account with any email. No moderation step is documented. | inferred | Low risk. |
| **Registration / travel** | ICSE-co-located events (Dublin, 25 Apr – 1 May 2027) normally require an author to register, and many require in-person presentation. | **unverified** for each workshop | Check each call. Budget for registration without an institution paying. |
| **Author block** | Single-author, independent, personal email as the correspondence address. Every venue permits this. | — | Put "Independent Researcher" and your Gmail on the camera-ready. |

## 3. Venues

**Closed as of today:**
- ICLR 2027 main track (full papers were due 25 Sep 2026 AoE)
- NeurIPS 2026 workshops: the suggested deadline was 29 Aug and notifications went out today, 29 Sep
  - Who Verifies the Agents? (29 Aug)
  - Evaluation of Interactive Agents (29 Aug)
  - SLM-Agents (6 Sep)

  All three fit the paper; next year's editions are the ones to watch.

**Open:**

| Venue | Deadline (AoE) | Length / format | Review | Archival | Fit |
|---|---|---|---|---|---|
| **AGENTVERIFY @ ICSE 2027**, Infrastructure for Trustworthy Software Agents · [site](https://agentverify2027.github.io/) · [HotCRP](https://icse2027-agentverify.hotcrp.com/) | abstract **1 Nov 2026**, paper **6 Nov 2026**; notification 11 Dec | not yet stated on site (**unverified**) | not stated (**unverified**) | ICSE workshops are IEEE-published (inferred from sibling workshops) | **Best.** The scope names evaluation methodology, benchmarking, reproducibility, and "the execution stack (prompts, tools, skills, sandboxes, telemetry, evaluation protocols)", which is this paper. |
| **AGENT @ ICSE 2027**, Agentic Engineering · [call](https://conf.researchr.org/home/icse-2027/agent-2027) · [HotCRP](https://icse2027-agent.hotcrp.com/) | **27 Nov 2026**; notification 11 Dec | full ≤ 8 pp, short ≤ 5 pp (both excluding references); IEEE proceedings template | **single-anonymous** (names allowed) | "published by IEEE" | Strong. Its topics include verification/testing and empirical studies of agents. It's also the backup if AGENTVERIFY is missed. |
| **FORGE 2027**, research track · [call](https://conf.researchr.org/track/forge-2027/forge-2027-research-papers) · [HotCRP](https://forge2027.hotcrp.com/) | abstract **25 Oct**, paper **30 Oct 2026**; notification 4 Jan | full ≤ 10 pp + 2 pp references; new-idea ≤ 4 + 2; `\documentclass[10pt,conference]{IEEEtran}` | double-anonymous | a conference, not a workshop | Good fit for a condensed long paper, but it's the tightest deadline. |
| **FORGE 2027**, Data & Benchmarking track | **15 Nov 2026** | page limit **unverified** (not on the landing page) | **unverified** | — | Plausible for an instrument/benchmark paper. Read the track page before choosing. |
| **LLM4Code @ ICSE 2027** · [site](https://llm4code.github.io/) | dates "TBA" on its own site; the ICSE umbrella lists workshop papers due 13 Nov 2026 | **unverified** | **unverified** | — | Topical, but the call isn't published yet. |
| **WSESE @ ICSE 2027**, Methodological Issues with Empirical Studies · [call](https://conf.researchr.org/home/icse-2027/wsese-2027) | the page shows contradictory dates (submission 29 Jan after notification 27 Nov): **unverified** | technical 6–8 pp; experience 4–6; vision 2–4 | not stated | IEEE ICSE workshop proceedings | Its methodology focus suits §5.5/§5.9, if the dates get fixed. |
| **AAAI-27 workshops** · [call](https://aaai.org/conference/aaai/aaai-27/workshops-call/) | workshop papers due to organisers **20 Nov 2026** (umbrella date) | per workshop | per workshop | usually non-archival | The individual workshop list isn't out yet (calls due to AAAI 2 Oct). Check again next week. |
| **TMLR** (journal, rolling) · [author guide](https://jmlr.org/tmlr/author-guide.html) | **none** | "any length", but unusually long papers delay review; the TMLR style file is mandatory; ≤ 100 MB anonymised supplementary material (code allowed) | double-blind, OpenReview | archival journal | **Best home for the long paper.** Its criteria ("claims supported by accurate, convincing and clear evidence"; interest to *some* of the audience) reward exactly this paper's careful negatives. It also permits prior non-archival workshop or arXiv versions. |
| **ICLR 2027 workshops** | usually Jan–Feb 2027 (**unverified**: the workshop list isn't out yet) | ~4–9 pp typical | OpenReview | usually non-archival | Watch for an ICBINB edition. ICLR 2026 ran ICBINB ("Where LLMs need to improve"), which is the negative-results lineage the workshop README wanted. |

### The decision you need to make: archival conflict

ICSE workshop and FORGE papers are **archival** (IEEE). TMLR forbids "reuse of written text,
figures or results" with a paper "published, accepted for publication, or submitted in
parallel at another archival, peer-reviewed venue", and it allows overlap only with venues
"publicly declared, in writing, to be non-archival".

So an IEEE workshop paper on the v2 grid plus a TMLR paper on the same grid is a conflict.
You have to pick one route:

- **Route A (recommended): TMLR for the long paper, plus an arXiv preprint.**
  1. Post the preprint once the front matter is fixed (§1.3).
  2. Submit to TMLR any time.
  3. Optionally, show the work at a **non-archival** workshop (ICLR 2027, AAAI-27) that allows
     under-review work. Several of the NeurIPS workshops above explicitly did.

  This route has no deadline pressure, puts the whole paper under review, and has no conflict.
- **Route B: AGENTVERIFY (6 Nov), or AGENT (27 Nov) if that's missed, for a 5–8 page
  instrument paper.** This gets a peer-reviewed IEEE publication fastest. The long paper then
  has to go to a venue that accepts extended versions of workshop papers, or be restructured
  so its results don't overlap. TMLR would be off the table for the same results.

## 4. Pre-submission checklist

**This week:**
- Create the OpenReview profile.
- Start an arXiv submission to get the endorsement link.
- Decide A or B.

**Route A:**
1. Rewrite the long paper's front matter (§1.3).
2. Resolve [5].
3. Stamp Figures 5 and 6.
4. Port the paper to the TMLR style file; `build_pdf.sh` currently targets a pandoc/xelatex article.
5. Anonymise it: remove the author name, repo paths and commit hashes from the body, and link an
   anonymised artifact (e.g. anonymous.4open.science; that host is **unverified** as TMLR-acceptable).
6. Run `selftest.py`, `stress_test.py` and `test_stats.py` from the anonymised copy. This proves
   the artifact works without repo paths; do it **before** submitting.
7. Post to arXiv (the de-anonymised version is fine: TMLR allows preprints).
8. Submit to TMLR with the supplementary ZIP (≤ 100 MB).

**Route B:**
1. Re-read AGENTVERIFY's call for its page limit and anonymity. As of today its site states neither.
2. Rewrite `workshop/harness_ablation.tex` from v2 plus the third arm, in IEEEtran.
3. Point `make_figures.py` at `stats-v2.json` / `stats-all3.json`.
4. Delete or replace the "Why the reframing" section in `workshop/README.md`.
5. Anonymise the paper if the review is double-blind (AGENT is single-anonymous).
6. Register the abstract by 1 Nov and submit by 6 Nov AoE.
7. Budget for ICSE registration and travel.
8. **Don't post an arXiv preprint until you've read the ICSE 2027 preprint/anonymity Q&A.**
   ICSE-family double-anonymous venues have their own rules (**unverified**), and a public
   preprint can break anonymity.

**Both routes:** before uploading, check the archived run directories for identifying paths.
The workshop README flagged this, and it hasn't been checked.

## Sources

- NeurIPS 2026 workshops: https://blog.neurips.cc/2026/08/10/announcing-the-neurips-2026-workshops/
- Who Verifies the Agents?: https://verify-agents-workshop.github.io/
- Evaluation of Interactive Agents: https://eval-interactive-agents-workshop.github.io/
- SLM-Agents: https://slmw2026.github.io/
- ICLR 2027 call for papers: https://www.iclr.cc/Conferences/2027/CallForPapers
- ICSE 2027 workshops: https://conf.researchr.org/track/icse-2027/icse-2027-workshops
- AGENTVERIFY 2027: https://agentverify2027.github.io/
- AGENT 2027: https://conf.researchr.org/home/icse-2027/agent-2027
- WSESE 2027: https://conf.researchr.org/home/icse-2027/wsese-2027
- LLM4Code: https://llm4code.github.io/
- FORGE 2027: https://conf.researchr.org/home/forge-2027 and https://conf.researchr.org/track/forge-2027/forge-2027-research-papers
- AAAI-27 workshops: https://aaai.org/conference/aaai/aaai-27/workshops-call/
- TMLR: https://jmlr.org/tmlr/author-guide.html and https://www.jmlr.org/tmlr/editorial-policies.html
- arXiv endorsement: https://info.arxiv.org/help/endorsement.html
- OpenReview moderation: https://docs.openreview.net/getting-started/frequently-asked-questions/why-does-it-take-two-weeks-to-moderate-my-profile
