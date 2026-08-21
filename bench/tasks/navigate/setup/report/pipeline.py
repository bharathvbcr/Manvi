from .aggregate import by_category, grand_total
from .loader import load
from .normalize import normalise_rows


def build_report():
    rows = load()
    rows = normalise_rows(rows)
    pairs = by_category(rows)
    return render_report(pairs)


def render_report(pairs):
    from .formatter import render
    return render(pairs, grand_total(pairs))
