def common_prefix_pairs(words):
    """Return the number of unordered pairs (i<j) of words that share a
    prefix of length >= 3."""
    count = 0
    for i in range(len(words)):
        for j in range(i + 1, len(words)):
            a, b = words[i], words[j]
            k = 0
            while k < len(a) and k < len(b) and a[k] == b[k]:
                k += 1
            if k >= 3:
                count += 1
    return count
