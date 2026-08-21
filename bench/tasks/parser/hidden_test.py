import sys
from kvparse import parse_kv

bad = 0
def eq(label, got, want):
    global bad
    if got != want:
        print("FAIL", label, "got", repr(got), "want", repr(want)); bad += 1

def raises(label, text):
    global bad
    try:
        parse_kv(text)
    except ValueError:
        return
    except Exception as e:
        print("FAIL", label, "raised", type(e).__name__, "not ValueError"); bad += 1
        return
    print("FAIL", label, "did not raise"); bad += 1

eq("basic", parse_kv("a = 1\nb=2"), {"a": "1", "b": "2"})
eq("comment", parse_kv("# c\na=1"), {"a": "1"})
eq("indented comment", parse_kv("   # c\na=1"), {"a": "1"})
eq("blank lines", parse_kv("\n\n  \na=1\n\n"), {"a": "1"})
eq("hash inline", parse_kv("a = 1 # two"), {"a": "1 # two"})
eq("first equals", parse_kv("a=b=c"), {"a": "b=c"})
eq("empty value", parse_kv("a="), {"a": ""})
eq("quoted spaces", parse_kv('q = "  spaced  "'), {"q": "  spaced  "})
eq("quoted hash", parse_kv('q = "a # b"'), {"q": "a # b"})
eq("quoted empty", parse_kv('q = ""'), {"q": ""})
eq("escaped quote", parse_kv(r'q = "say \"hi\""'), {"q": 'say "hi"'})
eq("escaped backslash", parse_kv(r'q = "a\\b"'), {"q": r"a\b"})
eq("other escape kept", parse_kv(r'q = "a\nb"'), {"q": r"a\nb"})
eq("single quote char", parse_kv('q = "'), {"q": '"'})
eq("not quoted both ends", parse_kv('q = "abc'), {"q": '"abc'})
eq("override", parse_kv("a=1\na=2"), {"a": "2"})
eq("key stripped", parse_kv("  a  = 1"), {"a": "1"})
eq("empty input", parse_kv(""), {})
eq("only comments", parse_kv("#a\n#b"), {})
raises("empty key", "=1")
raises("no equals", "just a line")
raises("no equals after ok", "a=1\nnope")
sys.exit(1 if bad else 0)
