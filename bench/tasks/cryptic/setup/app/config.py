DEFAULTS = {
    "currency": "USD",
    "precision": 2,
    "include_tax": True,
    "tax_rate": 0.08,
}


def load(overrides=None):
    cfg = dict(DEFAULTS)
    if overrides:
        for k, v in overrides.items():
            if k not in cfg:
                raise KeyError(k)
            cfg[k] = v
    return cfg
