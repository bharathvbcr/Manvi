from cache import TTLCache


class Clock:
    def __init__(self): self.t = 1000.0
    def __call__(self): return self.t


def main():
    c = Clock()
    cache = TTLCache(capacity=2, ttl=10, clock=c)
    cache.put("a", 1)
    cache.put("b", 2)
    assert cache.get("a") == 1
    cache.put("c", 3)            # "b" is least recently used, so "b" goes
    assert cache.get("b") is None, "evicted the wrong entry"
    assert cache.get("a") == 1
    assert cache.get("c") == 3
    print("ok")


main()
