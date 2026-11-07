package serve

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
)

// OpTasksOrganize proposes a priority, an order and duplicate groups for the
// open board. It is a host operation: registered by WorkbenchModule, never
// offered to an agent, and it changes nothing on the board.
//
// It makes no model call. The proposal is a fixed set of named rules over
// fields the store already holds, so the same board always yields the same
// proposal. A person accepts a row before the host writes it, through the
// existing revision-checked items.put. This op does not write.
const OpTasksOrganize = "work.tasks.organize"

const (
	// organizePageSize and organizeMaxPages bound one status column's read,
	// the same page the suggestion read uses. Past the cap the unread count
	// comes from items.list total; a cut page with no total is an error.
	organizePageSize = suggestPageSize
	organizeMaxPages = suggestMaxPages
)

// organizeMaxHydrate bounds items.get calls for cards a list page returned
// without links. A variable so a test can exhaust it. 2000 covers a board of
// 500 tasks across the open columns with room to spare; the rest are counted.
var organizeMaxHydrate = 2000

// organizeOrderNote is the column-order contract the board shows beside
// Priority first. The two agree on priority and due date once a proposed
// priority is stored. Suggest next weighs more than those two.
const organizeOrderNote = "Column order is priority, then due date, using the proposed priority. Priority first uses those two signals on the stored priority. Suggest next also weighs severity, blockers, age, and verification."

// organizeDiffers names the signals Suggest next weighs and this proposal's
// column order does not. The order itself is priority, then due date.
var organizeDiffers = []string{"severity", "blockers", "age", "verification"}

// OrganizeParams is the work.tasks.organize request. Both fields are optional.
type OrganizeParams struct {
	// RequestID is echoed back so a host can pair a reply with its ask.
	RequestID string `json:"request_id,omitempty"`
	// RepositoryID keeps changes, column order and groups that touch that
	// repository. Blockers and duplicates are still judged against the whole
	// board. The unread count is rows the read cap did not fetch, before this
	// filter, so a capped read can include other repositories.
	RepositoryID string `json:"repository_id,omitempty"`
}

// OrganizeChange is one task whose priority would change. From and To are the
// priority scale (0 urgent through 3 low). Nothing here has been written.
type OrganizeChange struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Revision int64  `json:"revision"`
	From     int    `json:"from"`
	To       int    `json:"to"`
	Rule     string `json:"rule"`
	Reason   string `json:"reason"`
}

// OrganizeOrder is one open column in proposed order: target priority, then
// the earlier due date, then id.
type OrganizeOrder struct {
	Status string   `json:"status"`
	IDs    []string `json:"ids"`
}

// OrganizeGroup is a cluster of open tasks joined by duplicate_of. Keep is
// the task the duplicate rule says stays. Members includes Keep. The op does
// not merge them.
type OrganizeGroup struct {
	Kind    string   `json:"kind"`
	Keep    string   `json:"keep"`
	Members []string `json:"members"`
}

// OrganizeCoverage is how much of the board the proposal saw.
type OrganizeCoverage struct {
	// Read is in-scope open tasks whose cards were fetched.
	Read int `json:"read"`
	// Unread is rows items.list did not return. It is exact, taken from total,
	// and it is not filtered by repository: a capped column is a cap on the
	// whole board.
	Unread int `json:"unread"`
	// Unhydrated is fetched open tasks whose links were not read.
	Unhydrated int `json:"unhydrated"`
	// Unrevisioned is in-scope tasks a rule would change that had no revision,
	// so the proposal left them out rather than send expected_revision 0.
	Unrevisioned int `json:"unrevisioned"`
	// Complete is true only when every column was read to its end, nothing was
	// left unread, and every fetched open task's links were read.
	Complete bool `json:"complete"`
	// Note is set when a repository filter is on and unread is non-zero, so
	// the cap is not mistaken for a count of that repository alone.
	Note string `json:"note,omitempty"`
}

// OrganizeResult is the work.tasks.organize answer. Slices are empty, never
// null. Method is "deterministic": the same board gives the same proposal.
type OrganizeResult struct {
	OK          bool             `json:"ok"`
	RequestID   string           `json:"request_id,omitempty"`
	GeneratedAt int64            `json:"generated_at"`
	Method      string           `json:"method"`
	Changes     []OrganizeChange `json:"changes"`
	Order       []OrganizeOrder  `json:"order"`
	Groups      []OrganizeGroup  `json:"groups"`
	Coverage    OrganizeCoverage `json:"coverage"`
	OrderNote   string           `json:"order_note"`
	Differs     []string         `json:"differs"`
}

