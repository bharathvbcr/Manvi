def search_range(nums, target):
    """Return (first, last) indices of target in sorted nums, or (-1, -1)."""
    lo, hi = 0, len(nums)
    while lo < hi:
        mid = (lo + hi) // 2
        if nums[mid] < target:
            lo = mid + 1
        else:
            hi = mid
    first = lo
    if first >= len(nums) or nums[first] != target:
        return (-1, -1)
    lo, hi = first, len(nums)
    while lo < hi:
        mid = (lo + hi) // 2
        if nums[mid] <= target:
            lo = mid
        else:
            hi = mid
    return (first, lo)
