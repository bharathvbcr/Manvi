// Package dcgrep is the harness's repository-search boundary.
//
// It does not implement search. Ripgrep's engine already does that — ignore-rule
// resolution, a line-oriented searcher, a regex compiler tuned for it — and a
// second implementation here would be a second set of answers to keep in step
// with the first. This package execs the `dcgrep` binary that links it and
// reads its JSON, exactly as the store and verifier boundaries do, and for the
// same reason: a process keeps CGO_ENABLED=0 and the single static binary
// intact, and a pattern that makes the engine misbehave cannot take the agent
// loop with it.
//
// What this package owns is the part a search service must not get wrong, which
// is refusing to answer when it cannot:
//
//   - A missing binary is an error naming what to build, never an empty match
//     list. `{"count":0}` means "this search ran and matched nothing", and a
//     model handed that for a search which never happened will act on it.
//     That is not hypothetical here: it is the exact shape of the regression
//     that cost a run when an unparseable pattern returned no matches.
//
//   - A binary speaking a different schema is refused rather than decoded
//     through the wrong shape.
package dcgrep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"manvi/internal/proc"
)

// BinaryEnv overrides binary discovery, so an operator who names a path means
// it. It is declared here rather than at the call site so the message that
// names the remedy and the code that reads it cannot drift apart.
const BinaryEnv = "MANVI_GREP_BINARY"

// SchemaVersion must match dcgrep's. A mismatch is refused rather than decoded
// through the wrong shape.
const SchemaVersion = 1

// identity is how the binary names itself to a health probe. A binary that
// answers with anything else is some other program that happens to sit at the
// configured path.
const identity = "dc-grep"

const (
	// defaultTimeout bounds one search. A cold walk of a large repository with
	// ignore rules to resolve is seconds, not minutes; anything past this is a
	// wedged child rather than a slow one.
	defaultTimeout = 30 * time.Second

	// maxOutput bounds the reply. The searcher caps its own match count, so a
	// legitimate result is kilobytes; this is the bound on a child gone wrong,
	// applied during the copy rather than checked after it so a runaway cannot
	// allocate the whole thing before anyone looks.
	maxOutput = 16 << 20

	// maxStderr bounds the diagnostic half. It exists to make a failure
	// reportable, not to capture a log.
	maxStderr = 64 << 10

	// waitDelay matches the other process boundaries. Killing a child does not
	// unblock Wait while something still holds its stdout pipe.
	waitDelay = 2 * time.Second
)

// Client runs the dcgrep binary against a repository.
type Client struct {
	// Binary is the path to `dcgrep`.
	Binary string
	// Root is the repository searched. Every result path is relative to it.
	Root string
	// Timeout bounds one invocation. Zero means defaultTimeout.
	Timeout time.Duration

	// maxOutput and maxStderr bound what one invocation may hand back, in
	// bytes. They are fields rather than the constants they replaced so a test
	// can drive the bound without generating megabytes to reach it — the same
	// reason the devmap boundary carries them as fields. Nothing outside this
	// package sets them.
	maxOutput int
	maxStderr int
}

// New builds a client with defaults.
func New(binary, root string) *Client {
	return &Client{
		Binary: binary, Root: root, Timeout: defaultTimeout,
		maxOutput: maxOutput, maxStderr: maxStderr,
	}
}

// Request is one search.
type Request struct {
	// Pattern is the regular expression to find.
	Pattern string `json:"pattern"`
	// Root is the repository. Set by Search from the client.
	Root string `json:"root"`
	// Path scopes the search under Root. Empty means the whole repository.
	Path string `json:"path,omitempty"`
	// MaxResults bounds the match list. Zero means the searcher's default.
	MaxResults int `json:"max_results,omitempty"`
	// IncludeIgnored searches files the ignore rules would exclude.
	IncludeIgnored bool `json:"include_ignored,omitempty"`
	// CaseInsensitive matches without regard to case.
	CaseInsensitive bool `json:"case_insensitive,omitempty"`
}

// Match is one matching line.
type Match struct {
	Path       string `json:"path"`
	LineNumber int    `json:"line_number"`
	Line       string `json:"line"`
	// LineTruncated marks a line the searcher clipped. A clipped line must
	// never read as the whole line.
	LineTruncated bool `json:"line_truncated,omitempty"`
}

