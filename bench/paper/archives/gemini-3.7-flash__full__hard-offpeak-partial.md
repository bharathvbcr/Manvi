# gemini-3.7-flash, full config, off-peak — PARTIAL (11 of 40 episodes)

**Not a completed cell. Do not compare against a 40-episode cell, and do not
pool with the peak `__hard` cell.**

## What this was

A deliberately time-shifted replication of the `gemini-3.7-flash__full__hard`
cell, launched 2026-08-23 05:00 CDT to test whether the endpoint's refusal rate
depends on time of day. Separate tag on purpose: mixing two demand conditions
inside one cell is the protocol-1 error this repository already has one example
of, and it would make a capacity artefact look like a harness effect.

Identical to the peak cell in every other respect: same client, same eight hard
tasks, same seeds 0-4, `max_steps=0`, `--max-wall 3600`.

## Why it stopped

At 07:41:07 CDT the Google project hit its **monthly spending cap**:

    HTTP 429: "Your project has exceeded its monthly spending cap.
               Please go to AI Studio at https://ai.studio/spend"

Every subsequent request failed regardless of model, size or pacing. The run was
killed. The three episodes written after that timestamp are billing artefacts,
not measurements, and are archived separately in
`../gemini-3.7-flash__full__hard-offpeak-spendcapped/`.

This directory holds only the 11 episodes completed **before** the cap.

## What is in here

Seed 0 complete (8 episodes) plus 3 episodes of seed 1. Pre-cap tally:
2 passes, 5 refusals (`error:ModelError`, HTTP 500 "high demand"), 2
`context_exhausted`, 2 other.

No `summary.json`: the runner was killed before writing one. Per-episode files
are complete and valid.

## What it does and does not support

It does **not** settle the time-of-day question. n=11 with an unplanned stop is
too small, and the pre-cap refusal rate (5/11) sits close enough to the peak
cell's 13/40 that nothing can be concluded either way. The properly powered test
of that question is in the completed peak cell, where refusal rate against
wall-clock hour gives Fisher exact p = 1.00 (no time effect).

Use this archive as a record of what was attempted, not as evidence.
