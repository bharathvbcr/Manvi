package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func releasesJSON(t *testing.T, rs ...releaseEntry) string {
	t.Helper()
	b, err := json.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func relEntry(tag string, pre bool) releaseEntry {
	return releaseEntry{TagName: tag, HTMLURL: "https://github.com/bharathvbcr/Manvi/releases/tag/" + tag, Prerelease: pre}
}

func checkerAt(srv *httptest.Server, prerelease bool) updateChecker {
	return updateChecker{client: srv.Client(), baseURL: srv.URL, timeout: 2 * time.Second, prerelease: prerelease}
}

func fakeReleases(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func stamped(v string) buildIdentity { return buildIdentity{Version: v, VersionFrom: "-ldflags"} }

func TestUpdateCheckOutcomes(t *testing.T) {
	list := releasesJSON(t, relEntry("v1.3.0-rc1", true), relEntry("v1.2.0", false), relEntry("v1.1.0", false))
	cases := []struct {
		name       string
		current    string
		prerelease bool
		want       string
		latest     string
	}{
		{"newer available", "v1.1.0", false, updateAvailable, "v1.2.0"},
		{"same version", "v1.2.0", false, updateUpToDate, "v1.2.0"},
		{"older remote", "v1.9.0", false, updateUpToDate, "v1.2.0"},
		{"prerelease ignored", "v1.2.0", false, updateUpToDate, "v1.2.0"},
		{"prerelease on request", "v1.2.0", true, updateAvailable, "v1.3.0-rc1"},
		{"unprefixed local version", "1.1.0", false, updateAvailable, "v1.2.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := fakeReleases(t, 200, list)
			rep, err := checkerAt(srv, tc.prerelease).check(context.Background(), stamped(tc.current))
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if rep.Status != tc.want || rep.Latest != tc.latest {
				t.Fatalf("report = %+v, want status %s latest %s", rep, tc.want, tc.latest)
			}
			if tc.want == updateAvailable && !strings.Contains(rep.URL, tc.latest) {
				t.Errorf("url %q does not name %s", rep.URL, tc.latest)
			}
		})
	}
}

func TestUpdateCheckSendsOnlyAStandardRequest(t *testing.T) {
	var got http.Header
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, path = r.Header.Clone(), r.URL.Path
		_, _ = io.WriteString(w, "[]")
	}))
	defer srv.Close()
	_, _ = checkerAt(srv, false).check(context.Background(), stamped("v1.0.0"))
	if got.Get("User-Agent") != "manvi/v1.0.0" {
		t.Errorf("User-Agent = %q", got.Get("User-Agent"))
	}
	for _, h := range []string{"Authorization", "Cookie", "X-Github-Token"} {
		if got.Get(h) != "" {
			t.Errorf("sent %s", h)
		}
	}
	if path != "/repos/bharathvbcr/Manvi/releases" {
		t.Errorf("path = %q", path)
	}
}

func TestUpdateCheckDevBuildCannotCompareAndMakesNoRequest(t *testing.T) {
	for _, v := range []string{"", "abc123", "1.2"} {
		srv, hits := fakeReleases(t, 200, "[]")
		rep, err := checkerAt(srv, false).check(context.Background(), stamped(v))
		if !errors.Is(err, errUpdateCannotCompare) {
			t.Fatalf("version %q: err = %v", v, err)
		}
		if rep.Status != updateCannotCompare || strings.Contains(rep.Error, "is up to date") || rep.Error == "" {
			t.Errorf("version %q: report = %+v", v, rep)
		}
		if hits.Load() != 0 {
			t.Errorf("version %q: made %d requests", v, hits.Load())
		}
	}
}

func TestUpdateCheckFailuresAreDistinctFromUpToDate(t *testing.T) {
	huge := `[{"tag_name":"v9.0.0","pad":"` + strings.Repeat("x", updateMaxBody) + `"}]`
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"forbidden", 403, `{"message":"rate limit"}`, "rate limited"},
		{"too many", 429, ``, "rate limited"},
		{"server error", 500, ``, "answered 500"},
		{"oversized", 200, huge, "larger than"},
		{"malformed json", 200, `{not json`, "not a release list"},
		{"not a list", 200, `{"tag_name":"v1.0.0"}`, "not a release list"},
		{"empty tag", 200, `[{"tag_name":""}]`, "empty tag"},
		// A lone non-semver tag used to be an error of its own. That was the
		// wrong expectation: it is skipped, so what fails is that nothing
		// eligible remains. Exit 8 still holds; only the reason differs.
		{"only non-semver tags", 200, `[{"tag_name":"nightly"}]`, "no eligible"},
		{"no releases", 200, `[]`, "no eligible"},
		{"only drafts and prereleases", 200, `[{"tag_name":"v2.0.0","draft":true},{"tag_name":"v2.1.0-rc1","prerelease":true}]`, "no eligible"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := fakeReleases(t, tc.status, tc.body)
			rep, err := checkerAt(srv, false).check(context.Background(), stamped("v1.0.0"))
			if !errors.Is(err, errUpdateCheckFailed) {
				t.Fatalf("err = %v, want errUpdateCheckFailed; report %+v", err, rep)
			}
			if rep.Status != updateFailed || !strings.Contains(rep.Error, tc.want) {
				t.Errorf("report = %+v, want error containing %q", rep, tc.want)
			}
		})
	}
}

