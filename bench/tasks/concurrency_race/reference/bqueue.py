import threading


class QueueClosed(Exception):
    pass


class BoundedQueue:
    """Fixed-capacity FIFO. See TASK.md."""

    def __init__(self, capacity):
        if capacity < 1:
            raise ValueError("capacity must be >= 1")
        self.capacity = capacity
        self._items = []
        self._cv = threading.Condition()
        self._closed = False

    def put(self, item):
        with self._cv:
            while True:
                if self._closed:
                    raise QueueClosed
                if len(self._items) < self.capacity:
                    self._items.append(item)
                    self._cv.notify_all()
                    return
                self._cv.wait()

    def get(self):
        with self._cv:
            while True:
                if self._items:
                    item = self._items.pop(0)
                    self._cv.notify_all()
                    return item
                if self._closed:
                    raise QueueClosed
                self._cv.wait()

    def qsize(self):
        with self._cv:
            return len(self._items)

    def close(self):
        with self._cv:
            if self._closed:
                return
            self._closed = True
            self._cv.notify_all()
