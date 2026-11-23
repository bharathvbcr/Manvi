package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const advKey = "AIzaSyADVB31-secret-key-0123456789"

func advProxy(t *testing.T, upstream string, client *http.Client) (*proxy, string) {
	t.Helper()
	capture, err := os.CreateTemp(t.TempDir(), "capture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = capture.Close() })
	return &proxy{capture: capture, upstream: upstream, client: client}, capture.Name()
}

func advRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func advServe(t *testing.T, p *proxy, method, target string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func TestAdvB31OpsKeyInQueryNeverReachesCaptureOrErrors(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer up.Close()
	p, path := advProxy(t, up.URL, up.Client())
	advServe(t, p, "POST", "/v1beta/x:streamGenerateContent?alt=sse&key="+advKey, nil, "{}")
	if got := advRead(t, path); strings.Contains(got, advKey) {
		t.Fatalf("key in the query reached the capture: %s", got)
	}
}

func TestAdvB31OpsKeyInHeaderValueNeverReachesCapture(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	p, path := advProxy(t, up.URL, up.Client())
	advServe(t, p, "POST", "/v1beta/x", map[string]string{"x-goog-api-key": advKey, "Authorization": "Bearer " + advKey}, "{}")
	if got := advRead(t, path); strings.Contains(got, advKey) {
		t.Fatalf("header value reached the capture: %s", got)
	}
}

// A key carried as a path segment (some gateways do) is recorded by
// EscapedPath. The change drops the query "whole"; the path is still written.
func TestAdvB31OpsKeyInPathNeverReachesCapture(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	p, path := advProxy(t, up.URL, up.Client())
	advServe(t, p, "POST", "/v1beta/"+advKey+"/models/x:generateContent", nil, "{}")
	if got := advRead(t, path); strings.Contains(got, advKey) {
		t.Fatalf("key in the path reached the capture: %s", got)
	}
}

// Characterisation, not a verdict: request bodies are captured verbatim by
// design, so a key a caller puts in a body field is recorded.
func TestAdvB31OpsBodyIsCapturedVerbatimByDesign(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	p, path := advProxy(t, up.URL, up.Client())
	advServe(t, p, "POST", "/v1beta/x", nil, `{"api_key":"`+advKey+`"}`)
	if got := advRead(t, path); !strings.Contains(got, advKey) {
		t.Fatalf("body no longer captured verbatim: %s", got)
	}
}

func TestAdvB31OpsTransportErrorsOfEveryKindOmitTheKey(t *testing.T) {
	tlsUp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer tlsUp.Close()
	var hits int
	loop := httptest.NewServer(nil)
	defer loop.Close()
	loop.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "/again?key="+advKey, http.StatusFound)
	})
	badLoc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://[::1?key="+advKey)
		w.WriteHeader(http.StatusFound)
	}))
	defer badLoc.Close()
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(500 * time.Millisecond) }))
	defer hang.Close()
	toRefused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/x?key="+advKey, http.StatusFound)
	}))
	defer toRefused.Close()
	scheme := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "gopher://x/y?key="+advKey, http.StatusFound)
	}))
	defer scheme.Close()

	for name, c := range map[string]struct {
		upstream string
		client   *http.Client
	}{
		"tls verification":      {tlsUp.URL, &http.Client{}},
		"tls handshake to http": {strings.Replace(tlsUp.URL, "https", "http", 1), &http.Client{}},
		"redirect loop":         {loop.URL, &http.Client{}},
		"bad Location":          {badLoc.URL, &http.Client{}},
		"client timeout":        {hang.URL, &http.Client{Timeout: 100 * time.Millisecond}},
		"redirect to refused":   {toRefused.URL, &http.Client{}},
		"unsupported scheme":    {scheme.URL, &http.Client{}},
		"custom transport":      {tlsUp.URL, &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{ServerName: advKey + ".example"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			p, path := advProxy(t, c.upstream, c.client)
			rec := advServe(t, p, "POST", "/v1beta/x?alt=sse&key="+advKey, nil, "{}")
			if rec.Code != http.StatusBadGateway {
				t.Skipf("status %d, not a transport failure", rec.Code)
			}
			if got := advRead(t, path); strings.Contains(got, advKey) {
				t.Errorf("capture carries the key: %s", got)
			}
			if strings.Contains(rec.Body.String(), advKey) {
				t.Errorf("502 body carries the key: %q", rec.Body.String())
			}
		})
	}
}

// openCapture on a symlink: O_TRUNC and fchmod both act on the link's target.
func TestAdvB31OpsCaptureSymlinkDoesNotTruncateOrChmodItsTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks and POSIX modes")
	}
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "gemini-wire.log")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	f, err := openCapture(link)
	if err == nil {
		_ = f.Close()
	}
	b, rerr := os.ReadFile(victim)
	if rerr != nil {
		t.Fatal(rerr)
	}
	info, _ := os.Stat(victim)
	if string(b) != "precious" || info.Mode().Perm() != 0o644 {
		t.Fatalf("through a symlink the capture opened (err=%v) and changed its target: content %q mode %v", err, b, info.Mode().Perm())
	}
}

// A group-writable file owned by someone else cannot be chmodded by us: the
// open must fail loudly rather than capture into a file another user controls.
func TestAdvB31OpsCaptureRefusesAFileThatCannotBeTightened(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a non-root POSIX user")
	}
	// /dev/null is world-writable and not ours: O_TRUNC and fchmod on it.
	before, err := os.Stat("/dev/null")
	if err != nil {
		t.Skip(err)
	}
	f, err := openCapture("/dev/null")
	if err == nil {
		_ = f.Close()
		t.Fatalf("openCapture accepted a file we do not own (/dev/null)")
	}
	after, _ := os.Stat("/dev/null")
	if after.Mode() != before.Mode() {
		t.Fatalf("mode of /dev/null changed %v -> %v", before.Mode(), after.Mode())
	}
}

func TestAdvB31OpsConcurrentRequestsKeepEveryBlockIntact(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Duration(len(r.URL.Path)%7) * time.Millisecond)
		_, _ = w.Write([]byte("RESP-" + r.Header.Get("X-Id") + "-" + strings.Repeat("z", 20000)))
	}))
	defer up.Close()
	p, path := advProxy(t, up.URL, up.Client())
	const n = 60
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := string(rune('A'+i%26)) + string(rune('a'+i/26))
			advServe(t, p, "POST", "/v1beta/x"+strings.Repeat("p", i), map[string]string{"X-Id": id}, "BODY-"+id+"-"+strings.Repeat("q", 5000))
		}()
	}
	wg.Wait()
	got := advRead(t, path)
	if c := strings.Count(got, "===== REQUEST "); c != n {
		t.Fatalf("%d request blocks, want %d", c, n)
	}
	if c := strings.Count(got, "===== END "); c != n {
		t.Fatalf("%d response blocks, want %d", c, n)
	}
	for i := range n {
		id := string(rune('A'+i%26)) + string(rune('a'+i/26))
		want := "RESP-" + id + "-" + strings.Repeat("z", 20000) + "\n===== END "
		if !strings.Contains(got, want) {
			t.Fatalf("response block for %s is not intact", id)
		}
	}
}
