package serve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/dcgrep"
	mapclient "github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
)

// work.scan.gaps is a deterministic repository audit. It asks DevMap, the text
// searcher, git and the verification store questions and turns the answers
// into proposals for the GitPulse inbox. It calls no model and never writes the
// board: the host files a proposal when a person accepts it, with the
// fingerprint label below, which is what later scans dedupe on (open cards only),
// and the marker line below in its description, which is a trace for people:
// item cards omit descriptions, so a scan cannot read it back.
//
// The scan runs on its own context, off the lane that took the request, so a
// slow index cannot hold the lane's 60 s request deadline. The op answers as
// soon as the scan is claimed; work.scan.get reads the outcome.
const (
	scanDefaultDeadline = 90 * time.Second
	scanDefaultCap      = 25
	scanMaxCap          = 100
	scanMaxActive       = 2
	scanRetained        = 32
	scanKnownRoots      = 32

	scanMaxChangedFiles  = 40
	scanMaxDeadChecks    = 60
	scanMaxDocFiles      = 200
	scanMaxDocBytes      = 256 << 10
	scanMaxSymbolsListed = 20
	scanMaxBoardPages    = 20
	scanTextMaxResults   = 200
	scanDeadConfidence   = 0.8
	scanGitOutputLimit   = 4 << 20

	// scanMarker prefixes the fingerprint line a proposal's description carries,
	// for a person reading the card. Dedupe does not read it (see scanLabelPrefix).
	scanMarker = "manvi-scan:"
	// scanLabelPrefix prefixes the fingerprint label a proposal asks for; it is
	// what a scan dedupes on.
	scanLabelPrefix = "scan:"
)

// Scan categories, as named in coverage and on proposals.
const (
	scanDead     = "dead_symbols"
	scanUntested = "changed_untested"
	scanGaps     = "verification_gaps"
	scanDocs     = "doc_drift"
)

// Scan states.
const (
	scanRunning   = "running"
	scanDone      = "done"
	scanDeadline  = "deadline"
	scanCancelled = "cancelled"
	scanFailed    = "failed"
)

// ScanDevmap is the slice of DevCouncil's devmap process client a scan uses.
type ScanDevmap interface {
	Status(context.Context) (*mapclient.Status, error)
	Dead(context.Context) (mapclient.Result[mapclient.DeadSymbol], error)
	Advanced(context.Context, mapclient.AdvancedQuery) (mapclient.AdvancedResult, error)
}

// ScanText is DevCouncil's repository search client.
type ScanText interface {
	Search(context.Context, dcgrep.Request) (*dcgrep.Result, error)
}

// ScanGaps reads persisted verification gaps; an empty task ID means all.
type ScanGaps interface {
	Gaps(ctx context.Context, taskID string) ([]store.GapRow, bool, error)
}

// ScanBoard is the read side of the workbench store, used to dedupe.
type ScanBoard interface {
	Workbench(ctx context.Context, method string, input json.RawMessage) (json.RawMessage, error)
}

// ScanDeps supplies per-root clients. Each factory takes the validated,
// canonical repository root: a scan must never answer for the sidecar's cwd.
type ScanDeps struct {
	Devmap func(root string) ScanDevmap
	Text   func(root string) ScanText
	// Gaps may be nil, or return nil for a root with no verification store;
	// the category is then reported unavailable.
	Gaps  func(root string) ScanGaps
	Board ScanBoard
	// Busy, when set, is asked for the scanned root and refuses the scan with
	// its answer: a managed harness block may be in the working tree. Nil means
	// none is known (no managed lane). See ManagedRunner.HarnessBusy.
	Busy func(root string) *Error
	// Deadline bounds one scan; zero means 90 s.
	Deadline time.Duration
}

// ScanRunner owns scan jobs. The claim is in-process: two concurrent requests
// for one root share a job. It does not survive a restart (see docs).
type ScanRunner struct {
	deps ScanDeps

	mu     sync.Mutex
	closed bool
	jobs   map[string]*scanJob // by scan id
	active map[string]*scanJob // by root; running scans only
	// stopping counts scans Cancel has taken out of active whose goroutines have
	// not returned yet. They are no longer a root's scan (a new request starts a
	// fresh one) but still hold a place against scanMaxActive, because a goroutine
	// stuck in a call that ignores its context is still running.
	stopping int
	order    []string // finished scan ids, oldest first
	known    map[string]map[string]struct{}
	roots    []string // known-fingerprint roots, oldest first
	wg       sync.WaitGroup
}

type scanJob struct {
	cancel context.CancelFunc
	view   ScanView
	// stopping is set by Cancel when it moved the job from active to the
	// runner's stopping count.
	stopping bool
}

