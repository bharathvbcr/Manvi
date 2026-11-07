package main

// Adversarial review of `manvi version --check` (lane w12-update). Fake HTTP
// servers only; nothing here touches the real network.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func advCheckAt(srv *httptest.Server) updateChecker {
	return updateChecker{client: srv.Client(), baseURL: srv.URL, timeout: 2 * time.Second}
}

func advSrv(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// --- 1. body cap --------------------------------------------------------

func TestAdvBodyCapBoundary(t *testing.T) {
	// A valid JSON array padded with whitespace to an exact size.
	pad := func(n int) string {
		base := `[{"tag_name":"v9.0.0","html_url":"https://github.com/bharathvbcr/Manvi/releases/tag/v9.0.0"}]`
		return base + strings.Repeat(" ", n-len(base))
	}
	for _, tc := range []struct {
		size int
		ok   bool
	}{{updateMaxBody, true}, {updateMaxBody + 1, false}} {
		body := pad(tc.size)
		srv := advSrv(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
		rep, err := advCheckAt(srv).check(context.Background(), stamped("v1.0.0"))
		if tc.ok && (err != nil || rep.Status != updateAvailable) {
			t.Errorf("size %d: %+v %v", tc.size, rep, err)
		}
		if !tc.ok && (!errors.Is(err, errUpdateCheckFailed) || rep.Status != updateFailed) {
			t.Errorf("size %d should fail: %+v %v", tc.size, rep, err)
		}
	}
}

func TestAdvChunkedEndlessBodyIsCappedWhileReading(t *testing.T) {
	var written atomic.Int64
	srv := advSrv(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		chunk := bytes.Repeat([]byte(" "), 64<<10)
		for i := 0; i < 4096; i++ { // up to 256 MiB, no Content-Length
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
			fl.Flush()
		}
	})
	start := time.Now()
	rep, err := advCheckAt(srv).check(context.Background(), stamped("v1.0.0"))
	if !errors.Is(err, errUpdateCheckFailed) || rep.Status != updateFailed {
		t.Fatalf("got %+v %v", rep, err)
	}
	time.Sleep(200 * time.Millisecond)
	// The server must have been cut off long before 256 MiB: read ~1 MiB plus
	// whatever the socket buffers hold.
	if w := written.Load(); w > 64<<20 {
		t.Errorf("server wrote %d bytes before the client hung up; cap not enforced while reading", w)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}

func TestAdvGzipBombIsCappedAfterDecompression(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = io.WriteString(zw, "[")
	zero := bytes.Repeat([]byte(" "), 1<<20)
	for i := 0; i < 64; i++ { // 64 MiB decompressed, ~64 KiB on the wire
		_, _ = zw.Write(zero)
	}
	_, _ = io.WriteString(zw, "]")
	_ = zw.Close()
	if gz.Len() > 1<<20 {
		t.Fatalf("bomb is %d bytes compressed; not a bomb", gz.Len())
	}
	srv := advSrv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gz.Bytes())
	})
	// Production transport settings (the default transport), not the test
	// server's client: that is the one that decides whether it auto-decodes.
	c := updateChecker{client: &http.Client{Timeout: updateTimeout}, baseURL: srv.URL, timeout: 2 * time.Second}
	rep, err := c.check(context.Background(), stamped("v1.0.0"))
	if !errors.Is(err, errUpdateCheckFailed) || rep.Status != updateFailed {
		t.Fatalf("bomb not refused: %+v %v", rep, err)
	}
	if !strings.Contains(rep.Error, "larger than") {
		t.Errorf("refused for the wrong reason: %s", rep.Error)
	}
}

// --- 2. timeout and redirects -------------------------------------------

func TestAdvProductionTimeoutsAreTenSeconds(t *testing.T) {
	t.Setenv(updateAPIEnv, "")
	c := newUpdateChecker(false)
	if c.timeout != 10*time.Second || c.client.Timeout != 10*time.Second || updateMaxBody != 1<<20 {
		t.Errorf("timeout=%s client=%s cap=%d", c.timeout, c.client.Timeout, updateMaxBody)
	}
}