// Skipped counts what the walk could not look inside. Every field is a hole in
// the answer's coverage, reported so a caller never infers completeness from
// the absence of a complaint.
type Skipped struct {
	TooLarge   int `json:"too_large"`
	Binary     int `json:"binary"`
	Unreadable int `json:"unreadable"`
	// UnrepresentableName counts files whose names are not valid UTF-8, so a
	// match is never reported under a path that names no file. Reachable on
	// Linux, where filenames are bytes; not on macOS, which rejects them.
	UnrepresentableName int `json:"unrepresentable_name"`
}

// Total is how many files went unsearched for any reason.
//
// Every reason belongs in this sum: it decides whether the caller reports the
// coverage note at all, so a reason left out is a file silently dropped from
// the warning that exists to say files were dropped.
func (s Skipped) Total() int {
	return s.TooLarge + s.Binary + s.Unreadable + s.UnrepresentableName
}

// Result is what the searcher returned.
type Result struct {
	OK      bool    `json:"ok"`
	Error   string  `json:"error"`
	Pattern string  `json:"pattern"`
	Count   int     `json:"count"`
	Matches []Match `json:"matches"`
	// Truncated reports that the match limit stopped the walk early.
	Truncated bool `json:"truncated"`
	// Limit is the bound that did the stopping, present when Truncated is.
	Limit              int     `json:"limit"`
	FilesSearched      int     `json:"files_searched"`
	Skipped            Skipped `json:"skipped"`
	IgnoreRulesApplied bool    `json:"ignore_rules_applied"`
}

// ErrNoBinary is returned when no searcher is configured or none can be found.
// Callers turn it into a message naming the remedy rather than an empty result.
var ErrNoBinary = errors.New("no dcgrep binary configured")