func NewScanRunner(deps ScanDeps) (*ScanRunner, error) {
	if deps.Devmap == nil || deps.Text == nil || deps.Board == nil {
		return nil, errors.New("scan runner requires devmap, text search and board clients")
	}
	if deps.Deadline <= 0 {
		deps.Deadline = scanDefaultDeadline
	}
	return &ScanRunner{deps: deps, jobs: map[string]*scanJob{}, active: map[string]*scanJob{}, known: map[string]map[string]struct{}{}}, nil
}

// GapScanParams is work.scan.gaps' request.
type GapScanParams struct {
	RepoRoot     string `json:"repo_root"`
	Since        string `json:"since,omitempty"`
	MaxProposals int    `json:"max_proposals,omitempty"`
}

// GapScanReadParams is work.scan.get's and work.scan.cancel's request.
type GapScanReadParams struct {
	ScanID string `json:"scan_id"`
}

// ScanCoverage states how far one category got. Anything other than "ok" means
// the category's absence of proposals is not evidence of absence of gaps.
type ScanCoverage struct {
	Category string `json:"category"`
	// Status is ok, truncated, walk_incomplete, degraded, unavailable or skipped.
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Hidden int    `json:"hidden,omitempty"`
}

// ScanSignal is one independent piece of evidence behind a proposal.
type ScanSignal struct {
	Source string `json:"source"`
	Result string `json:"result"`
}

// ScanProposal is a candidate inbox task.
type ScanProposal struct {
	Fingerprint string `json:"fingerprint"`
	Category    string `json:"category"`
	// Status is new, known (proposed by an earlier scan, not yet on the board)
	// or attached (an open board item already holds it).
	Status            string       `json:"status"`
	AttachedItemID    string       `json:"attached_item_id,omitempty"`
	AttachedItemTitle string       `json:"attached_item_title,omitempty"`
	Title             string       `json:"title"`
	Description       string       `json:"description"`
	Kind              string       `json:"kind"`
	Priority          int          `json:"priority"`
	Labels            []string     `json:"labels"`
	Signals           []ScanSignal `json:"signals,omitempty"`
}

// ScanView is a scan's observable state.
type ScanView struct {
	ScanID       string         `json:"scan_id"`
	State        string         `json:"state"`
	RepoRoot     string         `json:"repo_root"`
	Since        string         `json:"since,omitempty"`
	MaxProposals int            `json:"max_proposals"`
	Joined       bool           `json:"joined,omitempty"`
	StartedAt    int64          `json:"started_at"`
	FinishedAt   int64          `json:"finished_at,omitempty"`
	Error        string         `json:"error,omitempty"`
	Generation   int            `json:"index_generation,omitempty"`
	Coverage     []ScanCoverage `json:"coverage"`
	// Complete is true only when every category ran to completion within its
	// bounds and the board was read in full.
	Complete      bool           `json:"complete"`
	BoardComplete bool           `json:"board_complete"`
	NewCount      int            `json:"new_count"`
	AttachedCount int            `json:"attached_count"`
	OverCap       int            `json:"over_cap"`
	Proposals     []ScanProposal `json:"proposals"`
}

// clone copies the view's slices so a reader never shares them with the job.
// Both are required arrays in the contract, so a copy of an empty or unset one is
// an empty array, never nil (which marshals as null).
func (v ScanView) clone() ScanView {
	v.Coverage = append(make([]ScanCoverage, 0, len(v.Coverage)), v.Coverage...)
	v.Proposals = append(make([]ScanProposal, 0, len(v.Proposals)), v.Proposals...)
	return v
}

func scanStrict(raw json.RawMessage, into any) *Error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return badRequest("scan params: %v", err)
	}
	if dec.More() {
		return badRequest("scan params: trailing data")
	}
	return nil
}

