class ProtocolError(Exception):
    pass


MAGIC = 0xA5
T_OPEN, T_MSG, T_ACK, T_CLOSE = 0x01, 0x02, 0x03, 0x04
FLAG_MORE = 0x01
MAX_LEN = 4096
HEADER = 7


def crc16(data):
    crc = 0xFFFF
    for b in data:
        crc ^= b << 8
        for _ in range(8):
            if crc & 0x8000:
                crc = ((crc << 1) ^ 0x1021) & 0xFFFF
            else:
                crc = (crc << 1) & 0xFFFF
    return crc


class Parser:
    def __init__(self):
        self._buf = bytearray()
        self._state = "INIT"  # INIT, READY, CLOSED, ERROR
        self._expect = 0
        self._frag = bytearray()

    def feed(self, data):
        if self._state == "ERROR":
            raise ProtocolError("parser is in ERROR")
        if data:
            self._buf.extend(data)
        events = []
        while True:
            try:
                ev = self._next()
            except ProtocolError:
                self._state = "ERROR"
                self._buf.clear()
                self._frag.clear()
                raise
            if ev is None:
                break
            if ev:
                events.append(ev)
        return events

    def _next(self):
        # resync
        while self._buf and self._buf[0] != MAGIC:
            del self._buf[0]
        if len(self._buf) < HEADER:
            return None
        length = (self._buf[5] << 8) | self._buf[6]
        if length > MAX_LEN:
            raise ProtocolError("oversize")
        need = HEADER + length + 2
        if len(self._buf) < need:
            return None
        frame = bytes(self._buf[:need])
        del self._buf[:need]
        body, crc_bytes = frame[:-2], frame[-2:]
        want = (crc_bytes[0] << 8) | crc_bytes[1]
        if crc16(body) != want:
            raise ProtocolError("crc")
        typ = frame[1]
        flags = frame[2]
        seq = (frame[3] << 8) | frame[4]
        payload = frame[HEADER:HEADER + length]
        return self._handle(typ, flags, seq, payload)

    def _handle(self, typ, flags, seq, payload):
        if self._state == "CLOSED":
            raise ProtocolError("closed")
        if typ == T_OPEN:
            if self._state != "INIT":
                raise ProtocolError("open")
            if payload != b"\x00\x00\x00\x01":
                raise ProtocolError("version")
            self._state = "READY"
            self._expect = (seq + 1) & 0xFFFF
            return ("open", 1)
        if typ == T_MSG:
            if self._state != "READY":
                raise ProtocolError("msg state")
            if seq != self._expect:
                raise ProtocolError("seq")
            self._expect = (self._expect + 1) & 0xFFFF
            if len(self._frag) + len(payload) > MAX_LEN:
                raise ProtocolError("frag")
            self._frag.extend(payload)
            if flags & FLAG_MORE:
                return ()  # not an event; feed() skips falsy... WAIT
            assembled = bytes(self._frag)
            self._frag.clear()
            return ("msg", assembled)
        if typ == T_ACK:
            if self._state != "READY":
                raise ProtocolError("ack state")
            if len(payload) != 2:
                raise ProtocolError("ack payload")
            ack_seq = (payload[0] << 8) | payload[1]
            return ("ack", ack_seq)
        if typ == T_CLOSE:
            if self._state != "READY":
                raise ProtocolError("close state")
            if payload:
                raise ProtocolError("close payload")
            if self._frag:
                raise ProtocolError("close pending frag")
            self._state = "CLOSED"
            return ("close",)
        raise ProtocolError("type")