// Organize reads the open board and returns a reviewable proposal. It calls
// items.list and items.get only.
func Organize(ctx context.Context, board SuggestStore, raw json.RawMessage) (OrganizeResult, error) {
	var params OrganizeParams
	if err := decodeStrict(raw, &params); err != nil {
		return OrganizeResult{}, &store.WorkbenchError{Code: "invalid_input", Message: err.Error()}
	}
	if len(params.RequestID) > 128 || len(params.RepositoryID) > 128 {
		return OrganizeResult{}, &store.WorkbenchError{Code: "invalid_input", Message: "request_id and repository_id are at most 128 bytes"}
	}
	now := suggestNow()
	items, read, err := organizeReadBoard(ctx, board)
	if err != nil {
		return OrganizeResult{}, err
	}
	if err := organizeHydrate(ctx, board, items); err != nil {
		return OrganizeResult{}, err
	}

	inScope := func(item *suggestItem) bool {
		return params.RepositoryID == "" || item.inRepository(params.RepositoryID)
	}
	byID := map[string]*suggestItem{}
	for _, item := range items {
		byID[item.ID] = item
	}
	unblocks := map[string]int{}
	for _, item := range items {
		if item.Links == nil {
			continue
		}
		for _, link := range *item.Links {
			if link.Kind != "blocks" || link.ItemID == "" || link.ItemID == item.ID {
				continue
			}
			if _, open := byID[link.ItemID]; open {
				unblocks[item.ID]++
			}
		}
	}

	proposed := map[string]int{}
	var changes []OrganizeChange
	unrevisioned := 0
	scoped := 0
	unhydrated := 0
	for _, item := range items {
		if item.Links == nil {
			unhydrated++
		}
		proposed[item.ID] = item.priority()
		if !inScope(item) {
			continue
		}
		scoped++
		to, rule, reason, ok := organizeTarget(item, unblocks[item.ID], now)
		if !ok || to == item.priority() {
			continue
		}
		if item.Revision <= 0 {
			unrevisioned++
			continue
		}
		proposed[item.ID] = to
		changes = append(changes, OrganizeChange{
			ID: item.ID, Title: truncateRunes(item.Title, 300), Status: item.Status,
			Revision: item.Revision, From: item.priority(), To: to, Rule: rule, Reason: reason,
		})
	}
	if changes == nil {
		changes = []OrganizeChange{}
	}
	statusRank := map[string]int{}
	for i, status := range suggestOpenStatuses {
		statusRank[status] = i
	}
	slices.SortFunc(changes, func(a, b OrganizeChange) int {
		if c := cmp.Compare(statusRank[a.Status], statusRank[b.Status]); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})

	order := []OrganizeOrder{}
	for _, status := range suggestOpenStatuses {
		var col []*suggestItem
		for _, item := range items {
			if item.Status == status && inScope(item) {
				col = append(col, item)
			}
		}
		if len(col) == 0 {
			continue
		}
		slices.SortFunc(col, func(a, b *suggestItem) int {
			if c := cmp.Compare(proposed[a.ID], proposed[b.ID]); c != 0 {
				return c
			}
			if c := cmp.Compare(organizeDue(a.DueAt), organizeDue(b.DueAt)); c != 0 {
				return c
			}
			return strings.Compare(a.ID, b.ID)
		})
		ids := make([]string, len(col))
		for i, item := range col {
			ids[i] = item.ID
		}
		order = append(order, OrganizeOrder{Status: status, IDs: ids})
	}

	groups := organizeGroups(items, byID, inScope)
	note := ""
	if params.RepositoryID != "" && read.unread > 0 {
		note = "The read cap is per column of the whole board, so some of the unread rows may belong to other repositories."
	}
	ended := read.ended
	return OrganizeResult{
		OK: true, RequestID: params.RequestID, GeneratedAt: now.Unix(), Method: "deterministic",
		Changes: changes, Order: order, Groups: groups,
		Coverage: OrganizeCoverage{
			Read: scoped, Unread: read.unread, Unhydrated: unhydrated, Unrevisioned: unrevisioned,
			Complete: ended && read.unread == 0 && unhydrated == 0,
			Note:     note,
		},
		OrderNote: organizeOrderNote,
		Differs:   slices.Clone(organizeDiffers),
	}, nil
}

