import sys
from patch import PatchError, apply_patch

bad = 0


def eq(label, got, want):
    global bad
    if got != want:
        print("FAIL", label, "got", repr(got), "want", repr(want))
        bad += 1


def raises(label, fn):
    global bad
    try:
        fn()
    except PatchError:
        return
    except Exception as e:
        print("FAIL", label, "raised", type(e).__name__)
        bad += 1
        return
    print("FAIL", label, "did not raise")
    bad += 1


src = {"a": 1, "b": [2, 3], "c": {"d": 4}}
out = apply_patch(src, [
    {"op": "add", "path": "/b/-", "value": 4},
    {"op": "remove", "path": "/a"},
])
eq("append no mutate", out, {"b": [2, 3, 4], "c": {"d": 4}})
eq("src list intact", src, {"a": 1, "b": [2, 3], "c": {"d": 4}})

# pointer escaping
doc = {"a/b": {"~c": 1}}
eq("escape", apply_patch(doc, [{"op": "test", "path": "/a~1b/~0c", "value": 1}]),
   {"a/b": {"~c": 1}})

# array add insert and dash
doc = [0, 1, 2]
eq("insert 0", apply_patch(doc, [{"op": "add", "path": "/0", "value": 9}]),
   [9, 0, 1, 2])
eq("append -", apply_patch([0, 1], [{"op": "add", "path": "/-", "value": 2}]),
   [0, 1, 2])
eq("append len", apply_patch([0, 1], [{"op": "add", "path": "/2", "value": 2}]),
   [0, 1, 2])

# replace overwrites, does not insert
eq("replace arr", apply_patch([0, 1, 2],
    [{"op": "replace", "path": "/1", "value": 9}]), [0, 9, 2])
eq("replace root", apply_patch({"a": 1},
    [{"op": "replace", "path": "", "value": [1, 2]}]), [1, 2])

# test
raises("test fail", lambda: apply_patch({"a": 1},
    [{"op": "test", "path": "/a", "value": 2}]))
eq("test ok", apply_patch({"a": 1},
    [{"op": "test", "path": "/a", "value": 1}]), {"a": 1})

# copy
eq("copy", apply_patch({"a": [1], "b": 0},
    [{"op": "copy", "from": "/a", "path": "/b"}]), {"a": [1], "b": [1]})
doc = {"a": [1]}
out = apply_patch(doc, [{"op": "copy", "from": "/a", "path": "/b"}])
out["b"].append(2)
eq("copy is deep", doc["a"], [1])

# move
eq("move", apply_patch({"a": 1, "b": 2},
    [{"op": "move", "from": "/a", "path": "/c"}]), {"b": 2, "c": 1})
eq("move same", apply_patch({"a": 1},
    [{"op": "move", "from": "/a", "path": "/a"}]), {"a": 1})
raises("move into self", lambda: apply_patch(
    {"a": {"b": 1}},
    [{"op": "move", "from": "/a", "path": "/a/b"}]))

# errors
raises("missing", lambda: apply_patch({}, [{"op": "remove", "path": "/x"}]))
raises("lead zero", lambda: apply_patch([1, 2], [{"op": "remove", "path": "/01"}]))
raises("unknown op", lambda: apply_patch({}, [{"op": "nope", "path": ""}]))
raises("bad ptr", lambda: apply_patch({}, [{"op": "test", "path": "x", "value": 1}]))
raises("add oob", lambda: apply_patch([1], [{"op": "add", "path": "/3", "value": 1}]))

# sequenced
eq("seq", apply_patch({"a": [1, 2]}, [
    {"op": "add", "path": "/a/-", "value": 3},
    {"op": "remove", "path": "/a/0"},
]), {"a": [2, 3]})

sys.exit(1 if bad else 0)