var scanRevision = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@^~{}-]{0,199}$`)

// scanGit is runGit for the scan, where a listing cut at the bound is an
// error: a truncated diff or ls-files would drop files from a category while
// its coverage still read as complete.
func scanGit(ctx context.Context, root string, args ...string) ([]byte, error) {
	out, truncated, err := runGit(ctx, root, scanGitOutputLimit, args...)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, fmt.Errorf("git %s: output exceeded %d bytes; the listing is incomplete", args[0], scanGitOutputLimit)
	}
	return out, nil
}

func scanReady(st *mapclient.Status, root string) error {
	var why string
	switch {
	case st == nil:
		why = "devmap returned no status"
	case st.NodeCount == 0:
		why = "the index is empty (node_count 0); it has not been built"
	case !st.ReaderReady || !st.QueryReady:
		why = fmt.Sprintf("the index is not query-ready (schema_relation %q)", st.SchemaRelation)
	case st.DegradedReason != nil:
		why = "the index is degraded: " + *st.DegradedReason
	case !st.IsFresh:
		why = "the index is stale relative to the source tree"
	default:
		return nil
	}
	return fmt.Errorf("devmap index for %s is not usable: %s; run devmap status (devmap_status) for detail and `devmap build --manifest` to rebuild. A scan refuses rather than report no gaps from an index it cannot trust", root, why)
}

// Start validates and claims a scan, then answers without waiting for it.
func (r *ScanRunner) Start(ctx context.Context, raw json.RawMessage) (any, *Error) {
	var p GapScanParams
	if e := scanStrict(raw, &p); e != nil {
		return nil, e
	}
	if p.MaxProposals < 0 || p.MaxProposals > scanMaxCap {
		return nil, badRequest("max_proposals must be in 0..%d (0 means %d)", scanMaxCap, scanDefaultCap)
	}
	if p.MaxProposals == 0 {
		p.MaxProposals = scanDefaultCap
	}
	root, e := canonicalRepoRoot(ctx, "repo_root", p.RepoRoot)
	if e != nil {
		return nil, e
	}
	// A scan reads the working tree and diffs it; with a harness block in it the
	// proposals would be about text the agent did not write.
	if busy := r.deps.Busy; busy != nil {
		if e := busy(root); e != nil {
			return nil, e
		}
	}
	if p.Since != "" {
		if !scanRevision.MatchString(p.Since) {
			return nil, badRequest("since is not a plain revision name")
		}
		if _, err := scanGit(ctx, root, "rev-parse", "--verify", "--quiet", "--end-of-options", p.Since+"^{commit}"); err != nil {
			if ctx.Err() != nil {
				return nil, dependencyError("git", ctx.Err())
			}
			return nil, badRequest("since does not name a commit in this repository")
		}
	}
	dm := r.deps.Devmap(root)
	if nilInterface(dm) {
		return nil, dependencyError("devmap", errors.New("no client for this root"))
	}
	status, err := dm.Status(ctx)
	if err != nil {
		return nil, dependencyError("devmap status", err)
	}
	if err := scanReady(status, root); err != nil {
		return nil, &Error{Code: ErrDependency, Message: err.Error()}
	}
	if err := ctx.Err(); err != nil {
		return nil, dependencyError("scan", err)
	}

	id, err := generationID()
	if err != nil {
		return nil, &Error{Code: ErrInternal, Message: err.Error()}
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, &Error{Code: ErrBusy, Message: "the scan runner is shutting down", Retryable: true}
	}
	if job := r.active[root]; job != nil {
		defer r.mu.Unlock()
		if job.view.Since != p.Since || job.view.MaxProposals != p.MaxProposals {
			return nil, &Error{Code: ErrBusy, Message: "a scan of this root with different parameters is running; read it with work.scan.get or cancel it", Retryable: true}
		}
		v := job.view.clone()
		v.Joined = true
		return v, nil
	}
	if len(r.active)+r.stopping >= scanMaxActive {
		r.mu.Unlock()
		return nil, &Error{Code: ErrBusy, Message: "too many scans are running", Retryable: true}
	}
	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.deps.Deadline)
	job := &scanJob{cancel: cancel, view: ScanView{
		ScanID: id, State: scanRunning, RepoRoot: root, Since: p.Since, MaxProposals: p.MaxProposals,
		StartedAt: time.Now().Unix(), Generation: status.GenerationID, Coverage: []ScanCoverage{}, Proposals: []ScanProposal{},
	}}
	r.jobs[id] = job
	r.active[root] = job
	r.wg.Add(1)
	view := job.view.clone()
	r.mu.Unlock()
	go r.run(jobCtx, job, dm)
	return view, nil
}

// Get reads a scan.
func (r *ScanRunner) Get(raw json.RawMessage) (any, *Error) {
	var p GapScanReadParams
	if e := scanStrict(raw, &p); e != nil {
		return nil, e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	job := r.jobs[p.ScanID]
	if job == nil {
		return nil, badRequest("unknown scan_id (finished scans are retained only briefly)")
	}
	return job.view.clone(), nil
}

// Cancel stops a running scan; cancelling a finished one is a no-op read.
func (r *ScanRunner) Cancel(raw json.RawMessage) (any, *Error) {
	var p GapScanReadParams
	if e := scanStrict(raw, &p); e != nil {
		return nil, e
	}
	r.mu.Lock()
	job := r.jobs[p.ScanID]
	if job == nil {
		r.mu.Unlock()
		return nil, badRequest("unknown scan_id (finished scans are retained only briefly)")
	}
	if job.view.State == scanRunning {
		job.view.State = scanCancelled
		// A request for this root from now on is a new scan: joining this one
		// would hand its caller a cancelled view in place of a scan.
		if r.active[job.view.RepoRoot] == job {
			delete(r.active, job.view.RepoRoot)
			job.stopping = true
			r.stopping++
		}
	}
	view := job.view.clone()
	r.mu.Unlock()
	job.cancel()
	return view, nil
}

// Close cancels every scan and waits for them, bounded.
func (r *ScanRunner) Close() error {
	r.mu.Lock()
	r.closed = true
	for _, job := range r.active {
		job.cancel()
	}
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("scans did not stop before shutdown")
	}
}

type scanFinding struct {
	key      string
	category string
	title    string
	body     string
	kind     string
	priority int
	signals  []ScanSignal
}

func scanFingerprint(category, key string) string {
	sum := sha256.Sum256([]byte(category + "\x00" + key))
	return hex.EncodeToString(sum[:8])
}

type scanState struct {
	root     string
	since    string
	cov      []ScanCoverage
	findings []scanFinding
}

func (s *scanState) note(category, status, detail string, hidden int) {
	s.cov = append(s.cov, ScanCoverage{Category: category, Status: status, Detail: detail, Hidden: hidden})
}

func (r *ScanRunner) run(ctx context.Context, job *scanJob, dm ScanDevmap) {
	defer r.wg.Done()
	defer job.cancel()
	st := &scanState{root: job.view.RepoRoot, since: job.view.Since}
	steps := []struct {
		category string
		run      func()
	}{
		{scanGaps, func() { r.scanGaps(ctx, st) }},
		{scanUntested, func() { r.scanUntested(ctx, st, dm) }},
		{scanDead, func() { r.scanDead(ctx, st, dm) }},
		{scanDocs, func() { r.scanDocs(ctx, st) }},
	}
	for _, step := range steps {
		if ctx.Err() != nil {
			st.note(step.category, "skipped", "scan stopped before this category ran: "+ctx.Err().Error(), 0)
			continue
		}
		step.run()
	}
	proposals, boardComplete, boardErr := r.dedupe(ctx, st)

	r.mu.Lock()
	defer r.mu.Unlock()
	v := &job.view
	v.FinishedAt = time.Now().Unix()
	v.Coverage = st.cov
	v.BoardComplete = boardComplete
	switch {
	case v.State == scanCancelled:
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		v.State = scanDeadline
	case ctx.Err() != nil:
		v.State = scanCancelled
	case boardErr != nil:
		v.State = scanFailed
		v.Error = "the board could not be read, so no proposal can be deduped: " + boardErr.Error()
	default:
		v.State = scanDone
	}
	if v.State != scanFailed && v.State != scanCancelled {
		known := r.known[v.RepoRoot]
		next := map[string]struct{}{}
		kept := make([]ScanProposal, 0, len(proposals))
		open := 0
		for _, p := range proposals {
			if p.Status == "attached" {
				kept = append(kept, p)
				v.AttachedCount++
				continue
			}
			if _, ok := known[p.Fingerprint]; ok {
				p.Status = "known"
			}
			if open >= v.MaxProposals {
				v.OverCap++
				continue
			}
			open++
			if p.Status == "new" {
				v.NewCount++
			}
			next[p.Fingerprint] = struct{}{}
			kept = append(kept, p)
		}
		v.Proposals = kept
		// A scan that finished replaces the remembered set (resolved gaps
		// drop out); an interrupted one only adds to it.
		if v.State != scanDone {
			for fp := range known {
				next[fp] = struct{}{}
			}
		}
		r.remember(v.RepoRoot, next)
	}
	complete := v.State == scanDone && boardComplete && v.OverCap == 0
	for _, c := range v.Coverage {
		if c.Status != "ok" {
			complete = false
		}
	}
	v.Complete = complete
	if job.stopping {
		r.stopping--
	} else if r.active[v.RepoRoot] == job {
		delete(r.active, v.RepoRoot)
	}
	r.order = append(r.order, v.ScanID)
	for len(r.order) > scanRetained {
		delete(r.jobs, r.order[0])
		r.order = r.order[1:]
	}
}

func (r *ScanRunner) remember(root string, set map[string]struct{}) {
	if _, ok := r.known[root]; !ok {
		r.roots = append(r.roots, root)
		for len(r.roots) > scanKnownRoots {
			delete(r.known, r.roots[0])
			r.roots = r.roots[1:]
		}
	}
	r.known[root] = set
}

// --- dedupe ---

var scanFingerprintPattern = regexp.MustCompile(`(?:` + regexp.QuoteMeta(scanMarker) + `\s*|\b` + regexp.QuoteMeta(scanLabelPrefix) + `)([0-9a-f]{16})\b`)

// boardItem is the part of an items.list card a scan reads. Cards omit
// descriptions, so a proposal's fingerprint is read from its label alone.
type boardItem struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Status string   `json:"status"`
	Labels []string `json:"labels"`
}

// scanBoardDone is the status of a finished card, which is not open.
const scanBoardDone = "done"

// boardIndex maps fingerprints to the open item that carries them, reading
// every page up to a bound. Open means on the board (archived:false) and not
// Done: a finished or archived card must not hold a recurring gap silent
// forever. complete is false when the bound cut the read.
func (r *ScanRunner) boardIndex(ctx context.Context) (index map[string]boardItem, complete bool, err error) {
	index = map[string]boardItem{}
	cursor := ""
	for page := 0; page < scanMaxBoardPages; page++ {
		input := map[string]any{"limit": 200, "archived": false}
		if cursor != "" {
			input["cursor"] = cursor
		}
		in, err := json.Marshal(input)
		if err != nil {
			return nil, false, err
		}
		out, err := r.deps.Board.Workbench(ctx, "items.list", in)
		if err != nil {
			return nil, false, err
		}
		var res struct {
			Items      []boardItem `json:"items"`
			HasMore    bool        `json:"has_more"`
			NextCursor string      `json:"next_cursor"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			return nil, false, fmt.Errorf("decode items.list: %w", err)
		}
		for _, item := range res.Items {
			if item.Status == scanBoardDone {
				continue
			}
			seen := scanFingerprintPattern.FindAllStringSubmatch(strings.Join(item.Labels, "\n"), -1)
			for _, m := range seen {
				if _, ok := index[m[1]]; !ok {
					index[m[1]] = item
				}
			}
		}
		if !res.HasMore {
			return index, true, nil
		}
		if res.NextCursor == "" {
			return index, false, nil
		}
		cursor = res.NextCursor
	}
	return index, false, nil
}

