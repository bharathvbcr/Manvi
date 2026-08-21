from patch import apply_patch

def main():
    doc = {"a": 1, "b": [2, 3]}
    out = apply_patch(doc, [
        {"op": "add", "path": "/c", "value": 4},
        {"op": "remove", "path": "/a"},
    ])
    assert out == {"b": [2, 3], "c": 4}
    assert doc == {"a": 1, "b": [2, 3]}
    print("ok")

main()
