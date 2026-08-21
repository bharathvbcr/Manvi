"""Amount normalisation.

Raw ledger rows carry integer cents. Everything downstream works in dollars.
"""


def to_dollars(cents):
    return round(cents / 100.0, 2)


def normalise_rows(rows):
    """Return rows with their amount converted from cents to dollars."""
    return [dict(r, amount=to_dollars(r["amount"])) for r in rows]