// dedupe turns findings into proposals, marking those an open board item
// already holds. A board that cannot be read is an error: with no way to dedupe,
// proposing everything would file duplicates.
func (r *ScanRunner) dedupe(ctx context.Context, st *scanState) ([]ScanProposal, bool, error) {
	if len(st.findings) == 0 {
		return nil, true, nil
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, false, err
	}
	// A scan that ran out of time still dedupes what it found.
	boardCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	index, complete, err := r.boardIndex(boardCtx)
	if err != nil {
		return nil, false, err
	}
	out := make([]ScanProposal, 0, len(st.findings))
	for _, f := range st.findings {
		fp := scanFingerprint(f.category, f.key)
		p := ScanProposal{
			Fingerprint: fp, Category: f.category, Status: "new", Title: truncateRunes(f.title, 280),
			Description: f.body + "\n\n" + scanMarker + " " + fp, Kind: f.kind, Priority: f.priority,
			Labels: []string{"manvi-scan", scanLabelPrefix + fp, "scan-" + strings.ReplaceAll(f.category, "_", "-")}, Signals: f.signals,
		}
		if item, ok := index[fp]; ok {
			p.Status, p.AttachedItemID, p.AttachedItemTitle = "attached", item.ID, item.Title
		}
		out = append(out, p)
	}
	return out, complete, nil
}

