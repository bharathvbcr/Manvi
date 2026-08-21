import random, sys
from searchlib import search_range

def ref(nums, t):
    idx = [i for i, v in enumerate(nums) if v == t]
    return (idx[0], idx[-1]) if idx else (-1, -1)

cases = [([1,2,2,2,5],2), ([1,2,3],4), ([],1), ([7],7), ([7],8),
         ([2,2,2,2],2), ([1,2,3,4,5],1), ([1,2,3,4,5],5), ([1,1,2,2,3,3],2)]
random.seed(11)
for _ in range(300):
    n = random.randint(0, 40)
    nums = sorted(random.randint(0, 8) for _ in range(n))
    cases.append((nums, random.randint(0, 9)))

bad = 0
for nums, t in cases:
    try:
        got = search_range(list(nums), t)
    except Exception as e:
        print("EXC", nums, t, e); bad += 1; continue
    if tuple(got) != ref(nums, t):
        print("FAIL", nums, t, "got", got, "want", ref(nums, t)); bad += 1
        if bad > 5: break
sys.exit(1 if bad else 0)
