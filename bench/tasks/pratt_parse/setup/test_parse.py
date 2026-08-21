from parse import parse

def main():
    assert parse("1+2*3") == ("+", 1, ("*", 2, 3))
    assert parse("(1+2)*3") == ("*", ("+", 1, 2), 3)
    print("ok")

main()