// --- categories ---

func (r *ScanRunner) scanGaps(ctx context.Context, st *scanState) {
	var src ScanGaps
	if r.deps.Gaps != nil {
		src = r.deps.Gaps(st.root)
	}
	if nilInterface(src) {
		st.note(scanGaps, "unavailable", "no verification store is configured for this repository", 0)
		return
	}
	if c, ok := src.(io.Closer); ok {
		// Read-only use of the store: the rows are taken before the close, so
		// its error cannot change what this scan reports.
		defer func() { _ = c.Close() }()
	}
	rows, truncated, err := src.Gaps(ctx, "")
	if err != nil {
		st.note(scanGaps, "unavailable", "verification store read failed: "+err.Error(), 0)
		return
	}
	type group struct {
		task, kind string
		rows       []store.GapRow
	}
	groups := map[string]*group{}
	var keys []string
	for _, row := range rows {
		k := row.TaskID + "\x00" + row.GapType
		if groups[k] == nil {
			groups[k] = &group{task: row.TaskID, kind: row.GapType}
			keys = append(keys, k)
		}
		groups[k].rows = append(groups[k].rows, row)
	}
	sort.Strings(keys)
	for _, k := range keys {
		g := groups[k]
		blocking := 0
		var b strings.Builder
		for i, row := range g.rows {
			if row.Blocking {
				blocking++
			}
			if i < scanMaxSymbolsListed {
				fmt.Fprintf(&b, "- [%s] %s", row.Severity, row.Description)
				if row.File != nil {
					fmt.Fprintf(&b, " (%s)", *row.File)
				}
				if row.RecommendedFix != "" {
					fmt.Fprintf(&b, " Fix: %s", row.RecommendedFix)
				}
				b.WriteString("\n")
			}
		}
		priority := 2
		if blocking > 0 {
			priority = 1
		}
		st.findings = append(st.findings, scanFinding{
			key: g.task + "|" + g.kind, category: scanGaps, kind: "bug", priority: priority,
			title:   fmt.Sprintf("Resolve %d open %s verification gap(s) on task %s", len(g.rows), g.kind, g.task),
			body:    fmt.Sprintf("Open verification gaps in the dc-store (%d blocking):\n\n%s", blocking, b.String()),
			signals: []ScanSignal{{Source: "dc-store gaps", Result: fmt.Sprintf("%d open row(s) of type %s", len(g.rows), g.kind)}},
		})
	}
	if truncated {
		st.note(scanGaps, "truncated", "the store capped the gap list; gaps beyond it were not read", 0)
		return
	}
	st.note(scanGaps, "ok", fmt.Sprintf("%d open gap row(s)", len(rows)), 0)
}

