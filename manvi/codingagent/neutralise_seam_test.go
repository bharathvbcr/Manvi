package codingagent

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// ft-bf52b840: every agent-authored string leaves codingagent through one seam,
// neutraliseEvent, applied at each adapter's Next. These tests fail if an event
// can reach a host without it.

// hostile is agent text carrying a terminal escape, a bidi override, a line
// separator, a soft hyphen and a Unicode tag character, as raw runes.
const hostile = "\x1b[2J\u202e\u2028\u00ad\U000E0041"

// hostileJSON is the same bytes as a JSON string body (ESC cannot be raw in JSON).
const hostileJSON = `\u001b[2J` + "\u202e\u2028\u00ad\U000E0041"

// rawRune reports the first rune in s that neutralisation must have removed.
func rawRune(s string) (rune, bool) {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if mustNeutralise(r, n) && (r != '\r' || !crlfAt(s, i)) {
			return r, true
		}
		i += n
	}
	return 0, false
}

func requireEventClean(t *testing.T, where string, e Event) {
	t.Helper()
	check := func(field, v string) {
		if r, bad := rawRune(v); bad {
			t.Errorf("%s: %s reaches the host with raw %U: %q", where, field, r, v)
		}
	}
	check("Text", e.Text)
	check("Status", e.Status)
	for _, tool := range e.Tools {
		check("Tool.ID", tool.ID)
		check("Tool.Name", tool.Name)
		check("Tool.Status", tool.Status)
		check("Tool.ParentID", tool.ParentID)
		check("Tool.Output", tool.Output)
	}
}

// Every Next method returns each event through neutraliseEvent, and no other
// exported function or method hands an Event to a caller. A new adapter whose
// Next skips the seam, or a new exported event source, fails here. This is the
// structural check; the behavioural one below drives real frames through Next.
func TestNeutraliseSeamEveryEventExitIsNeutraliseEvent(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, entry := range entries {
		if name := entry.Name(); strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			file, err := parser.ParseFile(fset, name, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, file)
		}
	}
	returnsEvent := func(fn *ast.FuncDecl) bool {
		if fn.Type.Results == nil {
			return false
		}
		for _, f := range fn.Type.Results.List {
			switch x := f.Type.(type) {
			case *ast.Ident:
				if x.Name == "Event" {
					return true
				}
			case *ast.ArrayType:
				if id, ok := x.Elt.(*ast.Ident); ok && id.Name == "Event" {
					return true
				}
			case *ast.StarExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "Event" {
					return true
				}
			}
		}
		return false
	}
	nexts := 0
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !returnsEvent(fn) || !fn.Name.IsExported() {
				continue
			}
			if fn.Recv == nil || fn.Name.Name != "Next" {
				t.Errorf("%s returns an Event to callers but is not an adapter Next: a new exit must go through neutraliseEvent and be added to this test", fn.Name.Name)
				continue
			}
			nexts++
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				ret, ok := n.(*ast.ReturnStmt)
				if !ok {
					return true
				}
				if !callsNeutraliseEvent(ret.Results[0]) {
					t.Errorf("%s: Next returns at %s without neutraliseEvent", fn.Name.Name, fset.Position(ret.Pos()))
				}
				return true
			})
		}
	}
	if nexts != 3 {
		t.Errorf("found %d adapter Next methods, want 3 (agy, claude, codex): update this test with the new adapter", nexts)
	}
}

func callsNeutraliseEvent(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == "neutraliseEvent"
}

type seamAdapter struct {
	name   string
	build  func(t *testing.T) (frames chan wireRead, next func() (Event, error))
	frames []wireRead
}

func jsonFrame(method, params string) wireRead {
	return wireRead{value: packet{Method: method, Params: json.RawMessage(params)}}
}

