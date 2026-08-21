import sys
from slugify import slugify

cases = {
    "Hello World": "hello-world",
    "A  B": "a-b",
    "Rock & Roll!": "rock-roll",
    "  leading and trailing  ": "leading-and-trailing",
    "---": "",
    "": "",
    "already-a-slug": "already-a-slug",
    "C++ / Rust": "c-rust",
    "a---b": "a-b",
    "Numbers 123 Stay": "numbers-123-stay",
    "!!!": "",
    "Tabs\tand\nnewlines": "tabs-and-newlines",
    "a_b": "a-b",
    "  ": "",
}
bad = 0
for src, want in cases.items():
    try:
        got = slugify(src)
    except Exception as e:
        print("EXC", repr(src), e); bad += 1; continue
    if got != want:
        print("FAIL", repr(src), "got", repr(got), "want", repr(want)); bad += 1
sys.exit(1 if bad else 0)
