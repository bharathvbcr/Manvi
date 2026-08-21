import random, sys, time
from dedupe import common_prefix_pairs

def ref(words):
    c = 0
    for i in range(len(words)):
        for j in range(i + 1, len(words)):
            a, b = words[i], words[j]
            k = 0
            while k < len(a) and k < len(b) and a[k] == b[k]:
                k += 1
            if k >= 3:
                c += 1
    return c

bad = 0
random.seed(99)
# correctness against the naive reference, on inputs small enough to be quick
for _ in range(60):
    n = random.randint(0, 30)
    ws = ["".join(random.choice("ab") for _ in range(random.randint(0, 6)))
          for _ in range(n)]
    got, want = common_prefix_pairs(list(ws)), ref(ws)
    if got != want:
        print("FAIL", ws, got, want); bad += 1; break
for edge in ([], ["ab"], ["abc", "abc"], ["abc"], ["", ""], ["abcd"] * 5):
    if common_prefix_pairs(list(edge)) != ref(edge):
        print("FAIL edge", edge); bad += 1
if bad:
    sys.exit(1)

# speed: measured naive is ~3.8s here and a prefix-bucket solution ~0.002s,
# so this gate separates them cleanly and fails fast.
random.seed(5)
words = ["".join(random.choice("abcde") for _ in range(8)) for _ in range(12000)]
t0 = time.time(); common_prefix_pairs(words); dt = time.time() - t0
if dt > 1.5:
    print(f"FAIL too slow at n=12000: {dt:.2f}s (budget 1.5s)")
    sys.exit(1)

random.seed(7)
words = ["".join(random.choice("abcde") for _ in range(8)) for _ in range(60000)]
t0 = time.time(); common_prefix_pairs(words); dt = time.time() - t0
if dt > 3.0:
    print(f"FAIL too slow at n=60000: {dt:.2f}s (budget 3.0s)"); bad += 1
sys.exit(1 if bad else 0)
