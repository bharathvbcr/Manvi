package main

// `manvi version --check`: ask the project's own release location whether a
// newer release exists.
//
// This is the only code in the binary that contacts the network to learn
// about Manvi itself, and it is reachable from exactly one place: the user
// typing --check. Nothing runs it at startup, on a timer or from a tool, and
// it is not registered as an agent tool, so a model cannot make the call. It
// never downloads or installs anything; it names the newer version and the
// page to fetch it from.
//
// Every outcome other than a completed comparison is a failure with its own
// message and exit status. "Could not find out" must never be readable as "up
// to date", which is the one wrong answer this command can give.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	// updateAPIEnv overrides the release API root, for a mirror or an air-gapped
	// operator. It is operator scope: read from the environment only.
	updateAPIEnv = "MANVI_UPDATE_API_URL"

	defaultUpdateAPI = "https://api.github.com"
	// releaseRepo is where this project publishes releases (the origin of this
	// repository and the module path both name it).
	releaseRepo = "bharathvbcr/Manvi"

	updateTimeout = 10 * time.Second
	// A page of releases is a few tens of KiB; a megabyte is far past any
	// honest answer, and the cap is what stops a hostile mirror streaming
	// forever into memory.
	updateMaxBody  = 1 << 20
	updatePageSize = 30
)

// Statuses of an update check, as written to `update_check.status` in JSON.
const (
	updateUpToDate      = "up_to_date"
	updateAvailable     = "update_available"
	updateCannotCompare = "cannot_compare"
	updateFailed        = "check_failed"
)

var (
	// errUpdateCheckFailed: the question could not be answered (network, rate
	// limit, a response that is not a usable release list).
	errUpdateCheckFailed = errors.New("update check failed")
	// errUpdateCannotCompare: this build has no release version to compare.
	errUpdateCannotCompare = errors.New("update check cannot compare")
)

// updateReport is the structured result, shared by the text and JSON output.
type updateReport struct {
	Status  string `json:"status"`
	Current string `json:"current,omitempty"`
	Latest  string `json:"latest,omitempty"`
	URL     string `json:"url,omitempty"`
	// SkippedTags counts release tags that are not semantic versions and so
	// took no part in the comparison; they are skipped, not fatal.
	SkippedTags int    `json:"skipped_tags,omitempty"`
	Source      string `json:"source,omitempty"`
	Error       string `json:"error,omitempty"`
}

// updateChecker holds everything the check touches outside the process, so a
// test points it at a fake server and never at the real network.
type updateChecker struct {
	client     *http.Client
	baseURL    string
	timeout    time.Duration
	prerelease bool
}

// newUpdateChecker builds the production checker. An unset or blank override
// means the default; an unusable one is reported by check, not ignored.
func newUpdateChecker(prerelease bool) updateChecker {
	base := strings.TrimSpace(os.Getenv(updateAPIEnv))
	if base == "" {
		base = defaultUpdateAPI
	}
	return updateChecker{
		client:     &http.Client{Timeout: updateTimeout},
		baseURL:    base,
		timeout:    updateTimeout,
		prerelease: prerelease,
	}
}

// checkerForVersion is the seam the command dispatches through, so a test can
// prove that plain `version` never constructs a request.
var checkerForVersion = newUpdateChecker