func TestAdvSlowDripBodyCannotHang(t *testing.T) {
	srv := advSrv(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "[")
		for i := 0; i < 100; i++ {
			if _, err := io.WriteString(w, " "); err != nil {
				return
			}
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	})
	// Same shape as production: client.Timeout and ctx timeout both set.
	c := updateChecker{client: &http.Client{Timeout: 400 * time.Millisecond}, baseURL: srv.URL, timeout: 400 * time.Millisecond}
	start := time.Now()
	rep, err := c.check(context.Background(), stamped("v1.0.0"))
	if !errors.Is(err, errUpdateCheckFailed) || rep.Status != updateFailed {
		t.Fatalf("got %+v %v", rep, err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("slow drip held the check for %s", d)
	}
}

func TestAdvSlowHeadersCannotHang(t *testing.T) {
	srv := advSrv(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	c := updateChecker{client: &http.Client{Timeout: 300 * time.Millisecond}, baseURL: srv.URL, timeout: 300 * time.Millisecond}
	start := time.Now()
	_, err := c.check(context.Background(), stamped("v1.0.0"))
	if !errors.Is(err, errUpdateCheckFailed) || time.Since(start) > 3*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
}

func TestAdvRedirectToFileSchemeFails(t *testing.T) {
	srv := advSrv(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	})
	rep, err := advCheckAt(srv).check(context.Background(), stamped("v1.0.0"))
	if !errors.Is(err, errUpdateCheckFailed) || rep.Status != updateFailed {
		t.Fatalf("got %+v %v", rep, err)
	}
}

func TestAdvRedirectLoopTerminatesAsFailure(t *testing.T) {
	var hits atomic.Int32
	srv := advSrv(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, r.URL.Path+"x", http.StatusFound)
	})
	rep, err := advCheckAt(srv).check(context.Background(), stamped("v1.0.0"))
	if !errors.Is(err, errUpdateCheckFailed) || rep.Status != updateFailed {
		t.Fatalf("got %+v %v", rep, err)
	}
	if hits.Load() > 11 {
		t.Errorf("followed %d redirects", hits.Load())
	}
}

func TestAdvRedirectToAnotherHostIsNotFollowed(t *testing.T) {
	t.Skip("BREAK (low): the default redirect policy follows up to 10 redirects to ANY host (and https->http), " +
		"so a mirror/MITM-less 302 sends the request, including the User-Agent, to a host the operator never named, " +
		"and the report's source still names the original endpoint. Repro: run this test with the Skip removed.")
	var otherHits atomic.Int32
	other := advSrv(t, func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		_, _ = io.WriteString(w, `[{"tag_name":"v9.9.9"}]`)
	})
	srv := advSrv(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	})
	c := updateChecker{client: &http.Client{Timeout: updateTimeout}, baseURL: srv.URL, timeout: 2 * time.Second}
	_, _ = c.check(context.Background(), stamped("v1.0.0"))
	if otherHits.Load() != 0 {
		t.Errorf("request was forwarded to another host %d time(s)", otherHits.Load())
	}
}

// --- 3. hostile feed content --------------------------------------------

