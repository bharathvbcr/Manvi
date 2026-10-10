package serve

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
)

// OpTasksSuggest ranks which open board tasks to work on next. It is a host
// operation: registered by WorkbenchModule, never offered to an agent, and it
// changes nothing on the board.
//
// It makes no model call. The ranking is a fixed point table over fields the
// store already holds, so the same board always yields the same order, every
// point is named in the answer, and asking costs nothing. A model would add
// latency and a place for the order to drift without adding a signal the table
// lacks; a host that wants prose can render the signals itself.
const OpTasksSuggest = "work.tasks.suggest"

// The per-run limits. A board over a limit is reported as incomplete, never
// presented as the whole board.
const (
	suggestDefaultLimit = 5
	suggestMaxLimit     = 20
	// suggestPageSize and suggestMaxPages bound one status column's read.
	suggestPageSize = 200
	suggestMaxPages = 10
	// suggestMaxHydrate bounds the items.get calls made for cards a list page
	// returned without their links.
	suggestMaxHydrate = 150
	// suggestMaxVerifyProbes bounds the evidence reads: only the best
	// candidates are checked, and the rest are counted as unchecked.
	suggestMaxVerifyProbes = 12
	// suggestMaxBlockers bounds the blocker ids named per blocked task.
	suggestMaxBlockers = 5
	// suggestMaxBlockedShown bounds the blocked tasks listed in the answer.
	suggestMaxBlockedShown = 20
)

// suggestOpenStatuses are the columns read: everything not Done. Done and
// archived cards are not work, and a blocker that is not in this set is not
// blocking.
var suggestOpenStatuses = []string{"inbox", "backlog", "ready", "in_progress", "review"}

// suggestStatuses are the columns a task may be suggested from. in_progress is
// already being worked and review is waiting on a person.
var suggestStatuses = map[string]bool{"inbox": true, "backlog": true, "ready": true}

// SuggestParams is the work.tasks.suggest request.
type SuggestParams struct {
	// RequestID is echoed back so a host can pair a reply with its ask. The
	// answer is a pure read, so a repeat is always safe.
	RequestID string `json:"request_id,omitempty"`
	// Limit is how many suggestions to return; zero means 5, at most 20.
	Limit int `json:"limit,omitempty"`
	// MaxPriority keeps only tasks at that priority or more urgent (0 urgent
	// through 3 low). Absent means every priority.
	MaxPriority *int `json:"max_priority,omitempty"`
	// RepositoryID keeps only tasks linked to that repository. Blocking is
	// still judged against the whole board.
	RepositoryID string `json:"repository_id,omitempty"`
}

// SuggestSignal is one named contribution to a task's score.
type SuggestSignal struct {
	Name   string `json:"name"`
	Points int    `json:"points"`
	Detail string `json:"detail"`
}

// Suggestion is one ranked task.
type Suggestion struct {
	Rank     int             `json:"rank"`
	ID       string          `json:"id"`
	Title    string          `json:"title"`
	Status   string          `json:"status"`
	Priority int             `json:"priority"`
	Score    int             `json:"score"`
	Reason   string          `json:"reason"`
	Signals  []SuggestSignal `json:"signals"`
}

// SuggestBlocked is an open task left out because an open task blocks it.
type SuggestBlocked struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	BlockedBy []string `json:"blocked_by"`
}

// SuggestCounts says what the ranking left out, by reason.
type SuggestCounts struct {
	InProgress int `json:"in_progress"`
	Review     int `json:"review"`
	Duplicate  int `json:"duplicate"`
	Blocked    int `json:"blocked"`
	// Filtered is what the request's repository or priority filter removed.
	Filtered int `json:"filtered"`
}

// SuggestBoard is how much of the board was read.
type SuggestBoard struct {
	Scanned int `json:"scanned"`
	// Complete is false when a bound cut a read; the ranking is then over a
	// sample: a blocker that was never read cannot be seen.
	Complete bool `json:"complete"`
	// Unhydrated counts open tasks whose links could not be read.
	Unhydrated int `json:"unhydrated"`
}

// SuggestVerification says how many top candidates had their verification
// record read. Unchecked candidates were ranked without that signal.
type SuggestVerification struct {
	Checked   int `json:"checked"`
	Unchecked int `json:"unchecked"`
	Failed    int `json:"failed"`
}

// SuggestResult is the work.tasks.suggest answer.
type SuggestResult struct {
	OK           bool                `json:"ok"`
	RequestID    string              `json:"request_id,omitempty"`
	GeneratedAt  int64               `json:"generated_at"`
	Suggestions  []Suggestion        `json:"suggestions"`
	Blocked      []SuggestBlocked    `json:"blocked"`
	Excluded     SuggestCounts       `json:"excluded"`
	Board        SuggestBoard        `json:"board"`
	Verification SuggestVerification `json:"verification"`
	Candidates   int                 `json:"candidates"`
}

