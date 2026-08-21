import time


class TTLCache:
    def __init__(self, capacity, ttl, clock=time.time):
        self.capacity = capacity
        self.ttl = ttl
        self.clock = clock
        self._data = {}
        self._order = []

    def _purge(self):
        now = self.clock()
        dead = [k for k, (_, stored) in self._data.items()
                if now - stored >= self.ttl]
        for k in dead:
            self._data.pop(k, None)
            if k in self._order:
                self._order.remove(k)

    def _touch(self, key):
        if key in self._order:
            self._order.remove(key)
        self._order.append(key)

    def get(self, key):
        self._purge()
        entry = self._data.get(key)
        if entry is None:
            return None
        self._touch(key)
        return entry[0]

    def put(self, key, value):
        self._purge()
        self._data[key] = (value, self.clock())
        self._touch(key)
        while len(self._data) > self.capacity:
            oldest = self._order.pop(0)
            self._data.pop(oldest, None)

    def __len__(self):
        self._purge()
        return len(self._data)
