import random
import sys
from proto import Parser, ProtocolError

bad = 0


def eq(label, got, want):
    global bad
    if got != want:
        print("FAIL", label, "got", repr(got), "want", repr(want))
        bad += 1


def raises(label, fn, exc=ProtocolError):
    global bad
    try:
        fn()
    except exc:
        return True
    except Exception as e:
        print("FAIL", label, "raised", type(e).__name__, "not", exc.__name__)
        bad += 1
        return False
    print("FAIL", label, "did not raise")
    bad += 1
    return False


def crc16(data):
    crc = 0xFFFF
    for b in data:
        crc ^= b << 8
        for _ in range(8):
            crc = ((crc << 1) ^ 0x1021) & 0xFFFF if crc & 0x8000 else (crc << 1) & 0xFFFF
    return crc


def frame(typ, payload=b"", flags=0, seq=0):
    header = bytes([0xA5, typ, flags,
                    (seq >> 8) & 0xFF, seq & 0xFF,
                    (len(payload) >> 8) & 0xFF, len(payload) & 0xFF])
    body = header + payload
    c = crc16(body)
    return body + bytes([(c >> 8) & 0xFF, c & 0xFF])


OPEN = lambda seq=0: frame(0x01, b"\x00\x00\x00\x01", seq=seq)
MSG = lambda payload, seq, flags=0: frame(0x02, payload, flags=flags, seq=seq)
ACK = lambda ack_seq, seq=99: frame(0x03, bytes([(ack_seq >> 8) & 0xFF, ack_seq & 0xFF]), seq=seq)
CLOSE = lambda seq=0: frame(0x04, b"", seq=seq)


# --- happy path split across feeds
p = Parser()
blob = OPEN(0) + MSG(b"ab", 1) + ACK(1) + CLOSE(2)
eq("empty feed", p.feed(b""), [])
got = []
for i in range(0, len(blob), 3):
    got.extend(p.feed(blob[i:i + 3]))
eq("chunked session", got, [("open", 1), ("msg", b"ab"), ("ack", 1), ("close",)])

# --- junk prefix resync
p = Parser()
eq("resync", p.feed(b"\x00\xff" + OPEN(5) + MSG(b"z", 6)),
   [("open", 1), ("msg", b"z")])

# --- bad crc
p = Parser()
p.feed(OPEN(0))
bad_crc = bytearray(MSG(b"x", 1))
bad_crc[-1] ^= 0x01
raises("bad crc", lambda: p.feed(bytes(bad_crc)))
raises("sticky error", lambda: p.feed(b""))
raises("sticky error 2", lambda: p.feed(OPEN(0)))

# --- wrong version
p = Parser()
raises("bad version", lambda: p.feed(frame(0x01, b"\x00\x00\x00\x02", seq=0)))

# --- MSG before OPEN
p = Parser()
raises("msg in init", lambda: p.feed(MSG(b"x", 0)))

# --- seq hole
p = Parser()
p.feed(OPEN(0))
raises("seq hole", lambda: p.feed(MSG(b"x", 2)))

# --- double open
p = Parser()
p.feed(OPEN(0))
raises("double open", lambda: p.feed(OPEN(1)))

# --- close then msg
p = Parser()
p.feed(OPEN(0) + CLOSE(1))
raises("msg after close", lambda: p.feed(MSG(b"x", 1)))

# --- unknown type
p = Parser()
p.feed(OPEN(0))
raises("bad type", lambda: p.feed(frame(0x99, b"", seq=1)))

# --- oversize length in header
p = Parser()
p.feed(OPEN(0))
hdr = bytes([0xA5, 0x02, 0, 0, 1, 0x10, 0x01])  # length 4097
raises("oversize", lambda: p.feed(hdr + b"\x00" * 8))

# --- fragments
p = Parser()
eq("frag1", p.feed(OPEN(0) + MSG(b"hel", 1, flags=1)), [("open", 1)])
eq("frag2", p.feed(MSG(b"lo", 2, flags=1)), [])
eq("frag3", p.feed(MSG(b"!", 3, flags=0)), [("msg", b"hello!")])

# --- close while fragments pending
p = Parser()
p.feed(OPEN(0) + MSG(b"xx", 1, flags=1))
raises("close pending frag", lambda: p.feed(CLOSE(2)))

# --- assembled overflow
p = Parser()
p.feed(OPEN(0))
p.feed(MSG(b"a" * 3000, 1, flags=1))
raises("frag overflow", lambda: p.feed(MSG(b"b" * 1100, 2, flags=0)))

# --- ACK payload and ignored frame seq
p = Parser()
p.feed(OPEN(10))
eq("ack", p.feed(ACK(42, seq=0)), [("ack", 42)])
eq("msg after ack uses open+1", p.feed(MSG(b"q", 11)), [("msg", b"q")])

# --- wraparound seq
p = Parser()
p.feed(OPEN(0xFFFF))
eq("wrap seq", p.feed(MSG(b"w", 0)), [("msg", b"w")])

# --- OPEN payload length wrong
p = Parser()
raises("open short", lambda: p.feed(frame(0x01, b"\x00\x01", seq=0)))

# --- CLOSE with payload
p = Parser()
p.feed(OPEN(0))
raises("close payload", lambda: p.feed(frame(0x04, b"x", seq=1)))

# --- do not mutate input
p = Parser()
buf = bytearray(OPEN(0))
p.feed(buf)
buf[0] = 0
eq("no mutate", p.feed(MSG(b"k", 1)), [("msg", b"k")])

# --- fuzzer: random chunking of a valid stream
random.seed(7)
valid = OPEN(3) + MSG(b"aa", 4, flags=1) + MSG(b"bb", 5) + ACK(5) + MSG(b"", 6) + CLOSE(7)
want = [("open", 1), ("msg", b"aabb"), ("ack", 5), ("msg", b""), ("close",)]
for trial in range(80):
    p = Parser()
    got = []
    i = 0
    while i < len(valid):
        n = random.randint(0, 5)
        got.extend(p.feed(valid[i:i + n]))
        i += n
    if got != want:
        print("FAIL fuzz chunk", trial, got)
        bad += 1
        break

# --- fuzzer: leftover incomplete then complete
p = Parser()
f = OPEN(0)
eq("partial", p.feed(f[:4]), [])
eq("rest", p.feed(f[4:]), [("open", 1)])

sys.exit(1 if bad else 0)
