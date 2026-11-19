package hygiene

import "testing"

func TestEscapeRendersWhatARendererWouldObey(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "a tool that lists files", "a tool that lists files"},
		{"newline and tab survive", "a\n\tb", "a\n\tb"},
		{"bidi override", "evil\u202egpj.exe", `evil\u202egpj.exe`},
		{"zero width joiner", "ig\u200dnore", `ig\u200dnore`},
		{"escape sequence", "\x1b[2Jhi", `\x1b[2Jhi`},
		{"private use", "a\ue000b", `a\ue000b`},
		{"invalid utf8", "a\xffb", `a\xffb`},
		{"crlf is a newline", "a\r\nb", "a\nb"},
		{"lone carriage return", "a\rb", `a\x0db`},
		{"non-ascii text", "日本語 ok", "日本語 ok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Escape(c.in); got != c.want {
				t.Errorf("Escape(%q) = %q, want %q", c.in, got, c.want)
			}
			if again := Escape(Escape(c.in)); again != Escape(c.in) {
				t.Errorf("Escape is not idempotent on %q: %q then %q", c.in, Escape(c.in), again)
			}
		})
	}
}
