Implement `apply_patch(doc, ops)` in `patch.py` exactly as `SPEC.md`
describes.

`python3 test_patch.py` covers add and remove on an object. The hidden
check covers pointer escaping, array `-`, `move` into a descendant,
`test` failures, and that the input document is not mutated.

Do not modify the test or the spec.
