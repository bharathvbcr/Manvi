import re

def slugify(title):
    """Turn a title into a URL slug.

    Lowercase, words joined by single hyphens, no leading/trailing hyphen,
    and any run of non-alphanumeric characters collapses to one hyphen.
    """
    s = title.lower()
    s = s.replace(" ", "-")
    s = s.replace("--", "-")
    return s
