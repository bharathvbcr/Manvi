from globmatch import matches

def main():
    assert matches("*.py", "src/foo.py") is True
    assert matches("a[bc]d", "abd") is True
    assert matches("a[!bc]d", "abd") is False
    assert matches("x?z", "xyz") is True
    assert matches("[]]a", "]a") is True
    assert matches("*", "") is True
    print("ok")

main()
