class ProtocolError(Exception):
    pass


class Parser:
    """Incremental session parser. See SPEC.md."""

    def feed(self, data):
        raise NotImplementedError
