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

const suggestTestNow = int64(1_800_000_000)

// suggestFakeStore serves items.list by status, items.get, and evidence.
type suggestFakeStore struct {
	items       []map[string]any // as items.list returns them
	withLinks   bool             // list cards carry links
	pageSize    int              // 0 means one page
	evidence    map[string][]store.EvidenceRow
	evidenceErr error
	getCalls    int
	listErr     error
	evidenceHit []string
}

func (f *suggestFakeStore) Workbench(_ context.Context, method string, input json.RawMessage) (json.RawMessage, error) {
	var in struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Cursor string `json:"cursor"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	switch method {
	case "items.get":
		f.getCalls++
		for _, it := range f.items {
			if it["id"] == in.ID {
				out, _ := json.Marshal(map[string]any{"item": it})
				return out, nil
			}
		}
		return nil, errors.New("not found")
	case "items.list":
		if f.listErr != nil {
			return nil, f.listErr
		}
		var column []map[string]any
		for _, it := range f.items {
			if it["status"] == in.Status {
				c := map[string]any{}
				for k, v := range it {
					if k == "links" && !f.withLinks {
						continue
					}
					c[k] = v
				}
				column = append(column, c)
			}
		}
		start := 0
		if in.Cursor != "" {
			fmt.Sscanf(in.Cursor, "%d", &start)
		}
		end, more := len(column), false
		if f.pageSize > 0 && start+f.pageSize < len(column) {
			end, more = start+f.pageSize, true
		}
		res := map[string]any{"items": column[start:end], "has_more": more}
		if more {
			res["next_cursor"] = fmt.Sprint(end)
		}
		out, _ := json.Marshal(res)
		return out, nil
	}
	return nil, errors.New("unexpected " + method)
}

func (f *suggestFakeStore) EvidenceList(_ context.Context, id string) ([]store.EvidenceRow, bool, error) {
	f.evidenceHit = append(f.evidenceHit, id)
	return f.evidence[id], false, f.evidenceErr
}

func sItem(id, status string, priority int, extra map[string]any) map[string]any {
	it := map[string]any{"id": id, "title": "task " + id, "status": status, "priority": priority,
		"updated_at": suggestTestNow, "labels": []string{}, "links": []map[string]string{},
		"repository_ids": []string{"r1"}, "primary_repository_id": "r1", "archived": false, "completed_at": nil}
	for k, v := range extra {
		it[k] = v
	}
	return it
}

func suggestRun(t *testing.T, f *suggestFakeStore, params string) SuggestResult {
	t.Helper()
	suggestNow = func() time.Time { return time.Unix(suggestTestNow, 0) }
	t.Cleanup(func() { suggestNow = time.Now })
	res, err := Suggest(context.Background(), f, json.RawMessage(params))
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	return res
}

func suggestIDs(r SuggestResult) []string {
	var ids []string
	for _, s := range r.Suggestions {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestSuggestRanksPriorityThenDueThenSeverity(t *testing.T) {
	day := int64(86400)
	f := &suggestFakeStore{withLinks: true, items: []map[string]any{
		sItem("low", "inbox", 3, nil),
		sItem("normal", "inbox", 2, nil),
		sItem("high", "backlog", 1, nil),
		sItem("urgent", "ready", 0, nil),
		sItem("normal-overdue", "inbox", 2, map[string]any{"due_at": suggestTestNow - 3*day}),
		sItem("normal-critical", "inbox", 2, map[string]any{"severity": "Critical"}),
	}}
	res := suggestRun(t, f, `{"limit":20}`)
	want := []string{"urgent", "normal-overdue", "normal-critical", "high", "normal", "low"}
	if got := suggestIDs(res); !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if res.Suggestions[0].Rank != 1 || res.Suggestions[0].Reason == "" || len(res.Suggestions[0].Signals) == 0 {
		t.Fatalf("first suggestion lacks rank/reason/signals: %+v", res.Suggestions[0])
	}
	if !strings.Contains(res.Suggestions[1].Reason, "overdue by 3 days") {
		t.Fatalf("reason does not name the due signal: %q", res.Suggestions[2].Reason)
	}
}

func TestSuggestTiesAreDeterministicAndOrderIndependent(t *testing.T) {
	var items []map[string]any
	for _, id := range []string{"d", "b", "a", "c"} {
		items = append(items, sItem(id, "inbox", 2, nil))
	}
	first := suggestIDs(suggestRun(t, &suggestFakeStore{withLinks: true, items: items}, `{}`))
	slices.Reverse(items)
	second := suggestIDs(suggestRun(t, &suggestFakeStore{withLinks: true, items: items}, `{}`))
	if !slices.Equal(first, []string{"a", "b", "c", "d"}) || !slices.Equal(first, second) {
		t.Fatalf("ties not broken by id: %v vs %v", first, second)
	}
}

func TestSuggestEarlierDueBreaksTieOnEqualScore(t *testing.T) {
	day := int64(86400)
	f := &suggestFakeStore{withLinks: true, items: []map[string]any{
		sItem("later", "inbox", 2, map[string]any{"due_at": suggestTestNow + 6*day}),
		sItem("sooner", "inbox", 2, map[string]any{"due_at": suggestTestNow + 5*day}),
	}}
	if got := suggestIDs(suggestRun(t, f, `{}`)); !slices.Equal(got, []string{"sooner", "later"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSuggestExcludesDoneArchivedStartedAndReview(t *testing.T) {
	f := &suggestFakeStore{withLinks: true, items: []map[string]any{
		sItem("ok", "inbox", 3, nil),
		sItem("done", "done", 0, map[string]any{"completed_at": 5}),
		sItem("archived", "inbox", 0, map[string]any{"archived": true}),
		sItem("doing", "in_progress", 0, nil),
		sItem("reviewing", "review", 0, nil),
	}}
	res := suggestRun(t, f, `{}`)
	if got := suggestIDs(res); !slices.Equal(got, []string{"ok"}) {
		t.Fatalf("got %v", got)
	}
	if res.Excluded.InProgress != 1 || res.Excluded.Review != 1 {
		t.Fatalf("exclusions not counted: %+v", res.Excluded)
	}
}

func TestSuggestBlockedTaskIsHeldBackAndBlockerIsBoosted(t *testing.T) {
	f := &suggestFakeStore{withLinks: true, items: []map[string]any{
		sItem("blocker", "inbox", 2, map[string]any{"links": []map[string]string{{"kind": "blocks", "item_id": "blocked"}}}),
		sItem("blocked", "inbox", 0, nil),
		sItem("other", "inbox", 2, nil),
	}}
	res := suggestRun(t, f, `{}`)
	if got := suggestIDs(res); !slices.Equal(got, []string{"blocker", "other"}) {
		t.Fatalf("got %v (blocked task must not be suggested; its blocker outranks a normal task)", got)
	}
	if res.Excluded.Blocked != 1 || len(res.Blocked) != 1 || res.Blocked[0].ID != "blocked" || !slices.Equal(res.Blocked[0].BlockedBy, []string{"blocker"}) {
		t.Fatalf("blocked not reported: %+v %+v", res.Excluded, res.Blocked)
	}
	if !strings.Contains(res.Suggestions[0].Reason, "blocks 1 open task") {
		t.Fatalf("reason: %q", res.Suggestions[0].Reason)
	}
}

func TestSuggestFinishedBlockerNoLongerBlocks(t *testing.T) {
	// The blocker is Done, so it is not on the open board: nothing blocks.
	f := &suggestFakeStore{withLinks: true, items: []map[string]any{
		sItem("was-blocked", "inbox", 1, nil),
		sItem("finished", "done", 3, map[string]any{"completed_at": 5, "links": []map[string]string{{"kind": "blocks", "item_id": "was-blocked"}}}),
	}}
	if got := suggestIDs(suggestRun(t, f, `{}`)); !slices.Equal(got, []string{"was-blocked"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSuggestDuplicateOfOpenTaskIsLeftOut(t *testing.T) {
	f := &suggestFakeStore{withLinks: true, items: []map[string]any{
		sItem("orig", "inbox", 2, nil),
		sItem("dupe", "inbox", 0, map[string]any{"links": []map[string]string{{"kind": "duplicate_of", "item_id": "orig"}}}),
	}}
	res := suggestRun(t, f, `{}`)
	if got := suggestIDs(res); !slices.Equal(got, []string{"orig"}) || res.Excluded.Duplicate != 1 {
		t.Fatalf("got %v %+v", got, res.Excluded)
	}
}

func TestSuggestReadsLinksWhenListCardsCarryNone(t *testing.T) {
	f := &suggestFakeStore{withLinks: false, items: []map[string]any{
		sItem("blocker", "inbox", 3, map[string]any{"links": []map[string]string{{"kind": "blocks", "item_id": "blocked"}}}),
		sItem("blocked", "inbox", 0, nil),
	}}
	res := suggestRun(t, f, `{}`)
	if got := suggestIDs(res); !slices.Equal(got, []string{"blocker"}) || f.getCalls != 2 {
		t.Fatalf("got %v after %d reads", got, f.getCalls)
	}
}

func TestSuggestUnreadableLinksFailClosed(t *testing.T) {
	// items.get fails for this card, so a blocker or duplicate cannot be ruled out.
	f := &suggestFakeStore{withLinks: false, items: []map[string]any{sItem("a", "inbox", 0, nil)}}
	f.items[0]["id"] = "a"
	broken := &brokenGetStore{suggestFakeStore: f}
	suggestNow = func() time.Time { return time.Unix(suggestTestNow, 0) }
	t.Cleanup(func() { suggestNow = time.Now })
	res, err := Suggest(context.Background(), broken, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Suggestions) != 0 || res.Board.Unhydrated != 1 || res.Excluded.Blocked != 1 {
		t.Fatalf("an unreadable card must not be suggested: %+v", res)
	}
}

type brokenGetStore struct{ *suggestFakeStore }

func (b *brokenGetStore) Workbench(ctx context.Context, method string, in json.RawMessage) (json.RawMessage, error) {
	if method == "items.get" {
		return nil, errors.New("store down")
	}
	return b.suggestFakeStore.Workbench(ctx, method, in)
}

func TestSuggestFiltersAndLimit(t *testing.T) {
	f := &suggestFakeStore{withLinks: true, items: []map[string]any{
		sItem("a", "inbox", 0, nil),
		sItem("b", "inbox", 1, nil),
		sItem("c", "inbox", 3, nil),
		sItem("other-repo", "inbox", 0, map[string]any{"repository_ids": []string{"r2"}, "primary_repository_id": "r2"}),
	}}
	res := suggestRun(t, f, `{"limit":1}`)
	if len(res.Suggestions) != 1 || res.Candidates != 4 {
		t.Fatalf("limit not applied or candidates miscounted: %+v", res)
	}
	res = suggestRun(t, f, `{"max_priority":1,"repository_id":"r1"}`)
	if got := suggestIDs(res); !slices.Equal(got, []string{"a", "b"}) || res.Excluded.Filtered != 2 {
		t.Fatalf("got %v %+v", got, res.Excluded)
	}
}

func TestSuggestRejectsBadParams(t *testing.T) {
	f := &suggestFakeStore{withLinks: true}
	for _, p := range []string{`{"limit":21}`, `{"limit":-1}`, `{"max_priority":4}`, `{"max_priority":-1}`, `{"bogus":1}`, `{`, `{"request_id":"` + strings.Repeat("x", 129) + `"}`} {
		if _, err := Suggest(context.Background(), f, json.RawMessage(p)); err == nil {
			t.Errorf("%s was accepted", p)
		}
	}
}

func TestSuggestEmptyBoardIsAnEmptyListNotNull(t *testing.T) {
	out, err := json.Marshal(suggestRun(t, &suggestFakeStore{withLinks: true}, ``))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"suggestions":[]`) || !strings.Contains(string(out), `"blocked":[]`) {
		t.Fatalf("empty lists must serialize as arrays: %s", out)
	}
}

