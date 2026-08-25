// Package devmap is the harness's repo-navigation boundary.
//
// It does not implement code intelligence. DevCouncil already has a mature Rust
// port of that — extraction, resolution, community detection, dead-code
// analysis — and reimplementing it here would be a second engine to keep in
// step with the first. This package execs that binary and reads its JSON,
// exactly as the store and verifier boundaries do, and for the same reason: a
// process keeps CGO_ENABLED=0 and the single static binary intact.
//
// What this package does own is the part a navigation service must not get
// wrong, which is knowing when its answers are worthless:
//
//   - An index that has never been built answers every query with an empty
//     result, and an empty result reads exactly like "there is nothing there".
//     Availability is therefore a positive assertion — a generation exists, it
//     committed, and it has nodes — not the absence of an error.
//
//   - An index built before the last edit answers confidently about code that
//     no longer exists. Staleness is carried on every answer rather than
//     checked once, because the interesting case is the query that runs after
//     the agent has been writing for ten minutes.
package devmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"manvi/internal/proc"
)

// Client runs the devmap binary against a repository.
type Client struct {
	// Binary is the path to `devmap`.
	Binary string
	// Root is the repository it indexes. devmap resolves its database relative
	// to the working directory, so this is set as the command's directory
	// rather than passed as a flag.
	Root string
	// Timeout bounds one invocation. A query is local and indexed; a build is
	// not, so Build uses its own longer bound.
	Timeout time.Duration
	// Budget caps how much a query may return, in the binary's own units.
	Budget int
	// maxOutput and maxStderr bound what one invocation may hand back, in
	// bytes. They are fields rather than the constants they replaced so a test
	// can drive the bound without generating megabytes to reach it; nothing
	// outside this package sets them.
	maxOutput int
	maxStderr int

	// probeMu guards the capability verdict below.
	probeMu sync.Mutex
	// probeErr is the verdict of the last capability probe: nil when the
	// binary answered with everything this harness requires, non-nil naming
	// why it did not. A healthy verdict is cached for the life of the client;
	// a failed one is retried after probeCooldown, because the remedy (a
	// rebuilt or replaced binary) lands underneath this path string without
	// this process knowing.
	probeErr error
	probedAt time.Time
	// probeTimeout bounds one probe. It is a field for the same reason
	// maxOutput is: tests drive it instead of waiting out seconds.
	probeTimeout time.Duration
}

// The probe's bounds and the failure cooldown. Every devmap build reports the
// same version string (`devmap 0.1.0`), so no version comparison can tell an
// outdated install from a current one — capability is the only evidence, and
// these bound how long collecting it may take and how often a failing binary
// is asked again.
const (
	defaultProbeTimeout = 10 * time.Second
	probeCooldown       = 60 * time.Second
	// maxProbeOutput bounds what one probe may capture. Help text is a few
	// hundred bytes; anything approaching this is a binary going wrong while
	// being assessed, and the hard cap stops it the way decode's does.
	maxProbeOutput = 64 << 10
)

// New builds a client with defaults.
func New(binary, root string) *Client {
	return &Client{
		Binary: binary, Root: root, Timeout: 30 * time.Second, Budget: 2000,
		maxOutput: defaultMaxOutput, maxStderr: defaultMaxStderr,
		probeTimeout: defaultProbeTimeout,
	}
}

// Probe asks the binary whether it supports what this harness requires, and
// remembers the verdict.
//
// Why this exists: the harness resolves `devmap` from PATH or a build
// directory and then hands it model-written queries, repository builds, and —
// through Manifest — the artifact the write gate reads. Manifest hard-depends
// on `--graph-output`. An outdated install reports the same version string as
// a current one (`devmap 0.1.0` for both), so nothing but an actual capability
// question distinguishes them, and without one the failure surfaced halfway
// through `manvi map refresh`: after a possibly minutes-long build, as an
// inscrutable clap parse error about a flag the operator never typed.
//
// The probe is one cheap subcommand (`manifest --help`) with three outcomes:
//
//   - support confirmed → cached healthy for the client's life;
//   - answered without the capability → refused, naming the remedy;
//   - no answer within probeTimeout → presumed hung, refused the same way.
//
// A refusal is retried only after probeCooldown. That is the fail-fast port:
// in DevCouncil's client the equivalent condition cost 123 seconds per call,
// once per call, forever — here it costs one bounded probe, then immediate
// errors, then one retry a minute later to notice a repaired install.
func (c *Client) Probe(ctx context.Context) error {
	c.probeMu.Lock()
	defer c.probeMu.Unlock()

	now := time.Now()
	if !c.probedAt.IsZero() {
		if c.probeErr == nil {
			return nil
		}
		if now.Sub(c.probedAt) < probeCooldown {
			return c.probeErr
		}
	}

	c.probeErr = c.runProbe(ctx)
	c.probedAt = now
	return c.probeErr
}

