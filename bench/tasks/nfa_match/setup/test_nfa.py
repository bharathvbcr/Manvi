from nfa import fullmatch

def main():
    assert fullmatch("a*b", "aaab") is True
    assert fullmatch("a*b", "aaac") is False
    assert fullmatch("a|bc", "bc") is True
    print("ok")

main()
