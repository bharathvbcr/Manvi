import sys
from report.pipeline import build_report
from report.aggregate import by_category, grand_total
from report.loader import load
from report.normalize import to_dollars, normalise_rows

bad = 0
def eq(label, got, want):
    global bad
    if got != want:
        print("FAIL", label, "got", repr(got), "want", repr(want)); bad += 1

out = build_report().strip().splitlines()
eq("row count", len(out), 4)
eq("alphabetical 1", out[0].split()[0], "hardware")
eq("alphabetical 2", out[1].split()[0], "meals")
eq("alphabetical 3", out[2].split()[0], "travel")
eq("hardware amount", out[0].split()[1], "2500.00")
eq("meals amount", out[1].split()[1], "54.25")
eq("travel amount", out[2].split()[1], "2149.00")
eq("total label", out[3].split()[0], "TOTAL")
eq("total", out[3].split()[1], "4703.25")

# the module contracts must still hold individually
eq("to_dollars", to_dollars(125000), 1250.0)
eq("to_dollars rounds", to_dollars(1175), 11.75)
eq("loader returns dollars", sorted(r["amount"] for r in load()),
   [11.75, 42.50, 899.00, 1250.00, 2500.00])
eq("normalise_rows still converts",
   normalise_rows([{"category": "x", "amount": 500}])[0]["amount"], 5.0)
eq("aggregate sorted", [k for k, _ in by_category(
    [{"category": "b", "amount": 1.0}, {"category": "a", "amount": 2.0}])],
   ["a", "b"])
eq("aggregate sums", dict(by_category(
    [{"category": "a", "amount": 1.5}, {"category": "a", "amount": 2.0}])),
   {"a": 3.5})
eq("grand_total", grand_total([("a", 1.5), ("b", 2.25)]), 3.75)
eq("empty aggregate", by_category([]), [])
eq("empty total", grand_total([]), 0)

sys.exit(1 if bad else 0)