// runProbe executes one capability question against the binary.
//
// It carries the same two guards as decode, because the probe faces the same
// producers: WaitDelay so a forked grandchild holding the stdout pipe cannot
// hold the probe open past its deadline (the shell dying does not close a pipe
// its sleeping child inherited), and an output bound so a binary that floods
// stdout cannot take this process's memory while being assessed.
func (c *Client) runProbe(ctx context.Context) error {
	if c.Binary == "" {
		return errors.New("no devmap binary configured")
	}
	timeout := c.probeTimeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := &capped{limit: maxProbeOutput, hard: true}
	cmd := exec.CommandContext(probeCtx, c.Binary, "--json", "manifest", "--help")
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = time.Second
	// Bounded the same way decode is, and for the same reason: the probe's
	// deadline has to cover the fork itself, not just the process it produces.
	// This site kept `cmd.Run()` after decode was fixed, which left the *first*
	// call the harness makes to a new binary — the one most likely to meet a
	// loaded machine — outside every bound the function advertises.
	runErr, timedOut := proc.RunBounded(probeCtx, cmd.Run)
	if timedOut {
		return fmt.Errorf(
			"devmap at %s did not answer `manifest --help` within %s and is presumed hung; "+
				"every command would stall the same way",
			c.Binary, timeout)
	}
	if runErr != nil {
		// Read only on the path where the goroutine has returned: a buffer an
		// abandoned writer still owns must not be read at all.
		detail := strings.TrimSpace(string(out.Bytes()))
		if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf(
				"devmap at %s did not answer `manifest --help` within %s and is presumed hung; "+
					"every command would stall the same way",
				c.Binary, timeout)
		}
		return fmt.Errorf(
			"devmap at %s could not answer its own --help (%v%s); it cannot be trusted to "+
				"build or query this repository",
			c.Binary, runErr, formatDetail(detail))
	}
	if !strings.Contains(string(out.Bytes()), "--graph-output") {
		return fmt.Errorf(
			"devmap at %s is too old: no --graph-output, so manifest cannot write the code graph "+
				"the write gate reads; rebuild it with `cargo build --release -p devmap-cli` in "+
				"DevCouncil's rust-port/, or point MANVI_MAP_BINARY at a current build",
			c.Binary)
	}
	return nil
}

// formatDetail renders a probe's captured output as one bounded clause.
func formatDetail(detail string) string {
	if detail == "" {
		return ""
	}
	const maxDetail = 200
	if len(detail) > maxDetail {
		detail = detail[:maxDetail] + "…"
	}
	return ": " + strings.ReplaceAll(detail, "\n", " ")
}

// Status is what the index says about itself.
type Status struct {
	DBPath         string  `json:"db_path"`
	GenerationID   int     `json:"generation_id"`
	NodeCount      int     `json:"node_count"`
	EdgeCount      int     `json:"edge_count"`
	PendingCount   int     `json:"pending_count"`
	Quarantined    int     `json:"quarantined_count"`
	IsFresh        bool    `json:"is_fresh"`
	DegradedReason *string `json:"degraded_reason"`
}

// Symbol is one search hit.
type Symbol struct {
	FilePath   string  `json:"file_path"`
	Kind       string  `json:"kind"`
	Score      float64 `json:"score"`
	SourceSpan string  `json:"source_span"`
	Name       string  `json:"symbol_name"`
}

// DeadSymbol is one dead-code candidate.
//
// Confidence and ExemptionReason are carried rather than collapsed into a
// boolean on purpose. A symbol reachable only through a build tag, a registry,
// or reflection looks callerless to any static analyser, and a tool that
// reports "dead" without the reason invites a deletion that breaks the build.
type DeadSymbol struct {
	FilePath        string  `json:"file_path"`
	Name            string  `json:"symbol_name"`
	Confidence      float64 `json:"confidence"`
	IsExempt        bool    `json:"is_exempt"`
	ExemptionReason *string `json:"exemption_reason"`
}

// Edge is one relation between two symbols, as `deps` reports it.
//
// This type exists because of a defect worth recording. The dependency queries
// were first decoded into the Symbol shape above, which shares no field names
// with what they actually return. Go's JSON decoder fills a struct with zero
// values for absent fields and reports no error, so every edge decoded to
// blanks and `graph_context` answered "no dependencies" for a file with 166 of
// them — a confident wrong answer produced by a decoder doing exactly what it
// is specified to do. The guard in assertShape is the general fix; this type is
// the specific one.
type Edge struct {
	SourceFile   string  `json:"source_file"`
	SourceSymbol string  `json:"source_symbol"`
	TargetFile   string  `json:"target_file"`
	TargetSymbol string  `json:"target_symbol"`
	Kind         string  `json:"edge_kind"`
	Confidence   float64 `json:"confidence"`
}

// identified reports whether the edge carries anything usable.
func (e Edge) identified() bool { return e.SourceFile != "" || e.TargetFile != "" }

type edgeResult struct {
	Items  []Edge `json:"items"`
	Hidden int    `json:"hidden"`
}

// searchResult is the wire shape of a query that returns symbols.
// identified reports whether the symbol carries anything usable.
func (s Symbol) identified() bool { return s.FilePath != "" || s.Name != "" }

