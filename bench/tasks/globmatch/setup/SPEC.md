# Glob matching

`matches(pattern, name) -> bool` decides whether the whole string `name` is
matched by `pattern`. This is the shell-style glob used by policy path rules, and
it is **not** path-aware: no metacharacter treats `/` specially.

## Metacharacters

- `*` matches any sequence of characters, including the empty sequence, and
  including `/`.
- `?` matches exactly one character, any character, including `/`.
- `[...]` is a character class matching exactly one character:
  - `[abc]` matches one of `a`, `b`, `c`.
  - If the first character after `[` is `!`, the class is negated: `[!abc]`
    matches any single character that is not `a`, `b` or `c`.
  - `a-z` inside a class is a range. A `-` first or last in the class is a
    literal `-`.
  - A `]` as the first character of the class (or first after `!`) is a literal
    `]` and does not close the class.
  - If there is no closing `]` anywhere after it, the `[` is a literal `[`.
- Every other character matches itself. There is **no** escape character: a
  backslash is an ordinary literal character.

## Matching

- The match is anchored: the entire `name` must be consumed by the entire
  `pattern`.
- Matching is case sensitive.
- An empty pattern matches only the empty name.

## Constraint

Implement the matcher yourself. You may not use the `fnmatch`, `glob`, `re`,
`regex` or `pathlib` modules — this logic has to be portable to languages that
have no such library, which is the whole point of the exercise.