func TestSuggestBoardReadFailureIsAnError(t *testing.T) {
	f := &suggestFakeStore{listErr: errors.New("store down")}
	if _, err := Suggest(context.Background(), f, nil); err == nil {
		t.Fatal("a board that cannot be read must not look like an empty board")
	}
}

func TestSuggestPagedBoardIsReadWhole(t *testing.T) {
	f := &suggestFakeStore{withLinks: true, pageSize: 2}
	for i := 0; i < 5; i++ {
		f.items = append(f.items, sItem(fmt.Sprintf("t%d", i), "inbox", 2, nil))
	}
	res := suggestRun(t, f, `{"limit":20}`)
	if res.Board.Scanned != 5 || !res.Board.Complete || len(res.Suggestions) != 5 {
		t.Fatalf("paging lost cards: %+v", res.Board)
	}
}

func TestSuggestVerificationStateMovesTheOrder(t *testing.T) {
	record := func(outcome string) []store.EvidenceRow {
		data, _ := json.Marshal(VerifyRecord{RequestID: "r", ItemID: "x", State: verifySettled, Outcome: outcome})
		return []store.EvidenceRow{{Type: verifyEvidenceType, DataJSON: data}}
	}
	f := &suggestFakeStore{withLinks: true, items: []map[string]any{
		sItem("passed", "ready", 1, nil), sItem("failed", "ready", 1, nil), sItem("plain", "ready", 1, nil),
	}, evidence: map[string][]store.EvidenceRow{"passed": record(VerifyPassed), "failed": record(VerifyFailed)}}
	res := suggestRun(t, f, `{}`)
	if got := suggestIDs(res); !slices.Equal(got, []string{"failed", "plain", "passed"}) {
		t.Fatalf("got %v", got)
	}
	if res.Verification.Checked != 3 || res.Verification.Failed != 1 {
		t.Fatalf("verification coverage: %+v", res.Verification)
	}
}