// identified reports whether the dead-code entry carries anything usable.
func (d DeadSymbol) identified() bool { return d.FilePath != "" || d.Name != "" }

type searchResult struct {
	Items  []Symbol `json:"items"`
	Hidden int      `json:"hidden"`
}

type deadResult struct {
	Items  []DeadSymbol `json:"items"`
	Hidden int          `json:"hidden"`
}

// Result wraps any answer with what the caller needs to judge it.
type Result[T any] struct {
	Items []T
	// Hidden is how many results the budget suppressed. Reported rather than
	// dropped: a capped sample presented as complete coverage is the same
	// failure as a check that did not run.
	Hidden int
	// Stale marks an answer drawn from an index older than the working tree.
	Stale bool
	// Degraded names why an answer is less than it appears.
	Degraded []string
}

// Clean reports whether the answer is complete and current.
func (r Result[T]) Clean() bool { return !r.Stale && r.Hidden == 0 && len(r.Degraded) == 0 }

// Refusal is one file the indexer declined to read, and the reason it gave.
type Refusal struct {
	Path   string
	Reason string
}

// Notices is what a command wrote to stderr, classified.
//
// This type exists because of where devmap splits its answer. A command reports
// its counts as JSON on stdout and what it could not do as text on stderr, and
// this package read only the first whenever the process exited zero. Since the
// producer began naming the files discovery refuses — oversized, unreadable, or
// not UTF-8 — that silence became the failure this package was written to
// prevent: a repository whose largest source is absent from the graph answers
// "no callers" for everything that file calls, under a build line reading
// `indexed 173 files` with nothing anywhere saying 174 existed.
type Notices struct {
	// Refused is how many files discovery declined. It is the producer's own
	// total, not a count of the entries below, which it caps at twenty —
	// counting what was printed would under-report exactly when there is most
	// to report.
	Refused int
	// Refusals names them, as far as the producer named them.
	Refusals []Refusal
	// Unrecognised carries the stderr lines this build could not classify.
	//
	// This is the guard that matters, and it is the same argument as
	// assertShape. The producer is a binary built from another repository, so
	// its wording can change without anything here failing to compile; a parser
	// that quietly matched nothing would then report a clean build for one that
	// refused half the tree. Anything unrecognised is carried out verbatim
	// instead, because an unread message is worse than an ugly one.
	Unrecognised []string
	// UnrecognisedHidden counts the unreadable lines past noticeLimit. stderr
	// is unbounded input from another process and this report is printed, so
	// what it retains is capped — but a cap that cannot be seen is the failure
	// this type exists to prevent, so the remainder is counted rather than
	// dropped.
	UnrecognisedHidden int
	// StreamTruncated marks notices read from a stderr that was cut off at the
	// boundary's byte bound, so what is missing is unknown rather than counted.
	//
	// It is a field rather than a line appended to the stream because the first
	// attempt was a line: it went through the classifier as one more thing this
	// build did not understand, landed past the retention cap, and was counted
	// into UnrecognisedHidden — the notice about a cap, lost to a cap. A
	// property of the stream does not belong inside it.
	StreamTruncated bool
	// StreamLimit is the bound that cut it, for the message.
	StreamLimit int
}

// noticeLimit bounds how many unreadable lines one report keeps. devmap names
// at most twenty refusals and writes nothing else to stderr under --json, so
// this is slack rather than a constraint on the producer as it stands.
const noticeLimit = 64

// Clean reports whether the command left nothing out and said nothing this
// build could not read.
func (n Notices) Clean() bool {
	return n.Refused == 0 && len(n.Unrecognised) == 0 && n.UnrecognisedHidden == 0 &&
		!n.StreamTruncated
}

// Degraded describes, one line each, what the command left out.
func (n Notices) Degraded() []string {
	var out []string
	if n.Refused > 0 {
		line := fmt.Sprintf("%d file(s) were refused by the indexer and are absent from the graph, "+
			"so a query about them returns the same empty answer as code that does not exist", n.Refused)
		if named := n.named(); named != "" {
			line += ": " + named
		}
		out = append(out, line)
	}
	for _, said := range n.Unrecognised {
		out = append(out, fmt.Sprintf(
			"devmap said something this build does not understand, so it is repeated rather than dropped: %q", said))
	}
	if n.UnrecognisedHidden > 0 {
		out = append(out, fmt.Sprintf(
			"and %d further line(s) this build does not understand, past the %d it keeps",
			n.UnrecognisedHidden, noticeLimit))
	}
	if n.StreamTruncated {
		// Distinct from the line above and deliberately so. That one counts
		// what was read and not kept; this one is what was never read, and how
		// much of it there was is not knowable from here.
		out = append(out, fmt.Sprintf(
			"devmap wrote more than %d bytes to stderr and the stream was truncated at that "+
				"bound, so an unknown number of notices — including refusals — were never read",
			n.StreamLimit))
	}
	return out
}