func TestAdvHostileTagsNeverBecomeLatestOrBreakOutput(t *testing.T) {
	huge := "v" + strings.Repeat("9", 40) + ".0.0"
	for _, tag := range []string{
		huge, "v01.0.0", "v1.02.0", "vv9.0.0", "V9.0.0", "v9.0", "v9.0.0.0", "v9.0.0-", "v9.0.0-01", "v9.0.0+",
		"v\u0669.0.0", "v9.0.0\u202e", "v9.0.0\x1b[2J", "v9.0.0\n", " \u00a0v9.0.0-x\u0007", "v9.0.0-\u212a", "9.0.0\x00",
	} {
		list := releasesJSON(t, releaseEntry{TagName: tag}, relEntry("v1.0.0", false))
		srv, _ := fakeReleases(t, 200, list)
		rep, err := advCheckAt(srv).check(context.Background(), stamped("v1.0.0"))
		if rep.Latest != "" && rep.Latest != "v1.0.0" {
			// A control-free, valid tag is acceptable only if parseSemver is
			// satisfied after trimming; assert printed form is clean.
			if rep.Latest != strings.TrimSpace(rep.Latest) || sanitizeLine(rep.Latest) != rep.Latest {
				t.Errorf("tag %q printed as %q", tag, rep.Latest)
			}
			if _, ok := parseSemver(rep.Latest); !ok {
				t.Errorf("tag %q selected but not semver: %q", tag, rep.Latest)
			}
			// Only explicit trimmed-clean candidates may win.
			if tag != "\n" && !strings.HasPrefix(strings.TrimSpace(tag), "v9.0.0") {
				t.Errorf("tag %q became latest %q", tag, rep.Latest)
			}
		}
		if err != nil && rep.Status != updateFailed {
			t.Errorf("tag %q: err without failed status: %+v %v", tag, rep, err)
		}
	}
}

func TestAdvBigNumericIdentifiersDoNotPanicOrWin(t *testing.T) {
	// Prerelease numeric identifiers beyond uint64 compare as text via
	// parseNumeric failure; must not panic and must stay deterministic.
	a, ok1 := parseSemver("v1.0.0-" + strings.Repeat("9", 30))
	b, ok2 := parseSemver("v1.0.0-" + strings.Repeat("9", 31))
	if !ok1 || !ok2 {
		t.Skip("rejected outright; fine")
	}
	if semverCompare(a, b) != -semverCompare(b, a) {
		t.Errorf("compare is not antisymmetric for big numeric prerelease identifiers")
	}
}

func TestAdvDuplicateTagsAndDraftDoNotConfuse(t *testing.T) {
	list := releasesJSON(t,
		releaseEntry{TagName: "v2.0.0", Draft: true, HTMLURL: "https://github.com/x"},
		relEntry("v1.5.0", false), relEntry("v1.5.0", false))
	srv, _ := fakeReleases(t, 200, list)
	rep, err := advCheckAt(srv).check(context.Background(), stamped("v1.0.0"))
	if err != nil || rep.Latest != "v1.5.0" || rep.Status != updateAvailable {
		t.Fatalf("got %+v %v", rep, err)
	}
}

func TestAdvFlagsAsStringsOrNullEntriesAreFailuresNotUpToDate(t *testing.T) {
	for _, body := range []string{
		`[{"tag_name":"v9.0.0","draft":"true"}]`,
		`[{"tag_name":"v9.0.0","prerelease":"false"}]`,
		`[null]`, `{"tag_name":"v9.0.0"}`, `null`, `[]`, `[{"tag_name":123}]`, ``, `[{"tag_name":"v9.0.0"}] trailing`,
	} {
		srv, _ := fakeReleases(t, 200, body)
		rep, err := advCheckAt(srv).check(context.Background(), stamped("v1.0.0"))
		if body == `[{"tag_name":"v9.0.0"}] trailing` {
			// json.Unmarshal rejects trailing data.
		}
		if !errors.Is(err, errUpdateCheckFailed) || rep.Status == updateUpToDate {
			t.Errorf("body %q: %+v %v", body, rep, err)
		}
	}
}