// organizeTarget is the first rule whose target priority differs from the
// task's current one. Rules only raise a priority, except stale_high, which
// lowers an inbox or backlog task by one step and is still only a proposal.
func organizeTarget(item *suggestItem, unblocks int, now time.Time) (to int, rule, reason string, ok bool) {
	from := item.priority()
	sev := ""
	if item.Severity != nil {
		sev = strings.ToLower(strings.TrimSpace(*item.Severity))
	}
	if sev == "critical" && from > 0 {
		return 0, "severity_critical", "critical severity", true
	}
	if sev == "high" && from > 1 {
		return 1, "severity_high", "high severity", true
	}
	if item.DueAt != nil && *item.DueAt > 0 && *item.DueAt < now.Unix() && from > 0 {
		overdueBy := now.Unix() - *item.DueAt
		days := int(overdueBy / 86400)
		detail := "overdue by less than a day"
		if days > 0 {
			detail = "overdue by " + plural(days, "day")
		}
		if days >= 7 {
			return 0, "overdue", detail, true
		}
		if from > 1 {
			return 1, "overdue", detail, true
		}
	}
	if item.DueAt != nil && *item.DueAt > 0 {
		left := *item.DueAt - now.Unix()
		if left >= 0 && left <= 2*86400 && from > 1 {
			return 1, "due_soon", "due within 2 days", true
		}
	}
	if item.Links != nil && unblocks >= 1 && from > 1 {
		return 1, "blocks_work", "blocks " + plural(unblocks, "open task"), true
	}
	if (from == 0 || from == 1) && (item.Status == "inbox" || item.Status == "backlog") && item.Links != nil && unblocks == 0 {
		noDue := item.DueAt == nil || *item.DueAt <= 0
		if noDue && sev != "critical" && sev != "high" && sev != "medium" && item.UpdatedAt > 0 {
			idle := now.Unix() - item.UpdatedAt
			if idle >= 90*86400 {
				days := int(idle / 86400)
				return from + 1, "stale_high", fmt.Sprintf("untouched %d days with no due date, severity, or blocked work", days), true
			}
		}
	}
	return from, "", "", false
}

// organizeDue matches Priority first: a missing or non-positive due date sorts
// last. The sentinel is MaxInt64; a board due date is a unix second well below it.
func organizeDue(due *int64) int64 {
	if due == nil || *due <= 0 {
		return int64(^uint64(0) >> 1)
	}
	return *due
}

type organizeReadState struct {
	unread int
	ended  bool
}

func organizeReadBoard(ctx context.Context, board SuggestStore) ([]*suggestItem, organizeReadState, error) {
	var items []*suggestItem
	state := organizeReadState{ended: true}
	seen := map[string]bool{}
	for _, status := range suggestOpenStatuses {
		kept, unread, ended, err := organizeReadColumn(ctx, board, status, seen)
		if err != nil {
			return nil, organizeReadState{}, err
		}
		items = append(items, kept...)
		state.unread += unread
		if !ended {
			state.ended = false
		}
	}
	return items, state, nil
}