// namedLimit bounds how many refusals one line names. The count above is always
// the whole total, so the cap shortens the message without shrinking the number.
const namedLimit = 10

func (n Notices) named() string {
	if len(n.Refusals) == 0 {
		return ""
	}
	shown := n.Refusals
	if len(shown) > namedLimit {
		shown = shown[:namedLimit]
	}
	parts := make([]string, 0, len(shown))
	for _, r := range shown {
		parts = append(parts, fmt.Sprintf("%s (%s)", r.Path, r.Reason))
	}
	joined := strings.Join(parts, ", ")
	if hidden := n.Refused - len(shown); hidden > 0 {
		joined += fmt.Sprintf(", and %d more", hidden)
	}
	return joined
}

// Merge folds a second command's notices into these, so a caller that runs two
// commands to produce one artifact reports one answer about it.
func (n *Notices) Merge(other Notices) {
	n.Refused += other.Refused
	n.Refusals = append(n.Refusals, other.Refusals...)
	n.UnrecognisedHidden += other.UnrecognisedHidden
	if other.StreamTruncated {
		n.StreamTruncated = true
		if other.StreamLimit > n.StreamLimit {
			n.StreamLimit = other.StreamLimit
		}
	}
	for _, said := range other.Unrecognised {
		n.note(said)
	}
}

// note retains an unreadable line, or counts it once the report is full.
func (n *Notices) note(line string) {
	if len(n.Unrecognised) >= noticeLimit {
		n.UnrecognisedHidden++
		return
	}
	n.Unrecognised = append(n.Unrecognised, line)
}

// BuildReport is what a build indexed, and what it left out.
type BuildReport struct {
	// Stats is the producer's own count of what it indexed, kept whole because
	// the payload has more than one shape: a build that committed a generation
	// reports files_indexed/symbols/edges, while one that found the tree
	// unchanged reports files/generation and no counts at all.
	Stats map[string]any
	// Disagreements names invariants this package checked against the index the
	// build had just written, and which did not hold. See checkAgainstIndex.
	Disagreements []string
	// Adopted carries the manifest's adoptions when a caller folds the two
	// commands into one report. A build adopts nothing itself — it writes no
	// artifact — but the caller that runs both hands the operator one account
	// of the operation, and an adoption dropped in that fold is an unreported
	// change to their files.
	Adopted []Adoption
	Notices
}

// Unchanged reports a build that committed nothing because the tree was
// byte-for-byte the one already indexed.
func (b *BuildReport) Unchanged() bool {
	unchanged, ok := b.Stats["unchanged"].(bool)
	return ok && unchanged
}

// Stat reads one numeric field of the payload, and says whether it was there.
//
// The bool is the point. JSON numbers arrive as float64 through `any`, and a
// missing key yields a nil interface that formats as `<nil>` — which is
// literally what an operator saw: `indexed <nil> files, <nil> symbols, <nil>
// edges`, printed for an unchanged build by a caller that assumed one payload
// shape. Absent and zero are different answers, and this returns them
// differently.
func (b *BuildReport) Stat(name string) (int, bool) {
	switch value := b.Stats[name].(type) {
	case float64:
		return int(value), true
	case int:
		return value, true
	default:
		return 0, false
	}
}

// Clean reports whether the build left nothing out and every check held.
func (b BuildReport) Clean() bool { return isClean(b.Notices, b.Disagreements, b.Adopted) }

// Degraded describes, one line each, what is less than it appears.
func (b BuildReport) Degraded() []string { return describe(b.Notices, b.Disagreements, b.Adopted) }

// ManifestReport is what a manifest run rendered, and what it left out.
//
// It is a separate answer from the build's because it is a separate command
// writing separate files: `devmap build` advances the index and writes no
// artifact at all, so a build that succeeded says nothing about whether the
// file the write gate reads was rewritten.
type ManifestReport struct {
	// GenerationID is the generation devmap says it rendered.
	//
	// It was being decoded into a map and dropped. It is the one exact way to
	// tell whether the artifact on disk came from the index the navigation
	// tools answer from: on this repository the two had drifted two
	// generations apart, and every consumer read them as one graph.
	GenerationID int
	// Disagreements names invariants this package checked against the index
	// the manifest had just been rendered from, and which did not hold.
	Disagreements []string
	// Adopted names the artifacts this run moved aside to take their paths.
	// See artifact.go: it is a change to files the operator did not ask for,
	// so it rides out on the report rather than only into a log.
	Adopted []Adoption
	Notices
}

// Clean reports whether the manifest left nothing out and every check held.
func (m ManifestReport) Clean() bool { return isClean(m.Notices, m.Disagreements, m.Adopted) }

// Degraded describes, one line each, what is less than it appears.
func (m ManifestReport) Degraded() []string { return describe(m.Notices, m.Disagreements, m.Adopted) }

