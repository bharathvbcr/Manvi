package fetch

import (
	"strings"
	"testing"
)

// The extractor's inputs are real web pages, which means malformed markup is
// the normal case rather than the edge one. Every test here is a shape a
// regexp-based stripper gets wrong.

func TestExtractDropsScriptsAndStyles(t *testing.T) {
	_, text := Extract(`<html><head>
		<style>body { color: red; content: "not prose"; }</style>
		<script>var x = 1; if (a < b) { alert("also not prose"); }</script>
		</head><body><p>The actual documentation.</p></body></html>`, "text/html")

	if !strings.Contains(text, "The actual documentation.") {
		t.Fatalf("the prose did not survive: %q", text)
	}
	for _, leaked := range []string{"color: red", "var x", "alert", "not prose"} {
		if strings.Contains(text, leaked) {
			t.Errorf("%q leaked into the text: %q", leaked, text)
		}
	}
}

// A `<` inside a script is an ordinary comparison and must not be read as the
// start of a tag — the failure that makes the rest of the script look like
// markup and its text like prose.
func TestExtractSurvivesComparisonsInsideScripts(t *testing.T) {
	_, text := Extract(`<body><script>for (i = 0; i < 10; i++) { }</script><p>after</p></body>`, "text/html")
	if strings.Contains(text, "i++") {
		t.Fatalf("script contents leaked: %q", text)
	}
	if !strings.Contains(text, "after") {
		t.Fatalf("the scanner lost its place after the script: %q", text)
	}
}

// A `>` inside a quoted attribute does not end the tag. A scanner that stopped
// at the first one would resume mid-attribute and print the remainder.
func TestExtractHonoursQuotedAttributes(t *testing.T) {
	_, text := Extract(`<p title="a > b" data-x='c > d'>visible</p>`, "text/html")
	if strings.Contains(text, "b\"") || strings.Contains(text, "d'") {
		t.Fatalf("attribute text leaked: %q", text)
	}
	if !strings.Contains(text, "visible") {
		t.Fatalf("the prose was lost: %q", text)
	}
}

// A comment can contain anything, including tags.
func TestExtractSkipsComments(t *testing.T) {
	_, text := Extract(`<body><!-- <p>hidden</p> secret --><p>shown</p></body>`, "text/html")
	if strings.Contains(text, "hidden") || strings.Contains(text, "secret") {
		t.Fatalf("comment contents leaked: %q", text)
	}
	if !strings.Contains(text, "shown") {
		t.Fatalf("the prose was lost: %q", text)
	}
}

// A body cut at the byte cap ends mid-markup routinely. Nothing here may
// panic, loop, or emit the truncated tag as prose.
func TestExtractSurvivesTruncatedMarkup(t *testing.T) {
	for _, body := range []string{
		`<p>text</p><div class="unterminated`,
		`<p>text</p><!-- unterminated comment`,
		`<p>text</p><script>var a = "unterminated`,
		`<`,
		`</`,
		`<!--`,
		``,
	} {
		_, text := Extract(body, "text/html")
		if strings.Contains(text, "unterminated") {
			t.Errorf("truncated markup leaked from %q: %q", body, text)
		}
	}
}

func TestExtractDecodesEntities(t *testing.T) {
	_, text := Extract(`<p>a &amp; b &mdash; c &lt;tag&gt; &#8212; &nbsp;d</p>`, "text/html")
	for _, want := range []string{"a & b", "<tag>", "—"} {
		if !strings.Contains(text, want) {
			t.Errorf("entity not decoded, wanted %q in %q", want, text)
		}
	}
	if strings.Contains(text, "&amp;") || strings.Contains(text, "&#8212;") {
		t.Fatalf("raw entities survived: %q", text)
	}
}

// Block elements imply line breaks. Without them a page of list items arrives
// as one sentence the model has to re-segment.
func TestExtractKeepsBlockStructure(t *testing.T) {
	_, text := Extract(`<ul><li>first</li><li>second</li></ul><p>para</p>`, "text/html")
	if strings.Contains(text, "firstsecond") {
		t.Fatalf("list items ran together: %q", text)
	}
	if !strings.Contains(text, "first") || !strings.Contains(text, "second") ||
		!strings.Contains(text, "para") {
		t.Fatalf("content was lost: %q", text)
	}
}

// Markup indentation must not dominate the output, and paragraph breaks must
// survive.
func TestExtractCollapsesWhitespaceWithoutFlattening(t *testing.T) {
	_, text := Extract("<p>one</p>\n\n\n\n<p>two</p>", "text/html")
	if strings.Contains(text, "\n\n\n") {
		t.Fatalf("blank-line runs were not collapsed: %q", text)
	}
	if !strings.Contains(text, "\n") {
		t.Fatalf("every break was flattened, so the page is one paragraph: %q", text)
	}
}

func TestExtractReadsTheTitle(t *testing.T) {
	title, _ := Extract(`<html><head><title>  Package fetch &mdash; docs </title></head><body>x</body></html>`,
		"text/html")
	if title != "Package fetch — docs" {
		t.Fatalf("title = %q", title)
	}
}

// Non-HTML passes through with only its whitespace normalised. Running a tag
// scanner over Markdown would eat its code fences.
func TestExtractLeavesNonHTMLAlone(t *testing.T) {
	md := "# Heading\n\n```go\nif a < b { return }\n```\n"
	_, text := Extract(md, "text/markdown")
	for _, want := range []string{"# Heading", "```go", "if a < b"} {
		if !strings.Contains(text, want) {
			t.Errorf("markdown was mangled, wanted %q in %q", want, text)
		}
	}
}

// The extracted text is bounded on runes, and says when it was cut. A byte cap
// upstream is not the same bound: markup reduces unpredictably.
func TestExtractedTextIsBoundedAndSaysSo(t *testing.T) {
	long := "<p>" + strings.Repeat("word ", maxExtractedRunes) + "</p>"
	_, text := Extract(long, "text/html")
	if len([]rune(text)) > maxExtractedRunes+200 {
		t.Fatalf("extracted %d runes, past the cap", len([]rune(text)))
	}
	if !strings.Contains(text, "not included") {
		t.Fatal("the text was cut without saying so, so a model reads a prefix as the whole page")
	}
}

// A multi-byte page cut at the byte cap ends mid-rune. The replacement
// character that produces is noise, not content.
func TestExtractDropsReplacementCharactersFromATruncatedBody(t *testing.T) {
	cut := []byte("<p>日本語のドキュメント</p>")
	_, text := Extract(string(cut[:len(cut)-6]), "text/html")
	if strings.ContainsRune(text, '�') {
		t.Fatalf("a mid-rune cut leaked into the text: %q", text)
	}
	if !strings.Contains(text, "日本語") {
		t.Fatalf("the readable prefix was lost: %q", text)
	}
}

// Deeply nested markup must not recurse: this is a scanner, and a page can nest
// as deep as it likes.
func TestExtractHandlesDeepNesting(t *testing.T) {
	body := strings.Repeat("<div>", 50000) + "deep" + strings.Repeat("</div>", 50000)
	_, text := Extract(body, "text/html")
	if !strings.Contains(text, "deep") {
		t.Fatal("content nested 50,000 deep was lost")
	}
}
