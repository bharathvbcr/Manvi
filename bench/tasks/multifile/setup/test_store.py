from store.db import DocStore

def main():
    s = DocStore()
    a = s.add("the quick brown fox")
    s.add("a slow brown dog")
    assert s.search("brown") == [a, a + 1]
    s.update(a, "a red bird")
    assert s.search("quick") == [], s.search("quick")
    assert s.search("red") == [a]
    print("ok")

main()
