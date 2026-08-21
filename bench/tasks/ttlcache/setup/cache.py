import time


class TTLCache:
    """A fixed-capacity cache whose entries also expire with age.

    See TASK.md for the contract this is supposed to satisfy.
    """

    def __init__(self, capacity, ttl, clock=time.time):
        self.capacity = capacity
        self.ttl = ttl
        self.clock = clock
        self._data = {}          # key -> (value, stored_at)
        self._order = []         # least recently used first

    def get(self, key):
        entry = self._data.get(key)
        if entry is None:
            return None
        value, stored = entry
        if self.clock() - stored >= self.ttl:
            return None
        return value

    def put(self, key, value):
        self._data[key] = (value, self.clock())
        if key not in self._order:
            self._order.append(key)
        while len(self._order) > self.capacity:
            oldest = self._order.pop(0)
            self._data.pop(oldest, None)

    def __len__(self):
        return len(self._data)
