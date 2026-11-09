package codingagent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// Adversary tests for ft-bf52b840 (wave 18, lane N). The former ...Gap... tests
// asserted the correct behaviour and failed until the gap was fixed.

// decodeNeutralised inverts neutraliseText for input without a raw backslash and
// reports the original, or false if out is not a sequence of whole escapes. A
// `\xNN` below 0x80 is read as the rune NN; one at or above 0x80 as the raw byte
// (a C1 rune renders as `\u{0085}`, never `\x85`).
func decodeNeutralised(out string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(out); {
		if out[i] != '\\' {
			b.WriteByte(out[i])
			i++
			continue
		}
		switch {
		case i+1 < len(out) && out[i+1] == '\\':
			b.WriteByte('\\')
			i += 2
		case i+3 < len(out) && out[i+1] == 'x':
			v, err := strconv.ParseUint(out[i+2:i+4], 16, 8)
			if err != nil {
				return "", false
			}
			if v < 0x80 {
				b.WriteRune(rune(v))
			} else {
				b.WriteByte(byte(v))
			}
			i += 4
		case i+2 < len(out) && out[i+1] == 'u' && out[i+2] == '{':
			j := strings.IndexByte(out[i:], '}')
			if j < 0 {
				return "", false
			}
			v, err := strconv.ParseUint(out[i+3:i+j], 16, 32)
			if err != nil {
				return "", false
			}
			b.WriteRune(rune(v))
			i += j + 1
		default:
			return "", false
		}
	}
	return b.String(), true
}

// ---- 7. Injectivity ----------------------------------------------------------

// Two different inputs must not render the same: the invalid byte 0x85 and the C1
// control U+0085 once both came out as `\x85`, so a reader could not tell what the
// agent sent. The byte keeps `\xNN`; the rune is `\u{0085}`.
func TestAdvW18NeutraliseInvalidByteAndC1RuneRenderDifferently(t *testing.T) {
	for c := byte(0x80); c <= 0x9f; c++ {
		invalid := string([]byte{c})
		c1 := string(rune(c))
		if a, b := neutraliseText(invalid), neutraliseText(c1); a == b {
			t.Errorf("invalid byte %#x and rune U+%04X both render %q", c, c, a)
		}
	}
}

