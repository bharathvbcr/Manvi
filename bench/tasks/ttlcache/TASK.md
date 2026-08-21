`python3 test_cache.py` fails: the cache evicts the wrong entry.

Fix `cache.py` so `TTLCache` satisfies this contract exactly.

- `get(key)` returns the stored value, or `None` if the key is absent or expired.
- `put(key, value)` stores the value and resets that entry's age.
- An entry is **expired** once `clock() - stored_at >= ttl`. An expired entry is
  indistinguishable from an absent one.
- A **use** is a `put`, or a `get` that returns a value. A `get` that returns
  `None` is not a use.
- The cache holds at most `capacity` live entries. When a `put` would exceed that,
  evict least-recently-used entries until it does not.
- Expired entries never occupy capacity and are never counted by `len()`.
- `len(cache)` is the number of live, unexpired entries.

`clock` is injected so this is testable; keep it that way, and keep the public API
(`TTLCache(capacity, ttl, clock=...)`, `.get`, `.put`, `len()`) unchanged.
Do not modify the test.
