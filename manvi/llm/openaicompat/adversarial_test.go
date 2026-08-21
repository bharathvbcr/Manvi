package openaicompat

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"manvi/llm"
)

// The fallback parser reads text a model produced, which means it reads
// whatever a model produced. It must never panic, never hang, and never invent
// a call — a fabricated tool call runs a tool nobody asked for.
func TestFallbackParserSurvivesHostileInput(t *testing.T) {
	cases := []string{
		"",
		"<tool_call>",
		"</tool_call>",
		"<tool_call></tool_call>",
		"<tool_call>{}</tool_call>",
		"<tool_call>null</tool_call>",
		"<tool_call>[]</tool_call>",
		"<tool_call>{\"name\":\"\"}</tool_call>",
		"<tool_call>{\"name\":null}</tool_call>",
		"<tool_call><function=></function></tool_call>",
		"<tool_call><function=x</tool_call>",
		"<tool_call><function=x><parameter=</function></tool_call>",
		"<tool_call><function=x><parameter=k>v</function></tool_call>",
		"<tool_call><function=x><parameter=>v</parameter></function></tool_call>",
		strings.Repeat("<tool_call>", 500),
		strings.Repeat("<tool_call><function=a></function></tool_call>", 200),
		"```",
		"``````",
		"```json```",
		"```json\nnull\n```",
		"```json\n{\"name\":\"a\",\"arguments\":\"not an object\"}\n```",
		"\x00\x01\x02<tool_call>\xff\xfe</tool_call>",
		"<tool_call>\n<function=x>\n<parameter=k>\n" + strings.Repeat("v", 100000) + "\n</parameter>\n</function>\n</tool_call>",
		"<think></think><tool_call><function=a><parameter=b>c</parameter></function></tool_call>",
	}
	for i, in := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d panicked on %.60q: %v", i, in, r)
				}
			}()
			text, calls, format := extractFallbackToolCalls(in, nil)
			for _, c := range calls {
				if strings.TrimSpace(c.Name) == "" {
					t.Errorf("case %d produced a call with no name", i)
				}
				if !json.Valid(c.Arguments) {
					t.Errorf("case %d produced invalid JSON arguments: %s", i, c.Arguments)
				}
			}
			if len(calls) == 0 && format != FallbackNone {
				t.Errorf("case %d reported format %q with no calls", i, format)
			}
			if len(calls) == 0 && text != in {
				t.Errorf("case %d rewrote text while recovering nothing", i)
			}
		}()
	}
}

// Randomised: arbitrary bytes must never yield a call with a broken shape.
func TestFallbackParserFuzz(t *testing.T) {
	fragments := []string{
		"<tool_call>", "</tool_call>", "<function=", ">", "</function>",
		"<parameter=", "</parameter>", "{", "}", "\"name\"", ":", ",",
		"```", "json", "\n", "abc", "123", "null", "true", "<think>", "</think>",
	}
	rng := rand.New(rand.NewSource(20260818))
	for i := 0; i < 4000; i++ {
		var b strings.Builder
		for j := 0; j < rng.Intn(24); j++ {
			b.WriteString(fragments[rng.Intn(len(fragments))])
		}
		in := b.String()
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on %q: %v", in, r)
				}
			}()
			_, calls, _ := extractFallbackToolCalls(in, nil)
			for _, c := range calls {
				if c.Name == "" || !json.Valid(c.Arguments) {
					t.Fatalf("malformed call recovered from %q: %+v", in, c)
				}
			}
		}()
	}
}

// The tag filter is fed arbitrary streaming fragments. Whatever the split, the
// text and reasoning it emits must reconstruct the input minus the tags — never
// duplicating content and never leaking a tag.
func TestTagFilterNeverDuplicatesOrLeaks(t *testing.T) {
	inputs := []string{
		"plain text with no tags at all",
		"<think>a</think>b",
		"a</think>b",
		"<think>a",
		"</think>",
		"<think><think>nested</think></think>",
		"<thought>alt</thought>tail",
		"a<think>b</think>c<think>d</think>e",
		strings.Repeat("<think>x</think>y", 50),
	}
	rng := rand.New(rand.NewSource(7))
	for _, in := range inputs {
		for trial := 0; trial < 40; trial++ {
			var f tagFilter
			var text, reasoning strings.Builder
			rest := in
			for len(rest) > 0 {
				n := 1 + rng.Intn(5)
				if n > len(rest) {
					n = len(rest)
				}
				for _, c := range f.feed(rest[:n]) {
					text.WriteString(c.text)
					reasoning.WriteString(c.reasoning)
				}
				rest = rest[n:]
			}
			fl := f.flush()
			text.WriteString(fl.text)
			reasoning.WriteString(fl.reasoning)

			combined := text.String() + reasoning.String()
			for _, tag := range allTags {
				if strings.Contains(combined, tag) {
					t.Fatalf("input %q split %d leaked %q", in, trial, tag)
				}
			}
			// Every non-tag byte of the input must appear exactly once.
			stripped := in
			for _, tag := range allTags {
				stripped = strings.ReplaceAll(stripped, tag, "")
			}
			if len(combined) != len(stripped) {
				t.Fatalf("input %q split %d: emitted %d bytes, input has %d non-tag bytes "+
					"(text=%q reasoning=%q)", in, trial, len(combined), len(stripped),
					text.String(), reasoning.String())
			}
		}
	}
}

