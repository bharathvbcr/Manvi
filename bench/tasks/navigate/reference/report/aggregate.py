def by_category(rows):
    """Total each category. Categories come back in alphabetical order."""
    totals = {}
    for r in rows:
        totals[r["category"]] = round(totals.get(r["category"], 0) + r["amount"], 2)
    return sorted(totals.items())


def grand_total(pairs):
    return round(sum(v for _, v in pairs), 2)
