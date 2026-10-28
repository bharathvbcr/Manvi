package transport

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// The oracle is an independent reading of the WHATWG event-stream grammar, kept
// to what this parser promises: events dispatched by a blank line with non-empty
// data, a final unterminated line processed at EOF. The fuzz drives both with
// random framings, comments, BOMs and chunkings (including empty reads and a
// final read that returns data with io.EOF).
//
// ---- WHATWG oracle (restricted to non-empty data, dispatched-by-blank-line events) ----

type oev struct{ name, data string }

func whatwg(stream []byte) []oev {
	s := string(stream)
	s = strings.TrimPrefix(s, "\xEF\xBB\xBF")
	// split lines on CRLF | CR | LF
	var lines []string
	cur := strings.Builder{}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
			lines = append(lines, cur.String())
			cur.Reset()
		case '\n':
			lines = append(lines, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(s[i])
		}
	}
	// Manvi (pre-existing, documented) processes a final unterminated line and dispatches a pending event at EOF.
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	lines = append(lines, "")
	var out []oev
	var name string
	var data []string
	for _, l := range lines {
		if l == "" {
			if len(data) > 0 {
				out = append(out, oev{name, strings.Join(data, "\n")})
			}
			name, data = "", nil
			continue
		}
		if l[0] == ':' {
			continue
		}
		f, v, ok := strings.Cut(l, ":")
		if !ok {
			f, v = l, ""
		}
		v = strings.TrimPrefix(v, " ")
		switch f {
		case "event":
			name = v
		case "data":
			data = append(data, v)
		}
	}
	return out
}

func manviEvents(r io.Reader) ([]oev, error) {
	s := NewSSE(io.NopCloser(r), "")
	var out []oev
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, oev{ev.Name, string(ev.Data)})
	}
}

// scripted reader: returns the given chunks, with optional (n>0, EOF) at end and empty reads between.
type chunkReader struct {
	chunks   [][]byte
	zeros    bool
	eofWithN bool
	i        int
	flip     bool
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.zeros && !c.flip && c.i < len(c.chunks) {
		c.flip = true
		return 0, nil
	}
	c.flip = false
	if c.i >= len(c.chunks) {
		return 0, io.EOF
	}
	ch := c.chunks[c.i]
	n := copy(p, ch)
	if n < len(ch) {
		c.chunks[c.i] = ch[n:]
		return n, nil
	}
	c.i++
	if c.eofWithN && c.i == len(c.chunks) {
		return n, io.EOF
	}
	return n, nil
}

func chunkify(b []byte, rng *rand.Rand) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		n := 1 + rng.IntN(7)
		if n > len(b) {
			n = len(b)
		}
		out = append(out, append([]byte(nil), b[:n]...))
		b = b[n:]
	}
	return out
}

func genStream(rng *rand.Rand) []byte {
	ends := []string{"\r\n", "\n", "\r"}
	var b bytes.Buffer
	if rng.IntN(3) == 0 {
		b.WriteString("\xEF\xBB\xBF")
	}
	nev := 1 + rng.IntN(4)
	for e := 0; e < nev; e++ {
		if rng.IntN(3) == 0 {
			b.WriteString(":c" + ends[rng.IntN(3)])
		}
		if rng.IntN(2) == 0 {
			b.WriteString("event: n" + fmt.Sprint(e) + ends[rng.IntN(3)])
		}
		nd := 1 + rng.IntN(3)
		for d := 0; d < nd; d++ {
			b.WriteString("data: v" + fmt.Sprint(e, d) + ends[rng.IntN(3)])
		}
		b.WriteString(ends[rng.IntN(3)]) // blank line
		if rng.IntN(4) == 0 {
			b.WriteString(ends[rng.IntN(3)]) // extra blank
		}
	}
	return b.Bytes()
}

func TestReaderMatchesTheEventStreamOracle(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for iter := 0; iter < 20000; iter++ {
		stream := genStream(rng)
		want := whatwg(stream)
		for mode := 0; mode < 4; mode++ {
			var r io.Reader
			switch mode {
			case 0:
				r = &chunkReader{chunks: chunkify(stream, rng)}
			case 1:
				r = &chunkReader{chunks: chunkify(stream, rng), zeros: true}
			case 2:
				r = &chunkReader{chunks: chunkify(stream, rng), eofWithN: true}
			case 3:
				r = iotest.OneByteReader(bytes.NewReader(stream))
			}
			got, err := manviEvents(r)
			if err != nil {
				t.Fatalf("mode %d stream %q: %v", mode, stream, err)
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("mode %d stream %q\n got  %v\n want %v", mode, stream, got, want)
			}
		}
	}
}

