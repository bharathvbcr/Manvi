import sys
from cache import TTLCache

bad = 0
def eq(label, got, want):
    global bad
    if got != want:
        print("FAIL", label, "got", repr(got), "want", repr(want)); bad += 1

class Clock:
    def __init__(self, t=1000.0): self.t = t
    def __call__(self): return self.t

# 1. get() must refresh recency
c = Clock(); k = TTLCache(2, 10, c)
k.put("a", 1); k.put("b", 2); k.get("a"); k.put("c", 3)
eq("lru: b evicted", k.get("b"), None)
eq("lru: a kept", k.get("a"), 1)
eq("lru: c kept", k.get("c"), 3)

# 2. put() on an existing key also refreshes recency
c = Clock(); k = TTLCache(2, 100, c)
k.put("a", 1); k.put("b", 2); k.put("a", 9); k.put("c", 3)
eq("put refresh: b evicted", k.get("b"), None)
eq("put refresh: a kept", k.get("a"), 9)

# 3. a missing get is not a use
c = Clock(); k = TTLCache(2, 100, c)
k.put("a", 1); k.put("b", 2); k.get("zzz"); k.put("c", 3)
eq("miss not a use", k.get("a"), None)
eq("miss keeps b", k.get("b"), 2)

# 4. expiry
c = Clock(); k = TTLCache(5, 10, c)
k.put("a", 1); c.t += 9
eq("not yet expired", k.get("a"), 1)
c.t += 1
eq("expired at ttl", k.get("a"), None)

# 5. put resets age
c = Clock(); k = TTLCache(5, 10, c)
k.put("a", 1); c.t += 8; k.put("a", 2); c.t += 8
eq("age reset by put", k.get("a"), 2)

# 6. expired entries do not occupy capacity
c = Clock(); k = TTLCache(2, 10, c)
k.put("a", 1); k.put("b", 2); c.t += 20
k.put("c", 3); k.put("d", 4)
eq("expired freed: c", k.get("c"), 3)
eq("expired freed: d", k.get("d"), 4)
eq("len after expiry", len(k), 2)

# 7. len counts only live entries
c = Clock(); k = TTLCache(5, 10, c)
k.put("a", 1); k.put("b", 2)
eq("len live", len(k), 2)
c.t += 20
eq("len all expired", len(k), 0)

# 8. capacity is respected under churn
c = Clock(); k = TTLCache(3, 1000, c)
for i in range(10):
    k.put(f"k{i}", i)
eq("len capped", len(k), 3)
eq("oldest gone", k.get("k0"), None)
eq("newest kept", k.get("k9"), 9)

# 9. capacity of 1
c = Clock(); k = TTLCache(1, 1000, c)
k.put("a", 1); k.put("b", 2)
eq("cap1 a gone", k.get("a"), None)
eq("cap1 b kept", k.get("b"), 2)

# 10. clock stays injectable
c = Clock(); k = TTLCache(2, 5, c)
k.put("x", 1); c.t += 5
eq("injected clock honoured", k.get("x"), None)

sys.exit(1 if bad else 0)
