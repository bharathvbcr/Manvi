// Package fnmatch implements Python's fnmatch semantics.
//
// This exists because Go's path.Match and Python's fnmatch disagree on the one
// question the write gate depends on: whether "*" crosses a path separator.
//
//	Python:  fnmatch("src/foo.py", "*.py")  == True
//	Go:      path.Match("*.py", "src/foo.py") == false
//
// Every DevCouncil path rule — the secret patterns, the restricted paths, the
// planned-file globs, forbidden_changes — is a Python fnmatch pattern. Porting
// them onto path.Match would narrow the secret and restricted rules (a gate
// that stops denying) and narrow planned-file matching (a gate that starts
// denying legitimate writes). Both failures are silent.
//
// Match is case-sensitive, which is what Python does on POSIX, where
// os.path.normcase is the identity function. DevCouncil's own patterns are
// checked this way on macOS and Linux today, and the parity fixture pins it.
//
// MatchFold is the deliberate exception, and it is a divergence from the
// incumbent rather than a port of it. Case-sensitive matching is a statement
// about strings; a write gate needs a statement about files. On APFS and NTFS —
// the default filesystems on two of the three platforms this runs on — ".ENV"
// and ".env" are the same file, so a case-sensitive secret-path check reads the
// pattern list and still lets the write through. Hard rules are matched with
// MatchFold for that reason. The cost is over-blocking a file that differs from
// a credential only by case, which is not a file anyone needs to write.
package fnmatch

import (
	"regexp"
	"strings"
	"sync"
)

var (
	cacheMu sync.RWMutex
	cache   = map[string]*regexp.Regexp{}
	// foldCache is separate from cache so a pattern compiled for one matching
	// mode can never be served to the other.
	foldCache = map[string]*regexp.Regexp{}
)

// Match reports whether name matches the shell-style pattern, using Python's
// fnmatch rules. An unparseable pattern never matches; it cannot panic and it
// cannot accidentally match everything.
func Match(pattern, name string) bool {
	re, err := compile(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(name)
}

// MatchAny reports whether name matches any of the patterns.
func MatchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if Match(p, name) {
			return true
		}
	}
	return false
}

// MatchFold reports whether name matches pattern ignoring ASCII and Unicode
// case. Use it wherever a mismatch would let a write reach a file the pattern
// was written to protect; see the package comment.
func MatchFold(pattern, name string) bool {
	re, err := compileFold(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(name)
}

// MatchAnyFold reports whether name matches any pattern, ignoring case.
func MatchAnyFold(patterns []string, name string) bool {
	for _, p := range patterns {
		if MatchFold(p, name) {
			return true
		}
	}
	return false
}

// QuoteMeta escapes a literal string so it matches only itself when used as a
// pattern.
//
// This is needed wherever a concrete path becomes a pattern — the override
// seam builds a grant's scope from the path that was blocked. Without it, a
// real file named "a[bc].go" yields a grant that also covers "ab.go" and
// "ac.go", so clearing one block silently clears three. Bracket escaping uses
// a single-character class rather than a backslash because that is what
// Python's fnmatch understands; a backslash is a literal there, not an escape.
func QuoteMeta(literal string) string {
	var b strings.Builder
	b.Grow(len(literal))
	for _, r := range literal {
		switch r {
		case '*', '?', '[':
			b.WriteByte('[')
			b.WriteRune(r)
			b.WriteByte(']')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func compile(pattern string) (*regexp.Regexp, error) {
	return compileInto(cache, pattern, "")
}

func compileFold(pattern string) (*regexp.Regexp, error) {
	return compileInto(foldCache, pattern, "(?i)")
}

func compileInto(store map[string]*regexp.Regexp, pattern, prefix string) (*regexp.Regexp, error) {
	cacheMu.RLock()
	re, ok := store[pattern]
	cacheMu.RUnlock()
	if ok {
		return re, nil
	}
	re, err := regexp.Compile(prefix + translate(pattern))
	if err != nil {
		return nil, err
	}
	cacheMu.Lock()
	store[pattern] = re
	cacheMu.Unlock()
	return re, nil
}

// translate converts a Python fnmatch pattern into an anchored Go regexp,
// following CPython's fnmatch.translate.
func translate(pattern string) string {
	var b strings.Builder
	b.WriteString(`(?s)\A`)

	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch c {
		case '*':
			// Consecutive stars collapse, so "**/x" is "*" then "/x" — which is
			// why "**/.env" needs a separator before ".env" and does not match a
			// bare ".env".
			b.WriteString(".*")
			for i+1 < len(runes) && runes[i+1] == '*' {
				i++
			}
		case '?':
			b.WriteString(".")
		case '[':
			if class, next, ok := charClass(runes, i); ok {
				b.WriteString(class)
				i = next
			} else {
				b.WriteString(regexp.QuoteMeta("["))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}

	b.WriteString(`\z`)
	return b.String()
}

// charClass parses a [...] set starting at runes[open], returning the regexp
// equivalent and the index of the closing bracket.
func charClass(runes []rune, open int) (string, int, bool) {
	i := open + 1
	if i < len(runes) && (runes[i] == '!' || runes[i] == '^') {
		i++
	}
	// A ']' immediately after the (negated) opening is a literal.
	if i < len(runes) && runes[i] == ']' {
		i++
	}
	for i < len(runes) && runes[i] != ']' {
		i++
	}
	if i >= len(runes) {
		return "", 0, false // unterminated: treat '[' as a literal
	}

	body := string(runes[open+1 : i])
	negated := false
	if strings.HasPrefix(body, "!") || strings.HasPrefix(body, "^") {
		negated = true
		body = body[1:]
	}
	if body == "" {
		return "", 0, false
	}

	// Escape the characters that mean something different inside a Go regexp
	// class than they do inside an fnmatch set. Ranges (a-z) are preserved.
	var esc strings.Builder
	for _, r := range body {
		switch r {
		case '\\', '[', ']', '^':
			esc.WriteRune('\\')
			esc.WriteRune(r)
		default:
			esc.WriteRune(r)
		}
	}

	if negated {
		return "[^" + esc.String() + "]", i, true
	}
	return "[" + esc.String() + "]", i, true
}