func seamAdapters() []seamAdapter {
	return []seamAdapter{
		{
			name: "agy",
			build: func(t *testing.T) (chan wireRead, func() (Event, error)) {
				s := advAgyIdle(t)
				s.emittedTurnStart = true
				return s.wire.frames, func() (Event, error) { return s.Next(context.Background()) }
			},
			frames: []wireRead{
				jsonFrame("session/update", `{"sessionId":"S","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"`+hostileJSON+`"}}}`),
				jsonFrame("session/update", `{"sessionId":"S","update":{"sessionUpdate":"tool_call","toolCallId":"id`+hostileJSON+`","name":"n`+hostileJSON+`","status":"s`+hostileJSON+`","rawInput":{"command":"`+hostileJSON+`"},"rawOutput":"out `+hostileJSON+`"}}`),
			},
		},
		{
			name: "claude",
			build: func(t *testing.T) (chan wireRead, func() (Event, error)) {
				s := w18aClaude(t)
				return s.wire.frames, func() (Event, error) { return s.Next(context.Background()) }
			},
			frames: []wireRead{
				{raw: []byte(`{"type":"assistant","session_id":"S","message":{"id":"m1","content":[{"type":"text","text":"` + hostileJSON + `"},{"type":"tool_use","id":"t` + hostileJSON + `","name":"n` + hostileJSON + `","parent_tool_use_id":"p` + hostileJSON + `","input":{"command":"` + hostileJSON + `"}}]}}`)},
				{raw: []byte(`{"type":"user","session_id":"S","message":{"content":[{"type":"tool_result","tool_use_id":"t` + hostileJSON + `","content":"out ` + hostileJSON + `","is_error":true}]}}`)},
				{raw: []byte(`{"type":"system","subtype":"permission_denied","session_id":"S","tool_name":"Bash` + hostileJSON + `","message":"` + hostileJSON + `"}`)},
				{raw: []byte(`{"type":"result","subtype":"error","is_error":true,"session_id":"S","result":"` + hostileJSON + `"}`)},
			},
		},
		{
			name: "codex",
			build: func(t *testing.T) (chan wireRead, func() (Event, error)) {
				s := w18Codex(t)
				return s.wire.frames, func() (Event, error) { return s.Next(context.Background()) }
			},
			frames: []wireRead{
				jsonFrame("item/started", `{"threadId":"thread","turnId":"turn","item":{"id":"i`+hostileJSON+`","type":"t`+hostileJSON+`","status":"s`+hostileJSON+`"}}`),
				jsonFrame("item/completed", `{"threadId":"thread","turnId":"turn","item":{"id":"m","type":"agentMessage","text":"`+hostileJSON+`"}}`),
				jsonFrame("item/commandExecution/outputDelta", `{"threadId":"thread","turnId":"turn","delta":"`+hostileJSON+`"}`),
			},
		},
	}
}

// Hostile bytes in every field-producing frame kind of every adapter come out of
// Next neutralised and visible, and Tool.Input is the one raw field (documented
// on neutraliseEvent; serve renders it through NeutraliseToolInput).
func TestNeutraliseSeamNextNeutralisesEveryFrameKind(t *testing.T) {
	for _, a := range seamAdapters() {
		t.Run(a.name, func(t *testing.T) {
			frames, next := a.build(t)
			for _, f := range a.frames {
				frames <- f
			}
			var sawText, sawTool bool
			for range a.frames {
				e, err := next()
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				requireEventClean(t, a.name, e)
				if strings.Contains(e.Text, `\x1b[2J\u{202E}\u{2028}\u{00AD}\u{E0041}`) {
					sawText = true
				}
				for _, tool := range e.Tools {
					sawTool = true
					if !strings.Contains(tool.ID, `\u{E0041}`) {
						t.Errorf("Tool.ID %q does not show the tag character", tool.ID)
					}
					if tool.Input != "" {
						if _, bad := rawRune(tool.Input); !bad {
							t.Errorf("Tool.Input %q was rewritten in the Event; it must stay raw", tool.Input)
						}
						if r, bad := rawRune(NeutraliseToolInput(tool.Input)); bad {
							t.Errorf("rendered Tool.Input keeps raw %U: %q", r, NeutraliseToolInput(tool.Input))
						}
					}
				}
			}
			if !sawText || !sawTool {
				t.Errorf("did not observe both a neutralised text (%v) and tool (%v) event", sawText, sawTool)
			}
		})
	}
}

