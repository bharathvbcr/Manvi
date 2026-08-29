package fetch

import (
	"html"
	"strings"
	"unicode"
)

// HTML to text, by hand and with no dependencies.
//
// The Go standard library has no HTML parser — golang.org/x/net/html is the
// usual answer and would be this module's first dependency ever. It is not
// worth one here, and the reason is what this extractor is for: a model reading
// a documentation page needs the prose in order, without the navigation and
// without the scripts. It does not need a DOM, and nothing downstream asks a
// question a tree would answer.
//
// What this deliberately is not: readability-grade main-content extraction. It
// does not score blocks or find the article. A documentation page run through
// this comes back with its navigation and footer attached, and that is an
// honest limitation rather than a bug to be fixed by degrees — the alternative
// is a heuristic that silently drops the one paragraph that mattered, which is
// a worse failure than a bit of chrome the model can skim past.
//
// The scanner is a scanner and not a regexp for a specific reason: `<` inside
// an attribute value, a comment containing a tag, and an unterminated tag at
// the end of a truncated body are all ordinary in real pages, and all three
// break a pattern that pretends markup is a regular language.

// maxExtractedRunes bounds the text handed back.
//
// A page arrives already capped in bytes, but bytes and context are not the
// same currency: a 2 MB page of markup can reduce to far less prose or, in a
// pathological case, to more characters than the model's window. Bounded again
// here, on runes, where the cost is actually paid.
const maxExtractedRunes = 40000

// Extract reduces a document to a title and its prose.
//
// contentType steers it: anything that is not HTML is passed through as text
// with only its whitespace normalised, because running a tag scanner over
// Markdown would eat its code fences.
func Extract(body, contentType string) (title, text string) {
	switch contentType {
	case "text/html", "application/xhtml+xml", "":
		return extractHTML(body)
	default:
		return "", bound(collapse(body))
	}
}

// extractHTML walks the markup once.
func extractHTML(body string) (title, text string) {
	var out strings.Builder
	out.Grow(len(body) / 2)

	var titleBuf strings.Builder
	inTitle := false

	for i := 0; i < len(body); {
		ch := body[i]
		if ch != '<' {
			// Text. Runs to the next tag, or to the end of a truncated body.
			end := strings.IndexByte(body[i:], '<')
			if end < 0 {
				end = len(body) - i
			}
			chunk := body[i : i+end]
			out.WriteString(chunk)
			if inTitle {
				titleBuf.WriteString(chunk)
			}
			i += end
			continue
		}

		// A comment or a doctype. Skipped whole, because their contents are
		// not text and a `<` inside one is not a tag.
		if strings.HasPrefix(body[i:], "<!--") {
			if end := strings.Index(body[i+4:], "-->"); end >= 0 {
				i += 4 + end + 3
			} else {
				i = len(body) // unterminated comment in a truncated body
			}
			continue
		}
		if strings.HasPrefix(body[i:], "<!") {
			i = skipTag(body, i)
			continue
		}

		name, closing, selfClosing, next := readTag(body, i)
		i = next
		if name == "" {
			continue
		}

		switch {
		case !closing && !selfClosing && isOpaque(name):
			// The contents of a script or a style are not markup, and must not
			// be scanned as markup. This is HTML's own raw-text rule and it is
			// load-bearing rather than an optimisation: a comparison inside a
			// script — `if (a < b)` — opens what looks like a tag, and the
			// scanner that tokenised its way through it consumed the closing
			// `</script>` as part of an attribute and never came back out. One
			// page did that and the extractor returned an empty document.
			//
			// So the close tag is found by search, not by parsing.
			i = skipRawText(body, i, name)
		case name == "title":
			inTitle = !closing
		case isBlock(name):
			// A newline where the markup implied one, so paragraphs and list
			// items do not run together into a single line of prose.
			out.WriteByte('\n')
		}
	}

	return bound(collapse(html.UnescapeString(titleBuf.String()))),
		bound(collapse(html.UnescapeString(out.String())))
}

