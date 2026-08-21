from .normalize import normalise_rows

RAW = [
    {"category": "travel", "amount": 125000},
    {"category": "meals", "amount": 4250},
    {"category": "travel", "amount": 89900},
    {"category": "hardware", "amount": 250000},
    {"category": "meals", "amount": 1175},
]


def load():
    """Load the ledger. Amounts come back in dollars, not cents."""
    return normalise_rows(RAW)