// isClean and describe are the one implementation of the verdict both reports
// give. They are functions over the two parts rather than an embedded type
// because embedding an unexported struct would stop a caller in another package
// constructing either report in a composite literal, which the TUI's tests do.
//
// An adoption counts against Clean for the same reason a refusal does: the
// caller uses this verdict to choose between a report and a notice, and taking
// over another producer's file is not something an operator should have to go
// looking for.
func isClean(n Notices, disagreements []string, adopted []Adoption) bool {
	return n.Clean() && len(disagreements) == 0 && len(adopted) == 0
}

func describe(n Notices, disagreements []string, adopted []Adoption) []string {
	out := append(n.Degraded(), disagreements...)
	for _, a := range adopted {
		out = append(out, a.String())
	}
	return out
}

// refusalHeader matches the line devmap prints before naming what it refused.
var refusalHeader = regexp.MustCompile(`discovery refused (\d+) file\(s\)`)

// refusalMore matches the line that stands in for the refusals it did not name.
var refusalMore = regexp.MustCompile(`^(?:…|\.\.\.) and \d+ more$`)

// readNotices classifies what a command wrote to stderr.
//
// An entry is split at its first ": " rather than its last, because the reason
// is a Rust debug rendering that contains one of its own
// (`Unreadable { reason: "..." }`) while a path containing ": " would be
// pathological. A line that is neither a header, an entry beneath one, nor the
// producer's own summary of the entries it did not print is unrecognised, and
// unrecognised lines survive into the report rather than being dropped.
func readNotices(stream said) Notices {
	n := Notices{StreamTruncated: stream.truncated, StreamLimit: stream.limit}
	inRefusals := false
	for _, raw := range strings.Split(string(stream.text), "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if m := refusalHeader.FindStringSubmatch(trimmed); m != nil {
			count, err := strconv.Atoi(m[1])
			if err != nil {
				// A header whose count will not parse is a changed contract,
				// not an absence of refusals.
				n.note(trimmed)
				continue
			}
			n.Refused += count
			inRefusals = true
			continue
		}
		if inRefusals && line != trimmed {
			if refusalMore.MatchString(trimmed) {
				continue
			}
			if path, reason, ok := strings.Cut(trimmed, ": "); ok {
				n.Refusals = append(n.Refusals, Refusal{Path: path, Reason: reason})
				continue
			}
		}
		inRefusals = false
		n.note(trimmed)
	}
	return n
}

// defaultMaxOutput bounds a reply. A search over a large repository is bounded
// by the budget, so anything approaching this is a binary that has gone wrong.
const defaultMaxOutput = 64 << 20

// defaultMaxStderr bounds the notices stream. devmap names at most twenty
// refusals there, so this is slack rather than a constraint on the producer as
// it stands.
const defaultMaxStderr = 4 << 20

// capped is an io.Writer that retains at most limit bytes.
//
// It exists because the bound it replaces was checked after cmd.Run() returned
// — which is after os/exec has copied the whole of the child's stdout into
// memory. A guard that reports the flood once the harness has already taken it
// is not a guard, and stderr had none at all.
//
// Overflow behaviour differs by stream and the difference is deliberate. stdout
// carries the answer, so exceeding the bound errors: that stops io.Copy, which
// closes the pipe and ends the producer, and a truncated JSON document must
// never be parsed as a short one. stderr carries the notices, which are
// advisory, so it truncates and records that it did — failing a build over its
// commentary would be the tail wagging the dog, but a notices report built from
// a cut-off stream must not read like one built from the whole of it.
type capped struct {
	buf       bytes.Buffer
	limit     int
	hard      bool
	truncated bool
}

// errCapped marks the write that stopped a stream, so decode can name the bound
// rather than reporting whatever exec made of the broken pipe.
var errCapped = errors.New("output exceeded its bound")

func (c *capped) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room > len(p) {
		return c.buf.Write(p)
	}
	if room > 0 {
		c.buf.Write(p[:room])
	}
	c.truncated = true
	if c.hard {
		return 0, errCapped
	}
	// Soft cap: the bytes are accepted and dropped, so the producer runs to its
	// own end rather than dying on a broken pipe mid-answer.
	return len(p), nil
}

func (c *capped) Bytes() []byte { return c.buf.Bytes() }

// said is what a command wrote to stderr, together with whether that is all of
// it. The two travel as one value because a caller that can read the text but
// not whether it is complete will report a partial stream as a whole one.
type said struct {
	text      []byte
	truncated bool
	limit     int
}