// SuggestStore is the read side of the profile store the ranking needs.
type SuggestStore interface {
	Workbench(ctx context.Context, method string, input json.RawMessage) (json.RawMessage, error)
	EvidenceList(ctx context.Context, taskID string) ([]store.EvidenceRow, bool, error)
}

type suggestLink struct {
	Kind   string `json:"kind"`
	ItemID string `json:"item_id"`
}

type suggestItem struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Status       string         `json:"status"`
	Priority     *int           `json:"priority"`
	Severity     *string        `json:"severity"`
	Labels       []string       `json:"labels"`
	DueAt        *int64         `json:"due_at"`
	UpdatedAt    int64          `json:"updated_at"`
	Archived     bool           `json:"archived"`
	CompletedAt  *int64         `json:"completed_at"`
	Repositories []string       `json:"repository_ids"`
	Primary      string         `json:"primary_repository_id"`
	Description  string         `json:"description"`
	Links        *[]suggestLink `json:"links"`
}

func (i suggestItem) priority() int {
	if i.Priority == nil || *i.Priority < 0 || *i.Priority > 3 {
		return 2
	}
	return *i.Priority
}

func (i suggestItem) inRepository(id string) bool {
	return i.Primary == id || slices.Contains(i.Repositories, id)
}

// suggestNow is the clock; a variable so a test can fix it.
var suggestNow = time.Now

// Suggest ranks the open board and returns the best few with their reasons.
func Suggest(ctx context.Context, board SuggestStore, raw json.RawMessage) (SuggestResult, error) {
	var params SuggestParams
	if err := decodeStrict(raw, &params); err != nil {
		return SuggestResult{}, &store.WorkbenchError{Code: "invalid_input", Message: err.Error()}
	}
	if len(params.RequestID) > 128 || len(params.RepositoryID) > 128 {
		return SuggestResult{}, &store.WorkbenchError{Code: "invalid_input", Message: "request_id and repository_id are at most 128 bytes"}
	}
	if params.Limit < 0 || params.Limit > suggestMaxLimit {
		return SuggestResult{}, &store.WorkbenchError{Code: "invalid_input", Message: fmt.Sprintf("limit is 1 to %d", suggestMaxLimit)}
	}
	if params.MaxPriority != nil && (*params.MaxPriority < 0 || *params.MaxPriority > 3) {
		return SuggestResult{}, &store.WorkbenchError{Code: "invalid_input", Message: "max_priority is 0 (urgent) to 3 (low)"}
	}
	limit := params.Limit
	if limit == 0 {
		limit = suggestDefaultLimit
	}
	now := suggestNow().UTC()

	items, complete, err := suggestReadBoard(ctx, board)
	if err != nil {
		return SuggestResult{}, err
	}
	result := SuggestResult{OK: true, RequestID: params.RequestID, GeneratedAt: now.Unix(),
		Suggestions: []Suggestion{}, Blocked: []SuggestBlocked{}, Board: SuggestBoard{Scanned: len(items), Complete: complete}}
	unhydrated, err := suggestHydrate(ctx, board, items)
	if err != nil {
		return SuggestResult{}, err
	}
	result.Board.Unhydrated = unhydrated

	byID := make(map[string]*suggestItem, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	// blockedBy[x] is the open tasks that name x in a blocks link; unblocks[x]
	// the open tasks x names. A link is read from the task that holds it: on T,
	// {blocks, X} means T blocks X.
	blockedBy := map[string][]string{}
	unblocks := map[string]int{}
	for _, item := range items {
		if item.Links == nil {
			continue
		}
		for _, link := range *item.Links {
			if link.Kind != "blocks" || link.ItemID == item.ID {
				continue
			}
			blockedBy[link.ItemID] = append(blockedBy[link.ItemID], item.ID)
			if _, open := byID[link.ItemID]; open {
				unblocks[item.ID]++
			}
		}
	}

	type candidate struct {
		item    *suggestItem
		signals []SuggestSignal
		score   int
	}
	var ranked []*candidate
	for _, item := range items {
		switch item.Status {
		case "in_progress":
			result.Excluded.InProgress++
			continue
		case "review":
			result.Excluded.Review++
			continue
		}
		if !suggestStatuses[item.Status] {
			continue
		}
		if item.Links == nil {
			// Its links could not be read, so neither can a duplicate or a
			// blocker be ruled out. Naming it a suggestion would be a guess.
			result.Excluded.Blocked++
			result.Blocked = appendBlocked(result.Blocked, item, []string{"links unreadable"})
			continue
		}
		if duplicateOfOpen(item, byID) {
			result.Excluded.Duplicate++
			continue
		}
		blockers := slices.Clone(blockedBy[item.ID])
		if len(blockers) > 0 {
			slices.Sort(blockers)
			result.Excluded.Blocked++
			result.Blocked = appendBlocked(result.Blocked, item, blockers)
			continue
		}
		if (params.RepositoryID != "" && !item.inRepository(params.RepositoryID)) ||
			(params.MaxPriority != nil && item.priority() > *params.MaxPriority) {
			result.Excluded.Filtered++
			continue
		}
		c := &candidate{item: item}
		c.signals = scoreSignals(item, unblocks[item.ID], now)
		for _, s := range c.signals {
			c.score += s.Points
		}
		ranked = append(ranked, c)
	}
	result.Candidates = len(ranked)

	order := func(a, b *candidate) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		if c := cmp.Compare(a.item.priority(), b.item.priority()); c != 0 {
			return c
		}
		if c := compareDue(a.item.DueAt, b.item.DueAt); c != 0 {
			return c
		}
		return strings.Compare(a.item.ID, b.item.ID)
	}
	slices.SortFunc(ranked, order)

	// Verification state is read for the best candidates only: one evidence
	// read each. It can move a task either way, so the order is settled again
	// after it.
	for i, c := range ranked {
		if i >= suggestMaxVerifyProbes {
			result.Verification.Unchecked++
			continue
		}
		signal, ok, err := verificationSignal(ctx, board, c.item.ID)
		if err != nil {
			if ctx.Err() != nil {
				return SuggestResult{}, ctx.Err()
			}
			result.Verification.Unchecked++
			continue
		}
		result.Verification.Checked++
		if ok {
			c.signals = append(c.signals, signal)
			c.score += signal.Points
			if signal.Name == "verification_failed" {
				result.Verification.Failed++
			}
		}
	}
	slices.SortFunc(ranked, order)

	for i, c := range ranked {
		if i == limit {
			break
		}
		result.Suggestions = append(result.Suggestions, Suggestion{
			Rank: i + 1, ID: c.item.ID, Title: truncateRunes(c.item.Title, 300), Status: c.item.Status,
			Priority: c.item.priority(), Score: c.score, Reason: suggestReason(c.signals), Signals: c.signals,
		})
	}
	return result, nil
}