type releaseEntry struct {
	TagName    string `json:"tag_name"`
	HTMLURL    string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// check compares the running build to the newest eligible release. The report
// is always populated; the error is non-nil for every outcome that is not a
// completed comparison.
func (c updateChecker) check(ctx context.Context, id buildIdentity) (updateReport, error) {
	rep := updateReport{Current: id.Version}

	// Decided before any request: a build that cannot be compared has no use
	// for the answer, and asking would be contact for nothing.
	cur, ok := parseSemver(id.Version)
	if !ok {
		rep.Status = updateCannotCompare
		if id.Version == "" {
			rep.Error = "this build carries no release version (no -ldflags stamp, and the module reports none), so it cannot be compared with a release; it is not known to be up to date"
		} else {
			rep.Error = fmt.Sprintf("this build's version %q is not a semantic version, so it cannot be compared with a release; it is not known to be up to date", id.Version)
		}
		return rep, fmt.Errorf("%w: %s", errUpdateCannotCompare, rep.Error)
	}

	fail := func(format string, a ...any) (updateReport, error) {
		rep.Status = updateFailed
		rep.Error = fmt.Sprintf(format, a...)
		return rep, fmt.Errorf("%w: %s", errUpdateCheckFailed, rep.Error)
	}

	target, err := c.releasesURL()
	if err != nil {
		return fail("%v", err)
	}
	// Everything printed or serialised names the endpoint through Redacted, so
	// credentials an operator put in the override URL never reach a CI log. The
	// request itself still carries the operator's own URL.
	endpoint := target.Redacted()
	rep.Source = endpoint

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return fail("building the request: %v", err)
	}
	// The standard request and nothing else: no credentials, no identifiers.
	req.Header.Set("User-Agent", "manvi/"+id.Version)
	req.Header.Set("Accept", "application/vnd.github+json")

	// The redirect policy is set here rather than trusted to whoever built the
	// client, so no caller can end up with the permissive default.
	client := *c.client
	client.CheckRedirect = sameSiteRedirects
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return fail("no answer from %s within %s", endpoint, c.timeout)
		}
		return fail("could not reach %s: %v", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		hint := ""
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			hint = " (Retry-After: " + sanitizeLine(ra) + ")"
		}
		return fail("%s answered %d: rate limited or refused%s; try again later", endpoint, resp.StatusCode, hint)
	case resp.StatusCode != http.StatusOK:
		return fail("%s answered %d", endpoint, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, updateMaxBody+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return fail("the response from %s did not finish within %s", endpoint, c.timeout)
		}
		return fail("reading the response: %v", err)
	}
	if len(body) > updateMaxBody {
		return fail("the response from %s is larger than %d bytes; refusing to read it", endpoint, updateMaxBody)
	}

	var releases []releaseEntry
	if err := json.Unmarshal(body, &releases); err != nil {
		return fail("the response from %s is not a release list: %v", endpoint, err)
	}

	tag, link, latest, skipped, err := c.newest(releases)
	rep.SkippedTags = skipped
	if err != nil {
		return fail("%v", err)
	}
	rep.Latest = tag
	rep.URL = link

	if semverCompare(latest, cur) > 0 {
		rep.Status = updateAvailable
	} else {
		rep.Status = updateUpToDate
	}
	return rep, nil
}

// releasesURL joins the configured root with this project's release list,
// refusing anything that is not an http(s) URL with a host.
func (c updateChecker) releasesURL() (*url.URL, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		// The text is not echoed: a malformed value may still hold credentials.
		return nil, fmt.Errorf("%s is not a valid URL", updateAPIEnv)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%s is not an http(s) URL: %q", updateAPIEnv, sanitizeLine(u.Redacted()))
	}
	u = u.JoinPath("repos", releaseRepo, "releases")
	q := u.Query()
	q.Set("per_page", strconv.Itoa(updatePageSize))
	u.RawQuery = q.Encode()
	return u, nil
}

// sameSiteRedirects follows a redirect only within the host the request was
// sent to, and never from https to http, so a response cannot move the request
// (and its User-Agent) to a host the operator never named. The hop limit is
// the standard library's own default of 10.
func sameSiteRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	first := via[0].URL
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing a redirect to the %q scheme", sanitizeLine(req.URL.Scheme))
	}
	if req.URL.Host != first.Host {
		return fmt.Errorf("refusing a redirect to another host (%s)", sanitizeLine(req.URL.Hostname()))
	}
	if first.Scheme == "https" && req.URL.Scheme != "https" {
		return errors.New("refusing a redirect from https to http")
	}
	return nil
}

