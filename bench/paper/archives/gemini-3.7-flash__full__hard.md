# gemini-3.7-flash, full config — COMPLETE (40 episodes), EXCLUDED BY DECLARATION

This is a complete, valid cell: 8 tasks x 5 seeds, `max_steps=0`,
`--max-wall 3600`, run 2026-08-22 18:26 - 2026-08-23 01:09 CDT.
**18/40 passed (45.0%); 13/40 refused by the endpoint.**

It is matched by `--tag hard`, so reproducing the paper's grid requires:

    python3 compare.py --tag hard --exclude gemini --json-out results/stats-hard.json

Without `--exclude`, this cell is picked up, the interaction arms are reassigned
(weaker becomes gemini rather than Ornith) and Tables 3-8 will not match.

## Why it is excluded — see the manuscript, section 4.3

Not because of the pass rate, and not because of the error rate as such. Because
**32.5% of episodes were never served**, and the refusals are not random with
respect to the outcome:

    json_patch          0/5 passed, 5/5 refused
    pratt_parse         2/5 passed, 3/5 refused
    nfa_match           2/5 passed, 3/5 refused
    cache_invalidation  3/5 passed, 2/5 refused
    concurrency_race    5/5 passed, 0/5 refused
    ot_transform        5/5 passed, 0/5 refused
    state_machine_fuzz  0/5 passed, 0/5 refused
    ast_transformer     1/5 passed, 0/5 refused

`json_patch` is refused every single time. Qwen passes it 5/5 and Ornith 5/5, so
those five failures say nothing about this model. This paper keeps serving
errors in the denominator on the assumption that they are noise; here they are
not, so the 45% is not comparable to arms where every episode ran.

## Hypotheses tested and rejected

- **Time of day.** Refusal rate, first half vs second half of the run:
  6/20 vs 7/20. Fisher exact two-sided **p = 1.00**. No demand gradient.
- **Task size.** 13% refused below 2580B vs 44% at or above, **p = 0.080** --
  does not reach significance, and the relationship is not monotonic: the two
  largest tasks are never refused while a mid-sized one always is.
- **Content pathology.** Checked locally: `json_patch` has zero backslashes,
  zero control characters, valid UTF-8, shorter maximum line length than several
  never-refused tasks, and round-trips through the JSON serializer exactly.

The mechanism remains undemonstrated. What is established is that the censoring
is task-correlated, which is sufficient to disqualify the arm.

## Client history

An earlier sweep of this model produced zero `finished` stops across 315
episodes and is archived as `*__hard-brokenwire`. That was a defect in our own
API client (function_result sent with no preceding function_call, and no thought
signature), not a property of the model. Fixed, and covered by
`bench/test_gemini_wire.py`. None of those numbers appear in the paper.
