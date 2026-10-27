package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/testsupport"
)

// contractViolations reports every way a JSON value departs from the committed
// serve contract's description of typeName: a null where the contract does not
// mark the field nullable, a wrong type, an absent required field, a field the
// contract does not declare.
func contractViolations(t *testing.T, typeName string, raw []byte) []string {
	t.Helper()
	committed, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	var contract serveContract
	if err := json.Unmarshal(committed, &contract); err != nil {
		t.Fatal(err)
	}
	field, ok := contract.Types[typeName]
	if !ok {
		t.Fatalf("the contract has no type %s", typeName)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	var out []string
	checkContractField(typeName, field, v, &out)
	return out
}

func checkContractField(path string, f *contractField, v any, out *[]string) {
	if f == nil {
		*out = append(*out, path+": the contract describes no such field")
		return
	}
	if v == nil {
		if !f.Nullable {
			*out = append(*out, fmt.Sprintf("%s: null, but the contract does not mark it nullable", path))
		}
		return
	}
	bad := func(want string) { *out = append(*out, fmt.Sprintf("%s: %T, want %s", path, v, want)) }
	switch f.Type {
	case "any":
	case "string":
		if _, ok := v.(string); !ok {
			bad("string")
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			bad("boolean")
		}
	case "number":
		if _, ok := v.(float64); !ok {
			bad("number")
		}
	case "integer":
		if n, ok := v.(float64); !ok || n != math.Trunc(n) {
			bad("integer")
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			bad("array")
			return
		}
		for i, e := range arr {
			checkContractField(fmt.Sprintf("%s[%d]", path, i), f.Items, e, out)
		}
	case "map":
		m, ok := v.(map[string]any)
		if !ok {
			bad("map")
			return
		}
		for k, e := range m {
			checkContractField(path+"."+k, f.Items, e, out)
		}
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			bad("object")
			return
		}
		names := make([]string, 0, len(f.Fields))
		for n := range f.Fields {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			child := f.Fields[n]
			val, present := m[n]
			if !present {
				if child != nil && child.Required {
					*out = append(*out, fmt.Sprintf("%s.%s: required field absent", path, n))
				}
				continue
			}
			checkContractField(path+"."+n, child, val, out)
		}
		for n := range m {
			if _, known := f.Fields[n]; !known {
				*out = append(*out, fmt.Sprintf("%s.%s: field not declared in the contract", path, n))
			}
		}
	default:
		*out = append(*out, fmt.Sprintf("%s: unknown contract type %q", path, f.Type))
	}
}

func requireConforms(t *testing.T, what string, view any) {
	t.Helper()
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range contractViolations(t, "ScanView", raw) {
		t.Errorf("%s: %s (reply %s)", what, p, raw)
	}
}

// Coverage and proposals are required arrays in the contract. ScanView.clone
// used to turn an empty slice into nil, which marshals as null: the claim reply,
// a read while the scan runs, and a finished scan with no proposals all went out
// outside the contract a host validates against.
func TestScanViewsConformToTheContractWhenCoverageOrProposalsAreEmpty(t *testing.T) {
	root := scanRepo(t, map[string]string{"README.md": "x\n"})
	h := newScanHarness(t, 0)
	release := make(chan struct{})
	h.dm.deadWait = func(ctx context.Context) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}
	started := h.start(t, fmt.Sprintf(`{"repo_root":%q}`, root))
	requireConforms(t, "claim reply", started)
	running, e := h.runner.Get(json.RawMessage(fmt.Sprintf(`{"scan_id":%q}`, started.ScanID)))
	if e != nil {
		t.Fatalf("Get: %+v", e)
	}
	requireConforms(t, "read while running", running)
	cancelled, e := h.runner.Cancel(json.RawMessage(fmt.Sprintf(`{"scan_id":%q}`, started.ScanID)))
	if e != nil {
		t.Fatalf("Cancel: %+v", e)
	}
	requireConforms(t, "cancel reply", cancelled)
	close(release)

	// A scan that finishes with nothing to propose.
	h2 := newScanHarness(t, 0)
	finished := h2.scan(t, root, "")
	if len(finished.Proposals) != 0 {
		t.Fatalf("the fixture was meant to find nothing: %+v", finished.Proposals)
	}
	requireConforms(t, "finished scan with no proposals", finished)
}

