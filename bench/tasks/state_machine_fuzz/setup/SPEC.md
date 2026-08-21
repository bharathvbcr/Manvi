# Binary session protocol

`Parser` is an incremental decoder. `feed(data: bytes) -> list` consumes any
number of bytes (including empty, including a partial frame) and returns the
events produced from every **complete, valid** frame it can now parse. Bytes
that are not yet a complete frame stay in an internal buffer.

Raise `ProtocolError` (a subclass of `Exception`) for a complete frame that
is corrupt or illegal in the current session state. After `ProtocolError` the
parser is in the `ERROR` state: every later `feed` raises `ProtocolError`
immediately, even on empty input.

Do not mutate the `data` argument.

## Frame layout

All integers are big-endian.

```
magic   u8     always 0xA5
type    u8
flags   u8
seq     u16
length  u16
payload `length` bytes
crc16   u16    CRC-CCITT (poly 0x1021, init 0xFFFF) over the bytes
               *before* the crc field (magic through end of payload)
```

Maximum `length` is 4096. A complete frame with `length > 4096` is a
`ProtocolError`.

## CRC-CCITT

```
crc = 0xFFFF
for each byte b:
    crc ^= (b << 8)
    repeat 8 times:
        if crc & 0x8000: crc = ((crc << 1) ^ 0x1021) & 0xFFFF
        else:            crc = (crc << 1) & 0xFFFF
```

## Resync

If the first buffered byte is not `0xA5`, **discard that one byte** and look
again. Do not raise. Incomplete frames (not enough bytes for header+payload+crc
once magic is aligned) wait for more `feed` calls.

## Types

| type | name  | payload                          |
|------|-------|----------------------------------|
| 0x01 | OPEN  | exactly 4 bytes, version `0x00000001` |
| 0x02 | MSG   | 0..4096 bytes                    |
| 0x03 | ACK   | exactly 2 bytes, acked seq       |
| 0x04 | CLOSE | empty                            |

Any other type on a CRC-valid frame is a `ProtocolError`.

## Session states

Start in `INIT`.

- `OPEN` is valid only in `INIT`. Payload must be `b"\x00\x00\x00\x01"`.
  Event: `("open", 1)`. Move to `READY`. `seq` of this frame becomes the
  session's starting sequence; the next expected MSG seq is that value `+ 1`
  (mod 65536).
- `MSG` is valid only in `READY`. Its `seq` must equal the next expected
  seq, then expected seq advances by 1 (mod 65536). See fragments below.
- `ACK` is valid only in `READY`. Event: `("ack", seq_from_payload)` where
  `seq_from_payload` is the u16 in the payload. Session seq is **not**
  changed by ACK. The frame's own `seq` field is ignored.
- `CLOSE` is valid only in `READY` and payload must be empty. Event:
  `("close",)`. Move to `CLOSED`.
- Any frame in `CLOSED` is a `ProtocolError`.
- A well-typed frame in the wrong state is a `ProtocolError`.

## MSG fragments

If `flags & 0x01` is set, this MSG is a **fragment**: append payload to an
assembly buffer and emit nothing yet. If `flags & 0x01` is clear, append
payload and emit `("msg", assembled_bytes)`, then clear the assembly buffer.

Each fragment is a MSG frame and **does** consume a sequence number, even
when it emits nothing.

If the assembled length would exceed 4096, raise `ProtocolError`.

A `CLOSE` or illegal frame while fragments are pending is a `ProtocolError`
(do not emit a partial message).

## Events

Each event is a tuple:

- `("open", 1)`
- `("msg", payload: bytes)`
- `("ack", seq: int)`
- `("close",)`

`feed` returns them in the order frames were completed.
