package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
)

// organizeFakeStore pages items.list the way dc-store does: total is the whole
// column, and it is omitted when omitTotal is set so a cut read cannot invent
// an unread count. It records every method. items.put and evidence are failures
// the proposal must never reach.
type organizeFakeStore struct {
	items     []map[string]any
	withLinks bool
	omitTotal bool
	// totalOverride, when set, is the total on every page. Nil means the column length.
	totalOverride *int
	// stuck makes the second page repeat the cursor and add no rows.
	stuck bool
	// bareCut returns has_more with an empty cursor on the first page.
	bareCut bool
	// hideRevision drops revision from list cards so a test can prove items.get
	// supplies it. The get payload still carries the stored revision.
	hideRevision bool
	methods      []string
	puts         int
	evidence     int
	listLimit    int
}

func (f *organizeFakeStore) Workbench(_ context.Context, method string, input json.RawMessage) (json.RawMessage, error) {
	f.methods = append(f.methods, method)
	if method == "items.put" {
		f.puts++
		return nil, errors.New("organize must not write")
	}
	var in struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Cursor string `json:"cursor"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	switch method {
	case "items.get":
		for _, it := range f.items {
			if it["id"] == in.ID {
				out, _ := json.Marshal(map[string]any{"item": it})
				return out, nil
			}
		}
		return nil, errors.New("not found")
	case "items.list":
		f.listLimit = in.Limit
		column := make([]map[string]any, 0)
		for _, it := range f.items {
			if it["status"] != in.Status {
				continue
			}
			c := map[string]any{}
			for k, v := range it {
				if k == "links" && !f.withLinks {
					continue
				}
				if k == "revision" && f.hideRevision {
					continue
				}
				c[k] = v
			}
			column = append(column, c)
		}
		start := 0
		if in.Cursor != "" {
			if _, err := fmt.Sscanf(in.Cursor, "%d", &start); err != nil {
				return nil, fmt.Errorf("fake board: cursor %q is not an offset: %w", in.Cursor, err)
			}
		}
		end, more := len(column), false
		limit := in.Limit
		if limit <= 0 {
			limit = organizePageSize
		}
		if start+limit < len(column) {
			end, more = start+limit, true
		}
		if f.stuck && in.Cursor != "" {
			res := map[string]any{"items": []any{}, "has_more": true, "next_cursor": in.Cursor}
			f.putTotal(res, len(column))
			out, _ := json.Marshal(res)
			return out, nil
		}
		if f.stuck && in.Cursor == "" {
			more = true
			end = len(column)
		}
		if f.bareCut {
			more = true
			end = len(column)
		}
		res := map[string]any{"items": column[start:end], "has_more": more}
		if more && !f.bareCut {
			next := end
			if f.stuck {
				next = 1
			}
			res["next_cursor"] = fmt.Sprint(next)
		}
		f.putTotal(res, len(column))
		out, _ := json.Marshal(res)
		return out, nil
	}
	return nil, errors.New("unexpected " + method)
}

func (f *organizeFakeStore) putTotal(res map[string]any, column int) {
	if f.omitTotal {
		return
	}
	if f.totalOverride != nil && column > 0 {
		res["total"] = *f.totalOverride
		return
	}
	res["total"] = column
}

func (f *organizeFakeStore) EvidenceList(context.Context, string) ([]store.EvidenceRow, bool, error) {
	f.evidence++
	return nil, false, errors.New("organize must not read verification")
}

func oItem(id, status string, priority int, extra map[string]any) map[string]any {
	it := map[string]any{
		"id": id, "title": "task " + id, "status": status, "priority": priority,
		"updated_at": suggestTestNow, "revision": 1, "labels": []string{},
		"links": []map[string]string{}, "repository_ids": []string{"r1"},
		"primary_repository_id": "r1", "archived": false, "completed_at": nil,
	}
	for k, v := range extra {
		it[k] = v
	}
	return it
}

func organizeRun(t *testing.T, f SuggestStore, params string) OrganizeResult {
	t.Helper()
	suggestNow = func() time.Time { return time.Unix(suggestTestNow, 0) }
	t.Cleanup(func() { suggestNow = time.Now })
	res, err := Organize(context.Background(), f, json.RawMessage(params))
	if err != nil {
		t.Fatalf("Organize: %v", err)
	}
	return res
}

func organizeErr(t *testing.T, f SuggestStore) error {
	t.Helper()
	suggestNow = func() time.Time { return time.Unix(suggestTestNow, 0) }
	t.Cleanup(func() { suggestNow = time.Now })
	_, err := Organize(context.Background(), f, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	return err
}

func changeByID(res OrganizeResult, id string) (OrganizeChange, bool) {
	for _, change := range res.Changes {
		if change.ID == id {
			return change, true
		}
	}
	return OrganizeChange{}, false
}

func TestOrganizeProposesTheFirstNamedRule(t *testing.T) {
	day := int64(86400)
	now := suggestTestNow
	f := &organizeFakeStore{withLinks: true, items: []map[string]any{
		oItem("critical", "inbox", 2, map[string]any{"severity": "Critical"}),
		oItem("high-sev", "inbox", 3, map[string]any{"severity": "High"}),
		oItem("overdue-week", "backlog", 2, map[string]any{"due_at": now - 7*day}),
		oItem("overdue-short", "backlog", 3, map[string]any{"due_at": now - 3*day}),
		oItem("overdue-already", "inbox", 1, map[string]any{"due_at": now - 3*day}),
		oItem("due-soon", "ready", 2, map[string]any{"due_at": now + day}),
		oItem("blocker", "inbox", 2, map[string]any{"links": []map[string]string{{"kind": "blocks", "item_id": "other"}}}),
		oItem("other", "inbox", 2, nil),
		oItem("stale", "inbox", 0, map[string]any{"updated_at": now - 90*day}),
		oItem("stale-high", "backlog", 1, map[string]any{"updated_at": now - 100*day}),
		oItem("stale-young", "inbox", 0, map[string]any{"updated_at": now - 89*day}),
		oItem("stale-medium", "inbox", 0, map[string]any{"severity": "medium", "updated_at": now - 120*day}),
		oItem("stale-doing", "in_progress", 0, map[string]any{"updated_at": now - 200*day}),
		oItem("stale-review", "review", 0, map[string]any{"updated_at": now - 200*day}),
		oItem("already", "inbox", 0, map[string]any{"severity": "critical"}),
		oItem("done-target", "done", 2, map[string]any{"completed_at": int64(5)}),
	}}
	res := organizeRun(t, f, `{"request_id":"q"}`)
	if res.Method != "deterministic" || res.RequestID != "q" || res.OrderNote != organizeOrderNote || !slices.Equal(res.Differs, organizeDiffers) {
		t.Fatalf("wire contract: %+v", res)
	}
	want := map[string]struct {
		from, to int
		rule     string
	}{
		"critical":      {2, 0, "severity_critical"},
		"high-sev":      {3, 1, "severity_high"},
		"overdue-week":  {2, 0, "overdue"},
		"overdue-short": {3, 1, "overdue"},
		"due-soon":      {2, 1, "due_soon"},
		"blocker":       {2, 1, "blocks_work"},
		"stale":         {0, 1, "stale_high"},
		"stale-high":    {1, 2, "stale_high"},
	}
	if len(res.Changes) != len(want) {
		t.Fatalf("changes = %d, want %d (%v)", len(res.Changes), len(want), changeIDs(res))
	}
	for id, w := range want {
		got, ok := changeByID(res, id)
		if !ok || got.From != w.from || got.To != w.to || got.Rule != w.rule || got.Revision != 1 || got.Reason == "" {
			t.Fatalf("%s = %+v, want %+v", id, got, w)
		}
	}
	week, _ := changeByID(res, "overdue-week")
	if !strings.Contains(week.Reason, "7 days") {
		t.Fatalf("overdue reason = %q", week.Reason)
	}
	short, _ := changeByID(res, "overdue-short")
	if !strings.Contains(short.Reason, "3 days") {
		t.Fatalf("short overdue reason = %q", short.Reason)
	}
	stale, _ := changeByID(res, "stale")
	if !strings.Contains(stale.Reason, "untouched 90 days with no due date, severity, or blocked work") {
		t.Fatalf("stale reason = %q", stale.Reason)
	}
	for _, id := range []string{"overdue-already", "other", "stale-young", "stale-medium", "stale-doing", "stale-review", "already", "done-target"} {
		if _, ok := changeByID(res, id); ok {
			t.Fatalf("%s was proposed and should not have been", id)
		}
	}
	// Critical wins over a later rule: the same task is not also overdue.
	if c, _ := changeByID(res, "critical"); c.Rule != "severity_critical" {
		t.Fatalf("first rule did not win: %+v", c)
	}
	if f.puts != 0 || f.evidence != 0 {
		t.Fatalf("writes %d evidence %d", f.puts, f.evidence)
	}
	for _, method := range f.methods {
		if method != "items.list" && method != "items.get" {
			t.Fatalf("unexpected method %s", method)
		}
	}
}

func changeIDs(res OrganizeResult) []string {
	ids := make([]string, len(res.Changes))
	for i, c := range res.Changes {
		ids[i] = c.ID
	}
	return ids
}

func TestOrganizeColumnOrderUsesTheProposedPriority(t *testing.T) {
	day := int64(86400)
	f := &organizeFakeStore{withLinks: true, items: []map[string]any{
		oItem("low", "inbox", 3, nil),
		oItem("soon", "inbox", 3, map[string]any{"due_at": suggestTestNow + day}),
		oItem("later", "inbox", 2, map[string]any{"due_at": suggestTestNow + 5*day}),
		oItem("plain", "inbox", 2, nil),
	}}
	res := organizeRun(t, f, `{}`)
	if len(res.Order) != 1 || res.Order[0].Status != "inbox" {
		t.Fatalf("order = %+v", res.Order)
	}
	// soon becomes high (due within 2 days); later and plain stay normal, later due first; low last.
	if got := res.Order[0].IDs; !slices.Equal(got, []string{"soon", "later", "plain", "low"}) {
		t.Fatalf("order = %v", got)
	}
}

func TestOrganizeGroupsDuplicatesWithoutMerging(t *testing.T) {
	f := &organizeFakeStore{withLinks: true, items: []map[string]any{
		oItem("a", "inbox", 2, map[string]any{"links": []map[string]string{{"kind": "duplicate_of", "item_id": "b"}}}),
		oItem("b", "inbox", 2, nil),
		oItem("c", "backlog", 2, map[string]any{"links": []map[string]string{{"kind": "duplicate_of", "item_id": "d"}}}),
		oItem("d", "backlog", 2, map[string]any{"links": []map[string]string{{"kind": "duplicate_of", "item_id": "c"}}}),
		oItem("alone", "inbox", 2, nil),
	}}
	res := organizeRun(t, f, `{}`)
	if len(res.Groups) != 2 {
		t.Fatalf("groups = %+v", res.Groups)
	}
	if res.Groups[0].Kind != "duplicate_of" || res.Groups[0].Keep != "b" || !slices.Equal(res.Groups[0].Members, []string{"a", "b"}) {
		t.Fatalf("chain group = %+v", res.Groups[0])
	}
	// A cycle keeps the lowest id. Groups are ordered by keep.
	if res.Groups[1].Keep != "c" || !slices.Equal(res.Groups[1].Members, []string{"c", "d"}) {
		t.Fatalf("cycle group = %+v", res.Groups[1])
	}
	if f.puts != 0 {
		t.Fatal("grouping wrote")
	}
}

func TestOrganizeCoversFiveHundred(t *testing.T) {
	items := make([]map[string]any, 500)
	for i := range items {
		id := fmt.Sprintf("t%04d", i)
		extra := map[string]any{}
		if i == 0 {
			extra["severity"] = "critical"
		}
		items[i] = oItem(id, "inbox", 2, extra)
	}
	f := &organizeFakeStore{withLinks: true, items: items}
	res := organizeRun(t, f, `{}`)
	if res.Coverage.Read != 500 || res.Coverage.Unread != 0 || !res.Coverage.Complete || res.Coverage.Unhydrated != 0 {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
	if len(res.Order) != 1 || len(res.Order[0].IDs) != 500 || res.Order[0].IDs[0] != "t0000" {
		t.Fatalf("order len %d first %v", len(res.Order[0].IDs), res.Order[0].IDs[0])
	}
	if len(res.Changes) != 1 || res.Changes[0].ID != "t0000" || res.Changes[0].To != 0 {
		t.Fatalf("changes = %+v", res.Changes)
	}
	if f.puts != 0 || f.evidence != 0 || f.listLimit != organizePageSize {
		t.Fatalf("puts %d evidence %d limit %d", f.puts, f.evidence, f.listLimit)
	}
}

func TestOrganizeReportsExactUnreadPastTheCap(t *testing.T) {
	items := make([]map[string]any, 2500)
	for i := range items {
		items[i] = oItem(fmt.Sprintf("t%04d", i), "inbox", 2, nil)
	}
	f := &organizeFakeStore{withLinks: true, items: items}
	res := organizeRun(t, f, `{}`)
	if res.Coverage.Read != 2000 || res.Coverage.Unread != 500 || res.Coverage.Complete {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
	if f.puts != 0 || f.evidence != 0 {
		t.Fatalf("puts %d evidence %d", f.puts, f.evidence)
	}
}

func TestOrganizeFailsWhenACutReadHasNoTotal(t *testing.T) {
	items := make([]map[string]any, 2001)
	for i := range items {
		items[i] = oItem(fmt.Sprintf("t%04d", i), "inbox", 2, nil)
	}
	err := organizeErr(t, &organizeFakeStore{withLinks: true, omitTotal: true, items: items})
	var refusal *store.WorkbenchError
	if !errors.As(err, &refusal) || refusal.Code != "unreadable_remainder" || !strings.Contains(refusal.Message, "did not say how many") {
		t.Fatalf("err = %v", err)
	}
}

func TestOrganizeFailsWhenTotalIsBelowTheRowsRead(t *testing.T) {
	total := 1
	err := organizeErr(t, &organizeFakeStore{withLinks: true, totalOverride: &total, items: []map[string]any{
		oItem("a", "inbox", 2, nil),
		oItem("b", "inbox", 2, nil),
	}})
	var refusal *store.WorkbenchError
	if !errors.As(err, &refusal) || refusal.Code != "unreadable_remainder" || !strings.Contains(refusal.Message, "less than") {
		t.Fatalf("err = %v", err)
	}
}

func TestOrganizeUnreadWhenTheListEndsShortOfItsTotal(t *testing.T) {
	total := 10
	f := &organizeFakeStore{withLinks: true, totalOverride: &total, items: []map[string]any{
		oItem("a", "inbox", 2, nil),
		oItem("b", "inbox", 2, nil),
		oItem("c", "inbox", 2, nil),
	}}
	res := organizeRun(t, f, `{}`)
	if res.Coverage.Read != 3 || res.Coverage.Unread != 7 || res.Coverage.Complete {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
}

func TestOrganizeAllowsAFinishedReadWithoutTotal(t *testing.T) {
	f := &organizeFakeStore{withLinks: true, omitTotal: true, items: []map[string]any{
		oItem("a", "inbox", 2, nil),
	}}
	res := organizeRun(t, f, `{}`)
	if res.Coverage.Read != 1 || res.Coverage.Unread != 0 || !res.Coverage.Complete {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
}

func TestOrganizeStuckCursorUsesTotal(t *testing.T) {
	total := 5
	f := &organizeFakeStore{withLinks: true, stuck: true, totalOverride: &total, items: []map[string]any{
		oItem("a", "inbox", 2, nil),
		oItem("b", "inbox", 2, nil),
	}}
	res := organizeRun(t, f, `{}`)
	if res.Coverage.Read != 2 || res.Coverage.Unread != 3 || res.Coverage.Complete {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
}

func TestOrganizeBareCutWithoutTotalIsAnError(t *testing.T) {
	err := organizeErr(t, &organizeFakeStore{withLinks: true, bareCut: true, omitTotal: true, items: []map[string]any{
		oItem("a", "inbox", 2, nil),
	}})
	var refusal *store.WorkbenchError
	if !errors.As(err, &refusal) || refusal.Code != "unreadable_remainder" {
		t.Fatalf("err = %v", err)
	}
}

func TestOrganizeHydrateCapLeavesTheRestUnjudged(t *testing.T) {
	prev := organizeMaxHydrate
	organizeMaxHydrate = 1
	t.Cleanup(func() { organizeMaxHydrate = prev })
	f := &organizeFakeStore{items: []map[string]any{
		oItem("blocker", "inbox", 2, map[string]any{"links": []map[string]string{{"kind": "blocks", "item_id": "other"}}}),
		oItem("second", "inbox", 2, nil),
		oItem("other", "inbox", 2, nil),
	}}
	res := organizeRun(t, f, `{}`)
	if res.Coverage.Unhydrated != 2 || res.Coverage.Complete {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
	got, ok := changeByID(res, "blocker")
	if !ok || got.Rule != "blocks_work" || got.To != 1 {
		t.Fatalf("hydrated change = %+v", got)
	}
	if _, ok := changeByID(res, "second"); ok {
		t.Fatal("judged a task whose links were not read as if they were known")
	}
}

func TestOrganizeCopiesRevisionFromGet(t *testing.T) {
	f := &organizeFakeStore{hideRevision: true, items: []map[string]any{
		oItem("crit", "inbox", 2, map[string]any{"severity": "critical", "revision": 7}),
	}}
	res := organizeRun(t, f, `{}`)
	got, ok := changeByID(res, "crit")
	if !ok || got.Revision != 7 || got.To != 0 {
		t.Fatalf("change = %+v ok %v", got, ok)
	}
}

func TestOrganizeLeavesAnUnrevisionedTaskOut(t *testing.T) {
	f := &organizeFakeStore{withLinks: true, items: []map[string]any{
		oItem("none", "inbox", 2, map[string]any{"severity": "critical", "revision": 0}),
		oItem("kept", "inbox", 2, map[string]any{"severity": "critical", "revision": 4}),
	}}
	res := organizeRun(t, f, `{}`)
	if _, ok := changeByID(res, "none"); ok {
		t.Fatal("proposed a change with no revision")
	}
	got, ok := changeByID(res, "kept")
	if !ok || got.Revision != 4 || got.From != 2 || got.To != 0 {
		t.Fatalf("kept = %+v", got)
	}
	// The board was read in full. The missing revision is counted apart from unread.
	if res.Coverage.Unrevisioned != 1 || !res.Coverage.Complete || res.Coverage.Unread != 0 {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
}

func TestOrganizeRepositoryFilterDoesNotHideBlockers(t *testing.T) {
	items := make([]map[string]any, 0, 2500)
	items = append(items, oItem("mine", "inbox", 2, map[string]any{
		"links": []map[string]string{{"kind": "blocks", "item_id": "theirs"}},
	}))
	items = append(items, oItem("theirs", "inbox", 2, map[string]any{
		"repository_ids": []string{"r2"}, "primary_repository_id": "r2",
		"severity": "critical",
	}))
	for i := 0; i < 2498; i++ {
		items = append(items, oItem(fmt.Sprintf("other%04d", i), "inbox", 2, map[string]any{
			"repository_ids": []string{"r2"}, "primary_repository_id": "r2",
		}))
	}
	f := &organizeFakeStore{withLinks: true, items: items}
	res := organizeRun(t, f, `{"repository_id":"r1"}`)
	if res.Coverage.Unread != 500 || res.Coverage.Read != 1 || res.Coverage.Complete {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
	if !strings.Contains(res.Coverage.Note, "whole board") {
		t.Fatalf("note = %q", res.Coverage.Note)
	}
	if len(res.Changes) != 1 || res.Changes[0].ID != "mine" || res.Changes[0].Rule != "blocks_work" {
		t.Fatalf("changes = %+v", res.Changes)
	}
	if len(res.Order) != 1 || !slices.Equal(res.Order[0].IDs, []string{"mine"}) {
		t.Fatalf("order = %+v", res.Order)
	}
}

func TestOrganizeRefusesUnknownFieldsAndLongIDs(t *testing.T) {
	f := &organizeFakeStore{withLinks: true}
	suggestNow = func() time.Time { return time.Unix(suggestTestNow, 0) }
	t.Cleanup(func() { suggestNow = time.Now })
	if _, err := Organize(context.Background(), f, json.RawMessage(`{"limit":1}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	long := strings.Repeat("x", 129)
	if _, err := Organize(context.Background(), f, json.RawMessage(`{"request_id":"`+long+`"}`)); err == nil {
		t.Fatal("long request id accepted")
	}
}