// Cancel marks a job cancelled while its goroutine may still be inside a call
// that ignores its context (a blocked subprocess read). A request for the same
// root after that used to join the dead job and was handed a cancelled view
// instead of a scan.
func TestScanStartedAfterCancelIsAFreshScanNotTheCancelledJob(t *testing.T) {
	root := scanRepo(t, map[string]string{"README.md": "x\n"})
	h := newScanHarness(t, 0)
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	h.dm.deadWait = func(context.Context) error {
		entered <- struct{}{}
		<-release // ignores the context, like a blocked read
		return nil
	}
	first := h.start(t, fmt.Sprintf(`{"repo_root":%q}`, root))
	<-entered
	if _, e := h.runner.Cancel(json.RawMessage(fmt.Sprintf(`{"scan_id":%q}`, first.ScanID))); e != nil {
		t.Fatalf("Cancel: %+v", e)
	}
	got, e := h.runner.Start(context.Background(), json.RawMessage(fmt.Sprintf(`{"repo_root":%q}`, root)))
	if e != nil {
		t.Fatalf("a scan requested after a cancel was refused: %+v", e)
	}
	second := asScanView(t, got)
	if second.ScanID == first.ScanID || second.Joined || second.State != scanRunning {
		t.Fatalf("the request after a cancel was handed %s joined=%v state=%s (the cancelled scan was %s)", second.ScanID, second.Joined, second.State, first.ScanID)
	}

	// The cancelled scan is still unwinding, so it still counts against the
	// bound on concurrent scans: cancelling and restarting cannot pile up
	// scans stuck in calls that ignore their context.
	other := scanRepo(t, map[string]string{"README.md": "y\n"})
	if _, e := h.runner.Start(context.Background(), json.RawMessage(fmt.Sprintf(`{"repo_root":%q}`, other))); e == nil || e.Code != ErrBusy {
		t.Fatalf("a third scan beside one running and one cancelled but unwinding: %+v", e)
	}

	close(release)
	if v := h.wait(t, second.ScanID); v.State != scanDone {
		t.Fatalf("the fresh scan = %s, want done", v.State)
	}
	if v := h.wait(t, first.ScanID); v.State != scanCancelled {
		t.Fatalf("the cancelled scan = %s, want cancelled", v.State)
	}
	// Once both goroutines have returned nothing is held against the bound. The
	// cancelled scan reads as cancelled before its goroutine has, so wait for the
	// count itself, not for the view.
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.runner.mu.Lock()
		held := len(h.runner.active) + h.runner.stopping
		h.runner.mu.Unlock()
		if held == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d scans are still held against the bound after both ended", held)
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.dm.deadWait = nil
	if v := h.scan(t, other, ""); v.State != scanDone {
		t.Fatalf("a scan after both ended = %s", v.State)
	}
}

// A proposal is "attached" when an open board item already holds it. Done and
// archived cards are not open, and a card is held by its scan:<fp> label: item
// cards omit descriptions, so the manvi-scan: line in a description is a trace
// for people and is never read back.
func TestScanBoardIndexHoldsOnlyOpenCardsAndReadsLabels(t *testing.T) {
	client := store.New(testsupport.DCStore(t), filepath.Join(t.TempDir(), "profile.sqlite"))
	t.Cleanup(client.Close)
	enhancementCall(t, client, "repositories.put", `{"request_id":"r","id":"r","expected_revision":0,"name":"repo","identity_key":"r"}`)
	put := func(id, status string, archived bool, labels string, description string) {
		enhancementCall(t, client, "items.put", fmt.Sprintf(
			`{"request_id":"p-%s","id":%q,"expected_revision":0,"title":"t %s","description":%q,"status":%q,"archived":%v,"labels":%s,"repository_ids":["r"],"primary_repository_id":"r"}`,
			id, id, id, description, status, archived, labels))
	}
	put("open", "backlog", false, `["manvi-scan","scan:00000000000000aa"]`, "")
	put("review", "review", false, `["scan:00000000000000ab"]`, "")
	put("done", "done", false, `["manvi-scan","scan:00000000000000bb"]`, "")
	put("arch", "done", true, `["manvi-scan","scan:00000000000000cc"]`, "")
	put("archopen", "backlog", true, `["scan:00000000000000ce"]`, "")
	put("deleted", "backlog", false, `["scan:00000000000000dd"]`, "")
	enhancementCall(t, client, "items.delete", `{"request_id":"d","id":"deleted","expected_revision":1}`)
	put("marker", "backlog", false, `[]`, "gap text\n\nmanvi-scan: 00000000000000ee")

	r := &ScanRunner{deps: ScanDeps{Board: client}}
	index, complete, err := r.boardIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Error("the board read was reported incomplete")
	}
	for fp, want := range map[string]string{"00000000000000aa": "open", "00000000000000ab": "review"} {
		if got := index[fp].ID; got != want {
			t.Errorf("fingerprint %s is held by %q, want the open card %q", fp, got, want)
		}
	}
	for fp, why := range map[string]string{
		"00000000000000bb": "a Done card is not open",
		"00000000000000cc": "an archived card is not open",
		"00000000000000ce": "an archived card is not open, whatever its status",
		"00000000000000dd": "a deleted card is not on the board",
		"00000000000000ee": "a description marker is never read: cards omit descriptions, so only the label dedupes",
	} {
		if item, held := index[fp]; held {
			t.Errorf("fingerprint %s is held by %q, but %s", fp, item.ID, why)
		}
	}
}
