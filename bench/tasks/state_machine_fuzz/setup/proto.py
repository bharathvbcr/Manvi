class ProtocolError(Exception):
    pass


_MAX_PAYLOAD = 4096
_HEADER = 7
_CRC = 2


class Parser:
    """Incremental session parser. See SPEC.md."""

    def __init__(self):
        self._buf = bytearray()
        self._state = "INIT"
        self._next_seq = 0
        self._frag = bytearray()

    def feed(self, data):
        if self._state == "ERROR":
            raise ProtocolError("parser is in ERROR")
        if data:
            self._buf.extend(data)
        events = []
        try:
            self._drain(events)
        except ProtocolError:
            self._state = "ERROR"
            self._frag.clear()
            raise
        return events

    def _drain(self, events):
        while True:
            while self._buf and self._buf[0] != 0xA5:
                del self._buf[0]
            if len(self._buf) < _HEADER:
                return
            length = (self._buf[5] << 8) | self._buf[6]
            if length > _MAX_PAYLOAD:
                raise ProtocolError("payload longer than 4096")
            total = _HEADER + length + _CRC
            if len(self._buf) < total:
                return
            frame = bytes(self._buf[:total])
            del self._buf[:total]
            self._accept(frame, events)

    def _accept(self, frame, events):
        body, crc_bytes = frame[:-2], frame[-2:]
        expect = (crc_bytes[0] << 8) | crc_bytes[1]
        if _crc16(body) != expect:
            raise ProtocolError("bad crc")
        kind = body[1]
        flags = body[2]
        seq = (body[3] << 8) | body[4]
        payload = body[_HEADER:]
        if kind == 0x01:
            self._open(seq, payload, events)
        elif kind == 0x02:
            self._msg(seq, flags, payload, events)
        elif kind == 0x03:
            self._ack(payload, events)
        elif kind == 0x04:
            self._close(payload, events)
        else:
            raise ProtocolError("unknown frame type")

    def _open(self, seq, payload, events):
        if self._state != "INIT":
            raise ProtocolError("OPEN is only valid in INIT")
        if payload != b"\x00\x00\x00\x01":
            raise ProtocolError("OPEN version must be 1")
        self._state = "READY"
        self._next_seq = (seq + 1) & 0xFFFF
        events.append(("open", 1))

    def _msg(self, seq, flags, payload, events):
        self._require_ready()
        if seq != self._next_seq:
            raise ProtocolError("MSG sequence does not match")
        if len(self._frag) + len(payload) > _MAX_PAYLOAD:
            raise ProtocolError("assembled message longer than 4096")
        self._next_seq = (self._next_seq + 1) & 0xFFFF
        self._frag.extend(payload)
        if flags & 0x01:
            return
        events.append(("msg", bytes(self._frag)))
        self._frag.clear()

    def _ack(self, payload, events):
        self._require_ready()
        if len(payload) != 2:
            raise ProtocolError("ACK payload must be 2 bytes")
        events.append(("ack", (payload[0] << 8) | payload[1]))

    def _close(self, payload, events):
        self._require_ready()
        if payload:
            raise ProtocolError("CLOSE payload must be empty")
        if self._frag:
            raise ProtocolError("CLOSE while a fragmented message is pending")
        self._state = "CLOSED"
        events.append(("close",))

    def _require_ready(self):
        if self._state != "READY":
            raise ProtocolError(f"frame is illegal in {self._state}")


def _crc16(data):
    crc = 0xFFFF
    for byte in data:
        crc ^= byte << 8
        for _ in range(8):
            if crc & 0x8000:
                crc = ((crc << 1) ^ 0x1021) & 0xFFFF
            else:
                crc = (crc << 1) & 0xFFFF
    return crc
