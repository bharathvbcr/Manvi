// geminiproxy is a loopback recording proxy for Google's interactions
// endpoint.
//
// It exists to answer one question the harness cannot answer about itself:
// what shape the live server actually uses for a streamed tool call's
// arguments. A short call carries the whole object inline; a long one -- the
// eight-prompt fan-out a multi-agent benchmark makes first -- cannot, because
// an SSE data line has to be valid JSON on its own, and the only remaining
// option is successive JSON strings. The adapter now reads both, and this
// records which one arrives so the claim rests on evidence.
//
// It forwards the request verbatim, including the caller's own API key header,
// and adds nothing. It listens on loopback only. The key is passed through
// untouched and is never written to the capture: only response bodies and
// request shapes are recorded, and the request capture omits headers entirely.
package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// defaultUpstream is the real API. It is overridable so this script can be
// smoke-tested against a stand-in without spending a live request, which is
// the difference between handing someone a benchmark and handing them an
// untested one.
const defaultUpstream = "https://generativelanguage.googleapis.com"

func main() {
	addr := envOr("PROXY_ADDR", "127.0.0.1:8899")
	capturePath := envOr("PROXY_CAPTURE", "gemini-wire.log")

	capture, err := os.Create(capturePath)
	if err != nil {
		log.Fatal(err)
	}
	defer capture.Close()

	upstream := envOr("PROXY_UPSTREAM", defaultUpstream)
	p := &proxy{capture: capture, upstream: upstream, client: &http.Client{Timeout: 10 * time.Minute}}
	log.Printf("geminiproxy on %s -> %s, capturing to %s", addr, upstream, capturePath)
	log.Fatal(http.ListenAndServe(addr, p))
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type proxy struct {
	mu       sync.Mutex
	n        int
	capture  *os.File
	upstream string
	client   *http.Client
}

func (p *proxy) note(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.capture, format, args...)
	p.capture.Sync()
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()

	p.mu.Lock()
	p.n++
	n := p.n
	p.mu.Unlock()

	// The request is recorded as its shape and its body. Header *names* are
	// recorded because a wrong header is a real failure mode and the names are
	// not secret; no header value is ever written, because one of them is the
	// caller's API key.
	names := make([]string, 0, len(r.Header))
	for k := range r.Header {
		names = append(names, k)
	}
	sort.Strings(names)
	p.note("\n===== REQUEST %d %s %s (%d bytes) headers=%v =====\n%s\n",
		n, r.Method, r.URL.RawPath+r.URL.Path+"?"+r.URL.RawQuery, len(body), names, string(body))

	out, err := http.NewRequestWithContext(r.Context(), r.Method,
		p.upstream+r.URL.Path+"?"+r.URL.RawQuery, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	for k, vs := range r.Header {
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}

	resp, err := p.client.Do(out)
	if err != nil {
		p.note("===== RESPONSE %d TRANSPORT ERROR: %v =====\n", n, err)
		http.Error(w, err.Error(), 502)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)

	// Streamed to the caller chunk by chunk and accumulated separately, then
	// written to the capture in one piece at the end.
	//
	// Writing each chunk to the capture as it arrived interleaved this proxy's
	// own "===== ... =====" markers into the middle of SSE frames, so a frame
	// could be split across a marker and read back as an event name that was
	// never sent. A capture that invents events is worse than no capture: this
	// file exists to be believed about what the wire carried.
	var recorded bytes.Buffer
	buf := make([]byte, 8192)
	for {
		read, err := resp.Body.Read(buf)
		if read > 0 {
			chunk := buf[:read]
			recorded.Write(chunk)
			if _, werr := w.Write(chunk); werr != nil {
				p.note("===== RESPONSE %d status=%d (caller went away) =====\n%s\n===== END %d =====\n",
					n, resp.StatusCode, recorded.String(), n)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			p.note("===== RESPONSE %d status=%d =====\n%s\n===== END %d (%v) =====\n",
				n, resp.StatusCode, recorded.String(), n, err)
			return
		}
	}
}