// Random strings drawn from the characters that matter decode back to themselves,
// so the rendering loses nothing, including the invalid bytes 0x80..0x9f beside
// the C1 runes U+0080..U+009F.
func TestAdvW18NeutraliseRenderingIsInjectiveOnRandomInput(t *testing.T) {
	alphabet := []string{
		"a", "Z", " ", "\n", "\t", "\r", "\r\n", "\x00", "\x1b", "\x7f", "\\", "\\\\", "x", "1", "b", "u", "{", "}", "<", ">", "+",
		"\u0085", "\u009f", "\u00a0", "\u00ad", "\u061c", "\u180e", "\u200e", "\u200f", "\u2028", "\u2029", "\u202e", "\u2060", "\u2064", "\u2065",
		"\u2066", "\u206f", "\ufeff", "\ufff9", "\ufffa", "\ufffb", "\ufffc", "\ufffd", "\U000e0000", "\U000e0041", "\U000e007f", "\U000e0080",
		"€", "日", "\U0001f600",
		"\u0080", "\xa0", "\xc3", "\xe2", "\xff", "\xed\xa0", "\x80", "\x85", "\x9f", // invalid UTF-8, including 0x80-0x9f
		`\x1b`, `\u{202E}`, `<U+202E>`,
	}
	rng := rand.New(rand.NewPCG(18, 4242))
	seen := map[string]string{}
	for n := 0; n < 20000; n++ {
		var b strings.Builder
		for k := rng.IntN(12); k >= 0; k-- {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		in := b.String()
		out := neutraliseText(in)
		if !utf8.ValidString(out) {
			t.Fatalf("neutraliseText(%q) = %q is not UTF-8", in, out)
		}
		back, ok := decodeNeutralised(out)
		if !ok || back != in {
			t.Fatalf("neutraliseText(%q) = %q decodes to %q (ok=%v)", in, out, back, ok)
		}
		if prev, dup := seen[out]; dup && prev != in {
			t.Fatalf("%q and %q both render %q", prev, in, out)
		}
		seen[out] = in
	}
}

// Rendering is also self-delimiting across events: the text of two events written
// one after the other reads the same as the text of the joined event, except for
// a CR that ends one chunk and a LF that starts the next (kept as `\x0d` + LF).
func TestAdvW18NeutraliseConcatenationDecodesToConcatenation(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	parts := []string{"\\", "x1b", "\x1b", "\u202e", "a", "\r", "\n", "{", "\U000e0041", "\\x", "u{"}
	for n := 0; n < 5000; n++ {
		var a, b strings.Builder
		for k := rng.IntN(5); k >= 0; k-- {
			a.WriteString(parts[rng.IntN(len(parts))])
		}
		for k := rng.IntN(5); k >= 0; k-- {
			b.WriteString(parts[rng.IntN(len(parts))])
		}
		joined := neutraliseText(a.String()) + neutraliseText(b.String())
		back, ok := decodeNeutralised(joined)
		if !ok || back != a.String()+b.String() {
			t.Fatalf("%q + %q renders %q which decodes to %q (ok=%v)", a.String(), b.String(), joined, back, ok)
		}
	}
}

// ---- 6. JSON ------------------------------------------------------------------

func TestAdvW18NeutraliseToolInputJSONShapes(t *testing.T) {
	deep := strings.Repeat("[", 20000) + strings.Repeat("]", 20000)
	for _, tc := range []struct {
		name, in string
		valid    bool // json.Valid(in); the rendering must be valid JSON iff the input was
	}{
		{"esc in key is invalid JSON, shown as text", "{\"a\x1bb\":1}", false},
		{"bidi in key", "{\"a\u202eb\":1}", true},
		{"bidi in value", "{\"k\":\"a\u202eb\u2028\"}", true},
		{"lone surrogate escape kept", `{"k":"\ud800"}`, true},
		{"literal backslash-x in a string", `{"k":"C:\\x1b"}`, true},
		{"invalid utf8 in a string", "{\"k\":\"a\xffb\xed\xa0\x80\"}", true},
		{"tag char in value", "{\"k\":\"\U000E0041\"}", true},
		{"del and c1 in value", "{\"k\":\"\x7f\u0085\"}", true},
		{"cr between tokens", "{\"k\":\r\n1}", true},
		{"top-level string", `"a\u202e"`, true},
		{"cut document", `{"k":"abc\u20`, false},
		{"deeper than the decoder allows", deep, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := json.Valid([]byte(tc.in)); got != tc.valid {
				t.Fatalf("fixture validity = %v, want %v", got, tc.valid)
			}
			out := NeutraliseToolInput(tc.in)
			if !utf8.ValidString(out) {
				t.Fatalf("rendering is not UTF-8: %q", out)
			}
			if len(out) < 200 {
				if r, bad := rawRune(out); bad && r != '\r' {
					t.Fatalf("raw %U survives in %q", r, out)
				}
			}
			if tc.valid && !json.Valid([]byte(out)) {
				t.Fatalf("a valid JSON input was rendered as invalid JSON: %q -> %q", tc.in, out)
			}
			if tc.valid {
				var want, got any
				if json.Unmarshal([]byte(tc.in), &want) != nil || json.Unmarshal([]byte(out), &got) != nil {
					t.Fatalf("not decodable: %q", out)
				}
				if !reflect.DeepEqual(want, got) {
					t.Fatalf("meaning changed: %v vs %v", want, got)
				}
			}
		})
	}
}