var scanSourceExt = map[string]bool{".go": true, ".rs": true, ".py": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true}

func scanIsTestPath(p string) bool {
	base := filepath.Base(p)
	return strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") || strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") || strings.Contains("/"+p, "/tests/") || strings.Contains("/"+p, "/testdata/") || strings.Contains("/"+p, "/__tests__/")
}

type scanAffected struct {
	BlastRadius struct {
		Unmatched      []string `json:"unmatched_targets"`
		WalkIncomplete *string  `json:"walk_incomplete"`
	} `json:"blast_radius"`
	Tests struct {
		Items          []json.RawMessage `json:"items"`
		Total          int               `json:"total"`
		WalkIncomplete *string           `json:"walk_incomplete"`
	} `json:"tests"`
}

func (r *ScanRunner) scanUntested(ctx context.Context, st *scanState, dm ScanDevmap) {
	if st.since == "" {
		st.note(scanUntested, "skipped", "no since revision was given", 0)
		return
	}
	out, err := scanGit(ctx, st.root, "diff", "--name-only", "-z", "--no-ext-diff", "--no-textconv", "--diff-filter=ACMR", st.since, "--")
	if err != nil {
		st.note(scanUntested, "unavailable", "git diff failed: "+err.Error(), 0)
		return
	}
	var files []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" && scanSourceExt[filepath.Ext(name)] && !scanIsTestPath(name) {
			files = append(files, name)
		}
	}
	sort.Strings(files)
	hidden := 0
	if len(files) > scanMaxChangedFiles {
		hidden = len(files) - scanMaxChangedFiles
		files = files[:scanMaxChangedFiles]
	}
	var unindexed, incomplete []string
	untested := 0
	for _, file := range files {
		if ctx.Err() != nil {
			st.note(scanUntested, "skipped", "stopped before every changed file was checked: "+ctx.Err().Error(), 0)
			return
		}
		res, err := dm.Advanced(ctx, mapclient.AdvancedQuery{Kind: mapclient.QueryAffected, Targets: []string{file}, Depth: 3})
		if err != nil {
			st.note(scanUntested, "unavailable", "devmap affected failed for "+file+": "+err.Error(), 0)
			return
		}
		var data scanAffected
		if err := json.Unmarshal(res.Data, &data); err != nil {
			st.note(scanUntested, "unavailable", "devmap affected answer for "+file+" did not decode", 0)
			return
		}
		switch {
		case len(data.BlastRadius.Unmatched) > 0:
			unindexed = append(unindexed, file)
		case data.BlastRadius.WalkIncomplete != nil || data.Tests.WalkIncomplete != nil:
			incomplete = append(incomplete, file)
		case data.Tests.Total == 0 && len(data.Tests.Items) == 0:
			untested++
			st.findings = append(st.findings, scanFinding{
				key: file, category: scanUntested, kind: "chore", priority: 2,
				title: "Add tests that reach the changes to " + file,
				body:  fmt.Sprintf("%s changed since %s and no test file is reachable from it in the DevMap graph (affected, depth 3).", file, st.since),
				signals: []ScanSignal{
					{Source: "git diff " + st.since, Result: "file changed"},
					{Source: "devmap affected", Result: "0 tests reach the file"},
				},
			})
		}
	}
	switch {
	case len(incomplete) > 0:
		st.note(scanUntested, "walk_incomplete", fmt.Sprintf("devmap's walk was capped for %d file(s) (%s); they were not judged", len(incomplete), strings.Join(incomplete, ", ")), 0)
	case len(unindexed) > 0:
		st.note(scanUntested, "degraded", fmt.Sprintf("%d changed file(s) are not in the index and were not judged (%s)", len(unindexed), strings.Join(unindexed, ", ")), 0)
	case hidden > 0:
		st.note(scanUntested, "truncated", fmt.Sprintf("checked %d of %d changed source files", len(files), len(files)+hidden), hidden)
	default:
		st.note(scanUntested, "ok", fmt.Sprintf("%d changed source file(s) checked, %d without a reaching test", len(files), untested), 0)
	}
}