// Available reports whether the index can answer questions.
//
// It requires a committed generation with nodes in it, not merely a binary that
// exits zero. An unbuilt index answers every query with an empty list, and a
// caller cannot distinguish that from "this symbol does not exist" — which is
// the difference between "no callers, safe to delete" and "no data".
func (c *Client) Available(ctx context.Context) (*Status, error) {
	// Capability before availability: an index that cannot be written or
	// queried correctly is not made useful by existing. The probe is cached,
	// so this costs one subcommand per client, not per call.
	if err := c.Probe(ctx); err != nil {
		return nil, fmt.Errorf("repo map unavailable: %w", err)
	}
	status, err := c.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("repo map unavailable: %w", err)
	}
	if status.GenerationID <= 0 {
		return status, errors.New("repo map unavailable: no generation has been committed; run `manvi map build`")
	}
	if status.NodeCount <= 0 {
		// `<= 0` rather than `== 0`. A negative count is a producer this build
		// cannot reason about, and the equality test let it through as
		// available — the one reading of a broken self-report that must not
		// happen, since every downstream answer is then trusted as current.
		return status, errors.New("repo map unavailable: the index holds no symbols, so every query would return an empty result that reads like an answer")
	}
	if status.DegradedReason != nil && *status.DegradedReason != "" {
		return status, fmt.Errorf("repo map degraded: %s", *status.DegradedReason)
	}
	return status, nil
}

// Status reads the index's self-report.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var status Status
	if _, err := c.decode(ctx, &status, c.Timeout, "status"); err != nil {
		return nil, err
	}
	return &status, nil
}

// Search finds symbols by name or path.
//
// The query is fenced behind `--`. It is a string the model wrote, devmap parses
// its command line with clap, and clap reads a leading dash as a flag: without
// the separator a search for a symbol named `-x` is a parse error rather than a
// search, and `search` and `deps` both carry a `--budget` this package reports
// the suppression against. An argument that reached the parser as a flag would
// make that accounting describe a bound the harness never set.
func (c *Client) Search(ctx context.Context, query string) (Result[Symbol], error) {
	return runQuery[Symbol, searchResult](ctx, c,
		func(r searchResult) ([]Symbol, int) { return r.Items, r.Hidden },
		"search", "--budget", strconv.Itoa(c.Budget), "--", query)
}

// Dead lists symbols with no discovered callers.
func (c *Client) Dead(ctx context.Context) (Result[DeadSymbol], error) {
	return runQuery[DeadSymbol, deadResult](ctx, c,
		func(r deadResult) ([]DeadSymbol, int) { return r.Items, r.Hidden },
		"dead", "--budget", strconv.Itoa(c.Budget))
}

// Deps returns every edge touching a file, in both directions.
//
// The command is named for dependencies but its answer is undirected: it
// returns edges where the file is the source and edges where it is the target,
// and on this repository every returned edge touches the queried file. Callers
// split them by comparing SourceFile and TargetFile to the path they asked
// about, which derives the direction from the data rather than from what the
// subcommand happens to be called.
func (c *Client) Deps(ctx context.Context, file string) (Result[Edge], error) {
	return runQuery[Edge, edgeResult](ctx, c,
		func(r edgeResult) ([]Edge, int) { return r.Items, r.Hidden },
		"deps", "--budget", strconv.Itoa(c.Budget), "--", file)
}

// Build indexes the repository. It is separate from the query path and carries
// its own timeout because it is the one operation whose cost scales with the
// repository rather than with the question.
//
// It returns what the build left out as well as what it indexed. See
// BuildReport for why those are one answer rather than two.
func (c *Client) Build(ctx context.Context, timeout time.Duration) (*BuildReport, error) {
	// Probed here, at the top of the one command whose cost scales with the
	// repository: an outdated binary used to surface only after a full build,
	// as a clap error about --graph-output. Failing in the first second names
	// the remedy before any work is spent.
	if err := c.Probe(ctx); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	var stats map[string]any
	stream, err := c.decode(ctx, &stats, timeout, "build", ".")
	if err != nil {
		return nil, err
	}
	report := &BuildReport{Stats: stats, Notices: readNotices(stream)}
	c.checkAgainstIndex(ctx, report)
	return report, nil
}

// checkAgainstIndex compares what the build says it analysed against what the
// index it just wrote actually holds.
//
// The two numbers come from different places — the build's own JSON and the
// store's self-report — and they have to describe the same graph, because the
// dead-code and subsystem answers every navigation tool gives are computed from
// the edge set the analyser saw, not from the edge set that was stored.
//
// They diverged. devmap resolved only the files a change could reach and handed
// its analyser 63 edges of 15,017: the stored graph was intact, every query
// about it was correct, and `dead` reported 433 candidates where a cold build
// found 14 — plainly-called symbols, listed as callerless, with nothing in the
// payload or the exit code to say so. That is fixed in the producer, but the
// binary is resolved from PATH and built from another repository, so this build
// cannot assume the devmap it just ran carries the fix. One comparison from
// outside catches it whatever the version.
//
// A failure to read the index back is recorded rather than raised: the build
// itself succeeded, and what is unknown is whether its analysis can be trusted.
func (c *Client) checkAgainstIndex(ctx context.Context, report *BuildReport) {
	analysed, ok := report.Stat("edges")
	if !ok {
		// An unchanged build reports no counts, having analysed nothing.
		return
	}
	status, err := c.Status(ctx)
	if err != nil {
		report.Disagreements = append(report.Disagreements, fmt.Sprintf(
			"the built index could not be read back, so whether its analysis covers the whole graph is unverified: %v", err))
		return
	}
	if status.EdgeCount != analysed {
		report.Disagreements = append(report.Disagreements, fmt.Sprintf(
			"the build analysed %d edge(s) but the index holds %d, so the dead-code and subsystem "+
				"results describe a different graph than the one stored; a symbol reported callerless "+
				"may be one whose callers the analysis never saw — run `manvi map rebuild` to discard "+
				"the index and recompute, and check that `devmap` on PATH is current",
			analysed, status.EdgeCount))
	}
}