func TestReaderMatchesTheOracleAtChunkBoundaries(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
	}{
		{"CR|LF", []string{"data: a\r", "\ndata: b\r\n\r\n"}},
		{"CR|nonLF", []string{"data: a\r", "data: b\n\n"}},
		{"CR|EOF", []string{"data: a\n\r"}},
		{"CR CR|LF", []string{"data: a\r\r", "\n"}},
		{"BOM split 1|2", []string{"\xEF", "\xBB\xBFdata: a\n\n"}},
		{"BOM split 2|1", []string{"\xEF\xBB", "\xBFdata: a\n\n"}},
		{"BOM split 1|1|1", []string{"\xEF", "\xBB", "\xBFdata: a\n\n"}},
		{"BOM mid-stream", []string{"data: a\n\n", "\xEF\xBB\xBFdata: b\n\ndata: c\n\n"}},
		{"BOM only then data", []string{"\xEF\xBB\xBF", "data: a\n\n"}},
		{"double BOM", []string{"\xEF\xBB\xBF\xEF\xBB\xBFdata: a\n\n"}},
		{"partial BOM then data", []string{"\xEF\xBBdata: a\n\n"}},
		{"EF then non-BOM", []string{"\xEF", "\xBC\x80data: a\n\n"}},
		{"only CR", []string{"\r"}},
		{"CR CR CR", []string{"\r", "\r", "\r"}},
		{"data with raw CR", []string{"data: a\rb\n\n"}},
		{"CRLF CR split", []string{"data: a\r\n", "\r", "\n"}},
	}
	for _, c := range cases {
		var all strings.Builder
		var bs [][]byte
		for _, ch := range c.chunks {
			all.WriteString(ch)
			bs = append(bs, []byte(ch))
		}
		want := whatwg([]byte(all.String()))
		for _, eofN := range []bool{false, true} {
			got, err := manviEvents(&chunkReader{chunks: append([][]byte(nil), bs...), eofWithN: eofN})
			if err != nil {
				t.Errorf("%s: err %v", c.name, err)
				continue
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("%s (eofWithN=%v): got %v want %v", c.name, eofN, got, want)
			}
		}
	}
}

// Does a long line with many CRs, bigger than the 1MiB buffer, work?
func TestLongAndManyCarriageReturnFramedEvents(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("data: ")
	b.WriteString(strings.Repeat("x", 3<<20)) // 3 MiB in one line
	b.WriteString("\r\n\r\n")
	b.WriteString("data: ok\r\r")
	got, err := manviEvents(bytes.NewReader(b.Bytes()))
	t.Logf("events=%d err=%v", len(got), err)
	if err != nil {
		var o *ErrOversizedFrame
		if !errors.As(err, &o) {
			t.Fatal(err)
		}
	}
	// many CRs
	b.Reset()
	for i := 0; i < 200000; i++ {
		b.WriteString("data: a\r\r")
	}
	got, err = manviEvents(bytes.NewReader(b.Bytes()))
	if err != nil || len(got) != 200000 {
		t.Fatalf("events=%d err=%v", len(got), err)
	}
}

// zeroReader returns (0, nil) forever. bufio gives up on such a source with
// io.ErrNoProgress after 100 reads; the layer under it must not turn that into
// a spin.
type zeroReader struct{ n int }

func (z *zeroReader) Read(p []byte) (int, error) { z.n++; return 0, nil }

func TestASourceThatReturnsNothingForeverIsNoProgress(t *testing.T) {
	z := &zeroReader{}
	done := make(chan error, 1)
	go func() {
		s := NewSSE(io.NopCloser(z), "")
		_, err := s.Next()
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrNoProgress) {
			t.Fatalf("err = %v, want io.ErrNoProgress", err)
		}
	case <-timeAfterMs(2000):
		t.Fatalf("payloadReader spun: %d empty reads in 2s without surfacing io.ErrNoProgress", z.n)
	}
}

// TestAZeroLengthReadReturnsAtOnce: Read(nil) is a no-op, not a request to loop
// until the source produces something.
func TestAZeroLengthReadReturnsAtOnce(t *testing.T) {
	r := &payloadReader{src: &zeroReader{}, sse: &SSE{}}
	done := make(chan struct{})
	go func() {
		n, err := r.Read(nil)
		if n != 0 || err != nil {
			t.Errorf("Read(nil) = %d, %v", n, err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-timeAfterMs(2000):
		t.Fatal("Read with an empty buffer did not return")
	}
}

// TestABOMPrefixThenNothingIsNoProgress: the BOM wait reads on only while the
// source is making progress.
func TestABOMPrefixThenNothingIsNoProgress(t *testing.T) {
	src := io.MultiReader(strings.NewReader("\xEF"), &zeroReader{})
	done := make(chan error, 1)
	go func() {
		_, err := NewSSE(io.NopCloser(src), "").Next()
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrNoProgress) {
			t.Fatalf("err = %v, want io.ErrNoProgress", err)
		}
	case <-timeAfterMs(2000):
		t.Fatal("the BOM wait spun on an empty source")
	}
}

// A BOM is only a mark at the start of a stream (WHATWG). A frame that happens
// to begin with the bytes later is ordinary text, and the preflight's field
// reader has to read it the way the stream reader does.
func TestAMidStreamBOMIsReadTheSameWayByReaderAndFrameFields(t *testing.T) {
	frame := "\xEF\xBB\xBFevent: interaction.created\ndata: {}"
	name, _ := FrameFields(frame)
	got, err := manviEvents(strings.NewReader("data: a\n\n" + frame + "\n\n"))
	if err != nil || len(got) != 2 {
		t.Fatalf("reader events = %v, err = %v", got, err)
	}
	if got[1].name != name {
		t.Errorf("FrameFields reads %q but the stream reader reads %q for the same mid-stream frame", name, got[1].name)
	}
	if name != "" {
		t.Errorf("a BOM that does not start the stream was stripped: name = %q", name)
	}
}

func timeAfterMs(ms int) <-chan time.Time { return time.After(time.Duration(ms) * time.Millisecond) }