// A turn end's deferred status and withdrawals, queued before Next is called,
// leave through the same seam.
func TestNeutraliseSeamDeferredEventsAreNeutralisedAtNext(t *testing.T) {
	s := w18aClaude(t)
	s.deferred = []Event{{Status: hostile, Text: hostile}}
	e, err := s.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requireEventClean(t, "deferred", e)
	if e.Status != `\x1b[2J\u{202E}\u{2028}\u{00AD}\u{E0041}` {
		t.Fatalf("deferred Status = %q", e.Status)
	}
}

// Request.Payload and Digest stay raw, so an approval whose payload carries a
// bidi rune and an ESC escape still validates and delivers: the digest
// semantics are unchanged by the seam.
func TestNeutraliseSeamApprovalWithHostilePayloadStillValidatesAndDelivers(t *testing.T) {
	s := w18aClaude(t)
	s.wire.frames <- wireRead{raw: []byte(`{"type":"control_request","request_id":"r1","session_id":"S","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls ` + hostileJSON + `"}}}`)}
	e, err := s.Next(context.Background())
	if err != nil || e.Request == nil {
		t.Fatalf("Next: %+v %v", e, err)
	}
	req := *e.Request
	if !strings.Contains(req.Payload, "\u202e") || !strings.Contains(req.Payload, `\u001b`) || strings.Contains(req.Payload, `\u{`) {
		t.Fatalf("Payload was rewritten: %q", req.Payload)
	}
	if req.Digest != payloadDigest(req.Payload) {
		t.Fatal("digest no longer matches the payload the host received")
	}
	if !json.Valid([]byte(req.Payload)) {
		t.Fatalf("Payload is not valid JSON: %q", req.Payload)
	}
	if err := s.ValidateResponse(req, Response{Decision: "allow_once"}); err != nil {
		t.Fatalf("ValidateResponse: %v", err)
	}
	retained := s.pending[req.ID]
	if retained == nil {
		t.Fatal("the adapter did not retain the callback")
	}
	if callbackStale(req, retained.source, false, false) {
		t.Fatal("approval is stale against the adapter's retained copy")
	}
	// Delivery: Respond writes exactly one control_response naming the provider's
	// request id, and a second answer to the same callback is refused.
	sink := &w18aRecorder{}
	s.wire.stdin = sink
	if err := s.Respond(context.Background(), req, Response{Decision: "allow_once"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if sink.writes() != 1 || !strings.Contains(sink.data[0], `"request_id":"r1"`) || !strings.Contains(sink.data[0], `"behavior":"allow"`) {
		t.Fatalf("delivered frames = %q", sink.data)
	}
	if err := s.Respond(context.Background(), req, Response{Decision: "allow_once"}); err == nil {
		t.Fatal("a second answer to the same callback was accepted")
	}
}

// Tool.Input rendered for a record keeps a JSON document parseable and equal in
// meaning, and shows what it neutralised.
func TestNeutraliseToolInputKeepsJSONValid(t *testing.T) {
	in := `{"command":"ls ` + hostileJSON + ` \"q\" C:\\dir","n":1}`
	if !json.Valid([]byte(in)) {
		t.Fatal("fixture is not valid JSON")
	}
	out := NeutraliseToolInput(in)
	if r, bad := rawRune(out); bad {
		t.Fatalf("raw %U left in %q", r, out)
	}
	var want, got map[string]any
	if json.Unmarshal([]byte(in), &want) != nil || json.Unmarshal([]byte(out), &got) != nil {
		t.Fatalf("not parseable: %q", out)
	}
	if got["n"] != want["n"] || !strings.Contains(fmt.Sprint(got["command"]), `C:\dir`) {
		t.Fatalf("meaning changed: %v vs %v", got, want)
	}
	if strings.Contains(out, "\u202e") || !strings.Contains(out, `\u202e`) {
		t.Fatalf("bidi not shown as a JSON escape: %q", out)
	}
	// A cut document or a string input is neutralised as text.
	if got := NeutraliseToolInput("ls \x1b[2J \\x1b"); got != `ls \x1b[2J \\x1b` {
		t.Fatalf("text input = %q", got)
	}
}
