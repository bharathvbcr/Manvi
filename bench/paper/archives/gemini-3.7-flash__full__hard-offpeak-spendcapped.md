# Billing artefacts — NOT measurements

Three episodes written after the Google project hit its **monthly spending cap**
at 2026-08-23 07:41:07 CDT during the off-peak run.

    HTTP 429: "Your project has exceeded its monthly spending cap."

Every request in this window failed for billing reasons, independent of the
model, the harness, the task, or the request size. `json_patch.rep1`,
`nfa_match.rep1` and `ot_transform.rep1` are all recorded as
`error:ModelError`, and two of them stopped at step 1 with 0 output tokens.

**These episodes carry no information about the model or the harness.** They are
retained only so the record of the interrupted run is complete, and so that the
step-1/0-token failure shape is not mistaken for a model behaviour if anyone
reads these files later.

`ot_transform` in particular passed 5/5 in the completed peak cell and 1/1
earlier in this same off-peak run; its failure here is purely the cap.

Do not include these in any count, rate, or figure.
