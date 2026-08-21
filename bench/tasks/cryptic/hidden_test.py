import sys
from app.invoice import invoice_total, line_total
from app.config import load, DEFAULTS

bad = 0
items = [{"qty": 2, "unit_price": 10.0}, {"qty": 1, "unit_price": 5.0}]
checks = [
    (invoice_total(items), 27.0),
    (invoice_total(items, {"include_tax": False}), 25.0),
    (invoice_total([]), 0),
    (invoice_total(items, {"tax_rate": 0.0}), 25.0),
    (invoice_total([{"qty": 3, "unit_price": 1.5}], {"include_tax": False}), 4.5),
]
for got, want in checks:
    if got != want:
        print("FAIL", got, "want", want); bad += 1

# The public config name must still be include_tax, not renamed to dodge the bug.
if "include_tax" not in DEFAULTS:
    print("FAIL: include_tax was removed from DEFAULTS"); bad += 1
if "include_taxes" in DEFAULTS:
    print("FAIL: config was renamed to include_taxes instead of fixing the caller"); bad += 1
try:
    load({"include_tax": False})
except Exception as e:
    print("FAIL: load no longer accepts include_tax:", e); bad += 1
sys.exit(1 if bad else 0)
