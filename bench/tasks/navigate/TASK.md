`python3 test_report.py` fails: the expense report's totals are wrong, and the
categories come out in the wrong order.

The correct report totals `4703.25` across five ledger rows, and lists categories
alphabetically (`hardware`, `meals`, `travel`).

Fix the code in `report/` so the test passes. Keep each module's documented
responsibility: amounts are stored as integer cents in the raw ledger and are
converted to dollars exactly once on the way out. Do not modify the test.
