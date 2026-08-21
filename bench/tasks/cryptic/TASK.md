`python3 test_invoice.py` fails with a `KeyError`.

Find the real cause and fix it so the test passes. The configuration key that
controls whether tax is applied is named `include_tax`, as `app/config.py`
defines it and as callers pass it — that name is correct and must not change.

Do not modify the test.
