from searchlib import search_range

def main():
    assert search_range([1, 2, 2, 2, 5], 2) == (1, 3)
    assert search_range([1, 2, 3], 4) == (-1, -1)
    print("ok")

main()