// Manifest writes the repo map and code graph artifacts, returning what it
// rendered and anything the binary said while doing it.
//
// It verifies the files afterwards. The caller's next line is "wrote <path>",
// and that was being printed on the strength of an exit code: a manifest that
// exits zero having written nothing leaves the previous artifact in place for
// the gate to keep deciding from, under a message saying it had just been
// replaced. An artifact that is not on disk after the command whose whole
// purpose is to write it is a failure of that command, not a degradation of it.
func (c *Client) Manifest(ctx context.Context, mapPath, graphPath string) (*ManifestReport, error) {
	// Any artifact at these paths that devmap did not write is moved aside
	// first, because devmap refuses to overwrite one and stops. See artifact.go
	// for why that refusal is right and why failing on it was not. Every exit
	// below this line rolls the move back, so a manifest that does not land
	// leaves the state directory exactly as it was found.
	adopted, err := adopt(mapPath, graphPath)
	if err != nil {
		return nil, err
	}

	var out struct {
		GenerationID int `json:"generation_id"`
	}
	stream, err := c.decode(ctx, &out, 5*time.Minute,
		"manifest", ".", "--output", mapPath, "--graph-output", graphPath)
	if err != nil {
		return nil, abandon(adopted, err)
	}
	for _, artifact := range []string{mapPath, graphPath} {
		info, statErr := os.Stat(artifact)
		switch {
		case statErr != nil:
			return nil, abandon(adopted, fmt.Errorf(
				"devmap manifest exited without error but %s is not on disk, so the "+
					"artifact the scope rung reads is whatever was there before: %w", artifact, statErr))
		case info.Size() == 0:
			return nil, abandon(adopted, fmt.Errorf(
				"devmap manifest wrote %s as an empty file, which holds no nodes and would "+
					"load as an index that has not been built", artifact))
		}
	}
	report := &ManifestReport{
		GenerationID: out.GenerationID, Adopted: adopted, Notices: readNotices(stream),
	}
	c.checkManifestAgainstIndex(ctx, report)
	return report, nil
}

// checkManifestAgainstIndex compares the generation the manifest rendered with
// the one the index holds.
//
// Both come from the same store through the same binary moments apart, so they
// agree unless something is wrong — a concurrent build, or a producer whose
// manifest reads a snapshot the query path does not. The check exists because
// the failure it catches is silent by construction: the artifact is a file, it
// outlives the run that wrote it, and every later consumer takes it at face
// value. Recorded rather than raised, on the same rule the build path uses: the
// artifact was written, and what is in doubt is whether it is the current one.
func (c *Client) checkManifestAgainstIndex(ctx context.Context, report *ManifestReport) {
	if report.GenerationID == 0 {
		report.Disagreements = append(report.Disagreements,
			"devmap manifest reported no generation, so whether the artifact it wrote matches "+
				"the index the navigation tools answer from is unverified")
		return
	}
	status, err := c.Status(ctx)
	if err != nil {
		report.Disagreements = append(report.Disagreements, fmt.Sprintf(
			"the index could not be read back after writing the artifact, so whether the two "+
				"describe the same tree is unverified: %v", err))
		return
	}
	if status.GenerationID != report.GenerationID {
		report.Disagreements = append(report.Disagreements, fmt.Sprintf(
			"the artifact was rendered from generation %d and the index now holds %d, so the "+
				"scope rung and the navigation tools would answer about different trees",
			report.GenerationID, status.GenerationID))
	}
}

// runQuery executes a query and attaches the freshness verdict to its result.
//
// Freshness is read per query rather than cached. The case that matters is the
// query issued after ten minutes of editing, when a status checked at startup
// would still say fresh.
func runQuery[T any, W any](ctx context.Context, c *Client,
	extract func(W) ([]T, int), args ...string) (Result[T], error) {

	var wire W
	if _, err := c.decode(ctx, &wire, c.Timeout, args...); err != nil {
		return Result[T]{}, err
	}
	items, hidden := extract(wire)
	if err := assertShape(args[0], items); err != nil {
		return Result[T]{}, err
	}
	result := Result[T]{Items: items, Hidden: hidden}

	status, err := c.Status(ctx)
	if err != nil {
		// The answer arrived but its currency could not be established. That is
		// a degradation, not a failure: the results are still useful, and
		// pretending they are current is what must not happen.
		result.Degraded = append(result.Degraded,
			fmt.Sprintf("index freshness unknown: %v", err))
		return result, nil
	}
	if !status.IsFresh {
		result.Stale = true
		result.Degraded = append(result.Degraded,
			"the index is older than the working tree; these answers describe code as it was at the last build")
	}
	if status.PendingCount > 0 {
		result.Degraded = append(result.Degraded,
			fmt.Sprintf("%d file(s) are pending indexing", status.PendingCount))
	}
	if status.Quarantined > 0 {
		result.Degraded = append(result.Degraded,
			fmt.Sprintf("%d file(s) are quarantined and are not represented", status.Quarantined))
	}
	if hidden > 0 {
		result.Degraded = append(result.Degraded,
			fmt.Sprintf("%d result(s) were suppressed by the budget; this is a sample, not the whole answer", hidden))
	}
	return result, nil
}