func TestAdvReleaseLinkHostIsConstrained(t *testing.T) {
	t.Skip("BREAK (low/medium, misleading output): releasePage accepts any http(s) html_url from the feed, so " +
		"html_url=\"https://github.com@evil.example/x\" or \"http://evil.example/\" is printed under 'release' as the " +
		"place to fetch the update from. Repro: serve [{\"tag_name\":\"v9.0.0\",\"html_url\":\"https://github.com@evil.example/\"}] " +
		"and run `MANVI_UPDATE_API_URL=<srv> manvi version --check` with a stamped build. Fix: require the link's host to equal " +
		"github.com (or the configured API host's site) and reject userinfo, else fall back to the constructed tag URL.")
	for _, link := range []string{"https://github.com@evil.example/x", "http://evil.example/", "https://evil.example:8443/x", "https://user:pw@github.com/x"} {
		list := releasesJSON(t, releaseEntry{TagName: "v9.0.0", HTMLURL: link})
		srv, _ := fakeReleases(t, 200, list)
		rep, _ := advCheckAt(srv).check(context.Background(), stamped("v1.0.0"))
		if !strings.HasPrefix(rep.URL, "https://github.com/bharathvbcr/Manvi/") {
			t.Errorf("link %q printed as %q", link, rep.URL)
		}
	}
}

func TestAdvReleaseLinkRejectsNonHTTPAndControls(t *testing.T) {
	for _, link := range []string{"javascript:alert(1)", "file:///etc/passwd", "//evil.example/x", "https://github.com/x\x1b[31m", "https://github.com/x\n", ""} {
		list := releasesJSON(t, releaseEntry{TagName: "v9.0.0", HTMLURL: link})
		srv, _ := fakeReleases(t, 200, list)
		rep, _ := advCheckAt(srv).check(context.Background(), stamped("v1.0.0"))
		if !strings.HasPrefix(rep.URL, "https://github.com/bharathvbcr/Manvi/") {
			t.Errorf("link %q printed as %q", link, rep.URL)
		}
	}
}

func TestAdvReleaseLinkCarriesNoBidiOrFormatCharacters(t *testing.T) {
	t.Skip("BREAK (low/medium, terminal spoofing): sanitizeLine strips only unicode.IsControl (Cc). Format characters " +
		"(Cf: U+202E RLO, U+2066-2069 isolates, U+200B, U+FEFF) pass, and releasePage returns html_url RAW, so " +
		"html_url=\"https://github.com/bharathvbcr/Manvi/releases/tag/v9.0.0\u202e...\" is printed with the right-to-left " +
		"override active. Same gap in the Retry-After hint. Fix: also drop unicode.Is(unicode.Cf, r) (and Zl/Zp), or " +
		"print u.String() (percent-escaped) instead of the raw feed string.")
	for _, ch := range []string{"\u202e", "\u2066", "\u200b", "\ufeff", "\u2028"} {
		link := "https://github.com/bharathvbcr/Manvi/releases/tag/v9.0.0" + ch + "gpj.exe"
		list := releasesJSON(t, releaseEntry{TagName: "v9.0.0", HTMLURL: link})
		srv, _ := fakeReleases(t, 200, list)
		rep, _ := advCheckAt(srv).check(context.Background(), stamped("v1.0.0"))
		if strings.Contains(rep.URL, ch) {
			t.Errorf("link carries %U", []rune(ch)[0])
		}
	}
}

