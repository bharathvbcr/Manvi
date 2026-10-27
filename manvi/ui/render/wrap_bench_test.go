package render

import (
	"strings"
	"testing"
)

// BenchmarkWrap160KB wraps one 160KB line to a 117-column body: with no space
// anywhere in it, which is all hard breaks, and as spaced words, which is all
// soft ones. A wrap that rescans what it has left at every break costs the
// square of the line, and a resize or a first layout pays it whole.
func BenchmarkWrap160KB(b *testing.B) {
	const b64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var tok strings.Builder
	for i := 0; tok.Len() < 160_000; i++ {
		tok.WriteByte(b64[(i*7+i/64)%64])
	}
	for _, tc := range []struct{ name, text string }{
		{"nospace", tok.String()},
		{"cjk", strings.Repeat("界", 53_000)},
		{"spaced", strings.Repeat("delta-text-20bytes! ", 8000)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			l := Styled(tc.text, Style{})
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = l.Wrap(117)
			}
		})
	}
}