// identifiable is implemented by every decoded item so the shape guard can ask
// whether the decode produced anything real.
type identifiable interface{ identified() bool }

// assertShape catches a decode that succeeded and produced nothing.
//
// This guards a whole class of boundary bug. Go's JSON decoder is specified to
// leave absent fields at their zero value and to return no error, so decoding a
// response into a struct whose field names do not match yields a full slice of
// blanks — and a caller reading "N items, all empty" reports "no results" with
// complete confidence. The producer here is a separate binary from a separate
// repository, so its field names can change without anything in this build
// failing to compile. If items came back and not one carries an identifier, the
// contract has moved.
func assertShape[T any](command string, items []T) error {
	if len(items) == 0 {
		return nil
	}
	for _, item := range items {
		if id, ok := any(item).(identifiable); !ok || id.identified() {
			return nil
		}
	}
	return fmt.Errorf(
		"devmap %s returned %d item(s) but none carried a field this build understands; "+
			"the response shape has changed and an empty answer would be indistinguishable from no results",
		command, len(items))
}

// decode runs the binary and unmarshals its stdout, returning what it wrote to
// stderr.
//
// stderr is handed back on success rather than discarded because this producer
// uses it for the half of its answer the JSON does not carry: the files it
// refused to index. Dropping it turned a partial index into one that read as
// complete. Callers that have nothing to say about stderr ignore it; the ones
// that do run it through readNotices.
func (c *Client) decode(ctx context.Context, into any, timeout time.Duration, args ...string) (said, error) {
	if c.Binary == "" {
		return said{}, errors.New("no devmap binary configured")
	}
	if c.Root == "" {
		return said{}, errors.New("no repository root configured")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := append([]string{"--json"}, args...)
	cmd := exec.CommandContext(ctx, c.Binary, full...)
	cmd.Dir = c.Root
	outLimit, errLimit := c.maxOutput, c.maxStderr
	if outLimit <= 0 {
		outLimit = defaultMaxOutput
	}
	if errLimit <= 0 {
		errLimit = defaultMaxStderr
	}
	stdout := &capped{limit: outLimit, hard: true}
	stderr := &capped{limit: errLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// The same bound as every other subprocess boundary here: killing the
	// process does not unblock Wait while a child holds the stdout pipe.
	cmd.WaitDelay = 2 * time.Second

	// Run on its own goroutine so the deadline covers the fork, not just the
	// process.
	//
	// exec.CommandContext arms its killer only once the child exists, and
	// WaitDelay bounds Wait — so both of them start counting after Start has
	// returned. A Start that does not return is outside every one of those
	// bounds, and this function's contract is that an invocation is bounded.
	// That gap is reachable: under the race detector, with two dozen forks in
	// flight and a loaded machine, Start wedged here for the full 900-second
	// package timeout with the 30-second context sitting unused.
	//
	// On the deadline path the goroutine is abandoned rather than waited for —
	// it is blocked in the kernel and there is nothing to cancel. It owns the
	// only references to stdout and stderr that anyone will read afterwards,
	// which is why nothing below this select touches them on that path: a
	// buffer still being written by an abandoned goroutine is a data race, and
	// a partial answer read out of one would be worse than the timeout.
	runErr, timedOut := proc.RunBounded(ctx, cmd.Run)
	if timedOut {
		return said{}, fmt.Errorf("devmap %s did not return within %s (the process could not be started or reaped): %w",
			args[0], timeout, ctx.Err())
	}
	// Checked before runErr, and this order is the fix. Breaking the pipe is
	// how the flood is stopped, so exec reports it as the command failing — and
	// reporting that as "devmap status failed: signal: broken pipe" would send
	// an operator to look at the wrong thing entirely.
	if stdout.truncated {
		return said{}, fmt.Errorf("devmap %s produced more than %d bytes and was stopped; "+
			"a partial answer is not a short one", args[0], outLimit)
	}
	if runErr != nil {
		detail := bytes.TrimSpace(stderr.Bytes())
		if len(detail) == 0 {
			detail = bytes.TrimSpace(stdout.Bytes())
		}
		return said{}, fmt.Errorf("devmap %s failed: %w (%s)", args[0], runErr, detail)
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), into); err != nil {
		return said{}, fmt.Errorf("devmap %s returned unparseable output: %w", args[0], err)
	}
	return said{text: stderr.Bytes(), truncated: stderr.truncated, limit: errLimit}, nil
}