// A valid JSON document containing a literal ESC is impossible, so a rendering that
// leaves `\u001b` alone cannot hide one; and a document whose string holds the six
// characters `\u001b` stays distinguishable from text-mode `\x1b`.
func TestAdvW18NeutraliseToolInputModesDoNotCollide(t *testing.T) {
	jsonMode := NeutraliseToolInput(`{"k":"\u001b"}`)
	textMode := NeutraliseToolInput("{\"k\":\"\x1b\"}")
	if jsonMode == textMode {
		t.Fatalf("both render %q", jsonMode)
	}
	if !json.Valid([]byte(jsonMode)) || json.Valid([]byte(textMode)) {
		t.Fatalf("json-mode %q valid=%v, text-mode %q valid=%v", jsonMode, json.Valid([]byte(jsonMode)), textMode, json.Valid([]byte(textMode)))
	}
}

// JSON mode keeps a carriage return between tokens. NeutraliseToolInput is exported,
// so a caller that prints its result raw would let `\r` overwrite a terminal line;
// serve is safe only because json.Marshal escapes it. Pin that the snapshot, the one
// call site, carries no raw CR.
func TestAdvW18NeutraliseToolInputCRIsHarmlessOnlyBecauseTheSnapshotMarshals(t *testing.T) {
	out := NeutraliseToolInput("{\"k\":\r1}")
	b, err := json.Marshal(Tool{ID: "t", Input: out})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsRune(b, '\r') {
		t.Fatalf("snapshot row carries a raw CR: %q", b)
	}
}

// ---- 2. Bounds ----------------------------------------------------------------

// Escaping grows a field by at most 4x (a 1-byte control becomes `\xNN`); the
// 4-byte tag rune becomes 9 bytes, a backslash 2. That factor is what serve now
// relies on in place of a second BoundTool, so it is pinned for every class of rune.
func TestAdvW18NeutraliseExpansionFactorIsAtMostFour(t *testing.T) {
	worst := 0.0
	for r := rune(0); r < 0x110000; r++ {
		if r >= 0xd800 && r < 0xe000 {
			continue
		}
		in := string(r)
		out := neutraliseText(in)
		if f := float64(len(out)) / float64(len(in)); f > worst {
			worst = f
		}
		if len(out) > 4*len(in) {
			t.Fatalf("U+%04X: %d bytes became %d", r, len(in), len(out))
		}
	}
	for b := 0x80; b < 0x100; b++ {
		if out := neutraliseText(string([]byte{byte(b)})); len(out) > 4 {
			t.Fatalf("invalid byte %#x became %q", b, out)
		}
	}
	if worst != 4 {
		t.Fatalf("worst factor is %v, expected the documented 4", worst)
	}
}

// A hostile tool at the adapter's own limits leaves the seam at no more than 4x on
// each escaped field and untouched Input, so job.tools is bounded by
// maxTools * (4*(4*256 + maxToolOutput) + maxToolInput) however hostile the bytes.
func TestAdvW18NeutraliseToolAtTheLimitsStaysWithinTheEscapedBound(t *testing.T) {
	esc := func(n int) string { return strings.Repeat("\x1b", n) }
	tool, ok := BoundTool(Tool{ID: esc(256), Name: esc(256), Status: esc(256), ParentID: esc(256), Input: esc(maxToolInput + 10), Output: esc(maxToolOutput + 10)})
	if !ok {
		t.Fatal("BoundTool dropped the tool")
	}
	e := neutraliseEvent(Event{Tools: []Tool{tool}})
	got := e.Tools[0]
	limit := map[string]int{"ID": 1024, "Name": 1024, "Status": 1024, "ParentID": 1024, "Input": maxToolInput, "Output": 4 * maxToolOutput}
	for field, v := range map[string]string{"ID": got.ID, "Name": got.Name, "Status": got.Status, "ParentID": got.ParentID, "Input": got.Input, "Output": got.Output} {
		if len(v) > limit[field] {
			t.Errorf("%s is %d bytes, bound %d", field, len(v), limit[field])
		}
	}
}

// ---- 3. Truncation ------------------------------------------------------------

