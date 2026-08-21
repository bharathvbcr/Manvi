from intervals import merge, intersect, subtract

def main():
    assert merge([(1, 3), (2, 6)]) == [(1, 6)]
    assert merge([(0, 5), (5, 9)]) == [(0, 9)], merge([(0, 5), (5, 9)])
    assert intersect([(0, 10)], [(4, 6)]) == [(4, 6)]
    assert subtract([(0, 10)], [(4, 6)]) == [(0, 4), (6, 10)]
    print("ok")

main()
