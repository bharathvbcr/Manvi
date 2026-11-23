## What this changes

<!-- One or two sentences. What behaviour is different after this PR? -->

Closes #

## Why

<!-- The problem being solved. If this fixes a bug, what was the root cause? -->

## Verification

**Required — every box must be genuinely checked, not assumed:**

- [ ] `./verify.sh` passes locally, or I name below every gate it reported as
      *NOT COVERED* or that I could not run, and why.
      <!-- Paste the verdict block. A run with --leg covers only that leg. -->
- [ ] Every commit carries a `Signed-off-by` line (`git commit -s`), which
      certifies the [Developer Certificate of Origin](https://developercertificate.org/).

**If this is a bug fix:**

- [ ] I added a test that **fails against the unfixed code** and passes now.
      <!-- Not "a test exists nearby" — a test that would have caught this bug. -->

**If this touches a DevCouncil component contract or a pin:**

- [ ] The change belongs in this repository (see CONTRIBUTING.md section 5).
- [ ] `scripts/module-pins.txt` is updated, and the live-contract tests ran
      against the new binary.

**Documentation:**

- [ ] The CHANGELOG has an entry under **Unreleased**, in this PR.
- [ ] User-visible behaviour is reflected in the README or `docs/`.
- [ ] No new dependency — or, if there is one, it is named below with justification.

## Anything reviewers should look at closely?

<!-- Trade-offs you made, alternatives you rejected, parts you are unsure about.
     "Nothing" is a valid answer. Flagging a weak spot is not a mark against the PR;
     it is the fastest route to a useful review. -->