func appendBlocked(list []SuggestBlocked, item *suggestItem, by []string) []SuggestBlocked {
	if len(list) >= suggestMaxBlockedShown {
		return list
	}
	if len(by) > suggestMaxBlockers {
		by = by[:suggestMaxBlockers]
	}
	return append(list, SuggestBlocked{ID: item.ID, Title: truncateRunes(item.Title, 300), BlockedBy: by})
}

func duplicateOfOpen(item *suggestItem, byID map[string]*suggestItem) bool {
	for _, link := range *item.Links {
		if link.Kind == "duplicate_of" {
			if _, open := byID[link.ItemID]; open {
				return true
			}
		}
	}
	return false
}

func compareDue(a, b *int64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return cmp.Compare(*a, *b)
}

var (
	suggestPriorityPoints = [4]int{100, 60, 25, 0}
	suggestPriorityNames  = [4]string{"urgent", "high", "normal", "low"}
	suggestSeverityPoints = map[string]int{"critical": 40, "high": 25, "medium": 10}
)

// scoreSignals is the whole ranking policy: every point a task earns, named.
func scoreSignals(item *suggestItem, unblocks int, now time.Time) []SuggestSignal {
	var out []SuggestSignal
	add := func(name string, points int, detail string) {
		if points != 0 {
			out = append(out, SuggestSignal{Name: name, Points: points, Detail: detail})
		}
	}
	p := item.priority()
	add("priority", suggestPriorityPoints[p], suggestPriorityNames[p]+" priority")
	if item.Severity != nil {
		sev := strings.ToLower(strings.TrimSpace(*item.Severity))
		add("severity", suggestSeverityPoints[sev], sev+" severity")
	}
	if item.DueAt != nil && *item.DueAt > 0 {
		left := *item.DueAt - now.Unix()
		switch {
		case left < 0:
			days := int(-left / 86400)
			add("due", 50+min(days, 14), fmt.Sprintf("overdue by %s", plural(days, "day")))
		case left <= 2*86400:
			add("due", 35, "due within 2 days")
		case left <= 7*86400:
			add("due", 20, "due within a week")
		}
	}
	if item.UpdatedAt > 0 && now.Unix() > item.UpdatedAt {
		days := int((now.Unix() - item.UpdatedAt) / 86400)
		add("age", min(days, 90)/9, fmt.Sprintf("untouched for %s", plural(days, "day")))
	}
	if unblocks > 0 {
		add("unblocks", min(unblocks, 3)*12, fmt.Sprintf("blocks %s", plural(unblocks, "open task")))
	}
	if item.Status == "ready" {
		add("ready", 10, "already marked ready")
	}
	if suggestFromScan(item) {
		add("scan_finding", 8, "raised by a repository gap scan")
	}
	return out
}

