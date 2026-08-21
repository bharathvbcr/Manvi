`python3 test_queue.py` fails on a handful of sequential cases, but that is not
the real bug.

`bqueue.py` implements `BoundedQueue`, a fixed-capacity FIFO that producers and
consumers are supposed to share across threads. It currently deadlocks under
contention, drops wakeups, and can raise when a waiter is woken into an empty
or full queue. Fix it so the following contract holds exactly.

- `BoundedQueue(capacity)` with `capacity >= 1`.
- `put(item)` blocks while the queue is full, then appends at the tail.
- `get()` blocks while the queue is empty, then pops the head.
- Order is FIFO: the n-th successful `get` returns the n-th successful `put`,
  even when many threads call both.
- `qsize()` is the number of items currently stored and is always in
  `0 .. capacity`. It is safe to call from any thread.
- `close()` makes the queue shut down. Every waiter is woken. After close:
  - a `get` of a remaining item still returns it;
  - a `get` on empty, and any `put`, raise `QueueClosed`;
  - a second `close` is a no-op.
- Do not import the stdlib `queue` module. Keep the public names
  (`BoundedQueue`, `QueueClosed`, `put`, `get`, `qsize`, `close`).

Do not modify the test.
