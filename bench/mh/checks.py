"""The one `check()` the bench suites share.

Every suite counts its assertions in PASS/FAIL and ends with a summary line whose
first number verify.sh reads as "how many checks ran", so the output shape here
is a contract: `  ok   <name>` / `  FAIL <name>  <detail>` per check, nothing
that looks like a count anywhere but the suite's own last line.
"""

PASS, FAIL = [], []


def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print(f"  {'ok  ' if cond else 'FAIL'} {name}"
          + (f"  {detail}" if not cond and detail else ""))
