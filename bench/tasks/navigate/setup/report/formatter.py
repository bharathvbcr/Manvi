def render(pairs, total):
    lines = [f"{name:<10} {amount:>10.2f}" for name, amount in pairs]
    lines.append(f"{'TOTAL':<10} {total:>10.2f}")
    return "\n".join(lines)