func TestOrganizeEmptyBoardIsAnEmptyProposal(t *testing.T) {
	res := organizeRun(t, &organizeFakeStore{withLinks: true}, `{}`)
	if res.Changes == nil || res.Order == nil || res.Groups == nil || len(res.Changes) != 0 || !res.Coverage.Complete {
		t.Fatalf("empty = %+v", res)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesContain(raw, `"changes":[]`) || !bytesContain(raw, `"groups":[]`) || bytesContain(raw, "null") {
		t.Fatalf("encoding = %s", raw)
	}
}

func bytesContain(raw []byte, part string) bool {
	return strings.Contains(string(raw), part)
}

func TestOrganizeLaneIsWork(t *testing.T) {
	if laneFor(OpTasksOrganize) != "work" {
		t.Fatalf("lane = %q", laneFor(OpTasksOrganize))
	}
}

func TestOrganizeArchivedRowsAreReadNotUnread(t *testing.T) {
	f := &organizeFakeStore{withLinks: true, items: []map[string]any{
		oItem("open", "inbox", 2, nil),
		oItem("old", "inbox", 0, map[string]any{"archived": true}),
	}}
	res := organizeRun(t, f, `{}`)
	if res.Coverage.Read != 1 || res.Coverage.Unread != 0 || !res.Coverage.Complete {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
}

func TestOrganizeDoesNotLowerWhenLinksWereNotRead(t *testing.T) {
	prev := organizeMaxHydrate
	organizeMaxHydrate = 0
	t.Cleanup(func() { organizeMaxHydrate = prev })
	f := &organizeFakeStore{items: []map[string]any{
		oItem("raise", "inbox", 2, map[string]any{"severity": "critical"}),
		oItem("stale", "inbox", 0, map[string]any{"updated_at": suggestTestNow - 100*86400}),
	}}
	res := organizeRun(t, f, `{}`)
	// Severity does not need links, so the raise still happens. The lower does not.
	if len(res.Changes) != 1 || res.Changes[0].ID != "raise" || res.Changes[0].Rule != "severity_critical" {
		t.Fatalf("changes = %+v", res.Changes)
	}
	if res.Coverage.Unhydrated != 2 {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
}

func TestOrganizeSameBoardIsTheSameProposal(t *testing.T) {
	day := int64(86400)
	items := []map[string]any{
		oItem("b", "inbox", 2, map[string]any{"due_at": suggestTestNow - 8*day}),
		oItem("a", "ready", 3, map[string]any{"severity": "high"}),
	}
	first, err := json.Marshal(organizeRun(t, &organizeFakeStore{withLinks: true, items: items}, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(items)
	second, err := json.Marshal(organizeRun(t, &organizeFakeStore{withLinks: true, items: items}, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("not reproducible\n%s\n%s", first, second)
	}
}
