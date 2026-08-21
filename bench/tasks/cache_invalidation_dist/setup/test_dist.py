from distcache import DistCache

def main():
    c = DistCache(3)
    c.put(0, "a", "1")
    assert c.get(0, "a") == "1"
    assert c.get(1, "a") == "1"
    assert c.get(2, "a") == "1"
    c.invalidate(1, "a")
    assert c.get(1, "a") is None
    assert c.get(0, "a") is None
    print("ok")

main()