// linkHosts are the hosts a release page may be on: GitHub itself, and for an
// override the site that goes with the configured API host (api.example ->
// example).
func (c updateChecker) linkHosts() []string {
	hosts := []string{"github.com"}
	if u, err := url.Parse(c.baseURL); err == nil && u.Hostname() != "" {
		hosts = append(hosts, strings.TrimPrefix(strings.ToLower(u.Hostname()), "api."))
	}
	return hosts
}

// newest picks the highest-versioned release the user is entitled to hear
// about. Drafts are never offered; prereleases only on request. A tag that is
// not a semantic version is skipped and counted: what must be readable is the
// feed, and one stray tag would otherwise disable the check for everyone until
// it was deleted. An entry with an empty tag is still an error, because that is
// a malformed feed rather than a foreign tag.
func (c updateChecker) newest(releases []releaseEntry) (tag, link string, ver semver, skipped int, err error) {
	var found bool
	for _, r := range releases {
		if r.Draft {
			continue
		}
		// Trimmed once, here: parseSemver accepts surrounding whitespace, and
		// the tag is printed, so only the trimmed (and therefore fully
		// validated, control-character-free) form leaves this function.
		name := strings.TrimSpace(r.TagName)
		if name == "" {
			return "", "", semver{}, skipped, errors.New("the release list contains an entry with an empty tag")
		}
		v, ok := parseSemver(name)
		if !ok {
			skipped++
			continue
		}
		if !c.prerelease && (r.Prerelease || len(v.pre) > 0) {
			continue
		}
		if !found || semverCompare(v, ver) > 0 {
			r.TagName = name
			tag, ver, found = name, v, true
			link = c.releasePage(r)
		}
	}
	if !found {
		return "", "", semver{}, skipped, errors.New("the release list holds no eligible semantic-version release")
	}
	return tag, link, ver, skipped, nil
}

// releasePage is the page the user is sent to. The API's own link is used only
// when it is https, on a host from linkHosts, with no userinfo and no explicit
// port, and carries no control or format characters; and what is printed is the
// parsed URL, not the feed's string. Anything else is replaced by the tag URL
// on GitHub, because the value goes to a terminal and comes from a server the
// operator may have pointed anywhere.
func (c updateChecker) releasePage(r releaseEntry) string {
	fallback := "https://github.com/" + releaseRepo + "/releases/tag/" + url.PathEscape(r.TagName)
	u, err := url.Parse(r.HTMLURL)
	if err != nil || sanitizeLine(r.HTMLURL) != r.HTMLURL {
		return fallback
	}
	if u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Opaque != "" {
		return fallback
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range c.linkHosts() {
		if host == h {
			return u.String()
		}
	}
	return fallback
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// sanitizeLine drops control characters (Cc), format characters (Cf: bidi
// overrides and isolates, zero-width marks) and line and paragraph separators
// (Zl, Zp), so a value from the network or the environment can neither rewrite
// the terminal nor reorder what is printed around it.
func sanitizeLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, s)
}

// semver is a parsed semantic version (semver.org 2.0.0). Build metadata is
// dropped: it never takes part in precedence.
type semver struct {
	major, minor, patch uint64
	pre                 []string
}

// parseSemver accepts an optional leading "v", which is how releases are
// tagged and how the module reports versions.
func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		if !validIdents(s[i+1:], false) {
			return semver{}, false
		}
		s = s[:i]
	}
	var pre string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s, pre = s[:i], s[i+1:]
		if !validIdents(pre, true) {
			return semver{}, false
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var nums [3]uint64
	for i, p := range parts {
		n, ok := parseNumeric(p)
		if !ok {
			return semver{}, false
		}
		nums[i] = n
	}
	v := semver{major: nums[0], minor: nums[1], patch: nums[2]}
	if pre != "" {
		v.pre = strings.Split(pre, ".")
	}
	return v, true
}