func TestUpdateCheckSkipsAStrayTagBesideARealRelease(t *testing.T) {
	body := `[{"tag_name":"nightly"},{"tag_name":"v2.0.0"},{"tag_name":"latest"},{"tag_name":"v1.0.0"}]`
	srv, _ := fakeReleases(t, 200, body)
	rep, err := checkerAt(srv, false).check(context.Background(), stamped("v1.0.0"))
	if err != nil {
		t.Fatalf("a stray tag disabled the check: %v", err)
	}
	if rep.Status != updateAvailable || rep.Latest != "v2.0.0" || rep.SkippedTags != 2 {
		t.Fatalf("report = %+v", rep)
	}
	var out bytes.Buffer
	writeUpdateReport(&out, rep)
	if !strings.Contains(out.String(), "skipped    2 release tag(s)") {
		t.Errorf("text does not report the skipped tags:\n%s", out.String())
	}
	j, _ := json.Marshal(rep)
	if !strings.Contains(string(j), `"skipped_tags":2`) {
		t.Errorf("json = %s", j)
	}
}

func TestUpdateCheckPrintedTagCarriesNoControlCharacters(t *testing.T) {
	// Whitespace around a tag parses as semver; control characters inside do
	// not, and are skipped. Neither may reach the terminal.
	body := `[{"tag_name":"v3.0.0\u001b[31m"},{"tag_name":" v2.0.0\n","html_url":"https://x.example/a\u001b[0m"}]`
	srv, _ := fakeReleases(t, 200, body)
	rep, err := checkerAt(srv, false).check(context.Background(), stamped("v1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	writeUpdateReport(&out, rep)
	for _, r := range out.String() {
		if r < ' ' && r != '\n' || r == 0x7f {
			t.Fatalf("control character %q in output:\n%q", r, out.String())
		}
	}
	if rep.Latest != "v2.0.0" || rep.SkippedTags != 1 || !strings.HasSuffix(rep.URL, "/releases/tag/v2.0.0") {
		t.Errorf("report = %+v", rep)
	}
}

func TestUpdateCheckTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	c := updateChecker{client: srv.Client(), baseURL: srv.URL, timeout: 100 * time.Millisecond}
	start := time.Now()
	rep, err := c.check(context.Background(), stamped("v1.0.0"))
	if !errors.Is(err, errUpdateCheckFailed) || !strings.Contains(rep.Error, "within") {
		t.Fatalf("err = %v, report %+v", err, rep)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}

func TestUpdateCheckRejectsUnusableOverride(t *testing.T) {
	for _, base := range []string{"ftp://example.com", "not a url", "https://"} {
		c := updateChecker{client: http.DefaultClient, baseURL: base, timeout: time.Second}
		if _, err := c.check(context.Background(), stamped("v1.0.0")); !errors.Is(err, errUpdateCheckFailed) {
			t.Errorf("%q: err = %v", base, err)
		}
	}
}

func TestUpdateCheckReleaseLinkIsNotTrustedBlindly(t *testing.T) {
	body := `[{"tag_name":"v2.0.0","html_url":"javascript:alert(1)"}]`
	srv, _ := fakeReleases(t, 200, body)
	rep, err := checkerAt(srv, false).check(context.Background(), stamped("v1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.URL != "https://github.com/bharathvbcr/Manvi/releases/tag/v2.0.0" {
		t.Errorf("url = %q", rep.URL)
	}
}

func TestSemverPrecedence(t *testing.T) {
	order := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "2.0.0", "10.0.0"}
	for i, lo := range order {
		for j, hi := range order {
			a, ok1 := parseSemver(lo)
			b, ok2 := parseSemver(hi)
			if !ok1 || !ok2 {
				t.Fatalf("parse %s / %s", lo, hi)
			}
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := semverCompare(a, b); got != want {
				t.Errorf("compare(%s, %s) = %d, want %d", lo, hi, got, want)
			}
		}
	}
	a, _ := parseSemver("v1.0.0+build.5")
	b, _ := parseSemver("1.0.0")
	if semverCompare(a, b) != 0 {
		t.Error("build metadata must not affect precedence")
	}
	for _, bad := range []string{"", "v1", "1.2", "1.2.3.4", "01.2.3", "1.2.x", "1.2.3-", "1.2.3-a..b", "1.2.3-01", "1.2.3+", "nightly"} {
		if _, ok := parseSemver(bad); ok {
			t.Errorf("parseSemver(%q) accepted", bad)
		}
	}
}

// --- command level -------------------------------------------------------

func withChecker(t *testing.T, version string, c func() updateChecker) *atomic.Int32 {
	t.Helper()
	t.Setenv("MANVI_HARNESS_INIT_ENABLED", "false")
	var built atomic.Int32
	oldChecker, oldStamp := checkerForVersion, stampedVersion
	checkerForVersion = func(prerelease bool) updateChecker {
		built.Add(1)
		u := c()
		u.prerelease = prerelease
		return u
	}
	stampedVersion = version
	t.Cleanup(func() { checkerForVersion, stampedVersion = oldChecker, oldStamp })
	return &built
}

func TestPlainVersionNeverTouchesTheNetwork(t *testing.T) {
	srv, hits := fakeReleases(t, 200, "[]")
	built := withChecker(t, "v1.0.0", func() updateChecker { return checkerAt(srv, false) })
	for _, args := range [][]string{{"version"}, {"--version"}, {"version", "--json"}, {"--version", "--json"}} {
		var out, notes bytes.Buffer
		if err := run(&out, &notes, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if built.Load() != 0 || hits.Load() != 0 {
		t.Fatalf("plain version built %d checkers and made %d requests", built.Load(), hits.Load())
	}
}

func TestVersionCheckTextAndStatus(t *testing.T) {
	srv, hits := fakeReleases(t, 200, releasesJSON(t, relEntry("v1.2.0", false)))
	withChecker(t, "v1.0.0", func() updateChecker { return checkerAt(srv, false) })
	var out, notes bytes.Buffer
	if got := exitStatus(&out, &notes, []string{"version", "--check"}); got != 0 {
		t.Fatalf("status %d: %s %s", got, out.String(), notes.String())
	}
	if hits.Load() != 1 || !strings.Contains(out.String(), "update available: v1.0.0 -> v1.2.0") || !strings.Contains(out.String(), "nothing was downloaded") {
		t.Fatalf("hits %d, output:\n%s", hits.Load(), out.String())
	}
}

func TestVersionCheckJSON(t *testing.T) {
	srv, _ := fakeReleases(t, 200, releasesJSON(t, relEntry("v1.2.0", false)))
	withChecker(t, "v1.2.0", func() updateChecker { return checkerAt(srv, false) })
	var out, notes bytes.Buffer
	if got := exitStatus(&out, &notes, []string{"version", "--json", "--check"}); got != 0 {
		t.Fatalf("status %d: %s", got, notes.String())
	}
	var got struct {
		Name   string        `json:"name"`
		Update *updateReport `json:"update_check"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not one JSON object: %v\n%s", err, out.String())
	}
	if got.Name != "manvi" || got.Update == nil || got.Update.Status != updateUpToDate || got.Update.Latest != "v1.2.0" {
		t.Errorf("got %+v / %+v", got, got.Update)
	}
}

func TestVersionCheckExitStatuses(t *testing.T) {
	cases := []struct {
		name    string
		version string
		status  int
		body    string
		want    int
		stderr  string
	}{
		{"rate limited", "v1.0.0", 429, "", 8, "rate limited"},
		{"bad json", "v1.0.0", 200, "{", 8, "not a release list"},
		{"dev build", "", 200, "[]", 9, "cannot compare"},
	}
	seen := map[int]bool{0: true}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := fakeReleases(t, tc.status, tc.body)
			withChecker(t, tc.version, func() updateChecker { return checkerAt(srv, false) })
			var out, notes bytes.Buffer
			got := exitStatus(&out, &notes, []string{"version", "--check"})
			if got != tc.want || !strings.Contains(notes.String(), tc.stderr) {
				t.Fatalf("status %d (want %d), stderr %q", got, tc.want, notes.String())
			}
			if strings.Contains(out.String(), "status     up to date") {
				t.Errorf("failure printed as up to date:\n%s", out.String())
			}
			seen[got] = true
		})
	}
	if !seen[8] || !seen[9] {
		t.Error("statuses 8 and 9 not both exercised")
	}
}

func TestVersionRefusesPrereleaseWithoutCheck(t *testing.T) {
	t.Setenv("MANVI_HARNESS_INIT_ENABLED", "false")
	for _, args := range [][]string{{"version", "--prerelease"}, {"version", "--check", "--check"}, {"version", "--check", "--x"}} {
		var out, notes bytes.Buffer
		if err := run(&out, &notes, args); err == nil {
			t.Errorf("%v succeeded", args)
		}
	}
}

func TestUpdateCheckerUsesTheDocumentedOverride(t *testing.T) {
	t.Setenv(updateAPIEnv, "http://mirror.invalid/api")
	if c := newUpdateChecker(false); c.baseURL != "http://mirror.invalid/api" || c.timeout != updateTimeout || c.client.Timeout != updateTimeout {
		t.Errorf("checker = %+v", c)
	}
	t.Setenv(updateAPIEnv, "  ")
	if c := newUpdateChecker(false); c.baseURL != defaultUpdateAPI {
		t.Errorf("blank override gave %q", c.baseURL)
	}
}
