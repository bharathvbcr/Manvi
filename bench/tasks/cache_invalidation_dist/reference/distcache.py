TOMB = "__tomb__"


class DistCache:
    def __init__(self, n_nodes=3):
        if n_nodes < 1:
            raise ValueError("n_nodes")
        self.n_nodes = n_nodes
        self._store = [{} for _ in range(n_nodes)]
        self._clock = [0] * n_nodes
        self._group = [0] * n_nodes

    def _reachable(self, a, b):
        return self._group[a] == self._group[b]

    def _apply(self, j, key, rec):
        cur = self._store[j].get(key)
        if cur is None or (rec[1], rec[2]) > (cur[1], cur[2]):
            self._store[j][key] = rec
        if rec[1] > self._clock[j]:
            self._clock[j] = rec[1]

    def _replicate(self, node, key, tag):
        self._clock[node] += 1
        rec = (tag, self._clock[node], node)
        for j in range(self.n_nodes):
            if self._reachable(node, j):
                self._apply(j, key, rec)

    def put(self, node, key, value):
        if not isinstance(value, str) or value == TOMB:
            raise ValueError("value")
        self._replicate(node, key, value)

    def invalidate(self, node, key):
        self._replicate(node, key, TOMB)

    def get(self, node, key):
        rec = self._store[node].get(key)
        if rec is None or rec[0] == TOMB:
            return None
        return rec[0]

    def split(self, groups):
        gmap = [None] * self.n_nodes
        seen = []
        for gi, g in enumerate(groups):
            for nid in g:
                if nid < 0 or nid >= self.n_nodes or gmap[nid] is not None:
                    raise ValueError("not a partition")
                gmap[nid] = gi
                seen.append(nid)
        if any(x is None for x in gmap):
            raise ValueError("not a partition")
        self._group = gmap

    def heal(self):
        self._group = [0] * self.n_nodes
        keys = set()
        for s in self._store:
            keys.update(s)
        for k in keys:
            best = None
            for s in self._store:
                rec = s.get(k)
                if rec is None:
                    continue
                if best is None or (rec[1], rec[2]) > (best[1], best[2]):
                    best = rec
            if best is not None:
                for s in self._store:
                    s[k] = best
        mx = max(self._clock) if self._clock else 0
        self._clock = [mx] * self.n_nodes
