import random, time
from dedupe import common_prefix_pairs

def main():
    assert common_prefix_pairs(["abcd", "abce", "xyzw"]) == 1
    assert common_prefix_pairs([]) == 0
    random.seed(5)
    words = ["".join(random.choice("abcde") for _ in range(8)) for _ in range(12000)]
    t0 = time.time()
    got = common_prefix_pairs(words)
    dt = time.time() - t0
    print(f"pairs={got} in {dt:.3f}s")
    assert dt < 1.0, f"too slow: {dt:.2f}s (budget 1.0s)"
    print("ok")

main()