func suggestFromScan(item *suggestItem) bool {
	if scanFingerprintPattern.MatchString(item.Description) {
		return true
	}
	for _, label := range item.Labels {
		if scanFingerprintPattern.MatchString(label) {
			return true
		}
	}
	return false
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// suggestReason is the one-line why: the largest contributions, in order.
func suggestReason(signals []SuggestSignal) string {
	sorted := slices.Clone(signals)
	slices.SortStableFunc(sorted, func(a, b SuggestSignal) int { return cmp.Compare(b.Points, a.Points) })
	var parts []string
	for _, s := range sorted {
		if s.Points <= 0 || len(parts) == 3 {
			continue
		}
		parts = append(parts, s.Detail)
	}
	if len(parts) == 0 {
		return "no urgency signal; ranked by tie-break"
	}
	return strings.Join(parts, "; ")
}

// verificationSignal reads the newest stored verification for a task. ok is
// false when there is none to speak of.
func verificationSignal(ctx context.Context, board SuggestStore, id string) (SuggestSignal, bool, error) {
	rows, _, err := board.EvidenceList(ctx, id)
	if err != nil {
		return SuggestSignal{}, false, err
	}
	for _, row := range rows {
		if row.Type != verifyEvidenceType {
			continue
		}
		var record VerifyRecord
		if json.Unmarshal(row.DataJSON, &record) != nil || record.RequestID == "" {
			continue
		}
		switch {
		case record.State == verifyRunning:
			return SuggestSignal{}, false, nil
		case record.Outcome == VerifyPassed:
			return SuggestSignal{Name: "verification_passed", Points: -25, Detail: "its last verification passed"}, true, nil
		case record.Outcome == VerifyFailed || record.Outcome == VerifyDegraded:
			return SuggestSignal{Name: "verification_failed", Points: 15, Detail: "its last verification found problems"}, true, nil
		}
		return SuggestSignal{}, false, nil
	}
	return SuggestSignal{}, false, nil
}

// suggestReadBoard reads every open column, page by page up to a bound.
// complete is false when a bound cut any column.
func suggestReadBoard(ctx context.Context, board SuggestStore) ([]*suggestItem, bool, error) {
	var items []*suggestItem
	complete := true
	seen := map[string]bool{}
	for _, status := range suggestOpenStatuses {
		cursor := ""
		for page := 0; ; page++ {
			if page == suggestMaxPages {
				complete = false
				break
			}
			input := map[string]any{"status": status, "archived": false, "limit": suggestPageSize}
			if cursor != "" {
				input["cursor"] = cursor
			}
			in, err := json.Marshal(input)
			if err != nil {
				return nil, false, err
			}
			out, err := board.Workbench(ctx, "items.list", in)
			if err != nil {
				return nil, false, err
			}
			var res struct {
				Items      []*suggestItem `json:"items"`
				HasMore    bool           `json:"has_more"`
				NextCursor string         `json:"next_cursor"`
			}
			if err := json.Unmarshal(out, &res); err != nil {
				return nil, false, fmt.Errorf("decode items.list: %w", err)
			}
			for _, item := range res.Items {
				if item == nil || item.ID == "" || seen[item.ID] || item.Archived || item.CompletedAt != nil {
					continue
				}
				seen[item.ID] = true
				item.Status = status
				items = append(items, item)
			}
			if !res.HasMore {
				break
			}
			if res.NextCursor == "" {
				complete = false
				break
			}
			cursor = res.NextCursor
		}
	}
	return items, complete, nil
}

// suggestHydrate reads the cards a list page returned without links through
// items.get, up to a bound. It returns how many were left unread.
func suggestHydrate(ctx context.Context, board SuggestStore, items []*suggestItem) (int, error) {
	unread, calls := 0, 0
	for _, item := range items {
		if item.Links != nil {
			continue
		}
		if calls == suggestMaxHydrate {
			unread++
			continue
		}
		calls++
		in, err := json.Marshal(map[string]string{"id": item.ID})
		if err != nil {
			return 0, err
		}
		out, err := board.Workbench(ctx, "items.get", in)
		if err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			unread++
			continue
		}
		var res struct {
			Item suggestItem `json:"item"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			unread++
			continue
		}
		links := []suggestLink{}
		if res.Item.Links != nil {
			links = *res.Item.Links
		}
		item.Links = &links
		item.Description = res.Item.Description
	}
	return unread, nil
}
