from report.pipeline import build_report

def main():
    out = build_report()
    lines = out.strip().splitlines()
    assert lines[-1].split()[1] == "4703.25", f"wrong total line: {lines[-1]!r}"
    assert lines[0].startswith("hardware"), f"categories not alphabetical: {lines[0]!r}"
    print("ok")

main()