// Search runs one search.
//
// Every error here names a fault, and none of them is reachable by a search
// that simply found nothing: that is a Result with Count zero.
func (c *Client) Search(ctx context.Context, req Request) (*Result, error) {
	if c == nil || c.Binary == "" {
		return nil, ErrNoBinary
	}
	req.Root = c.Root

	var out Result
	if err := c.run(ctx, "search", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// run is the one place this package execs the searcher.
//
// Both operations go through it so the bound, the caps, the group isolation
// and — most of all — the refusal to read a reply as an answer unless it says
// ok cannot be present at one call site and missing at the other.
func (c *Client) run(ctx context.Context, command string, req any, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("request could not be rendered: %w", err)
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// #nosec G204 -- c.Binary is the searcher this harness configured and
	// command is one of the two literals this file passes ("search", "files").
	// Neither reaches here from a caller, let alone from a model.
	cmd := exec.CommandContext(ctx, c.Binary, command)
	// See proc.ConfigureGroup. The searcher spawns nothing today, which is
	// exactly the argument that was made at the boundaries where a grandchild
	// later appeared and held the pipe open past the deadline.
	proc.ConfigureGroup(cmd)
	cmd.Stdin = bytes.NewReader(body)
	stdout := &cappedBuffer{limit: c.outputBound()}
	stderr := &cappedBuffer{limit: c.stderrBound()}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay

	runErr, timedOut := proc.RunBounded(ctx, cmd.Run)
	if timedOut {
		// RunBounded abandons the goroutine on a deadline, so the buffers may
		// still be written. Nothing below reads them on this path.
		return fmt.Errorf("searcher timed out after %s", timeout)
	}
	if stdout.overflow {
		// Refused rather than decoded from the prefix. A truncated reply can
		// still be valid JSON — the match array simply ends early — and
		// accepting it would report a capped sample as the whole result.
		return fmt.Errorf("searcher produced more than %d bytes", c.outputBound())
	}

	if err := json.Unmarshal(bytes.TrimSpace(stdout.buf.Bytes()), out); err != nil {
		if runErr != nil {
			return fmt.Errorf("searcher failed: %w (stderr: %s)", runErr, bytes.TrimSpace(stderr.buf.Bytes()))
		}
		return fmt.Errorf("searcher returned unparseable output: %w (%q)", err, stdout.buf.String())
	}
	// The OK flag is the only thing separating a real empty answer from a
	// reply that decoded into a zero value — `null` and `[]` both unmarshal
	// without error, leaving every field at zero.
	if ok, reason := okOf(out); !ok {
		if reason == "" {
			reason = "no reason given"
		}
		return errors.New(reason)
	}
	// And saying ok is not the same as being usable. Everything below is a
	// property the caller is about to rely on, checked here rather than assumed
	// because the thing that produced it is a separate program in a different
	// language that this build does not compile.
	return validate(out)
}

// validate refuses a reply that said ok but cannot be believed.
//
// Every check here was written against a reply the fuzzer produced and the
// client accepted:
//
//   - `{"OK":true,"Count":1}` — Go matches JSON field names case-insensitively,
//     so a reply can claim one match while sending none. A caller reading
//     `count` and a caller reading `matches` then disagree about the same
//     answer, and one of them acts on a match that does not exist.
//   - `{"ok":true,"count":1,"paths":["/etc/passwd"]}` and `["../escape"]` — an
//     absolute or climbing path presented as repository contents. The searcher
//     does not emit these; that is exactly why the client must not depend on it
//     not emitting them. Containment asserted at one end only is containment
//     that a change at the other end silently removes.
//   - `{"ok":true,"truncated":true,"limit":0}` — truncation with no limit, so
//     the caller cannot say how much it is missing.
//   - `{"ok":true,"skipped":{"too_large":-5}}` — a negative count, which makes
//     the coverage note arithmetic meaningless.
func validate(out any) error {
	switch reply := out.(type) {
	case *Result:
		if reply.Count != len(reply.Matches) {
			return fmt.Errorf("searcher reported %d matches and sent %d", reply.Count, len(reply.Matches))
		}
		for _, match := range reply.Matches {
			if err := validPath(match.Path); err != nil {
				return fmt.Errorf("match path: %w", err)
			}
			if match.LineNumber < 1 {
				return fmt.Errorf("match in %s carries line number %d, which names no line",
					match.Path, match.LineNumber)
			}
		}
		if reply.Truncated && reply.Limit < 1 {
			return errors.New("searcher reported truncation without the limit that caused it")
		}
		return validSkipped(reply.Skipped, reply.FilesSearched)
	case *ListResult:
		if reply.Count != len(reply.Paths) {
			return fmt.Errorf("searcher reported %d paths and sent %d", reply.Count, len(reply.Paths))
		}
		for _, path := range reply.Paths {
			if err := validPath(path); err != nil {
				return fmt.Errorf("listed path: %w", err)
			}
		}
		if reply.Truncated && reply.Limit < 1 {
			return errors.New("searcher reported truncation without the limit that caused it")
		}
		return validSkipped(reply.Skipped, 0)
	default:
		return errors.New("the client validated a reply shape it does not know")
	}
}

// validPath refuses anything that is not a repository-relative file path.
//
// The rule is the same one containedRelOf applies on the tool side, asserted
// again here because this is where bytes from another process become a path a
// caller will open.
func validPath(path string) error {
	if path == "" {
		return errors.New("is empty, so it names no file")
	}
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\`) {
		return fmt.Errorf("%q is absolute, and every path here is relative to the repository", path)
	}
	if vol := filepath.VolumeName(filepath.FromSlash(path)); vol != "" {
		return fmt.Errorf("%q names a volume, so it is not inside the repository", path)
	}
	for _, element := range strings.Split(path, "/") {
		if element == ".." {
			return fmt.Errorf("%q climbs out of the repository", path)
		}
	}
	if !utf8.ValidString(path) {
		return fmt.Errorf("%q is not valid UTF-8, so it names no file that can be reopened", path)
	}
	return nil
}

// validSkipped refuses counts that cannot be counts.
//
// They matter because the caller sums them to decide whether to warn that its
// answer is incomplete. A negative reason can cancel a positive one and turn a
// partial result into one that reports full coverage.
func validSkipped(skipped Skipped, filesSearched int) error {
	for name, count := range map[string]int{
		"too_large":            skipped.TooLarge,
		"binary":               skipped.Binary,
		"unreadable":           skipped.Unreadable,
		"unrepresentable_name": skipped.UnrepresentableName,
		"files_searched":       filesSearched,
	} {
		if count < 0 {
			return fmt.Errorf("searcher reported %s = %d, which is not a count", name, count)
		}
	}
	return nil
}

// okOf reads the ok flag and error text off whichever reply shape was decoded.
//
// A type switch rather than reflection or an interface on the wire structs:
// there are two shapes, both defined in this file, and a switch that stops
// compiling when a third is added is the behaviour wanted. Silently returning
// true for an unknown shape is the one thing this must not do.
func okOf(out any) (bool, string) {
	switch reply := out.(type) {
	case *Result:
		return reply.OK, reply.Error
	case *ListResult:
		return reply.OK, reply.Error
	default:
		return false, "the client decoded a reply shape it does not know how to check"
	}
}

// MaxListResults is the ceiling the searcher enforces on a listing, mirrored
// here so a caller that needs every candidate can ask for all of them by name
// rather than by guessing a large number.
//
// It must not exceed the searcher's own MAX_MAX_RESULTS; asking for more is
// clamped silently on that side, and a caller that believed the larger number
// would think it had the whole tree.
const MaxListResults = 5000

// ListRequest is one file listing.
type ListRequest struct {
	// Root is the repository. Set by List from the client.
	Root string `json:"root"`
	// Path scopes the listing under Root. Empty means the whole repository.
	Path string `json:"path,omitempty"`
	// MaxResults bounds the path list. Zero means the searcher's default.
	MaxResults int `json:"max_results,omitempty"`
	// IncludeIgnored lists files the ignore rules would exclude.
	IncludeIgnored bool `json:"include_ignored,omitempty"`
}

// ListResult is the set of files a search would open.
type ListResult struct {
	OK                 bool     `json:"ok"`
	Error              string   `json:"error"`
	Count              int      `json:"count"`
	Paths              []string `json:"paths"`
	Truncated          bool     `json:"truncated"`
	Limit              int      `json:"limit"`
	Skipped            Skipped  `json:"skipped"`
	IgnoreRulesApplied bool     `json:"ignore_rules_applied"`
}

// List enumerates the files a search would open.
//
// It exists so the harness has one answer to "what files are in this
// repository". While the two tools walked separately, `devcouncil_find_files`
// reported `dist/generated.go` and `devcouncil_grep` would never open it —
// two readers of one repository, disagreeing, with no way for the agent
// holding both answers to tell which was about the tree it was editing.
func (c *Client) List(ctx context.Context, req ListRequest) (*ListResult, error) {
	if c == nil || c.Binary == "" {
		return nil, ErrNoBinary
	}
	req.Root = c.Root
	var out ListResult
	if err := c.run(ctx, "files", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Available reports whether the searcher can be reached, by asking it to
// identify itself rather than by the absence of an error.
func (c *Client) Available(ctx context.Context) error {
	if c == nil || c.Binary == "" {
		return ErrNoBinary
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// #nosec G204 -- the configured binary and a literal subcommand.
	cmd := exec.CommandContext(ctx, c.Binary, "health")
	proc.ConfigureGroup(cmd)
	stdout := &cappedBuffer{limit: c.stderrBound()}
	cmd.Stdout = stdout
	cmd.WaitDelay = waitDelay
	if runErr, timedOut := proc.RunBounded(ctx, cmd.Run); timedOut {
		return errors.New("health probe timed out")
	} else if runErr != nil {
		return runErr
	}

	var health struct {
		OK            bool   `json:"ok"`
		Searcher      string `json:"searcher"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.buf.Bytes()), &health); err != nil {
		return fmt.Errorf("unparseable health reply: %w", err)
	}
	if !health.OK || health.Searcher != identity {
		return fmt.Errorf("%s identified itself as %q, not %q", c.Binary, health.Searcher, identity)
	}
	if health.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%s speaks schema %d, this harness speaks %d",
			c.Binary, health.SchemaVersion, SchemaVersion)
	}
	return nil
}

// outputBound and stderrBound resolve the per-invocation caps, so a zero-valued
// Client built by a caller that did not go through New is bounded rather than
// unbounded. A cap that defaults to "none" is the failure the cap exists for.
func (c *Client) outputBound() int {
	if c.maxOutput <= 0 {
		return maxOutput
	}
	return c.maxOutput
}

func (c *Client) stderrBound() int {
	if c.maxStderr <= 0 {
		return maxStderr
	}
	return c.maxStderr
}

// cappedBuffer forwards at most limit bytes and records that it stopped.
//
// Capped during the copy rather than checked after it: a child gone rogue on a
// large repository would otherwise allocate the whole reply before the bound
// was ever consulted. The write is reported as complete so io.Copy does not
// turn a cap into io.ErrShortWrite, close the pipe, and hand the child a
// SIGPIPE — which is how a search that ran fine comes back as a failure.
type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := c.limit - c.buf.Len()
	if remaining <= 0 {
		c.overflow = true
		return len(p), nil
	}
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		c.overflow = true
		return len(p), nil
	}
	return c.buf.Write(p)
}