func TestSuggestVerificationReadsAreBounded(t *testing.T) {
	f := &suggestFakeStore{withLinks: true}
	for i := 0; i < suggestMaxVerifyProbes+8; i++ {
		f.items = append(f.items, sItem(fmt.Sprintf("t%02d", i), "inbox", 2, nil))
	}
	res := suggestRun(t, f, `{"limit":20}`)
	if len(f.evidenceHit) != suggestMaxVerifyProbes || res.Verification.Unchecked != 8 {
		t.Fatalf("probes=%d %+v", len(f.evidenceHit), res.Verification)
	}
}

func TestSuggestVerificationFailureDoesNotHideTheRanking(t *testing.T) {
	f := &suggestFakeStore{withLinks: true, items: []map[string]any{sItem("a", "inbox", 0, nil)}, evidenceErr: errors.New("down")}
	res := suggestRun(t, f, `{}`)
	if len(res.Suggestions) != 1 || res.Verification.Unchecked != 1 || res.Verification.Checked != 0 {
		t.Fatalf("an unreadable record must count as unchecked, not as clean: %+v", res)
	}
}

func TestSuggestScanFindingIsAHint(t *testing.T) {
	f := &suggestFakeStore{withLinks: true, items: []map[string]any{
		sItem("plain", "inbox", 2, nil),
		sItem("scanned", "inbox", 2, map[string]any{"labels": []string{"scan:0123456789abcdef"}}),
	}}
	res := suggestRun(t, f, `{}`)
	if got := suggestIDs(res); !slices.Equal(got, []string{"scanned", "plain"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSuggestCancelledContextIsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &suggestFakeStore{withLinks: false, items: []map[string]any{sItem("a", "inbox", 0, nil)}}
	cf := &cancelStore{suggestFakeStore: f}
	if _, err := Suggest(ctx, cf, nil); err == nil {
		t.Fatal("a cancelled request must not return a ranking")
	}
}

type cancelStore struct{ *suggestFakeStore }

func (c *cancelStore) Workbench(ctx context.Context, m string, in json.RawMessage) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.suggestFakeStore.Workbench(ctx, m, in)
}

func TestSuggestIsServedAsAHostOp(t *testing.T) {
	if laneFor(OpTasksSuggest) != "work" {
		t.Fatalf("lane = %q", laneFor(OpTasksSuggest))
	}
}
