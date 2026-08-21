import threading


class QueueClosed(Exception):
    pass


class BoundedQueue:
    """Fixed-capacity FIFO. See TASK.md.

    This implementation looks plausible in a single thread and falls over
    the moment more than one waiter exists.
    """

    def __init__(self, capacity):
        if capacity < 1:
            raise ValueError("capacity must be >= 1")
        self.capacity = capacity
        self._items = []
        self._cv = threading.Condition()
        self._closed = False

    def put(self, item):
        with self._cv:
            if self._closed:
                raise QueueClosed
            if len(self._items) >= self.capacity:
                self._cv.wait()
            self._items.append(item)
            self._cv.notify()

    def get(self):
        with self._cv:
            if not self._items:
                self._cv.wait()
            item = self._items.pop(0)
            self._cv.notify()
            return item

    def qsize(self):
        return len(self._items)

    def close(self):
        self._closed = True
