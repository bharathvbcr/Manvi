package transport

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func benchStream(n int, eol string, dataLen int) []byte {
	var b bytes.Buffer
	payload := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + strings.Repeat("a", dataLen) + `"}}`
	for i := 0; i < n; i++ {
		b.WriteString("event: content_block_delta" + eol + "data: " + payload + eol + eol)
	}
	return b.Bytes()
}

type benchChunked struct {
	b []byte
	n int
}

func (c *benchChunked) Read(p []byte) (int, error) {
	if len(c.b) == 0 {
		return 0, io.EOF
	}
	k := c.n
	if k > len(p) {
		k = len(p)
	}
	if k > len(c.b) {
		k = len(c.b)
	}
	copy(p, c.b[:k])
	c.b = c.b[k:]
	return k, nil
}

func benchRun(b *testing.B, data []byte, chunk int) {
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		var r io.Reader
		if chunk == 0 {
			r = bytes.NewReader(data)
		} else {
			r = &benchChunked{b: data, n: chunk}
		}
		s := NewSSE(io.NopCloser(r), "")
		for {
			if _, err := s.Next(); err != nil {
				break
			}
		}
	}
}

func BenchmarkSSELF2000Small(b *testing.B)      { benchRun(b, benchStream(2000, "\n", 40), 0) }
func BenchmarkSSECRLF2000Small(b *testing.B)    { benchRun(b, benchStream(2000, "\r\n", 40), 0) }
func BenchmarkSSELF2000Chunked(b *testing.B)    { benchRun(b, benchStream(2000, "\n", 40), 1400) }
func BenchmarkSSECRLF2000Chunked(b *testing.B)  { benchRun(b, benchStream(2000, "\r\n", 40), 1400) }
func BenchmarkSSELFLarge(b *testing.B)          { benchRun(b, benchStream(5, "\n", 200000), 0) }
func BenchmarkSSECRLFLarge(b *testing.B)        { benchRun(b, benchStream(5, "\r\n", 200000), 0) }
func BenchmarkSSELFLargeChunked(b *testing.B)   { benchRun(b, benchStream(5, "\n", 200000), 1400) }
func BenchmarkSSECRLFLargeChunked(b *testing.B) { benchRun(b, benchStream(5, "\r\n", 200000), 1400) }
