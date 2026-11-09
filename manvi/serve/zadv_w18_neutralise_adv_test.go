package serve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// Adversary tests for ft-bf52b840 (wave 18, lane N), serve side. A test named
// The former ...Gap... tests asserted the correct behaviour and failed on the
// implementer's first head; they are renamed now that they pass.

// wellFormedNeutralised reports whether every backslash in out starts one whole
// escape of the neutralised form: `\\`, `\xNN` or `\u{HEX}`.
func wellFormedNeutralised(out string) bool {
	isHex := func(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }
	for i := 0; i < len(out); i++ {
		if out[i] != '\\' {
			continue
		}
		switch {
		case i+1 < len(out) && out[i+1] == '\\':
			i++
		case i+3 < len(out) && out[i+1] == 'x' && isHex(out[i+2]) && isHex(out[i+3]):
			i += 3
		case i+2 < len(out) && out[i+1] == 'u' && out[i+2] == '{':
			j := i + 3
			for j < len(out) && isHex(out[j]) {
				j++
			}
			if j == i+3 || j >= len(out) || out[j] != '}' {
				return false
			}
			i = j
		default:
			return false
		}
	}
	return true
}

// The drive loop cuts Event.Text at maxManagedOutput, and it does so after the
// adapter's seam has escaped it, so the cut can land inside an escape. 1 pad byte
// plus 32767 whole `\x1b` escapes is 131069 bytes; the next escape is cut after
// `\x1`. The saved record then ends in a fragment that is not a neutralised form,
// and a `\` at the end is exactly what the doubled-backslash rule says cannot occur.
func TestAdvW18NeutraliseTextCutAtTheOutputCapEndsOnAWholeEscape(t *testing.T) {
	for _, tc := range []struct{ name, unit string }{
		{"control", `\x1b`},
		{"invisible", `\u{202E}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var session *managedTestSession
			m := newManagedLifecycle(t, func(ctx context.Context, options codingagent.Options) *managedTestSession {
				config := codingagent.Effective{Cwd: options.Cwd, ApprovalPolicy: "on-request", ApprovalsReviewer: "user", Sandbox: codingagent.Sandbox{Type: "readOnly"}, Model: "fixture", ModelProvider: "fixture"}
				config.Thread.ID = "thread"
				config.Thread.Cwd = options.Cwd
				session = &managedTestSession{ctx: ctx, config: config, events: make(chan codingagent.Event, 8), leaveOpen: true}
				return session
			})
			m.start("launch")
			if session == nil {
				t.Fatal("the managed runner never started a provider session")
			}
			// Enough whole escapes to pass the cap, with a pad that makes the cap fall
			// inside one: cap-1 is not a multiple of the unit width.
			pad := "p"
			text := pad + strings.Repeat(tc.unit, maxManagedOutput/len(tc.unit)+2)
			session.events <- codingagent.Event{TurnID: "turn", Text: text, Status: "completed"}
			_, raw := m.finished()
			var row struct {
				Item struct {
					Output string `json:"output"`
				} `json:"item"`
			}
			if json.Unmarshal(raw, &row) != nil {
				t.Fatalf("bad run: %s", raw)
			}
			out := row.Item.Output
			if len(out) == 0 {
				var all map[string]any
				_ = json.Unmarshal(raw, &all)
				item, _ := all["item"].(map[string]any)
				delete(item, "brief")
				t.Fatalf("run has no output: %v", item)
			}
			// Cut at the last whole escape at or below the cap: at most one escape short.
			if len(out) > maxManagedOutput || len(out) < maxManagedOutput-len(tc.unit)+1 {
				t.Fatalf("saved output is %d bytes, expected it cut at the cap %d or one whole escape under", len(out), maxManagedOutput)
			}
			if !utf8.ValidString(out) {
				t.Fatalf("saved output is not UTF-8")
			}
			if !wellFormedNeutralised(out) {
				t.Fatalf("saved output ends mid-escape: %q", out[len(out)-8:])
			}
		})
	}
}

// The saved output is written to the store inside one JSON request capped at 256 KiB
// ("request must be a JSON object of at most 256 KiB"), while maxManagedOutput caps
// the unescaped bytes at 128 KiB. A backslash is two bytes after the neutraliser and
// four after the request is JSON-encoded, so 65,536 backslashes in agent text (131072
// neutralised bytes, under the cap) make the protocol write exceed 256 KiB and the
// run is failed with its whole output lost. The same shape exists for any byte JSON
// doubles (newline, quote) at twice the volume, so it is older than the card; the
// neutraliser halves the volume needed for backslashes.
func TestAdvW18NeutraliseBackslashFloodDoesNotLoseTheOutput(t *testing.T) {
	var session *managedTestSession
	m := newManagedLifecycle(t, func(ctx context.Context, options codingagent.Options) *managedTestSession {
		config := codingagent.Effective{Cwd: options.Cwd, ApprovalPolicy: "on-request", ApprovalsReviewer: "user", Sandbox: codingagent.Sandbox{Type: "readOnly"}, Model: "fixture", ModelProvider: "fixture"}
		config.Thread.ID = "thread"
		config.Thread.Cwd = options.Cwd
		session = &managedTestSession{ctx: ctx, config: config, events: make(chan codingagent.Event, 8), leaveOpen: true}
		return session
	})
	m.start("launch")
	if session == nil {
		t.Fatal("the managed runner never started a provider session")
	}
	// What the adapter's seam hands the host for 65,536 agent backslashes.
	session.events <- codingagent.Event{TurnID: "turn", Text: strings.Repeat("\\", maxManagedOutput), Status: "completed"}
	run, raw := m.finished()
	var row struct {
		Item struct {
			Output string `json:"output"`
			Reason string `json:"reason"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &row) != nil {
		t.Fatalf("bad run: %s", raw)
	}
	t.Logf("flood: state %q output %d bytes", run.ProviderState, len(row.Item.Output))
	if run.ProviderState != "completed" || row.Item.Output == "" {
		t.Fatalf("provider state %q, output %d bytes, reason %q", run.ProviderState, len(row.Item.Output), row.Item.Reason)
	}
}

// ---- 1. Double application ------------------------------------------------------

// The snapshot renders Tool.Input and nothing else through the neutraliser; the
// fields the adapter's seam already escaped (ID, Name, Status, ParentID, Output)
// come out of savedOutput as they went in, so a backslash is doubled once, not
// again at the snapshot. And a second savedOutput of the same job is identical.
func TestAdvW18NeutraliseSavedOutputDoesNotEscapeAgainWhatTheSeamEscaped(t *testing.T) {
	job := &managedJob{}
	// What the seam produced for the agent's `a\b`, ESC and a bidi mark.
	job.tools = []codingagent.Tool{{
		ID: `i\\d\x1b`, Name: `n\\\u{202E}`, Status: `s\\`, ParentID: `p\\`, Output: `o\\x1b \x1b`,
		Input: "{\"k\":\"a\\\\b ‮\"}",
	}}
	first, _ := savedOutput(job)
	second, _ := savedOutput(job)
	if first != second {
		t.Fatal("savedOutput is not a pure function of the job: it changed the tool rows")
	}
	_, snapshot, ok := strings.Cut(first, toolsMarker)
	if !ok {
		t.Fatalf("no snapshot in %q", first)
	}
	var tools []codingagent.Tool
	if err := json.Unmarshal([]byte(snapshot), &tools); err != nil || len(tools) != 1 {
		t.Fatalf("snapshot: %v %q", err, snapshot)
	}
	got, want := tools[0], job.tools[0]
	if got.ID != want.ID || got.Name != want.Name || got.Status != want.Status || got.ParentID != want.ParentID || got.Output != want.Output {
		t.Fatalf("the snapshot escaped the seam's output again:\n got %+v\nwant %+v", got, want)
	}
	var in map[string]string
	if err := json.Unmarshal([]byte(got.Input), &in); err != nil || in["k"] != "a\\b ‮" {
		t.Fatalf("Input changed meaning: %v %q", err, got.Input)
	}
	if strings.Contains(got.Input, "‮") {
		t.Fatalf("Input still carries a raw bidi rune: %q", got.Input)
	}
}

// ---- 2. Bounds ------------------------------------------------------------------

// serve no longer re-bounds a tool: the adapters do before the seam. The record's
// own cap still holds against the worst bytes: 64 tools at the escaped limits (4x on
// each string; Input at its 4096 bytes, up to 6/2x again when JSON-marshalled) never
// put more than maxManagedOutput into the saved output, and when they cannot fit the
// snapshot is left out whole and the record marked truncated.
func TestAdvW18NeutraliseWorstCaseToolFloodKeepsTheSavedOutputWithinItsCap(t *testing.T) {
	job := &managedJob{}
	esc := func(n int) string { return strings.Repeat(`\x1b`, n) }
	for i := 0; i < maxManagedTools+5; i++ {
		id := esc(60) + string(rune('a'+i%26)) + string(rune('A'+i/26))
		noteTools(job, []codingagent.Tool{{ID: id, Name: esc(256), Status: esc(256), ParentID: esc(256), Input: strings.Repeat("\x00", 4096), Output: esc(8192)}})
	}
	if len(job.tools) != maxManagedTools || !job.truncated {
		t.Fatalf("tools = %d truncated = %v", len(job.tools), job.truncated)
	}
	job.output.WriteString("text")
	out, truncated := savedOutput(job)
	if len(out) > maxManagedOutput {
		t.Fatalf("saved output is %d bytes, cap %d", len(out), maxManagedOutput)
	}
	if strings.Contains(out, toolsMarker) || !truncated {
		t.Fatalf("a snapshot that cannot fit was written (marker=%v) or not marked truncated (%v)", strings.Contains(out, toolsMarker), truncated)
	}
}

// One hostile tool at the adapter limits does fit, and its snapshot stays valid
// JSON that decodes back to exactly the strings it was given.
func TestAdvW18NeutraliseOneWorstCaseToolFitsAndRoundTrips(t *testing.T) {
	job := &managedJob{}
	esc := func(n int) string { return strings.Repeat(`\x1b`, n) }
	tool := codingagent.Tool{ID: esc(60), Name: esc(60), Status: esc(60), ParentID: esc(60), Input: strings.Repeat("‮", 1365), Output: esc(2048)}
	noteTools(job, []codingagent.Tool{tool})
	out, _ := savedOutput(job)
	_, snapshot, ok := strings.Cut(out, toolsMarker)
	if !ok {
		t.Fatalf("snapshot omitted at %d bytes", len(out))
	}
	var tools []codingagent.Tool
	if err := json.Unmarshal([]byte(snapshot), &tools); err != nil || len(tools) != 1 {
		t.Fatalf("not parseable: %v", err)
	}
	if tools[0].Output != tool.Output || tools[0].ID != tool.ID {
		t.Fatal("round trip changed the strings")
	}
}

// Before the seam moved, noteTools ran codingagent.BoundTool on every tool, so a
// provider that is not one of the three adapters could not put an unbounded tool
// in the job. It now trusts the caller. A provider session is an interface
// (ManagedSession), and the host holds up to maxManagedTools of these in memory for
// the life of the run, so a tool with a 1 MiB output and a 1 MiB id is now kept whole.
func TestAdvW18NeutraliseNoteToolsBoundsAToolFromAnyProvider(t *testing.T) {
	job := &managedJob{}
	huge := strings.Repeat("x", 1<<20)
	noteTools(job, []codingagent.Tool{{ID: "t1", Name: "n", Output: huge, Input: huge}})
	noteTools(job, []codingagent.Tool{{ID: huge, Name: "n"}})
	var total int
	for _, tool := range job.tools {
		total += len(tool.ID) + len(tool.Name) + len(tool.Status) + len(tool.ParentID) + len(tool.Input) + len(tool.Output)
	}
	// 4x the raw limits is what the escape can add to a bounded tool.
	const perTool = 4*(4*256+8192) + 4096
	if total > maxManagedTools*perTool || total > 2*perTool {
		t.Fatalf("job.tools holds %d bytes from two tools, more than two bounded tools can (%d)", total, 2*perTool)
	}
}
