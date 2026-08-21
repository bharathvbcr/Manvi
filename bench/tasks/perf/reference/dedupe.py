from collections import Counter


def common_prefix_pairs(words):
    counts = Counter(w[:3] for w in words if len(w) >= 3)
    return sum(k * (k - 1) // 2 for k in counts.values())