var scanQualifier = regexp.MustCompile(`[^.:]+$`)

func (r *ScanRunner) scanDead(ctx context.Context, st *scanState, dm ScanDevmap) {
	res, err := dm.Dead(ctx)
	if err != nil {
		st.note(scanDead, "unavailable", "devmap dead failed: "+err.Error(), 0)
		return
	}
	if res.Stale || len(res.Degraded) > 0 {
		st.note(scanDead, "degraded", fmt.Sprintf("devmap qualified the answer (stale=%t, %s); no dead-code proposal was made", res.Stale, strings.Join(res.Degraded, "; ")), res.Hidden)
		return
	}
	text := r.deps.Text(st.root)
	if nilInterface(text) {
		st.note(scanDead, "unavailable", "no text search client; the second signal cannot be taken", res.Hidden)
		return
	}
	type hit struct {
		name   string
		conf   float64
		result string
	}
	byFile := map[string][]hit{}
	var files []string
	checked, inconclusive, exempt := 0, 0, 0
	for _, d := range res.Items {
		if d.IsExempt {
			exempt++
			continue
		}
		name := scanQualifier.FindString(d.Name)
		if d.Confidence < scanDeadConfidence || scanIsTestPath(d.FilePath) || len(name) < 3 || name == "main" || name == "init" {
			continue
		}
		if checked >= scanMaxDeadChecks {
			inconclusive++
			continue
		}
		if ctx.Err() != nil {
			st.note(scanDead, "skipped", "stopped before every candidate was checked: "+ctx.Err().Error(), res.Hidden)
			return
		}
		checked++
		result, ok, err := r.unreferenced(ctx, text, d.FilePath, name)
		if err != nil {
			st.note(scanDead, "unavailable", "text search failed: "+err.Error(), res.Hidden)
			return
		}
		if !ok {
			if result != "" {
				inconclusive++
			}
			continue
		}
		if byFile[d.FilePath] == nil {
			files = append(files, d.FilePath)
		}
		byFile[d.FilePath] = append(byFile[d.FilePath], hit{d.Name, d.Confidence, result})
	}
	sort.Strings(files)
	for _, file := range files {
		hits := byFile[file]
		var b strings.Builder
		signals := make([]ScanSignal, 0, 2*len(hits))
		for i, h := range hits {
			if i < scanMaxSymbolsListed {
				fmt.Fprintf(&b, "- %s: devmap dead_symbols confidence %.2f (not exempt); text search: %s\n", h.name, h.conf, h.result)
			}
			signals = append(signals,
				ScanSignal{Source: "devmap dead_symbols " + h.name, Result: fmt.Sprintf("confidence %.2f, not exempt", h.conf)},
				ScanSignal{Source: "text search (ignore rules bypassed) " + h.name, Result: h.result})
		}
		st.findings = append(st.findings, scanFinding{
			key: file, category: scanDead, kind: "chore", priority: 3, signals: signals,
			title: fmt.Sprintf("Remove or wire up %d unreferenced symbol(s) in %s", len(hits), file),
			body: "Each symbol below was flagged by two independent signals: DevMap reports it dead with high confidence, and a text search that ignores .gitignore finds no other reference to its name. " +
				"Registries, reflection and build tags can still hide a use; check before deleting.\n\n" + b.String(),
		})
	}
	detail := fmt.Sprintf("%d candidate(s) text-checked, %d exempt skipped, %d inconclusive", checked, exempt, inconclusive)
	switch {
	case res.Hidden > 0:
		st.note(scanDead, "truncated", "devmap capped the dead-symbol list; "+detail, res.Hidden)
	case inconclusive > 0:
		st.note(scanDead, "truncated", detail, 0)
	default:
		st.note(scanDead, "ok", detail, 0)
	}
}

