class Index:
    """Maps a token to the set of document ids containing it."""

    def __init__(self):
        self._m = {}

    def add(self, doc_id, tokens):
        for t in tokens:
            self._m.setdefault(t, set()).add(doc_id)

    def lookup(self, token):
        return self._m.get(token, set())

    def remove(self, doc_id, tokens):
        for t in tokens:
            if t in self._m:
                self._m[t].discard(doc_id)
