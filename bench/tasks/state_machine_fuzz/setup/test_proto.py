from proto import Parser

def _crc(data):
    crc = 0xFFFF
    for b in data:
        crc ^= b << 8
        for _ in range(8):
            crc = ((crc << 1) ^ 0x1021) & 0xFFFF if crc & 0x8000 else (crc << 1) & 0xFFFF
    return crc

def _frame(typ, payload=b"", flags=0, seq=0):
    header = bytes([0xA5, typ, flags, (seq >> 8) & 0xFF, seq & 0xFF,
                    (len(payload) >> 8) & 0xFF, len(payload) & 0xFF])
    body = header + payload
    c = _crc(body)
    return body + bytes([(c >> 8) & 0xFF, c & 0xFF])

def main():
    p = Parser()
    open_f = _frame(0x01, b"\x00\x00\x00\x01", seq=0)
    msg_f = _frame(0x02, b"hi", seq=1)
    close_f = _frame(0x04, b"", seq=2)
    assert p.feed(open_f) == [("open", 1)]
    assert p.feed(msg_f) == [("msg", b"hi")]
    assert p.feed(close_f) == [("close",)]
    print("ok")

main()
