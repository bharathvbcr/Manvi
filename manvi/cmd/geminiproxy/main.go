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
// request shapes are recorded, and the request capture omits header values and
// query values entirely.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
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

// maxProxyRequestBytes is the most this loopback recorder will accept from a
// peer. Past it the request is refused whole: forwarding a truncated body
// would record a request the upstream never saw.
const maxProxyRequestBytes = 32 << 20

func main() {
	addr := envOr("PROXY_ADDR", "127.0.0.1:8899")
	capturePath := envOr("PROXY_CAPTURE", "gemini-wire.log")

	capture, err := openCapture(capturePath)
	if err != nil {
		log.Fatal(err)
	}
	defer capture.Close()

	upstream := envOr("PROXY_UPSTREAM", defaultUpstream)
	p := &proxy{capture: capture, upstream: upstream, client: &http.Client{Timeout: 10 * time.Minute}}
	log.Printf("geminiproxy on %s -> %s, capturing to %s", addr, upstream, capturePath)
	log.Fatal(http.ListenAndServe(addr, p))
}

// openCapture creates the capture file readable by its owner alone. The capture
// holds request bodies, which carry the caller's prompts, so a file left
// group- or world-readable by the umask (or by an earlier run) is tightened too.
//
// The path is opened without following a final symlink, and what was opened
// must be a regular file this user owns before it is truncated or chmodded: a
// capture path that is a link to someone's file, a device or another user's file
// is refused rather than rewritten.
func openCapture(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|noFollow, 0o600) // #nosec G304 -- the operator's own PROXY_CAPTURE path
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || !ownedByCurrentUser(info) {
		_ = f.Close()
		return nil, fmt.Errorf("capture %s is not a regular file owned by this user", path)
	}
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// requestLabel is how the capture names a request: its method and the last
// path segment. Earlier segments are dropped because a gateway may carry a key
// in the path, and nothing in this repository can tell a key from a name.
func requestLabel(r *http.Request) string {
	p := r.URL.EscapedPath()
	last := p[strings.LastIndex(p, "/")+1:]
	return r.Method + " .../" + last
}

// transportClass is the kind of failure an upstream request ended in, and
// nothing else: net/http's error text carries the request URL, query and any
// redirect target, and with them the caller's key.
func transportClass(err error) string {
	var netErr net.Error
	var certInvalid x509.CertificateInvalidError
	var unknownAuth x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var certVerify *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var op *net.OpError
	var dns *net.DNSError
	text := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return "timeout"
	case strings.Contains(text, "redirect") || strings.Contains(text, "Location header"):
		return "redirect"
	case errors.As(err, &certInvalid) || errors.As(err, &unknownAuth) || errors.As(err, &hostname) ||
		errors.As(err, &certVerify) || errors.As(err, &record) || strings.Contains(text, "tls:"):
		return "tls"
	case errors.As(err, &dns) || (errors.As(err, &op) && op.Op == "dial"):
		return "dial"
	case strings.Contains(text, "unsupported protocol scheme") || strings.Contains(text, "malformed HTTP") || strings.Contains(text, "http:"):
		return "protocol"
	default:
		return "other"
	}
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
	_, _ = fmt.Fprintf(p.capture, format, args...)
	p.capture.Sync()
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxProxyRequestBytes+1))
	r.Body.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxProxyRequestBytes {
		http.Error(w, "request body exceeds proxy limit", http.StatusRequestEntityTooLarge)
		return
	}

	p.mu.Lock()
	p.n++
	n := p.n
	p.mu.Unlock()

	// The request is recorded as its shape and its body. Header *names* are
	// recorded because a wrong header is a real failure mode and the names are
	// not secret; no header value is ever written, because one of them is the
	// caller's API key. The query is dropped whole, since the API also takes the
	// key as a `key` parameter and a query has no part that is safe to keep.
	names := make([]string, 0, len(r.Header))
	for k := range r.Header {
		names = append(names, k)
	}
	sort.Strings(names)
	p.note("\n===== REQUEST %d %s (%d bytes) headers=%v =====\n%s\n",
		n, requestLabel(r), len(body), names, string(body))

	out, err := http.NewRequestWithContext(r.Context(), r.Method,
		p.upstream+r.URL.Path+"?"+r.URL.RawQuery, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		class := transportClass(err)
		p.note("===== RESPONSE %d TRANSPORT ERROR: %s =====\n", n, class)
		http.Error(w, "upstream request failed: "+class, http.StatusBadGateway)
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
