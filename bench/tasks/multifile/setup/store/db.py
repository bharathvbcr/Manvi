from .index import Index


def tokenize(text):
    return text.lower().split()


class DocStore:
    def __init__(self):
        self.docs = {}
        self.index = Index()
        self._next = 1

    def add(self, text):
        doc_id = self._next
        self._next += 1
        self.docs[doc_id] = text
        self.index.add(doc_id, tokenize(text))
        return doc_id

    def update(self, doc_id, text):
        self.docs[doc_id] = text
        self.index.add(doc_id, tokenize(text))

    def delete(self, doc_id):
        text = self.docs.pop(doc_id, None)
        if text is not None:
            self.index.remove(doc_id, tokenize(text))

    def search(self, token):
        return sorted(self.index.lookup(token.lower()))
