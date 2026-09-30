# Literature review: strengthening Related Work

For: `bench/paper/harness_architecture.md` ("Harness Architecture as an Experimental Object: Measuring Switchable Components on Local Coding Agents").
Prepared 2026-09-30 with the ScholarLM-local MCP server for discovery and independent fetches (arXiv abstract pages, Crossref) for verification. Read-only with respect to the paper: no manuscript, bibliography or appendix was edited, and nothing was committed. Proposed sentences add context only; none changes a number or a claim about the paper's own results.

**Summary.** 9 additions accepted and verified (section 2); 2 existing references now have verified peer-reviewed versions, 1 has an author-stated venue only, and 1 has a *related* published record that is not verified as the same work (section 3); 19 candidate records rejected, not pursued, or left as unverified leads (section 4). Coverage is uneven: the local server could reach only OpenAlex, which answered 5 of 24 logged search attempts before its daily quota ran out, so research questions 2, 3 and 6 are gaps in the search rather than findings (section 5).

## 1. Method

**Scope.** Read-only review of `bench/paper/harness_architecture.md` (Abstract, §1, §2.1-2.7, §4.5, §6, References, Appendices B and C). The only file written in the repository is this one. Nothing was committed. The paper's own results are not restated or changed below; every proposed sentence adds context only.

**Search tool.** ScholarLM-local MCP (`mcp__scholarlm-local__*`) only: `wisdevListProviders`, `scholarlmGetAuthStatus`, `wisdevSearchPapers`, `wisdevPaperLookup`, `scholarlmPaperclipSearch`. No cloud ScholarLM, no plugin ScholarLM, no WisDev connection, and no other literature tool was used for discovery.

**What the local server could actually reach.** `scholarlmGetAuthStatus` reported `accessLevel: anonymous`, `authMode: local_stdio`, `effectiveTier: free`, `maxQualityMode: fast`, and the model-backed search stages (query expansion, screening) ran degraded or disabled on every call. At that tier:

- **Gated (search not run):** `arxiv`, `dblp`, `crossref`, `core`, `google_scholar`, `papers_with_code` -- each returned `SEARCH_PROVIDERS_GATED_BY_TIER` ("provider tier access 1") when requested explicitly. So the suggested per-provider fallbacks (`["arxiv"]`, `["crossref"]`, `["dblp"]`) were not available at all in this session.
- **Not configured:** Paperclip (`PAPERCLIP_API_KEY` unset).
- **Reachable but rate-limited:** `openalex` and `semantic_scholar`. Semantic Scholar returned 429 or a rate-gate deferral on every call and never answered. OpenAlex answered **5 of 24 logged attempts** (several rows cover repeated retries); the rest were upstream 429s ("retry after at least 30-38 seconds") or the server's own rate gate / circuit breaker ("provider rate gate deferred for Ns", "requested provider is temporarily unavailable"). Spacing calls 60-240 s apart with background waits did not reliably clear it, and at about 01:02 UTC OpenAlex returned "retry after at least 82666 seconds", i.e. the daily quota was exhausted; discovery stopped there.
- `wisdevPaperLookup` on an arXiv ID failed on all capable providers (arxiv 429, semantic_scholar 429, crossref "not a DOI").

Consequently **all discovery in this review came from five successful OpenAlex searches** (three of which produced on-topic hits). OpenAlex relevance ranking on these queries was poor (most hits were off-topic reviews in medicine, education and security), so recall is low and the absence of a paper here is not evidence that it does not exist.

**Verification.** Independent of the search tool, and never trusting its abstract:

- arXiv: the abstract page `https://arxiv.org/abs/<id>` was fetched (WebFetch, and a small Python `urllib` parser in the session scratchpad that reads the page's `citation_title` / `citation_author` / `citation_date` meta tags, the abstract block, submission history, Comments and Journal-ref). The parser was needed because WebFetch's summariser paraphrased abstracts and once misspelled an author; every accepted summary is grounded in the verbatim abstract text from the parser.
- Crossref: `https://api.crossref.org/works/<doi>` for DOIs, and `works?query.bibliographic=<title>` to look for published versions of existing references.
- Acceptance rule (as Appendix B): title, author list (full, or first author + count for very large lists) and year agree between the provider record and the independent page, and the fetched abstract supports the sentence proposed. Where the provider's author spelling disagreed with the source of record, the source of record is used and the disagreement is reported.

**Verification-side failures (also recorded, not treated as negative results):** DBLP HTML and DBLP API both returned an Anubis "Access Denied" page; OpenReview returned a browser-verification challenge; `export.arxiv.org/api/query` returned HTTP 429; two Crossref queries returned HTTP 429 (one retried successfully, one -- a title check for DarwinX -- not retried).

**Dates.** All searches and fetches on 2026-09-30 (UTC), approximately 00:40-01:10. Year filters: `yearFrom` 2024 or 2025 as noted per query.

### Query log

Every search attempted, including those that did not run. "NOT RUN" means the provider did not execute the query; it is not an empty result.

| # | Time (UTC, 2026-09-30) | Tool / provider | Query / call | Outcome |
|---|---|---|---|---|
| 1 | ~00:40 | wisdevListProviders | - | 21 providers; openalex and semantic_scholar flagged prior_failure |
| 2 | ~00:40 | wisdevSearchPapers, sources=[arxiv], 2024+ | agent harness scaffold engineering coding agent performance | NOT RUN: SEARCH_PROVIDERS_GATED_BY_TIER (arxiv above tier) |
| 3 | ~00:40 | wisdevSearchPapers, sources=[dblp] | agent harness scaffold coding agent performance | NOT RUN: gated by tier |
| 4 | ~00:40 | wisdevSearchPapers, sources=[crossref] | same | NOT RUN: gated by tier |
| 5 | ~00:40 | wisdevSearchPapers, default sources, 2024+ | same | openalex answered 15 (all off-topic); semantic_scholar 429 |
| 6 | ~00:41 | scholarlmPaperclipSearch, source=arxiv | coding agent harness scaffold ablation SWE-bench | NOT RUN: Paperclip not configured (PAPERCLIP_API_KEY unset) |
| 7 | ~00:41 | scholarlmGetAuthStatus | - | anonymous, local_stdio, effectiveTier free, maxQualityMode fast |
| 8 | ~00:41 | openalex, 2024+ | SWE-bench coding agent scaffold ablation study tool design | 20 results -> AHE, DarwinX, DGM, disclosure audit, SWE-smith, SWE-Gym, Agent Laboratory, General Agent Evaluation |
| 9 | ~00:42 | openalex, 2025+ | automated harness optimization LLM agent evolution | NOT RUN: 429, retry after 32 s |
| 10 | ~00:42 | openalex, 2024+ | reward hacking coding agents modify tests benchmark | NOT RUN: provider unavailable; then 429 / rate gate deferred (retried 5 times over ~4 min, all failed) |
| 11 | ~00:43 | sources=[google_scholar] / [core] / [papers_with_code] | reward hacking coding agents modify tests benchmark | NOT RUN: all gated by tier |
| 12 | ~00:45 | sources=[semantic_scholar] | same | NOT RUN: rate gate deferred 1m43s |
| 13 | ~00:45 | wisdevPaperLookup | 2505.22954 | FAILED: arxiv 429, semantic_scholar 429, crossref "not a DOI" |
| 14 | ~00:48 | openalex, 2024+ | language model agents reward hacking test tampering evaluation | 25 results -> Denison et al., McKee-Reid et al. (rest off-topic) |
| 15 | ~00:49 | semantic_scholar, 2024+ | LLM agent evaluation variance repeated runs confidence intervals statistical significance | NOT RUN: 429 |
| 16 | ~00:50 | openalex, 2024+ | statistical significance confidence intervals variance language model evaluation benchmarks | 429, then circuit open; on retry 25 results, none relevant (clinical/medical LLM evaluations) |
| 17 | ~00:52 | openalex, 2024+ | error bars evals statistical analysis of language model evaluations | 25 results -> Miller; Menkveld et al. (rest off-topic) |
| 18 | ~00:53 | openalex, 2025+ | ablation LLM agent scaffold components tool design context management software engineering agents | NOT RUN: 429 (twice) |
| 19 | ~00:54-00:57 | openalex, 2024+ | small language models function calling tool use benchmark open-weight | NOT RUN: 429 / rate gate deferred (seven attempts) |
| 20 | ~00:57 | openalex, 2025+ | harness engineering LLM agents survey component taxonomy evaluation | NOT RUN: 429 |
| 21 | ~01:01 | openalex, 2024+ | small language models function calling tool use benchmark open-weight | NOT RUN: rate gate deferred 51 s, then 28 s (after a 4-minute wait) |
| 22 | ~01:02 | openalex, 2025+ | harness engineering LLM agents survey | NOT RUN: **429, "retry after at least 82666 seconds"** (about 23 h: daily quota exhausted) |
| 23 | ~01:02 | semantic_scholar, 2025+ | harness engineering LLM agents survey | NOT RUN: 429 |
| 24 | ~01:03 | wisdevEvidenceSearch (default routing) | claim: GPU utilization overstates hardware usage during batch-1 LLM agent inference; power draw is a better measure of energy cost | NOT RUN: routed to dblp, arxiv, papers_with_code, all gated by tier |

Successful searches: 5 (rows 5, 8, 14, 16, 17), all answered by OpenAlex; rows 5 and 16 returned nothing on-topic. After row 22 the OpenAlex quota was exhausted for the day, so no further discovery was possible through ScholarLM-local in this session. Queries planned but never run for that reason: SWE-bench validity / contamination (RQ4), preregistration in ML (RQ5), GPU power vs utilization in LLM inference (RQ6).

## 2. Accepted additions

Each entry below passed the paper's Appendix B standard: title, author list and year agree between the ScholarLM-local search record and a page fetched independently (arXiv abstract page, or the Crossref REST record), and the one-line summary is grounded in the abstract text as fetched from that independent page, not in the provider's abstract. Every provider record here came from OpenAlex through ScholarLM-local; where the provider's author metadata disagreed with the source of record, the BibTeX follows the source of record and the discrepancy is stated.

The paper has been converted to pandoc keys while this review ran (e.g. `@lee2026metaharness`, `@diciccio1996bootstrap`, `@ernst2023registered`), so proposed edits and Appendix B rows use keys. BibTeX follows the house style of `bench/paper/references.bib`: ASCII only, TeX escapes, double-braced titles, and the arXiv ID repeated in `howpublished` because the bibliography style does not print `eprint`.

---

### A1. `lin2026ahe` -- Agentic Harness Engineering (automated harness evolution, with component ablations)

```bibtex
@misc{lin2026ahe,
  author        = {Lin, Jiahang and Liu, Shichun and Pan, Chengjun and Lin, Lizhi and Dou, Shihan and Xi, Zhiheng and Huang, Xuanjing and Yan, Hang and Han, Zhenhua and Gui, Tao and Jiang, Yu-Gang},
  title         = {{Agentic Harness Engineering: Observability-Driven Automatic Evolution of Coding-Agent Harnesses}},
  year          = {2026},
  howpublished  = {arXiv preprint arXiv:2604.25850},
  eprint        = {2604.25850},
  archivePrefix = {arXiv},
  doi           = {10.48550/arXiv.2604.25850}
}
```

- **Verification.** Fetched `https://arxiv.org/abs/2604.25850` (HTML meta tags + abstract block). Title matches exactly. Eleven authors on both records, same order and surnames. Year 2026 on both (v1 28 Apr 2026, v4 18 May 2026). **Discrepancy:** the OpenAlex record gives the first author as "Jiaxu Lin"; the arXiv page gives "Jiahang Lin". The BibTeX uses the arXiv spelling. No journal-ref.
- **What the source says (from the fetched abstract).** A closed loop that evolves coding-agent harness components, each given a file-level representation, with every edit paired with a prediction checked in the next round; ten iterations raise Terminal-Bench 2 pass@1 from 69.7% to 77.0%, the frozen harness transfers across three other model families (+5.1 to +10.1 pp), and ablations "localize the gain to tools, middleware, and long-term memory rather than the system prompt".
- **Where it belongs.** (a) §2.1, second paragraph, after the sentence "Two systems automate its construction on different objectives -- AutoHarness ... while Self-Harness has the harness revise itself [@zhang2026selfharness]." (b) §6, "The component that does not generalise", after the sentence "...a harness ablation reported on one model is a statement about that model...".
- **Proposed edits.**
  - (a) "A third, Agentic Harness Engineering, evolves a coding-agent harness whose components are each exposed as a separately editable file, and its ablations attribute the gain to tools, middleware and long-term memory rather than to the system prompt [@lin2026ahe]. That is a component-level attribution, but it is made within one search trajectory, not a paired one-factor ablation with the model and seeds held fixed."
  - (b) "This does not contradict reports that an evolved harness transfers across model families as a bundle [@lin2026ahe]: our full-versus-baseline effect also has the same sign and a similar size on all three arms (§5.4, §5.7). What does not transfer here is the per-component effect."
- **Appendix B row.**
  `| @lin2026ahe | arXiv:2604.25850 | Verified -- title, all eleven authors, submitted 28 April 2026 (v4 18 May 2026). The search provider's record gives the first author's given name as "Jiaxu"; the arXiv page gives "Jiahang", which is used. Abstract confirms the ablation attribution to tools, middleware and memory cited in §2.1. |`

---

### A2. `zhang2026darwinx` -- DarwinX (population-based harness evolution, frozen model)

```bibtex
@misc{zhang2026darwinx,
  author        = {Zhang, Yifan and Dai, Yutong and Tan, Juntao and Yang, Luyu and Mullur, Rishi and Hoang, Thai and Hu, Zhiyuan and Zhu, James and Mui, Phil and Savarese, Silvio and Xu, Ran and Chen, Zeyuan},
  title         = {{DarwinX: Evolving Agent Harnesses Through Natural Selection}},
  year          = {2026},
  howpublished  = {arXiv preprint arXiv:2608.07545},
  eprint        = {2608.07545},
  archivePrefix = {arXiv},
  doi           = {10.48550/arXiv.2608.07545}
}
```

- **Verification.** Fetched `https://arxiv.org/abs/2608.07545`. Title, all twelve authors (same order and spelling as the OpenAlex record) and year (v1 31 Jul 2026) match. No journal-ref.
- **What the source says.** Treats harness self-evolution as selection over a population of harnesses (prompts, tools, skills, control flow) with the model frozen, admitting only variants that extend coverage without regressing, with fitness taken from each benchmark's own verifier; reports about +17 points on average across four benchmarks and transfer of a Terminal-Bench 2.1 harness to SWE-bench Verified.
- **Where it belongs.** §2.1, second paragraph, same place as A1 (the automated-construction sentence), and optionally §2.4 as the sole mention of verifier-derived fitness.
- **Proposed edit.** "Population-based search over harnesses with the model frozen reaches the same conclusion from the search side -- 'an LLM agent's capability depends not only on model weights but on its harness' -- and takes its fitness from each benchmark's own verifier [@zhang2026darwinx]. When the verifier is the fitness function, whether the agent can reach the verifier is no longer only an evaluation concern (§2.4)."
- **Appendix B row.**
  `| @zhang2026darwinx | arXiv:2608.07545 | Verified -- title, all twelve authors, submitted 31 July 2026. Abstract confirms the frozen-model population search and verifier-derived fitness cited in §2.1. |`

---

### A3. `zhang2025dgm` -- Darwin Goedel Machine (self-modifying coding agents)

```bibtex
@misc{zhang2025dgm,
  author        = {Zhang, Jenny and Hu, Shengran and Lu, Cong and Lange, Robert and Clune, Jeff},
  title         = {{Darwin {G}\"{o}del Machine: Open-Ended Evolution of Self-Improving Agents}},
  year          = {2025},
  howpublished  = {arXiv preprint arXiv:2505.22954},
  eprint        = {2505.22954},
  archivePrefix = {arXiv},
  doi           = {10.48550/arXiv.2505.22954}
}
```

- **Verification.** The provider returned a record under DOI `10.70777/si.v2i3.15063`. Fetched that DOI from Crossref: title and all five authors match, but the container is "SuperIntelligence - Robotics - Safety & Alignment" from publisher "Carlson Research LLC" (2025-07-20). We do **not** cite that record: we could not establish that it is a peer-reviewed venue. The arXiv ID was **not** in the provider record; it was located separately and then confirmed against both the Crossref DOI record and the arXiv page. Fetched `https://arxiv.org/abs/2505.22954`: title ("Darwin Godel Machine" in arXiv's ASCII metadata), all five authors in the same order, v1 29 May 2025 (v3 12 Mar 2026). No journal-ref.
- **What the source says.** A system that iteratively rewrites its own coding-agent code, keeps an archive of generated agents, and validates each change on coding benchmarks; it "automatically improves its coding capabilities (e.g., better code editing tools, long-context window management, peer-review mechanisms)", raising SWE-bench from 20.0% to 50.0% and Polyglot from 14.2% to 30.7%, with sandboxing and human oversight.
- **Where it belongs.** §2.1, end of the second paragraph (after the Self-Harness discussion), or §2.2 after "Each of these is a claim about which component pays".
- **Proposed edit.** "Open-ended self-modification of coding agents rediscovers components of the kind our flags switch -- 'better code editing tools, long-context window management, peer-review mechanisms' [@zhang2025dgm], loosely the territory of `nativetools`, `outcap` and the checklist/gate pair -- but reports them as the output of a search, not as separately measured effects."
- **Appendix B row.**
  `| @zhang2025dgm | arXiv:2505.22954 | Verified -- title, all five authors, submitted 29 May 2025 (v3 12 March 2026). A Crossref record (10.70777/si.v2i3.15063, Carlson Research LLC) carries the same title and authors; it is not cited because its peer-review status could not be established. Abstract confirms the discovered components listed in §2.1. |`

---

### A3b. `bandel2026general` -- General Agent Evaluation (factorial over agent architectures x backbones)

```bibtex
@misc{bandel2026general,
  author        = {Bandel, Elron and Yehudai, Asaf and Eden, Lilach and Sagron, Yehoshua and Perlitz, Yotam and Venezian, Elad and Razinkov, Natalia and Ergas, Natan and Ifergan, Shlomit Shachor and Shlomov, Segev and Jacovi, Michal and Choshen, Leshem and Ein-Dor, Liat and Katz, Yoav and Shmueli-Scheuer, Michal},
  title         = {{General Agent Evaluation}},
  year          = {2026},
  howpublished  = {arXiv preprint arXiv:2602.22953},
  eprint        = {2602.22953},
  archivePrefix = {arXiv},
  doi           = {10.48550/arXiv.2602.22953},
  note          = {Presented at the ICLR 2026 Workshop on Agents in the Wild}
}
```

- **Verification.** Fetched `https://arxiv.org/abs/2602.22953`. Title, all fifteen authors in the same order, year (v1 26 Feb 2026, v2 11 May 2026) match the OpenAlex record (OpenAlex writes "Shlomit Shachor Ifergan", arXiv "Ifergan, Shlomit Shachor" -- same person, name order only). Comments: "Presented at the ICLR 2026 Workshop on Agents in the Wild" -- a workshop presentation, not an archival proceedings paper. No journal-ref.
- **What the source says.** A full factorial of 5 agent architectures x 5 backbone models (three closed, two open-weight) x 6 benchmarks, including software engineering; "agent architecture choice swings results by up to 12pp within a single model, yet backbone model choice dominates overall performance", and the open-weight models "exhibit 'generality sinks' absent from frontier closed-source models: they consistently collapse on specific agent architectures or benchmarks".
- **Where it belongs.** (a) §2.2, second paragraph, after "All four are claims about which component pays, and none was measured by holding the rest of the system fixed and flipping one flag." (b) §6, "The component that does not generalise", after "...emitting a well-formed tool call in plain text when the native path is withdrawn -- and it is not a monotone function of anything we measured."
- **Proposed edits.**
  - (a) "A controlled comparison one level up crosses whole agent architectures with backbone models in a full factorial and finds that architecture moves results by up to 12 points within one model while the backbone dominates overall [@bandel2026general]. That design holds the model fixed, as ours does, but its unit is the architecture (tool-calling, MCP, code-generation, CLI), not a component inside one loop."
  - (b) "Model-specific sensitivity has also been reported one level up: open-weight backbones collapse on particular agent architectures where frontier closed models do not [@bandel2026general]. All three of our arms are open-weight models, and the `nativetools` sensitivity appears on only one of them, so on this evidence the sensitivity is a property of the individual model rather than of the open/closed distinction." (Reviewer note: "all three arms are open-weight" is our reading of the model names -- `gpt-oss-120b` is an open-weight release served here through an API; confirm the wording with the authors.)
- **Appendix B row.**
  `| @bandel2026general | arXiv:2602.22953 | Verified -- title, all fifteen authors, submitted 26 February 2026 (v2 11 May 2026); arXiv comments state an ICLR 2026 workshop presentation. Abstract confirms the architecture x backbone factorial, the 12 pp within-model swing and the open-weight "generality sinks" cited in §2.2 and §6. |`

---

### A4. `denison2024subterfuge` -- Sycophancy to Subterfuge (reward tampering generalises)

```bibtex
@misc{denison2024subterfuge,
  author        = {Denison, Carson and MacDiarmid, Monte and Barez, Fazl and Duvenaud, David and Kravec, Shauna and Marks, Samuel and Schiefer, Nicholas and Soklaski, Ryan and Tamkin, Alex and Kaplan, Jared and Shlegeris, Buck and Bowman, Samuel R. and Perez, Ethan and Hubinger, Evan},
  title         = {{Sycophancy to Subterfuge: Investigating Reward-Tampering in Large Language Models}},
  year          = {2024},
  howpublished  = {arXiv preprint arXiv:2406.10162},
  eprint        = {2406.10162},
  archivePrefix = {arXiv},
  doi           = {10.48550/arXiv.2406.10162}
}
```

- **Verification.** Fetched `https://arxiv.org/abs/2406.10162`. Title, all fourteen authors (same order), year (v1 14 Jun 2024, v3 29 Jun 2024) match the OpenAlex record. No journal-ref. A Crossref bibliographic query for the title returned no matching published version.
- **What the source says.** LLM assistants trained on a curriculum of increasingly gameable environments generalise from easy specification gaming (sycophancy) to rarer, more blatant forms; "a small but non-negligible proportion of the time" they generalise zero-shot to "directly rewriting their own reward function"; retraining mitigates but does not eliminate this, and harmlessness training does not prevent it.
- **Where it belongs.** §2.4, first paragraph, after the Lodkaew et al. sentence ("...and they propose randomized tests and capped evaluation as countermeasures.").
- **Proposed edit.** "The limiting case is an agent that edits the grader itself: models trained on a curriculum of gameable environments occasionally generalise zero-shot to rewriting their own reward function, and neither retraining nor harmlessness training removes the behaviour [@denison2024subterfuge]. That result concerns models trained on gameable curricula, not the off-the-shelf models evaluated here, but it is the reason §3.2 makes the grader structurally unreachable rather than asking the model not to touch it."
- **Appendix B row.**
  `| @denison2024subterfuge | arXiv:2406.10162 | Verified -- title, all fourteen authors, submitted 14 June 2024. Abstract confirms the zero-shot reward-function rewriting cited in §2.4. |`

---

### A5. `mckeereid2024honesty` -- Honesty to Subterfuge (in-context reflection alone elicits specification gaming)

```bibtex
@misc{mckeereid2024honesty,
  author        = {McKee-Reid, Leo and Str\"{a}ter, Christoph and Martinez, Maria Angelica and Needham, Joe and Balesni, Mikita},
  title         = {{Honesty to Subterfuge: In-Context Reinforcement Learning Can Make Honest Models Reward Hack}},
  year          = {2024},
  howpublished  = {arXiv preprint arXiv:2410.06491},
  eprint        = {2410.06491},
  archivePrefix = {arXiv},
  doi           = {10.48550/arXiv.2410.06491}
}
```

- **Verification.** Fetched `https://arxiv.org/abs/2410.06491`. Title, five authors in the same order, year (v1 9 Oct 2024) match. **Discrepancy:** OpenAlex gives the fourth author as "Jon Needham"; arXiv gives "Joe Needham", which is used. No journal-ref.
- **What the source says.** Frontier models (gpt-4o, gpt-4o-mini, o1-preview, o1-mini) "can engage in specification gaming without training on a curriculum of tasks, purely from in-context iterative reflection"; the abstract's own summary of prior work names "modifying task checklists to appear more successful" as one such behaviour.
- **Where it belongs.** §2.3, after "...their failure output -- not the model's own reflection -- is what re-enters the context." Alternatively §2.4, second paragraph, after the sentence about "the open question in @kouremetis2026cheats".
- **Proposed edit.** "This is not a cosmetic difference. A loop that returns failures to the model and asks it to try again gives the model exactly the kind of iterative in-context feedback that has been shown to elicit specification gaming from frontier models even without training [@mckeereid2024honesty]. A gate that feeds back test failures therefore has to be paired with a grader the agent cannot edit; ours is (§3.2), and Appendix D is the case where that pairing mattered."
- **Appendix B row.**
  `| @mckeereid2024honesty | arXiv:2410.06491 | Verified -- title, all five authors, submitted 9 October 2024. The search provider's record gives the fourth author as "Jon Needham"; the arXiv page gives "Joe Needham", which is used. Abstract confirms specification gaming from in-context iterative reflection, as cited in §2.3. |`

---

### A6. `miller2024errorbars` -- Adding Error Bars to Evals

```bibtex
@misc{miller2024errorbars,
  author        = {Miller, Evan},
  title         = {{Adding Error Bars to Evals: A Statistical Approach to Language Model Evaluations}},
  year          = {2024},
  howpublished  = {arXiv preprint arXiv:2411.00640},
  eprint        = {2411.00640},
  archivePrefix = {arXiv},
  doi           = {10.48550/arXiv.2411.00640}
}
```

- **Verification.** Fetched `https://arxiv.org/abs/2411.00640`. Title, single author, year (v1 1 Nov 2024) match. No journal-ref.
- **What the source says.** "Evaluations are experiments", yet the evaluation literature "has largely ignored the literature from other sciences on experiment analysis and planning"; it gives formulas for analysing evaluation data, "measuring differences between two models", and planning an evaluation, with reporting recommendations that minimise statistical noise.
- **Where it belongs.** §2.6, first paragraph, the sentence "...percentile bootstrap confidence intervals throughout following the standard treatment [@diciccio1996bootstrap]...". Also usable in §4.5 at "...following the standard treatment of bootstrap intervals [@diciccio1996bootstrap]."
- **Proposed edit.** "...percentile bootstrap confidence intervals throughout following the standard treatment [@diciccio1996bootstrap], in line with the argument that language-model evaluations are experiments and should be analysed and planned as such, including when the quantity of interest is a difference between two systems [@miller2024errorbars]..."
- **Appendix B row.**
  `| @miller2024errorbars | arXiv:2411.00640 | Verified -- title, sole author E. Miller, submitted 1 November 2024. Abstract confirms the evaluations-as-experiments framing cited in §2.6. |`

---

### A7. `menkveld2024nonstandard` -- Nonstandard Errors (analysis choices add uncertainty)

```bibtex
@article{menkveld2024nonstandard,
  author  = {Menkveld, Albert J. and Dreber, Anna and Holzmeister, Felix and Huber, Juergen and Johannesson, Magnus and Kirchler, Michael and others},
  title   = {{Nonstandard Errors}},
  journal = {The Journal of Finance},
  year    = {2024},
  volume  = {79},
  number  = {3},
  pages   = {2339--2390},
  doi     = {10.1111/jofi.13337}
}
```

- **Verification.** Fetched `https://api.crossref.org/works/10.1111/jofi.13337` (twice: once via WebFetch for title/venue/abstract, once parsed as JSON to count authors). Title, first author (A. J. Menkveld), venue, volume 79, issue 3, pages 2339-2390, year 2024 (online 17 Apr 2024) match the OpenAlex record. **Author count on Crossref: 343**; the BibTeX lists the first six and `others` (first author + count, per the verification standard). Crossref spells the fourth author "Juergen Huber"; the ASCII form is kept.
- **What the source says.** Evidence-generating-process variation across researchers adds uncertainty ("nonstandard errors"); when 164 teams tested the same hypotheses on the same data, those errors were sizable, smaller for more reproducible research, reduced by peer-review stages, and underestimated by participants.
- **Where it belongs.** §2.6, second paragraph, after "...reviewing a protocol before results exist removes the degrees of freedom that outcome-dependent analysis introduces." (the @ernst2023registered sentence)
- **Proposed edit.** "How large those degrees of freedom are has been measured directly outside our field: when 164 teams tested the same hypotheses on the same data, the dispersion of their answers -- 'nonstandard errors' -- was sizable and was underestimated by the teams themselves [@menkveld2024nonstandard]. Fixing the estimator, the interval and the multiplicity correction before the first episode is our defence against the same dispersion."
- **Appendix B row.**
  `| @menkveld2024nonstandard | doi:10.1111/jofi.13337 | Verified via Crossref -- *The Journal of Finance*, vol. 79, no. 3, pp. 2339-2390, 2024; first author A. J. Menkveld, 343 authors on the Crossref record (cited as first six *et al.*). Abstract confirms the 164-team same-data design cited in §2.6. |`

---

### A8. `moghadasi2026disclose` -- disclosure audit of agent benchmark papers

```bibtex
@misc{moghadasi2026disclose,
  author        = {Moghadasi, Mahdi Naser and Ghaderi, Faezeh},
  title         = {{What Twelve {LLM} Agent Benchmark Papers Disclose About Themselves: A Pilot Audit and an Open Scoring Schema}},
  year          = {2026},
  howpublished  = {arXiv preprint arXiv:2605.21404},
  eprint        = {2605.21404},
  archivePrefix = {arXiv},
  doi           = {10.48550/arXiv.2605.21404}
}
```

- **Verification.** Fetched `https://arxiv.org/abs/2605.21404`. Title, both authors, year (v1 20 May 2026) match. Comments field: "Submission to IEEE Big Data 2026" -- i.e. not yet published. No journal-ref.
- **What the source says.** A single-auditor pilot audit of twelve benchmark papers (eight agent, four static) on five disclosure fields, motivated by two papers reporting the same benchmark and model yet disagreeing "and you cannot tell why -- the scaffold, the sampling settings, the subset, or the evaluator version"; mean disclosure 0.38 for agent papers vs 0.66 for static, with none of the eight agent papers disclosing inference cost and none fully disclosing a content-addressed container image of the evaluation environment.
- **Where it belongs.** §6, "What the architecture contributes", after "...are all recorded per episode or computed per report." (or §2.6 after the sentence about the serialized `protocol` object).
- **Proposed edit.** "A pilot audit of agent-benchmark papers found that none of eight disclosed inference cost in any form and none fully disclosed a content-addressed image of the evaluation environment, and it names the scaffold and the sampling settings among the things a reader cannot recover [@moghadasi2026disclose]. Serializing the protocol object into every result file is our answer to that specific gap; it is a single-auditor pilot and we cite it as such."
- **Appendix B row.**
  `| @moghadasi2026disclose | arXiv:2605.21404 | Verified -- title, both authors, submitted 20 May 2026; arXiv comments state a submission to IEEE Big Data 2026, not a publication. Abstract confirms the cost and harness-specification disclosure gaps cited in §6. |`

## 3. Existing references: published versions

Checked each arXiv-only reference's abstract page for a Journal-ref / Comments field, then queried Crossref (`api.crossref.org/works?query.bibliographic=<title>`) for a published record. DBLP and OpenReview could not be used (see Method). Report only; nothing in the paper was edited.

| Ref | Current identifier | Finding | Evidence | Status |
|---|---|---|---|---|
| @liu2024survey Liu et al. survey | arXiv:2409.02977 | **Published:** *ACM Transactions on Software Engineering and Methodology*, article 3796507, doi:10.1145/3796507, published online 5 March 2026 (received 25 Sep 2024, accepted 31 Oct 2025). Same title; same seven authors in the same order (J. Liu, K. Wang, Y. Chen, X. Peng, Z. Chen, L. Zhang, Y. Lou). | `https://api.crossref.org/works/10.1145/3796507`; arXiv comments field reads "Accepted by TOSEM" (v2, 3 Dec 2025). | **Verified.** Recommend citing the TOSEM version, keeping the arXiv ID as a preprint note. |
| @shen2024smallllms Shen et al. | arXiv:2401.07324 | **Published:** *Proceedings of the 2024 Conference on Empirical Methods in Natural Language Processing*, pp. 16658-16680, doi:10.18653/v1/2024.emnlp-main.929. Same title; same eight authors in the same order. | `https://api.crossref.org/works/10.18653/v1/2024.emnlp-main.929`. The arXiv page has no journal-ref (last version v3, 16 Feb 2024, comments "On progress"). | **Verified.** Recommend citing the EMNLP 2024 version. |
| @wang2024openhands OpenHands | arXiv:2407.16741 | arXiv comments field: "Accepted by ICLR 2025". ICLR proceedings carry no DOI and the OpenReview page returned a browser-verification challenge to WebFetch, so the venue could not be confirmed at the publisher. | `https://arxiv.org/abs/2407.16741` (latest version 18 Apr 2025). | **Author-stated, not independently verified.** If cited as ICLR 2025, say so on the strength of the arXiv comment only; do not invent a proceedings DOI. |
| @aleithan2024swebenchplus SWE-Bench+ | arXiv:2410.06992 | Crossref has *"SWE-Bench+: Enhanced LLM Coding Benchmark"*, Proc. 3rd ACM Int. Conf. on AI-Powered Software (AIware 2026), pp. 332-339, doi:10.1145/3805760.3814924, 5 Jul 2026. **The title differs** ("Enhanced LLM Coding Benchmark" vs "Enhanced Coding Benchmark for LLMs") **and so does the author list**: H. Xue, R. Aleithan, N. Enan, G. Uddin, S. Wang (first author changed; M. M. Mohajer and E. Nnorom absent; N. Enan added). Crossref has no abstract for it. | `https://api.crossref.org/works/10.1145/3805760.3814924`; arXiv page shows no journal-ref (v2, 10 Oct 2024). | **Related, not verified as the same work.** Do not substitute. If the authors want the published form, they need to confirm it reports the same audit (solution leakage, weak tests) before citing it for §2.4's claim. |
| @lee2026metaharness Meta-Harness | arXiv:2603.28052 | No journal-ref (v1 only, 30 Mar 2026); Crossref title query returned nothing matching. | arXiv abs page; Crossref query. | No published version found. |
| @merrill2026terminalbench Terminal-Bench | arXiv:2601.11868 | No journal-ref (v1 only, 17 Jan 2026); Crossref title query returned nothing matching. | arXiv abs page; Crossref query. | No published version found. |
| @lodkaew2026deceive Lodkaew et al. | arXiv:2606.07379 | No journal-ref (v2, 8 Jun 2026); Crossref title query returned nothing matching. | arXiv abs page; Crossref query. | No published version found. |
| @kouremetis2026cheats Kouremetis et al. | arXiv:2607.21763 | No journal-ref (v1, 23 Jul 2026); Crossref title query returned nothing matching. | arXiv abs page; Crossref query. | No published version found. |
| @nicholson2026drift Nicholson | arXiv:2601.19934 | No journal-ref (v1, 12 Jan 2026); Crossref title query returned nothing matching. | arXiv abs page; Crossref query. | No published version found. |
| @chen2023selfdebug Self-Debug | arXiv:2304.05128 | No journal-ref or comments (v2, 5 Oct 2023). A Crossref title query first returned HTTP 429; retried later, it returned nothing matching (ICLR proceedings carry no Crossref DOIs, so this is expected either way). | arXiv abs page; Crossref query. | No published version found in Crossref; a conference venue was not checked (OpenReview inaccessible). |
| @siegel2024corebench CORE-Bench | arXiv:2409.11363 | No journal-ref (v2, 22 Jun 2026); Crossref title query returned nothing matching. | arXiv abs page (fetched twice); Crossref query. | No published version found. |
| @fourney2024magenticone Magentic-One | arXiv:2411.04468 | No journal-ref (v1, 7 Nov 2024); two Crossref queries returned nothing matching. | arXiv abs page; Crossref queries. | No published version found. |
| @zhou2026externalization Externalization review | arXiv:2604.08224 | No journal-ref (v1, 9 Apr 2026); Crossref title query returned nothing matching. | arXiv abs page; Crossref query. | No published version found. |
| @lou2026autoharness AutoHarness | arXiv:2603.03329 | No journal-ref (v1, 10 Feb 2026); Crossref title query returned nothing matching. | arXiv abs page; Crossref query. | No published version found. |
| @zhang2026selfharness Self-Harness | arXiv:2606.09498 | No journal-ref; now at v3 (20 Aug 2026; v2 12 Aug 2026). Re-checked: the current (v3) abstract still contains the sentence quoted in §2.1 verbatim, and still lists eight authors. | arXiv abs page, parsed. | No published version found; quote still valid. |
| @xia2024agentless / @xia2025demystifying Agentless | arXiv:2407.01489 / doi:10.1145/3715754 | Crossref confirms @xia2025demystifying: *Proc. ACM Softw. Eng.*, 19 Jun 2025, same four authors. Already cited in both forms. | Crossref query. | No change. |
| @ernst2023registered Ernst & Baldassarre | doi:10.1007/s10664-022-10277-5 | Crossref re-confirms title, both authors, *Empirical Software Engineering*, March 2023. | Crossref query. | No change. |

## 4. Rejected or not-pursued candidates

| Candidate (as returned) | Identifier | Reason |
|---|---|---|
| Darwin Goedel Machine, Crossref record in "SuperIntelligence - Robotics - Safety & Alignment" (Carlson Research LLC) | doi:10.70777/si.v2i3.15063 | Metadata matches the arXiv paper, but the venue's peer-review status could not be established. Rejected **as a version**; the arXiv record is accepted instead (A3). |
| SWE-Bench+: Enhanced LLM Coding Benchmark (AIware 2026) | doi:10.1145/3805760.3814924 | Not a search hit; found while checking @aleithan2024swebenchplus. Different title and author list from @aleithan2024swebenchplus; no abstract on Crossref to confirm same content. See section 3. |
| SWE-smith (Yang et al., 2025); Training SWE Agents and Verifiers with SWE-Gym (Pan et al., 2024); SWE-bench Multimodal (Yang et al., 2024) | doi:10.52202/085713-3239; arXiv:2412.21139; arXiv:2410.03859 | On-topic for SWE agents but about training data or new benchmarks, not about harness components, validity, or statistics. Not needed for any sentence in §2; not verified. |
| MLE-bench (Chan et al., 2024); Agent Laboratory (Schmidgall et al., 2025); BioAgent Bench (2026); Vibe Code Bench (2026) | arXiv:2410.07095; doi:10.18653/v1/2025.findings-emnlp.320; arXiv:2601.21800; doi:10.1145/3786335.3813180 | New benchmarks / applications; no claim in §2 they would support. Not verified. |
| Agentic Software Engineering: Foundational Pillars and a Research Roadmap (Hassan et al., 2025) | arXiv:2509.06216 | Position/roadmap paper; §2.2 already cites a survey @liu2024survey. Not verified. |
| Evaluating Goal Drift in Language Model Agents (Arike et al., AIES 2025) | doi:10.1609/aies.v8i1.36541 | Long-horizon goal adherence; tangential to the loop-breaking flag but the paper makes no goal-drift claim. Not pursued. |
| Chatbot Arena (Chiang et al., 2024) | arXiv:2403.04132 | Human-preference leaderboard; not relevant to pass-rate statistics. |
| Reasoning Models Don't Always Say What They Think (Chen et al., 2025) | arXiv:2505.05410 | Chain-of-thought faithfulness; the paper does not rely on reasoning-channel content as evidence. Not pursued. |
| Beyond Reward Hacking: Causal Rewards (Wang et al., 2025) | arXiv:2501.09620 | Reward-model training method; not about agents gaming graders. |
| Reward-hacking / security surveys (Deng et al. 2025, Park et al. 2024 "AI deception", Wang et al. 2025) | doi:10.1145/3716628; doi:10.1016/j.patter.2024.100988; doi:10.1145/3764113 | General surveys; §2.4 is better served by the two primary studies accepted (A4, A5). |
| "Harness Engineering for LLM Agents: A Survey of Harness Component Taxonomy, Evaluation, and Model-Harness Coevolution" (Li, Wu, Chang; Preprints.org, 30 Jun 2026) and "Harness-Aware Evaluation of LLM Agents: Systems, Benchmarks, and Protocols" (Radwan, Vasilakos, Raza; Preprints.org, 24 Sep 2026) | doi:10.20944/preprints202606.2203.v1; doi:10.20944/preprints202609.2140.v1 | **Leads only, not accepted.** Surfaced incidentally in Crossref title queries run to check existing references, not by the ScholarLM-local search, so they fall outside this review's discovery method; titles look directly on-topic for §2.1. Neither was verified beyond the Crossref listing (no abstract read, no second source). Worth a targeted search and verification. |
| Remaining OpenAlex hits (clinical LLM evaluation, education, 6G, oncology, protein design, etc.) | - | Off-topic retrieval noise from OpenAlex relevance ranking. |

## 5. Gaps by research question

| RQ | Coverage | Status |
|---|---|---|
| 1. Harness / scaffold engineering; automated harness search beyond Meta-Harness, AutoHarness, Self-Harness | A1 (AHE), A2 (DarwinX), A3 (DGM), A3b (General Agent Evaluation) | **Covered.** Four verified additions, three of them 2026. Two on-topic harness-engineering survey preprints surfaced outside the search method and are listed as unverified leads in section 4. |
| 2. Component-level / one-factor ablations of agent scaffolds | A1 (ablation attribution inside a search loop) and A3b (architecture x backbone factorial) are the nearest verified work. | **Partial.** Nothing verifiable was found that ablates individual inner-loop components (output truncation, verification gate, loop detection, checklist) one at a time with the model held fixed. The dedicated query (log #18) never ran because of rate limits, so this is a gap in the search, not evidence that no such study exists. |
| 3. Small / local open-weight models as coding agents; small-model tool use | A3b reports open-weight "generality sinks". The existing @shen2024smallllms is now verified as EMNLP 2024 (section 3). | **Gap in search.** The dedicated query (log #19) failed on every attempt. No new small-model tool-use paper was verified. |
| 4. Reward hacking / test tampering / benchmark validity | A4 (Denison et al.), A5 (McKee-Reid et al.). SWE-Bench+ published-version question in section 3. | **Partial.** Two verified primary studies on reward tampering / specification gaming. No new SWE-bench-validity paper was verified; no query specific to SWE-bench validity or contamination ran successfully. |
| 5. Statistical rigour: variance, seeds, bootstrap CIs, multiple comparisons, preregistration | A6 (Miller), A7 (Menkveld et al.), A8 (Moghadasi & Ghaderi). | **Partial.** Nothing verified specifically on multiple-comparison correction or preregistration *in ML*; the one query aimed at LLM-evaluation variance returned only clinical/medical evaluation papers. |
| 6. Energy / compute measurement of agentic inference (GPU utilization vs power) | none | **Gap in search.** No query on this topic ran successfully. Appendix C remains without an external citation for the utilization-versus-power point. |

**Recommended follow-up.** Re-run the queries that never executed (log rows marked NOT RUN), ideally with an authenticated ScholarLM tier that admits `arxiv`, `dblp` and `crossref`, or with an OpenAlex polite-pool / API key configured for the local server. RQ2, RQ3 and RQ6 are where the missing coverage is most likely to matter for reviewers.
