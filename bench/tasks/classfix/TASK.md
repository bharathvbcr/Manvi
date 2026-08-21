`python3 test_slug.py` fails.

Fix `slugify.py` so it satisfies its documented contract for **all** inputs, not
only the ones in the test:

- lowercase
- every run of characters that are not letters or digits collapses to a single `-`
- no leading or trailing `-`
- an input with no letters or digits produces the empty string

Do not modify the test.
