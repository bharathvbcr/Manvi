# Archive provenance notes

`bench/results/` is gitignored, so the `PROVENANCE.md` files that sit next to the
archived episode data do not survive a clone. These are copies, kept in the
tracked tree so that a reader of the paper can find out what each archived
directory is without having the data.

Each file here mirrors `bench/results/<name>/PROVENANCE.md`.

| File | What it documents |
|---|---|
| `gemini-3.7-flash__full__hard.md` | The complete 40-episode third-arm cell, excluded by declaration (§4.3) |
| `gemini-3.7-flash__full__hard-offpeak-partial.md` | 11 episodes of an interrupted time-shifted replication |
| `gemini-3.7-flash__full__hard-offpeak-spendcapped.md` | 3 episodes written after the API project hit its spending cap — not measurements |

The `*__hard-brokenwire` sweep (315 episodes, pre-fix client) has no note of its
own; it is described in §4.3 of the manuscript and in
`gemini-3.7-flash__full__hard.md` under "Client history".
