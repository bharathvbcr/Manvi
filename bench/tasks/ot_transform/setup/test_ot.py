from ot import apply, transform

def main():
    doc = "ac"
    a = ("ins", 1, "b", 1)
    b = ("ins", 2, "d", 2)
    a2, b2 = transform(a, b)
    assert apply(apply(doc, a), b2) == "abcd"
    assert apply(apply(doc, b), a2) == "abcd"
    print("ok")

main()
