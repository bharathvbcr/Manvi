package openaicompat

import (
	"encoding/json"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// schemaOffers reports whether name is one of the tools the request offered.
//
// The offered schemas are the only authority on what exists to be called, and
// this is the check that makes the fallback parser safe. That parser runs on
// every assistant message that carried no structured tool_calls — which is
// every ordinary prose answer, not just a misconfigured server — and it reads
// any JSON object with a "name" key as a call. Unchecked, a fenced
// package.json in an explanation becomes an executable call whose fence is
// deleted from the answer, and a model discussing this harness's own tool
// format names a real tool with real arguments nobody asked for.
//
// This replaced a name check that validated only the *shape* of the string and
// was never called from anywhere. Shape was the wrong question: "rm_rf" is a
// well-formed name for a tool that does not exist. A name that was not offered
// is prose, and is left in the text as prose.
func schemaOffers(tools []llm.ToolSchema, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// FallbackFormat names how a tool call was recovered from plain text.
type FallbackFormat string

const (
	// FallbackNone means the server parsed the call itself and delivered it in
	// the tool_calls field, which is the healthy case.
	FallbackNone FallbackFormat = ""
	// FallbackHermes is <tool_call>{"name":…,"arguments":{…}}</tool_call>.
	FallbackHermes FallbackFormat = "hermes-json"
	// FallbackQwenXML is the nested-XML form Qwen3's chat template mandates:
	//
	//	<tool_call>
	//	<function=name>
	//	<parameter=key>
	//	value
	//	</parameter>
	//	</function>
	//	</tool_call>
	FallbackQwenXML FallbackFormat = "qwen-xml"
	// FallbackFencedJSON is a markdown-fenced JSON object naming a tool.
	FallbackFencedJSON FallbackFormat = "fenced-json"
)

// extractFallbackToolCalls recovers tool calls a server left in the text.
//
// This path should never run. Every serving stack worth using parses tool calls
// itself and returns them in tool_calls; when this fires it means the server is
// not configured with a parser for the model it is serving, and the harness is
// compensating. That is worth saying out loud — the caller records the format —
// because a silent compensation hides a misconfigured server, and a silent
// *failure* to compensate is worse: the call surfaces as prose, the loop sees no
// tool calls, and a turn that did nothing reports success.
func extractFallbackToolCalls(rawText string, tools []llm.ToolSchema) (string, []llm.ToolCallBlock, FallbackFormat) {
	trimmed := strings.TrimSpace(rawText)
	if trimmed == "" {
		return rawText, nil, FallbackNone
	}

	if cleaned, calls := extractTaggedCalls(trimmed, tools); len(calls) > 0 {
		format := FallbackHermes
		for _, c := range calls {
			if c.xml {
				format = FallbackQwenXML
				break
			}
		}
		blocks := make([]llm.ToolCallBlock, 0, len(calls))
		for _, c := range calls {
			blocks = append(blocks, llm.ToolCallBlock{
				ID:        llm.CallID(synthesizeCallID("fallback")),
				Name:      c.name,
				Arguments: c.args,
			})
		}
		return cleaned, blocks, format
	}

	if cleaned, calls := extractFencedCalls(trimmed, tools); len(calls) > 0 {
		return cleaned, calls, FallbackFencedJSON
	}

	return rawText, nil, FallbackNone
}

type parsedCall struct {
	name string
	args json.RawMessage
	xml  bool
}

// extractTaggedCalls walks every <tool_call>…</tool_call> block, accepting
// either the JSON body or the nested-XML body. Every block is walked rather
// than only the first, because a model that emits two calls in one message
// means both, and pairing the first opening tag with the last closing one — as
// the previous implementation did for fenced blocks — merges two calls into one
// malformed extraction.
func extractTaggedCalls(data string, tools []llm.ToolSchema) (string, []parsedCall) {
	const open, close = "<tool_call>", "</tool_call>"
	if !strings.Contains(data, open) {
		return data, nil
	}

	var calls []parsedCall
	var clean strings.Builder
	rest := data
	for {
		start := strings.Index(rest, open)
		if start < 0 {
			clean.WriteString(rest)
			break
		}
		clean.WriteString(rest[:start])
		body := rest[start+len(open):]
		end := strings.Index(body, close)
		if end < 0 {
			// Unterminated: a truncated response, not a call. Keep the text as
			// it stands rather than inventing a call from half a payload.
			clean.WriteString(open)
			clean.WriteString(body)
			break
		}
		payload := strings.TrimSpace(body[:end])
		rest = body[end+len(close):]

		if parsed, ok := parseCallPayload(payload, tools); ok {
			calls = append(calls, parsed...)
			continue
		}
		// A block that could not be read is left in the text exactly as the
		// model wrote it. parseCallPayload's contract has always said so, but
		// it only held when *nothing* was recovered, because a rejected
		// payload was written nowhere: with one good call beside one bad one,
		// the bad one vanished from the answer as well as from the calls, so a
		// request the model made neither ran nor was visible to anyone.
		clean.WriteString(open)
		clean.WriteString(body[:end])
		clean.WriteString(close)
	}
	if len(calls) == 0 {
		return data, nil
	}
	return strings.TrimSpace(clean.String()), calls
}

// parseCallPayload reads one tool-call body in either supported spelling.
//
// It returns a slice because the XML spelling permits more than one
// <function=…> block inside a single <tool_call>, and a model that writes two
// means two.
func parseCallPayload(payload string, tools []llm.ToolSchema) ([]parsedCall, bool) {
	var found []parsedCall
	if strings.HasPrefix(payload, "<function=") {
		xml, ok := parseXMLCalls(payload, tools)
		if !ok {
			return nil, false
		}
		found = xml
	} else {
		name, args, ok := parseJSONCall(payload)
		if !ok {
			return nil, false
		}
		found = []parsedCall{{name: name, args: args}}
	}
	// Both spellings meet here, so the offered-set check sits here rather than
	// in each parser. Rejecting leaves the block in the text — the caller
	// writes an unrecognised block back verbatim — so a model that asked for a
	// tool it was not given has its request surface as prose instead of
	// vanishing.
	//
	// One unoffered name rejects the whole block rather than the single call.
	// A block is one thing the model asked for; running half of a two-call
	// block is a silent partial execution, and the model is never told which
	// half it got.
	for _, call := range found {
		if !schemaOffers(tools, call.name) {
			return nil, false
		}
	}
	return found, true
}

func parseJSONCall(payload string) (string, json.RawMessage, bool) {
	var body struct {
		Name       string          `json:"name"`
		Tool       string          `json:"tool"`
		Arguments  json.RawMessage `json:"arguments"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		// Models emit Python literals and trailing commas often enough that
		// refusing them means dropping real calls. Repair, then insist the
		// result is valid JSON — a payload that still will not parse is not a
		// call, and inventing one from it would be worse than reporting none.
		if err := json.Unmarshal([]byte(repairJSONLiterals(payload)), &body); err != nil {
			return "", nil, false
		}
	}
	name := body.Name
	if name == "" {
		name = body.Tool
	}
	if name == "" {
		return "", nil, false
	}
	args := body.Arguments
	if len(args) == 0 {
		args = body.Parameters
	}
	if len(args) == 0 {
		return name, json.RawMessage("{}"), true
	}
	object, ok := argumentsAsObject(sanitizeJSONArguments(string(args)))
	if !ok {
		return "", nil, false
	}
	return name, object, true
}

// argumentsAsObject insists that a recovered call's arguments are a JSON
// object, which is the only shape a tool can be invoked with.
//
// The value used to be passed through verbatim, so a model writing
// {"name":"read_file","arguments":"not an object"} produced a tool call whose
// Arguments was a JSON *string*. Nothing here objected; the failure surfaced
// far away, in whichever consumer unmarshalled a ToolCallBlock into a map,
// with nothing left pointing back at the response that caused it. A call that
// cannot be invoked is not a call, and saying so here leaves the model's text
// intact as prose instead.
//
// The one non-object spelling that is still a call is a JSON-encoded object
// *string*: OpenAI's own wire spells tool arguments that way, and models copy
// what they were trained on. That is unwrapped once — once, not repeatedly, so
// a doubly-encoded payload is refused rather than chased.
func argumentsAsObject(args json.RawMessage) (json.RawMessage, bool) {
	trimmed := strings.TrimSpace(string(args))
	if trimmed == "" {
		return nil, false
	}
	switch trimmed[0] {
	case '{':
		if json.Valid([]byte(trimmed)) {
			return json.RawMessage(trimmed), true
		}
	case '"':
		var inner string
		if json.Unmarshal([]byte(trimmed), &inner) != nil {
			return nil, false
		}
		unwrapped := strings.TrimSpace(string(sanitizeJSONArguments(inner)))
		if strings.HasPrefix(unwrapped, "{") && json.Valid([]byte(unwrapped)) {
			return json.RawMessage(unwrapped), true
		}
	}
	return nil, false
}

// parseXMLCalls reads the nested-XML body Qwen3's template prescribes.
//
// Parameter values are strings on the wire, and are re-typed only when the text
// is unambiguously a JSON scalar or container. Guessing more than that would
// turn a path like "0755" into a number, and a filename like "null" into
// nothing at all.
//
// Every <function=…>…</function> block in the payload is read, and each one is
// bounded by its own closing tag. The previous shape took the first function
// name and then scanned for <parameter=> to the end of the whole payload,
// straight past the </function> that ended its block — so two calls in one
// <tool_call> merged into one. Not merely lossy: the second tool disappeared
// and *its* arguments were attached to the first, which is how a "target"
// written for delete_file arrived as an extra argument on read_file.
//
// A block that is not well formed rejects the entire payload rather than
// contributing what it managed to read. A <tool_call> is one thing the model
// asked for, and half-executing it — running the complete first call while
// dropping a truncated second — is a silent partial execution the model is
// never told about. Refused, the markup stays in the text as prose and the
// loop sees a turn that made no calls, which is recoverable.
func parseXMLCalls(payload string, tools []llm.ToolSchema) ([]parsedCall, bool) {
	const fOpen, fClose = "<function=", "</function>"

	var calls []parsedCall
	rest := payload
	for {
		start := strings.Index(rest, fOpen)
		if start < 0 {
			break
		}
		rest = rest[start+len(fOpen):]
		nameEnd := strings.Index(rest, ">")
		if nameEnd < 0 {
			return nil, false
		}
		name := strings.TrimSpace(strings.TrimSuffix(rest[:nameEnd], "/"))
		if name == "" {
			return nil, false
		}
		rest = rest[nameEnd+1:]

		// The block's own body, so a following block's parameters cannot be
		// read as this one's. A missing closing tag is tolerated for the last
		// block — some templates omit it — but only when the parameters inside
		// it are themselves complete; an unterminated parameter is what a
		// response cut off at max_tokens looks like, and is refused below.
		body := rest
		if closeAt := strings.Index(rest, fClose); closeAt >= 0 {
			body, rest = rest[:closeAt], rest[closeAt+len(fClose):]
		} else {
			rest = ""
		}
		if strings.Contains(body, fOpen) {
			// A function block opening inside another one is not a spelling
			// any template produces, and guessing which of the two the
			// parameters belong to is exactly the merge this rewrite exists
			// to stop.
			return nil, false
		}

		args, complete := xmlParameters(body, paramTypes(tools, name))
		if !complete {
			return nil, false
		}
		calls = append(calls, parsedCall{name: name, args: args, xml: true})
	}
	if len(calls) == 0 {
		return nil, false
	}
	return calls, true
}

// xmlParameters reads the <parameter=key>value</parameter> pairs of one
// function block.
//
// Finding where a value *ends* is the whole difficulty. The grammar has no
// escaping, so a value is free to contain the literal "</parameter>" — and it
// routinely does, because the values are file contents and this harness's own
// fixtures are full of tool-call markup. Taking the first "</parameter>", as
// this used to, truncated such a value silently: write_file received "prefix "
// where the model wrote "prefix </parameter> suffix", and the short file was
// written to disk with no error raised anywhere.
//
// The value therefore ends at the *last* "</parameter>" before the next
// "<parameter=" (or before the end of the block, for the final parameter).
// That is exact for both shapes that matter: successive parameters still close
// at their own tag, and an embedded closing tag stays inside the value it
// belongs to. It is still a heuristic — a value containing a literal
// "<parameter=" cuts short — but that spelling has no plausible source, where
// XML content does.
//
// The second return reports that every parameter it opened was also closed. A
// parameter left open is a response truncated mid-value, and the caller refuses
// the whole call rather than dispatching a tool with the arguments that
// happened to arrive first.
func xmlParameters(body string, types map[string]string) (json.RawMessage, bool) {
	const pOpen, pClose = "<parameter=", "</parameter>"

	args := map[string]json.RawMessage{}
	rest := body
	for {
		start := strings.Index(rest, pOpen)
		if start < 0 {
			break
		}
		rest = rest[start+len(pOpen):]
		keyEnd := strings.Index(rest, ">")
		if keyEnd < 0 {
			return nil, false
		}
		key := strings.TrimSpace(rest[:keyEnd])
		rest = rest[keyEnd+1:]

		region := rest
		if next := strings.Index(rest, pOpen); next >= 0 {
			region = rest[:next]
		}
		end := strings.LastIndex(region, pClose)
		if end < 0 {
			return nil, false
		}
		// The template puts the value on its own lines; the surrounding
		// newlines are framing, not content.
		value := strings.Trim(rest[:end], "\n")
		rest = rest[end+len(pClose):]
		if key == "" {
			continue
		}
		args[key] = typedParam(value, types[key])
	}

	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// paramTypes maps a tool's parameter names to their declared JSON Schema type.
//
// The declared schema is the only authority on what a value means. Guessing
// from the text cannot work: "0755" is a file mode, "1.10" is a version, and
// "null" is a plausible filename, yet each reads as a number or a literal. The
// nested-XML form the template prescribes carries no types at all, so without
// the schema every one of those is a coin flip that produces a wrong value
// which looks like a right one.
func paramTypes(tools []llm.ToolSchema, toolName string) map[string]string {
	for _, t := range tools {
		if t.Name != toolName {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Type any `json:"type"`
			} `json:"properties"`
		}
		if json.Unmarshal(t.InputSchema, &schema) != nil {
			return nil
		}
		out := make(map[string]string, len(schema.Properties))
		for name, prop := range schema.Properties {
			switch v := prop.Type.(type) {
			case string:
				out[name] = v
			case []any:
				// A union such as ["string","null"]: take the first concrete
				// type, which is what a caller would most likely mean.
				for _, entry := range v {
					if str, ok := entry.(string); ok && str != "null" {
						out[name] = str
						break
					}
				}
			}
		}
		return out
	}
	return nil
}

// typedParam encodes one XML parameter value according to its declared type.
//
// An undeclared parameter stays a string. That is the conservative answer: a
// string reaching a schema that wanted a number is a validation error the model
// is told about and can correct, whereas a number reaching a schema that wanted
// a string is accepted and silently wrong.
func typedParam(value, declared string) json.RawMessage {
	trimmed := strings.TrimSpace(value)
	asString := func() json.RawMessage {
		encoded, err := json.Marshal(value)
		if err != nil {
			return json.RawMessage(`""`)
		}
		return encoded
	}

	switch declared {
	case "number", "integer":
		if trimmed == "" {
			return asString()
		}
		var n json.Number
		if json.Unmarshal([]byte(trimmed), &n) == nil {
			return json.RawMessage(trimmed)
		}
		return asString()
	case "boolean":
		switch strings.ToLower(trimmed) {
		case "true":
			return json.RawMessage("true")
		case "false":
			return json.RawMessage("false")
		}
		return asString()
	case "object", "array":
		// json.Valid is not the question. It is true of every scalar, so a
		// parameter declared "object" whose text was "123" came back as the
		// number 123, and one declared "array" whose text was "true" came back
		// as the boolean true — schema-driven coercion emitting precisely the
		// kind the schema said it was not. The opening byte is what separates a
		// container from a scalar, and it has to agree with the declaration
		// before the text is passed through as JSON.
		open := byte('{')
		if declared == "array" {
			open = '['
		}
		if trimmed != "" && trimmed[0] == open && json.Valid([]byte(trimmed)) {
			return json.RawMessage(trimmed)
		}
		return asString()
	default:
		return asString()
	}
}

// extractFencedCalls reads a markdown-fenced JSON object that names a tool.
//
// Each fence is considered on its own, and only a fence naming an offered tool
// becomes a call. An earlier version required a "devcouncil_" prefix here while
// accepting anything in the tagged path, so the same call was recovered or
// dropped depending on how the model happened to wrap it; the offered set is
// now the single authority for both spellings.
func extractFencedCalls(data string, tools []llm.ToolSchema) (string, []llm.ToolCallBlock) {
	var calls []llm.ToolCallBlock
	var clean strings.Builder
	rest := data
	idx := 0
	for {
		start := strings.Index(rest, "```")
		if start < 0 {
			clean.WriteString(rest)
			break
		}
		body := rest[start+3:]
		end := strings.Index(body, "```")
		if end < 0 {
			clean.WriteString(rest)
			break
		}
		inner := strings.TrimSpace(body[:end])
		after := body[end+3:]
		inner = strings.TrimPrefix(inner, "json")
		inner = strings.TrimSpace(inner)

		if name, args, ok := parseJSONCall(inner); ok && schemaOffers(tools, name) {
			clean.WriteString(rest[:start])
			calls = append(calls, llm.ToolCallBlock{
				ID:        llm.CallID(synthesizeCallID("fallback")),
				Name:      name,
				Arguments: args,
			})
			idx++
		} else {
			clean.WriteString(rest[:start+3+end+3])
		}
		rest = after
	}
	if len(calls) == 0 {
		return data, nil
	}
	return strings.TrimSpace(clean.String()), calls
}