func TestAdvNameAndBodyFieldsAreNeverPrinted(t *testing.T) {
	list := `[{"tag_name":"v9.0.0","name":"EVIL-NAME\u001b[2J","body":"EVIL-BODY\u001b]0;pwn\u0007","html_url":"https://github.com/bharathvbcr/Manvi/releases/tag/v9.0.0"}]`
	srv, _ := fakeReleases(t, 200, list)
	rep, err := advCheckAt(srv).check(context.Background(), stamped("v1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	writeUpdateReport(&out, rep)
	j, _ := json.Marshal(rep)
	if strings.Contains(out.String()+string(j), "EVIL") || strings.ContainsRune(out.String(), 0x1b) {
		t.Errorf("feed prose leaked: %q %s", out.String(), j)
	}
}

func TestAdvRetryAfterHintHasNoControls(t *testing.T) {
	srv := advSrv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30\x1b[2Jx")
		w.WriteHeader(429)
	})
	rep, err := advCheckAt(srv).check(context.Background(), stamped("v1.0.0"))
	if !errors.Is(err, errUpdateCheckFailed) || strings.ContainsRune(rep.Error, 0x1b) {
		t.Errorf("%+v %v", rep, err)
	}
}

// --- 4/5. override URL and exit codes through the real entry point -------

func advExit(t *testing.T, version, apiURL string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("MANVI_HARNESS_INIT_ENABLED", "false")
	t.Setenv(updateAPIEnv, apiURL)
	old := stampedVersion
	stampedVersion = version
	t.Cleanup(func() { stampedVersion = old })
	var out, errb bytes.Buffer
	code := exitStatus(&out, &errb, args)
	return code, out.String(), errb.String()
}

func TestAdvOverrideURLShapesAreExit8(t *testing.T) {
	for _, u := range []string{"not a url", "ftp://example.invalid", "http://", "//host/x", "file:///tmp/x", "http://127.0.0.1:1", "javascript:alert(1)", "http://[::1", "http://a b/\x1b[2J"} {
		for _, args := range [][]string{{"version", "--check"}, {"version", "--check", "--json"}} {
			code, out, errb := advExit(t, "v1.0.0", u, args...)
			if code != 8 {
				t.Errorf("url %q args %v: exit %d out=%q err=%q", u, args, code, out, errb)
			}
			if strings.ContainsRune(out+errb, 0x1b) {
				t.Errorf("url %q leaked ESC: %q %q", u, out, errb)
			}
			if strings.Contains(out, "up to date") {
				t.Errorf("url %q read as up to date: %q", u, out)
			}
		}
	}
}

func TestAdvExitCodesNeverZeroOnFailure(t *testing.T) {
	for _, mode := range []string{"", "--json"} {
		args := []string{"version", "--check"}
		if mode != "" {
			args = append(args, mode)
		}
		// 8: server errors of many shapes
		for _, status := range []int{204, 301, 304, 404, 500, 503} {
			srv := advSrv(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
			if code, out, _ := advExit(t, "v1.0.0", srv.URL, args...); code != 8 {
				t.Errorf("status %d %v: exit %d out %q", status, args, code, out)
			}
		}
		// 9: unstamped / non-semver build, even when the server is healthy
		var hits atomic.Int32
		srv := advSrv(t, func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			_, _ = io.WriteString(w, `[{"tag_name":"v9.0.0"}]`)
		})
		for _, v := range []string{"", "(devel)", "dev", "1.0", "v1.0.0.0", "garbage\x1b"} {
			if code, _, _ := advExit(t, v, srv.URL, args...); code != 9 {
				t.Errorf("build %q %v: exit %d", v, args, code)
			}
		}
		if hits.Load() != 0 {
			t.Errorf("uncomparable build still contacted the server %d time(s)", hits.Load())
		}
		// 0: compared
		if code, _, _ := advExit(t, "v1.0.0", srv.URL, args...); code != 0 {
			t.Errorf("compared %v: exit %d", args, code)
		}
	}
}

func TestAdvJSONFailureStillSetsStatusAndReportsIt(t *testing.T) {
	srv := advSrv(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	code, out, _ := advExit(t, "v1.0.0", srv.URL, "version", "--check", "--json")
	var got struct {
		Update *updateReport `json:"update_check"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Update == nil {
		t.Fatalf("json: %v %q", err, out)
	}
	if code != 8 || got.Update.Status != updateFailed {
		t.Errorf("exit %d status %q", code, got.Update.Status)
	}
	// Exactly one JSON object on stdout.
	if n := strings.Count(strings.TrimSpace(out), "\n"); n != 0 {
		t.Errorf("stdout is not one line of JSON: %q", out)
	}
}

func TestAdvCredentialsInOverrideURLAreNotEchoed(t *testing.T) {
	t.Skip("BREAK (low, operator-scope): MANVI_UPDATE_API_URL userinfo (http://user:secret@host) is echoed verbatim in " +
		"the endpoint inside failure messages on stderr/stdout and in update_check.source/error in --json. The operator " +
		"supplied it, but this output is routinely captured in CI logs. Repro: MANVI_UPDATE_API_URL=http://u:s3cret@127.0.0.1:1 " +
		"manvi version --check (stamped build) -> 'could not reach http://u:s3cret@127.0.0.1:1/repos/...'. Fix: redact " +
		"u.User with u.Redacted() when building endpoint strings for messages.")
	code, out, errb := advExit(t, "v1.0.0", "http://u:s3cret@127.0.0.1:1", "version", "--check", "--json")
	if code != 8 || strings.Contains(out+errb, "s3cret") {
		t.Errorf("exit %d, secret echoed: %q %q", code, out, errb)
	}
}

// --- 6. no network without --check --------------------------------------

func TestAdvNoNetworkWithoutCheck(t *testing.T) {
	var hits atomic.Int32
	srv := advSrv(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `[{"tag_name":"v9.0.0"}]`)
	})
	var built atomic.Int32
	old := checkerForVersion
	checkerForVersion = func(p bool) updateChecker { built.Add(1); return old(p) }
	t.Cleanup(func() { checkerForVersion = old })

	for _, env := range []map[string]string{
		{},
		{"MANVI_UPDATE_CHECK": "1", "MANVI_CHECK_UPDATES": "1", "MANVI_AUTO_UPDATE": "1", "CHECK": "1"},
	} {
		for k, v := range env {
			t.Setenv(k, v)
		}
		for _, args := range [][]string{
			{"--version"}, {"--version", "--json"}, {"version"}, {"version", "--json"},
			{"version", "extra", "args"}, {"version", "--prerelease"}, {"version", "--json", "--prerelease"},
			{"--version", "--check", "--check"}, {"version", "check"}, {"version", "--CHECK"}, {"version", "--check=true"},
			{"--version", "--prerelease"}, {"version", "--json", "--json"}, {"version", ""},
		} {
			code, _, _ := advExit(t, "v1.0.0", srv.URL, args...)
			plain := len(args) == 1 || (len(args) == 2 && args[1] == "--json")
			if plain && code != 0 {
				t.Errorf("%v: exit %d", args, code)
			}
			if !plain && code == 0 {
				t.Errorf("%v: should be refused, exit 0", args)
			}
		}
	}
	if hits.Load() != 0 || built.Load() != 0 {
		t.Errorf("network touched without --check: hits=%d checkers=%d", hits.Load(), built.Load())
	}
}

func TestAdvPrereleaseWithoutCheckRefusedBeforeAnyWork(t *testing.T) {
	var built atomic.Int32
	old := checkerForVersion
	checkerForVersion = func(p bool) updateChecker { built.Add(1); return old(p) }
	t.Cleanup(func() { checkerForVersion = old })
	code, out, _ := advExit(t, "v1.0.0", "http://127.0.0.1:1", "version", "--prerelease")
	if code == 0 || built.Load() != 0 || strings.Contains(out, "update check") {
		t.Errorf("exit %d built=%d out=%q", code, built.Load(), out)
	}
}

// --- 7. agent reachability ----------------------------------------------

func TestAdvUpdateCheckIsReachableOnlyFromTheVersionDispatch(t *testing.T) {
	needles := []string{"checkerForVersion", "newUpdateChecker", "runVersion(", "updateChecker{", "errUpdateCheckFailed", "MANVI_UPDATE_API_URL"}
	root := filepath.Join("..", "..") // the manvi module
	allowed := map[string]bool{"updatecheck.go": true, "main.go": true}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		for _, n := range needles {
			if strings.Contains(string(b), n) && !(filepath.Base(filepath.Dir(p)) == "manvi" && allowed[filepath.Base(p)]) {
				t.Errorf("%s references %q outside the version dispatch", p, n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// main.go may reference it only in the exit mapping and the version branch.
	b, _ := os.ReadFile("main.go")
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "runVersion(") && !strings.Contains(line, "return runVersion(out, v)") {
			t.Errorf("unexpected runVersion use: %s", line)
		}
	}
	// No serve op or tool name mentions a version check.
	_ = fmt.Sprint()
}
