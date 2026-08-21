# The KV config format

`parse_kv(text)` takes the whole file as one string and returns a `dict` mapping
`str` keys to `str` values.

Rules, in order of precedence:

1. A line whose first non-space character is `#` is a comment and is ignored
   entirely. A `#` anywhere else on the line is an ordinary character.
2. A blank line, or a line of only whitespace, is ignored.
3. Otherwise the line must contain a `=`. The key is everything before the
   **first** `=`, the value is everything after it. Both are stripped of leading
   and trailing whitespace.
4. An empty key (the line starts with `=`) is an error: raise `ValueError`.
   A line with no `=` at all is also a `ValueError`.
5. If the value starts with `"` and ends with `"` and is at least two characters
   long, those two quotes are removed and the text between them is taken
   literally — whitespace inside is preserved, and `#` has no special meaning.
   Inside such a quoted value, `\"` means a literal `"` and `\\` means a literal
   backslash. No other escape sequence is recognised; a backslash before anything
   else stays a backslash.
6. A later assignment to the same key replaces the earlier one.