// organizeReadColumn pages one status. fetched counts every row the store
// returned, including archived or completed cards, so unread is total minus
// rows actually read and not a guess about which of them were open.
func organizeReadColumn(ctx context.Context, board SuggestStore, status string, seen map[string]bool) ([]*suggestItem, int, bool, error) {
	var kept []*suggestItem
	cursor := ""
	fetched := 0
	var total *int
	var cut bool
	for page := 0; ; page++ {
		if page == organizeMaxPages {
			cut = true
			break
		}
		input := map[string]any{"status": status, "archived": false, "limit": organizePageSize}
		if cursor != "" {
			input["cursor"] = cursor
		}
		in, err := json.Marshal(input)
		if err != nil {
			return nil, 0, false, err
		}
		out, err := board.Workbench(ctx, "items.list", in)
		if err != nil {
			return nil, 0, false, err
		}
		var res struct {
			Items      []*suggestItem `json:"items"`
			HasMore    bool           `json:"has_more"`
			NextCursor string         `json:"next_cursor"`
			Total      *int           `json:"total"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			return nil, 0, false, fmt.Errorf("decode items.list: %w", err)
		}
		fetched += len(res.Items)
		if res.Total != nil {
			total = res.Total
		}
		for _, item := range res.Items {
			if item == nil || item.ID == "" || seen[item.ID] || item.Archived || item.CompletedAt != nil {
				continue
			}
			seen[item.ID] = true
			item.Status = status
			kept = append(kept, item)
		}
		if !res.HasMore {
			if total != nil && *total < fetched {
				return nil, 0, false, organizeTotalError(*total, fetched)
			}
			if total != nil && *total > fetched {
				return kept, *total - fetched, false, nil
			}
			return kept, 0, true, nil
		}
		if res.NextCursor == "" || res.NextCursor == cursor {
			cut = true
			break
		}
		cursor = res.NextCursor
	}
	if !cut {
		return kept, 0, true, nil
	}
	if total == nil {
		return nil, 0, false, &store.WorkbenchError{
			Code:    "unreadable_remainder",
			Message: "items.list had more tasks and did not say how many, so the unread count would be a guess",
		}
	}
	if *total < fetched {
		return nil, 0, false, organizeTotalError(*total, fetched)
	}
	return kept, *total - fetched, false, nil
}

func organizeTotalError(total, fetched int) error {
	return &store.WorkbenchError{
		Code:    "unreadable_remainder",
		Message: fmt.Sprintf("items.list total %d is less than the %d tasks already read", total, fetched),
	}
}

// organizeHydrate reads links for open cards the list omitted, best-first, up
// to organizeMaxHydrate. A card it does not read keeps Links == nil, which the
// proposal treats as unknown: it will not say the task blocks work, and it
// will not lower the task's priority.
func organizeHydrate(ctx context.Context, board SuggestStore, items []*suggestItem) error {
	type entry struct {
		item  *suggestItem
		index int
	}
	var queue []entry
	for i, item := range items {
		if item.Links == nil {
			queue = append(queue, entry{item, i})
		}
	}
	slices.SortFunc(queue, func(a, b entry) int {
		if c := cmp.Compare(a.item.priority(), b.item.priority()); c != 0 {
			return c
		}
		if c := compareDue(a.item.DueAt, b.item.DueAt); c != 0 {
			return c
		}
		return cmp.Compare(a.index, b.index)
	})
	for n, e := range queue {
		if n == organizeMaxHydrate {
			break
		}
		item := e.item
		in, err := json.Marshal(map[string]string{"id": item.ID})
		if err != nil {
			return err
		}
		out, err := board.Workbench(ctx, "items.get", in)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		var res struct {
			Item suggestItem `json:"item"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			continue
		}
		links := []suggestLink{}
		if res.Item.Links != nil {
			links = *res.Item.Links
		}
		item.Links = &links
		if res.Item.Revision > 0 {
			item.Revision = res.Item.Revision
		}
	}
	return nil
}

func organizeGroups(items []*suggestItem, byID map[string]*suggestItem, inScope func(*suggestItem) bool) []OrganizeGroup {
	adj := map[string]map[string]struct{}{}
	link := func(a, b string) {
		if adj[a] == nil {
			adj[a] = map[string]struct{}{}
		}
		if adj[b] == nil {
			adj[b] = map[string]struct{}{}
		}
		adj[a][b] = struct{}{}
		adj[b][a] = struct{}{}
	}
	for _, item := range items {
		if item.Links == nil {
			continue
		}
		for _, edge := range *item.Links {
			if edge.Kind != "duplicate_of" || edge.ItemID == "" || edge.ItemID == item.ID {
				continue
			}
			if _, open := byID[edge.ItemID]; !open {
				continue
			}
			link(item.ID, edge.ItemID)
		}
	}
	seen := map[string]bool{}
	groups := []OrganizeGroup{}
	for _, id := range slices.Sorted(maps.Keys(adj)) {
		if seen[id] {
			continue
		}
		var members []string
		stack := []string{id}
		seen[id] = true
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			members = append(members, cur)
			for next := range adj[cur] {
				if seen[next] {
					continue
				}
				seen[next] = true
				stack = append(stack, next)
			}
		}
		if len(members) < 2 {
			continue
		}
		slices.Sort(members)
		scoped := false
		for _, member := range members {
			if item := byID[member]; item != nil && inScope(item) {
				scoped = true
				break
			}
		}
		if !scoped {
			continue
		}
		keep := ""
		for _, member := range members {
			item := byID[member]
			if item == nil || duplicateLoses(item, byID) {
				continue
			}
			if keep == "" || member < keep {
				keep = member
			}
		}
		if keep == "" {
			keep = members[0]
		}
		groups = append(groups, OrganizeGroup{Kind: "duplicate_of", Keep: keep, Members: members})
	}
	slices.SortFunc(groups, func(a, b OrganizeGroup) int { return strings.Compare(a.Keep, b.Keep) })
	return groups
}