// The seam runs after BoundTool, so a field cut at its byte cap is cut before it is
// escaped and never ends mid-escape or mid-rune, whatever the cut lands on.
func TestAdvW18NeutraliseBoundsCutBeforeTheEscapeNotInsideIt(t *testing.T) {
	for _, unit := range []string{"\x1b", "\\", "\u202e", "\U000E0041", "€", "\xff", "a\\"} {
		for pad := 0; pad < len(unit)+1; pad++ {
			out := strings.Repeat("p", pad) + strings.Repeat(unit, maxToolOutput)
			tool, ok := BoundTool(Tool{ID: "t", Output: out})
			if !ok {
				t.Fatal("dropped")
			}
			rendered := neutraliseEvent(Event{Tools: []Tool{tool}}).Tools[0].Output
			if !utf8.ValidString(rendered) || !wellFormedEscapes(rendered) {
				t.Fatalf("unit %q pad %d: cut output is malformed: ...%q", unit, pad, rendered[max(0, len(rendered)-12):])
			}
		}
	}
}

// ---- 5. The seam test ---------------------------------------------------------

// neutraliseEvent must account for every string field of Event and Tool: each is
// either rewritten or on the raw-by-design list. A field added to either struct
// without touching the seam fails here, which the frame-driven test cannot see
// because it checks a hand-written list of fields.
func TestAdvW18NeutraliseEveryStringFieldIsHandledOrListedRaw(t *testing.T) {
	rawEvent := map[string]bool{"Request": true, "TurnID": true, "ResolvedID": true, "CancelledID": true}
	rawTool := map[string]bool{"Input": true}
	const bad = "\x1b\u202e\\"
	var fill func(v reflect.Value)
	fill = func(v reflect.Value) {
		if !v.CanSet() {
			return
		}
		switch v.Kind() {
		case reflect.String:
			v.SetString(bad)
		case reflect.Struct:
			for i := range v.NumField() {
				fill(v.Field(i))
			}
		case reflect.Slice:
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
			fill(v.Index(0))
		case reflect.Pointer:
			v.Set(reflect.New(v.Type().Elem()))
			fill(v.Elem())
		}
	}
	var e Event
	fill(reflect.ValueOf(&e).Elem())
	out := neutraliseEvent(e)
	check := func(kind string, raw map[string]bool, before, after reflect.Value) {
		for i := range before.NumField() {
			name := before.Type().Field(i).Name
			if before.Field(i).Kind() != reflect.String {
				if before.Field(i).Kind() == reflect.Slice && name != "Tools" && name != "Questions" {
					t.Errorf("%s.%s: a new collection field; teach neutraliseEvent or list it raw", kind, name)
				}
				continue
			}
			same := before.Field(i).String() == after.Field(i).String()
			if same != raw[name] {
				t.Errorf("%s.%s: raw=%v but listed raw=%v; neutraliseEvent and the raw-by-design list disagree", kind, name, same, raw[name])
			}
		}
	}
	check("Event", rawEvent, reflect.ValueOf(e), reflect.ValueOf(out))
	check("Tool", rawTool, reflect.ValueOf(e.Tools[0]), reflect.ValueOf(out.Tools[0]))
	if out.Tools[0] == e.Tools[0] {
		t.Error("Tool was not rewritten at all")
	}
}