// parseNumeric parses a digit string with no leading zero (except "0").
func parseNumeric(p string) (uint64, bool) {
	if p == "" || (len(p) > 1 && p[0] == '0') {
		return 0, false
	}
	for _, r := range p {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(p, 10, 64)
	return n, err == nil
}

// validIdents checks a dot-separated identifier list; numeric prerelease
// identifiers may not carry leading zeros.
func validIdents(s string, numericNoLeadingZero bool) bool {
	if s == "" {
		return false
	}
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return false
		}
		numeric := true
		for _, r := range id {
			switch {
			case r >= '0' && r <= '9':
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-':
				numeric = false
			default:
				return false
			}
		}
		if numeric && numericNoLeadingZero && len(id) > 1 && id[0] == '0' {
			return false
		}
	}
	return true
}

// semverCompare returns -1, 0 or 1 by semver precedence.
func semverCompare(a, b semver) int {
	for _, p := range [][2]uint64{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if p[0] != p[1] {
			if p[0] < p[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		if c := compareIdent(a.pre[i], b.pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) < len(b.pre):
		return -1
	case len(a.pre) > len(b.pre):
		return 1
	}
	return 0
}

func compareIdent(a, b string) int {
	an, aok := parseNumeric(a)
	bn, bok := parseNumeric(b)
	switch {
	case aok && bok:
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		}
		return 0
	case aok:
		return -1
	case bok:
		return 1
	}
	return strings.Compare(a, b)
}

// versionArgs is the parsed tail of `manvi version`.
type versionArgs struct {
	json, check, prerelease bool
}

const versionUsage = "usage: version [--json] [--check [--prerelease]]"

// parseVersionArgs accepts the flags in any order and refuses duplicates and
// unknowns. --prerelease means nothing without --check, and is refused
// rather than silently ignored.
func parseVersionArgs(args []string) (versionArgs, error) {
	var v versionArgs
	for _, a := range args {
		switch a {
		case "--json":
			if v.json {
				return versionArgs{}, errors.New(versionUsage)
			}
			v.json = true
		case "--check":
			if v.check {
				return versionArgs{}, errors.New(versionUsage)
			}
			v.check = true
		case "--prerelease":
			if v.prerelease {
				return versionArgs{}, errors.New(versionUsage)
			}
			v.prerelease = true
		default:
			return versionArgs{}, errors.New(versionUsage)
		}
	}
	if v.prerelease && !v.check {
		return versionArgs{}, errors.New(versionUsage)
	}
	return v, nil
}

// runVersion is the whole `version` command after the arguments are parsed.
// Without --check it makes no request and constructs no checker.
func runVersion(out io.Writer, v versionArgs) error {
	if !v.check {
		if v.json {
			return writeVersionJSON(out, nil)
		}
		writeVersion(out)
		return nil
	}
	rep, checkErr := checkerForVersion(v.prerelease).check(context.Background(), currentBuild())
	if v.json {
		if err := writeVersionJSON(out, &rep); err != nil {
			return err
		}
		return checkErr
	}
	writeVersion(out)
	writeUpdateReport(out, rep)
	return checkErr
}

// writeUpdateReport prints the outcome. Failures are described on stderr by
// the exit-status mapping; the line here keeps the report self-contained when
// stdout is read alone.
func writeUpdateReport(out io.Writer, rep updateReport) {
	_, _ = fmt.Fprintln(out, "update check")
	switch rep.Status {
	case updateAvailable:
		_, _ = fmt.Fprintf(out, "  status     update available: %s -> %s\n", rep.Current, rep.Latest)
		_, _ = fmt.Fprintf(out, "  release    %s\n", rep.URL)
		_, _ = fmt.Fprintln(out, "  nothing was downloaded or installed")
	case updateUpToDate:
		_, _ = fmt.Fprintf(out, "  status     up to date: %s is the newest release (latest %s)\n", rep.Current, rep.Latest)
	case updateCannotCompare:
		_, _ = fmt.Fprintf(out, "  status     cannot compare: %s\n", rep.Error)
	default:
		_, _ = fmt.Fprintf(out, "  status     check failed: %s\n", rep.Error)
	}
	if rep.SkippedTags > 0 {
		_, _ = fmt.Fprintf(out, "  skipped    %d release tag(s) that are not semantic versions\n", rep.SkippedTags)
	}
}