// unreferenced is the second dead-code signal: does the symbol's name occur
// anywhere besides its own definition? ok is true only for a conclusive
// "no"; a truncated search is inconclusive and returns a non-empty reason.
func (r *ScanRunner) unreferenced(ctx context.Context, text ScanText, file, name string) (reason string, ok bool, err error) {
	res, err := text.Search(ctx, dcgrep.Request{
		Pattern: `\b` + regexp.QuoteMeta(name) + `\b`, IncludeIgnored: true, MaxResults: scanTextMaxResults,
	})
	if err != nil {
		return "", false, err
	}
	if res == nil || !res.OK {
		return "", false, errors.New("text search refused the query")
	}
	inFile := 0
	for _, m := range res.Matches {
		p := filepath.ToSlash(m.Path)
		if p == ".git" || strings.HasPrefix(p, ".git/") || strings.HasPrefix(p, ".devcouncil/") || strings.HasPrefix(p, ".gitpulse/") {
			continue
		}
		if p != file {
			return fmt.Sprintf("referenced in %s", p), false, nil
		}
		if inFile++; inFile > 1 {
			return fmt.Sprintf("referenced %d times inside %s", inFile, file), false, nil
		}
	}
	if res.Truncated {
		return "search hit its result limit before ruling references out", false, nil
	}
	return "no match outside its own definition", true, nil
}

var scanDocPath = regexp.MustCompile("`([A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+)`")

var scanDocExt = map[string]bool{".go": true, ".rs": true, ".py": true, ".ts": true, ".tsx": true, ".js": true, ".mjs": true, ".json": true,
	".md": true, ".yaml": true, ".yml": true, ".toml": true, ".sh": true, ".sql": true, ".html": true, ".css": true}

func (r *ScanRunner) scanDocs(ctx context.Context, st *scanState) {
	out, err := scanGit(ctx, st.root, "ls-files", "-z", "--", "*.md")
	if err != nil {
		st.note(scanDocs, "unavailable", "git ls-files failed: "+err.Error(), 0)
		return
	}
	var docs []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" && !strings.Contains(strings.ToLower(filepath.Base(name)), "changelog") {
			docs = append(docs, name)
		}
	}
	sort.Strings(docs)
	hidden := 0
	if len(docs) > scanMaxDocFiles {
		hidden = len(docs) - scanMaxDocFiles
		docs = docs[:scanMaxDocFiles]
	}
	// Every read and lookup goes through the repository as an os.Root: a
	// tracked document can be a symlink out of the work tree, and a path a
	// document names can climb out of it, and neither is this scan's to read.
	repo, err := os.OpenRoot(st.root)
	if err != nil {
		st.note(scanDocs, "unavailable", "opening the repository root failed: "+err.Error(), 0)
		return
	}
	defer func() { _ = repo.Close() }()
	skipped := 0
	for _, doc := range docs {
		if ctx.Err() != nil {
			st.note(scanDocs, "skipped", "stopped before every document was read: "+ctx.Err().Error(), 0)
			return
		}
		data, err := repo.ReadFile(doc)
		if err != nil || len(data) > scanMaxDocBytes {
			skipped++
			continue
		}
		var missing []string
		seen := map[string]bool{}
		for _, m := range scanDocPath.FindAllStringSubmatch(string(data), -1) {
			ref := m[1]
			if seen[ref] || !scanDocExt[filepath.Ext(ref)] || strings.HasPrefix(ref, ".") {
				continue
			}
			seen[ref] = true
			if _, e1 := repo.Stat(ref); e1 == nil {
				continue
			}
			if _, e2 := repo.Stat(filepath.Join(filepath.Dir(doc), ref)); e2 == nil {
				continue
			}
			missing = append(missing, ref)
		}
		if len(missing) == 0 {
			continue
		}
		sort.Strings(missing)
		var b strings.Builder
		for i, ref := range missing {
			if i < scanMaxSymbolsListed {
				fmt.Fprintf(&b, "- `%s`\n", ref)
			}
		}
		st.findings = append(st.findings, scanFinding{
			key: doc, category: scanDocs, kind: "docs", priority: 3,
			title:   fmt.Sprintf("Fix %d stale path reference(s) in %s", len(missing), doc),
			body:    fmt.Sprintf("%s names paths that do not exist in the work tree (checked from the repository root and the document's directory):\n\n%s", doc, b.String()),
			signals: []ScanSignal{{Source: "filesystem", Result: fmt.Sprintf("%d named path(s) missing", len(missing))}},
		})
	}
	switch {
	case hidden > 0 || skipped > 0:
		st.note(scanDocs, "truncated", fmt.Sprintf("%d document(s) beyond the cap and %d unreadable or oversized were not checked", hidden, skipped), hidden+skipped)
	default:
		st.note(scanDocs, "ok", fmt.Sprintf("%d document(s) checked", len(docs)), 0)
	}
}