// The implementer's AST walk only sees exported functions in this directory whose
// result is Event, []Event or *Event. This one reads every exported surface of the
// package, interface methods, struct fields, channels, maps and func types included,
// and finds that nothing but an adapter's Next, the Event type and the unexported
// seam mentions Event, so a new route (a channel, a callback, a field) fails here.
func TestAdvW18NeutraliseNoExportedSurfaceCarriesAnEventButNext(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	text := func(n ast.Node) string {
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, n); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	// mentionsEvent looks at a type expression, a field list or a struct, and for a
	// struct only at its exported fields (an adapter's unexported queues are not API).
	var mentionsEvent func(n ast.Node) bool
	mentionsEvent = func(n ast.Node) bool {
		switch x := n.(type) {
		case nil:
			return false
		case *ast.FieldList:
			if x == nil {
				return false
			}
			for _, f := range x.List {
				if mentionsEvent(f.Type) {
					return true
				}
			}
			return false
		case *ast.StructType:
			for _, f := range x.Fields.List {
				exported := len(f.Names) == 0
				for _, id := range f.Names {
					exported = exported || id.IsExported()
				}
				if exported && mentionsEvent(f.Type) {
					return true
				}
			}
			return false
		}
		return strings.Contains(text(n), "Event")
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() || d.Name.Name == "Next" && d.Recv != nil {
					continue
				}
				if d.Recv != nil {
					if recv := d.Recv.List[0].Type; !exportedRecv(recv) {
						continue
					}
				}
				if (d.Type.Results != nil && mentionsEvent(d.Type.Results)) || mentionsEvent(d.Type.Params) {
					t.Errorf("%s: exported %s moves an Event outside Next", name, d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if !s.Name.IsExported() || s.Name.Name == "Event" {
							continue
						}
						if mentionsEvent(s.Type) {
							t.Errorf("%s: exported type %s carries an Event", name, s.Name.Name)
						}
					case *ast.ValueSpec:
						for _, id := range s.Names {
							if id.IsExported() && (s.Type != nil && mentionsEvent(s.Type)) {
								t.Errorf("%s: exported %s carries an Event", name, id.Name)
							}
						}
					}
				}
			}
		}
	}
}

func exportedRecv(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.StarExpr:
		return exportedRecv(x.X)
	case *ast.Ident:
		return x.IsExported()
	case *ast.IndexExpr:
		return exportedRecv(x.X)
	}
	return false
}

// ---- 4. Bypass: error strings built from agent bytes ---------------------------

// A permission mode or model the agent names is echoed into an error with %q, which
// Go escapes for every non-printable rune (ESC, bidi, the tag block, U+00AD), so the
// error that reaches a run's reason carries no raw control.
func TestAdvW18NeutraliseClaudeErrorsEchoAgentBytesQuoted(t *testing.T) {
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"mode", "version"} {
		s := w18aClaude(t)
		s.options.Cwd = cwd
		s.reported, s.build = "default", "1.0"
		mode, version := "default", "1.0"
		if field == "mode" {
			mode = "x" + hostile
		} else {
			version = "x" + hostile
		}
		frame := fmt.Sprintf(`{"type":"system","subtype":"init","session_id":"S","cwd":%q,"permissionMode":%q,"model":"m","claude_code_version":%q}`, cwd, mode, version)
		_, _, ferr := s.event([]byte(frame))
		if ferr == nil {
			t.Fatalf("%s: no error", field)
		}
		if r, badRune := rawRune(ferr.Error()); badRune || strings.Contains(ferr.Error(), "\x1b") {
			t.Errorf("%s: error carries raw %U: %q", field, r, ferr.Error())
		}
	}
}

// ---- 4. Bypass: what reaches an approver (documentation) ----------------------

// Request is raw by design. This pins what that means for the approver: the
// Payload (digested) and each Question's header and text carry the agent's bytes
// untouched, so a UI that shows them must neutralise at display. If the owner
// later neutralises Questions, this is the test to change.
func TestAdvW18NeutraliseCodexQuestionTextReachesTheApproverRaw(t *testing.T) {
	s := w18Codex(t)
	s.items = map[string]json.RawMessage{"i": json.RawMessage(`{}`)}
	e, ok, err := s.event(packet{ID: json.RawMessage("5"), Method: "item/tool/requestUserInput",
		Params: json.RawMessage(`{"threadId":"thread","turnId":"turn","itemId":"i","questions":[{"id":"q","header":"H\u001b[2J","question":"Q\u202e?","isSecret":false}]}`)})
	if err != nil || !ok || e.Request == nil || len(e.Request.Questions) != 1 {
		t.Fatalf("event: %+v ok=%v err=%v", e, ok, err)
	}
	q := e.Request.Questions[0]
	if q.Header != "H\x1b[2J" || q.Question != "Q\u202e?" {
		t.Fatalf("questions were rewritten: %+v", q)
	}
	e = neutraliseEvent(e)
	if e.Request.Questions[0] != q {
		t.Fatal("the seam rewrote Request.Questions")
	}
}
