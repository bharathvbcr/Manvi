from .config import load


def line_total(item, cfg):
    total = item["qty"] * item["unit_price"]
    if cfg["include_taxes"]:
        total *= (1 + cfg["tax_rate"])
    return round(total, cfg["precision"])


def invoice_total(items, overrides=None):
    cfg = load(overrides)
    return round(sum(line_total(i, cfg) for i in items), cfg["precision"])
