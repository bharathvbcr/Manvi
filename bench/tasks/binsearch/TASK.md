`test_search.py` hangs instead of passing.

Fix `searchlib.py` so that `search_range(nums, target)` returns a tuple
`(first, last)` of the first and last indices at which `target` occurs in the
sorted list `nums`, both inclusive, or `(-1, -1)` when the target is absent.

Run the test with `python3 test_search.py`. Do not modify the test.
