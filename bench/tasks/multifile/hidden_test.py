import sys
from store.db import DocStore

bad = 0
def check(label, got, want):
    global bad
    if got != want:
        print("FAIL", label, "got", got, "want", want); bad += 1

s = DocStore()
a = s.add("the quick brown fox")
b = s.add("a slow brown dog")
check("initial", s.search("brown"), [a, b])
s.update(a, "a red bird")
check("stale token gone", s.search("quick"), [])
check("new token", s.search("red"), [a])
check("other doc intact", s.search("brown"), [b])
s.delete(b)
check("deleted", s.search("brown"), [])
check("deleted doc gone", s.search("slow"), [])
# update to text sharing a token with its old text must keep the token
c = s.add("shared token here")
s.update(c, "shared words now")
check("kept shared", s.search("shared"), [c])
check("dropped old", s.search("token"), [])
# case insensitivity both ways
d = s.add("MiXeD Case")
check("case", s.search("mixed"), [d])
check("case2", s.search("CASE"), [d])
# updating a doc twice
e = s.add("one")
s.update(e, "two")
s.update(e, "three")
check("twice", s.search("one"), [])
check("twice2", s.search("two"), [])
check("twice3", s.search("three"), [e])
sys.exit(1 if bad else 0)