// skipRawText advances past the contents of a raw-text element and its closing
// tag.
//
// An unterminated element — a body cut at the byte cap mid-script — consumes
// the rest of the input rather than resuming inside it. That is the safe
// direction: the alternative is emitting a JavaScript fragment as prose.
func skipRawText(body string, i int, name string) int {
	closer := "</" + name
	for j := i; ; {
		k := indexFold(body[j:], closer)
		if k < 0 {
			return len(body)
		}
		at := j + k
		// The match must be the whole tag name, not a prefix of a longer one:
		// "</scriptable" is not the close tag of "script".
		after := at + len(closer)
		if after >= len(body) || !isNameByte(body[after]) {
			return skipTag(body, after)
		}
		j = after
	}
}

// indexFold is a case-insensitive IndexString for ASCII needles, which is what
// tag names are. strings.EqualFold on a sliding window would allocate; this
// does not.
func indexFold(haystack, needle string) int {
	n := len(needle)
	for i := 0; i+n <= len(haystack); i++ {
		if strings.EqualFold(haystack[i:i+n], needle) {
			return i
		}
	}
	return -1
}

// readTag reads one tag, returning its lowercased name, whether it closes,
// whether it closed itself, and the offset just past it.
//
// Quoted attribute values are honoured, because `<a title="a > b">` is valid
// and a scanner that stopped at the first `>` would resume mid-attribute and
// emit the rest of it as prose.
func readTag(body string, i int) (name string, closing, selfClosing bool, next int) {
	j := i + 1
	if j < len(body) && body[j] == '/' {
		closing = true
		j++
	}
	start := j
	for j < len(body) && isNameByte(body[j]) {
		j++
	}
	name = strings.ToLower(body[start:j])
	next = skipTag(body, j)
	// `<svg/>` closes itself and has no contents to skip. Looking for its close
	// tag would consume the rest of the document.
	if next >= 2 && next <= len(body) && body[next-2] == '/' {
		selfClosing = true
	}
	return name, closing, selfClosing, next
}

// skipTag advances past the remainder of a tag, respecting quotes.
func skipTag(body string, i int) int {
	var quote byte
	for ; i < len(body); i++ {
		c := body[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i + 1
		}
	}
	// Unterminated: a body cut at the byte cap ends mid-tag routinely.
	return len(body)
}

func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// isOpaque names elements whose text content is not prose.
//
// `script` and `style` are the whole reason this exists — their contents are
// code, and a page whose JavaScript is read as text is mostly JavaScript. The
// rest are markup the reader is not meant to see.
func isOpaque(name string) bool {
	switch name {
	case "script", "style", "noscript", "template", "svg", "canvas", "iframe", "object":
		return true
	}
	return false
}

// isBlock names elements that imply a line break.
//
// Not a complete list of block-level elements and not trying to be: it is the
// set whose absence actually runs text together in documentation — headings,
// paragraphs, list items, table rows, code blocks.
func isBlock(name string) bool {
	switch name {
	case "p", "div", "br", "hr", "section", "article", "header", "footer", "nav", "main", "aside",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"ul", "ol", "li", "dl", "dt", "dd",
		"table", "thead", "tbody", "tr", "th", "td",
		"pre", "blockquote", "figure", "figcaption", "form", "fieldset":
		return true
	}
	return false
}

// collapse normalises whitespace without destroying paragraph structure.
//
// Runs of spaces become one space and runs of blank lines become one blank
// line. Both halves matter: markup indentation would otherwise dominate the
// output, and flattening every newline would turn a page into one paragraph
// the model has to re-segment.
func collapse(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	newlines, spaces, wrote := 0, 0, false
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r':
			newlines++
			spaces = 0
		case unicode.IsSpace(r):
			spaces++
		default:
			if wrote {
				switch {
				case newlines >= 2:
					b.WriteString("\n\n")
				case newlines == 1:
					b.WriteByte('\n')
				case spaces > 0:
					b.WriteByte(' ')
				}
			}
			// Replacement characters come from a body cut mid-rune at the byte
			// cap. Dropped rather than passed on: they are noise the model
			// would have to reason about.
			if r != '�' {
				b.WriteRune(r)
				wrote = true
			}
			newlines, spaces = 0, 0
		}
	}
	return strings.TrimSpace(b.String())
}

// bound caps the text on a rune boundary and says that it did.
func bound(s string) string {
	runes := []rune(s)
	if len(runes) <= maxExtractedRunes {
		return s
	}
	return string(runes[:maxExtractedRunes]) +
		"\n\n[… the rest of this document was not included: it exceeds the extraction limit]"
}
