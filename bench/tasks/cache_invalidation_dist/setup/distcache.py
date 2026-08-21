class DistCache:
    """Replicated cache. See SPEC.md."""

    def __init__(self, n_nodes=3):
        self.n_nodes = n_nodes
        self._store = [{} for _ in range(n_nodes)]

    def put(self, node, key, value):
        for s in self._store:
            s[key] = value

    def get(self, node, key):
        return self._store[node].get(key)

    def invalidate(self, node, key):
        self._store[node].pop(key, None)

    def split(self, groups):
        pass

    def heal(self):
        src = self._store[0]
        for i in range(1, self.n_nodes):
            self._store[i] = dict(src)
