`python3 test_store.py` fails: after a document is updated, searching for a word
from its **old** text still returns it.

Fix the bug so the index always reflects the current text of each document.
Deleting a document must also remove it from the index, and searching is
case-insensitive. Do not modify the test.
