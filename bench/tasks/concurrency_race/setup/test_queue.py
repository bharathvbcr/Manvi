from bqueue import BoundedQueue, QueueClosed

def main():
    q = BoundedQueue(4)
    q.put("a")
    q.put("b")
    q.put("c")
    assert q.qsize() == 3
    assert q.get() == "a"
    assert q.get() == "b"
    assert q.get() == "c"
    assert q.qsize() == 0
    q.close()
    try:
        q.put("x")
    except QueueClosed:
        pass
    else:
        raise AssertionError("put after close should raise")
    print("ok")

main()
