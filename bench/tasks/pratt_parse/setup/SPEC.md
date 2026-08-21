# Arithmetic expressions

`parse(expr: str)` tokenises `expr` and returns an AST. Numbers are
non-negative integers. The AST is:

- an `int`, or
- `("neg", x)`, `("fact", x)`,
- `("pow", a, b)`, `("*", a, b)`, `("/", a, b)`, `("+", a, b)`, `("-", a, b)`

`eval_ast(tree)` (you do **not** have to implement this; it is how the
hidden test checks you) interprets the tree over integers:

- `+` `-` `*` as usual
- `/` is **truncation toward zero** (C99): `(-7)/2 == -3`
- `pow` is integer exponentiation; a negative exponent is `ValueError`
- `neg` is unary minus
- `fact` is factorial on `n >= 0`; otherwise `ValueError`

## Precedence, tightest first

1. postfix `!`  — `3!!` is `fact(fact(3))`
2. `^` — **right**-associative: `2^3^2` is `pow(2, pow(3, 2))`
3. unary `+` and unary `-` — `--2` is `neg(neg(2))`. Unary binds **looser**
   than `^`, so `-2^2` is `neg(pow(2, 2))` i.e. `-4`.
4. `*` `/` — left-associative
5. binary `+` `-` — left-associative

`()` group. Whitespace is ignored.

## Tokens

A number is a run of digits. Operators are the single characters
`+ - * / ^ ! ( )`. Anything else is a `ValueError`.

## Errors (`ValueError`)

Empty input; trailing operator; two binary operators in a row (except
unary after a binary); unmatched parens; empty parens; a number with a
leading `+` that is not unary (so `++1` is two unaries, that is fine);
factorial of a missing operand (`!3`); leftover input after one expression.

You may not use `eval`, `exec`, `ast.parse`, or the `re` module.