func TestCompactionAndFilterKeepValidUTF8(t *testing.T) {
	// Multi-byte content split at arbitrary byte boundaries.
	in := strings.Repeat("日本語<think>思考</think>テキスト", 20)
	rng := rand.New(rand.NewSource(11))
	var f tagFilter
	var out strings.Builder
	rest := in
	for len(rest) > 0 {
		n := 1 + rng.Intn(4)
		if n > len(rest) {
			n = len(rest)
		}
		for _, c := range f.feed(rest[:n]) {
			out.WriteString(c.text)
			out.WriteString(c.reasoning)
		}
		rest = rest[n:]
	}
	fl := f.flush()
	out.WriteString(fl.text + fl.reasoning)
	if !utf8.ValidString(out.String()) {
		t.Fatal("the filter produced invalid UTF-8 from a byte-split stream")
	}
}

func TestParamTypesToleratesBrokenSchemas(t *testing.T) {
	for _, raw := range []string{
		``, `null`, `[]`, `{"properties":null}`, `{"properties":[]}`,
		`{"properties":{"a":{"type":123}}}`,
		`{"properties":{"a":{"type":[]}}}`,
		`{"properties":{"a":{"type":["null"]}}}`,
		`{"properties":{"a":null}}`,
		`not json`,
	} {
		tools := []llm.ToolSchema{{Name: "t", InputSchema: json.RawMessage(raw)}}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("schema %q panicked: %v", raw, r)
				}
			}()
			_ = paramTypes(tools, "t")
			_, calls, _ := extractFallbackToolCalls(
				"<tool_call><function=t><parameter=a>1</parameter></function></tool_call>", tools)
			for _, c := range calls {
				if !json.Valid(c.Arguments) {
					t.Fatalf("schema %q produced invalid arguments %s", raw, c.Arguments)
				}
			}
		}()
	}
}

// A tool call may only name a tool the request actually offered.
//
// The fallback parser exists to recover calls from a server that did not parse
// them itself, and it runs on every assistant message that carried no
// structured tool_calls — which is every ordinary prose answer. Without this
// check it reads any JSON object with a "name" key as a call, so a fenced
// package.json in an explanation becomes an executable call, and a coding agent
// discussing its own tool format can name a real tool with real arguments. The
// offered schemas are the only authority on what exists to be called.
func TestFallbackNeverInventsAToolThatWasNotOffered(t *testing.T) {
	offered := []llm.ToolSchema{{Name: "read_file"}, {Name: "write_file"}}

	cases := []struct {
		name string
		text string
	}{
		{"fenced package.json", "Here is the file:\n\n```json\n{\"name\": \"my-project\", \"version\": \"1.0.0\"}\n```\n\nRun npm install."},
		{"fenced explanation of a real tool", "For example you would send:\n\n```json\n{\"name\": \"write_file\", \"arguments\": {\"path\": \"/etc/hosts\", \"content\": \"pwned\"}}\n```\n\nThat is the shape."},
		{"tagged call naming an unoffered tool", "<tool_call>{\"name\":\"delete_everything\",\"arguments\":{}}</tool_call>"},
		{"xml call naming an unoffered tool", "<tool_call><function=rm_rf><parameter=path>/</parameter></function></tool_call>"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, calls, _ := extractFallbackToolCalls(tc.text, offered)
			for _, c := range calls {
				if !schemaOffers(offered, c.Name) {
					t.Fatalf("fabricated a call to %q, which was never offered (args: %s)", c.Name, c.Arguments)
				}
			}
		})
	}

	// With nothing offered there is nothing that can legitimately be called.
	for _, tc := range cases {
		if _, calls, _ := extractFallbackToolCalls(tc.text, nil); len(calls) > 0 {
			t.Errorf("%s: recovered %d call(s) when the request offered no tools", tc.name, len(calls))
		}
	}
}

// A tool that WAS offered must still be recovered, or the guard above would
// have fixed fabrication by breaking the feature.
func TestFallbackStillRecoversAnOfferedTool(t *testing.T) {
	offered := []llm.ToolSchema{{Name: "read_file"}}
	for _, in := range []string{
		"<tool_call>{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}</tool_call>",
		"<tool_call><function=read_file><parameter=path>a.go</parameter></function></tool_call>",
		"```json\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"a.go\"}}\n```",
	} {
		_, calls, format := extractFallbackToolCalls(in, offered)
		if len(calls) != 1 || calls[0].Name != "read_file" {
			t.Fatalf("%.40q: recovered %d calls, want 1 read_file (format %q)", in, len(calls), format)
		}
	}
}
