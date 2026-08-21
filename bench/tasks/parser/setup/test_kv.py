from kvparse import parse_kv

def main():
    assert parse_kv("a = 1\nb=2") == {"a": "1", "b": "2"}
    assert parse_kv("# comment\na=1") == {"a": "1"}
    assert parse_kv('q = "  spaced  "') == {"q": "  spaced  "}
    print("ok")

main()
