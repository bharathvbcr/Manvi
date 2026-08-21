Implement `Parser` in `proto.py` exactly as `SPEC.md` describes.

`python3 test_proto.py` only feeds a few complete, well-formed frames. The
hidden check is a stateful fuzzer: random chunking, junk prefixes, bad CRCs,
sequence holes, fragment reassembly, oversize payloads, and illegal
transitions. A parser that "works on the happy path" will crash or accept
frames it must reject.

Do not modify the test or the spec.
