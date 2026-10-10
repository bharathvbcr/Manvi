package openaicompat

import (
	"encoding/json"
	"strings"
)

func isIdentChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
}

// replaceWordLiteral rewrites a bare Python literal outside string values.
//
// It walks bytes rather than runes, and asks strings.HasPrefix(text[i:], …)
// rather than strings.HasPrefix(string(runes[i:]), …). The difference is not
// stylistic: converting a rune slice back to a string copies it, so the old
// spelling allocated one copy of the remaining payload per input rune. That
// made this quadratic in both time and memory — 16KB of unquoted content cost
// 2.1 seconds and 427MB, doubling the input quadrupled both, and the input is
// model output arriving from the network. It runs in Response(), after the
// stream ended and the stall watchdog was stopped, so nothing bounded it: a
// truncated Python-repr tool-call payload was a denial of service with a
// straight face.
//
// Bytes are safe here because every character this function decides on —
// the quote marks, the backslash, the ASCII word literals, and the identifier
// characters either side of a match — is ASCII, and no byte of a multi-byte
// UTF-8 sequence can be mistaken for one. Bytes it does not decide on are
// copied through untouched, so multi-byte content survives verbatim.
func replaceWordLiteral(text, oldWord, newWord string) string {
	if oldWord == "" || !strings.Contains(text, oldWord) {
		return text
	}
	var sb strings.Builder
	sb.Grow(len(text))
	inString := false
	var quoteChar byte
	escaped := false
	n := len(text)

	for i := 0; i < n; {
		c := text[i]
		if inString {
			sb.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == quoteChar {
				inString = false
			}
			i++
			continue
		}

		if c == '"' || c == '\'' {
			inString = true
			quoteChar = c
			sb.WriteByte(c)
			i++
			continue
		}

		if c == oldWord[0] && strings.HasPrefix(text[i:], oldWord) {
			wordLen := len(oldWord)
			prevIsWord := i > 0 && isIdentChar(rune(text[i-1]))
			nextIsWord := (i+wordLen < n) && isIdentChar(rune(text[i+wordLen]))
			if !prevIsWord && !nextIsWord {
				sb.WriteString(newWord)
				i += wordLen
				continue
			}
		}

		sb.WriteByte(c)
		i++
	}
	return sb.String()
}

// stripTrailingCommas removes a comma that is followed, past any whitespace, by
// a closing brace or bracket, outside string values.
//
// It walks the payload with the same string awareness as replaceWordLiteral. It
// used to apply strings.ReplaceAll for ",}" and ",]" to the whole payload, which
// rewrote the contents of string values: a regex "a,]" became "a]" and a
// separator ",}" became "}", and because the repaired payload then parsed, the
// corruption was accepted and a patch or shell tool received text the model
// never wrote. Only the bytes the walk is certain are structure are touched.
func stripTrailingCommas(text string) string {
	if !strings.Contains(text, ",") {
		return text
	}
	var sb strings.Builder
	sb.Grow(len(text))
	inString := false
	var quoteChar byte
	escaped := false
	n := len(text)
	for i := 0; i < n; i++ {
		c := text[i]
		if inString {
			sb.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == quoteChar {
				inString = false
			}
			continue
		}
		switch c {
		case '"', '\'':
			inString = true
			quoteChar = c
		case ',':
			j := i + 1
			for j < n && (text[j] == ' ' || text[j] == '\t' || text[j] == '\n' || text[j] == '\r') {
				j++
			}
			if j < n && (text[j] == '}' || text[j] == ']') {
				continue // drop the comma, keep the whitespace
			}
		}
		sb.WriteByte(c)
	}
	return sb.String()
}

func repairJSONLiterals(s string) string {
	repaired := stripTrailingCommas(s)
	repaired = replaceWordLiteral(repaired, "True", "true")
	repaired = replaceWordLiteral(repaired, "False", "false")
	repaired = replaceWordLiteral(repaired, "None", "null")
	return repaired
}

// sanitizeJSONArguments returns arguments valid as JSON where they can be
// made so. See sanitizeJSONArgumentsReport.
func sanitizeJSONArguments(raw string) json.RawMessage {
	args, _ := sanitizeJSONArgumentsReport(raw)
	return args
}

// sanitizeJSONArgumentsReport is sanitizeJSONArguments plus whether the payload
// was rewritten to get there — a fence stripped, a substring extracted, a
// literal or trailing comma repaired. The caller records it, because a tool
// receiving arguments the model did not literally send is something an operator
// needs to be able to see. Empty arguments are `{}`, which is a claim about the
// absence of arguments rather than a rewrite; whether an empty payload may be
// believed is the caller's decision, because only the caller knows if the call
// was framed as complete.
func sanitizeJSONArgumentsReport(raw string) (json.RawMessage, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return json.RawMessage("{}"), false
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed), false
	}
	// Strip markdown code fences: ```json ... ``` or ``` ... ```
	if strings.HasPrefix(trimmed, "```") {
		lines := strings.Split(trimmed, "\n")
		if len(lines) >= 2 && strings.HasPrefix(lines[0], "```") {
			lines = lines[1:]
			if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
				lines = lines[:len(lines)-1]
			}
			trimmed = strings.TrimSpace(strings.Join(lines, "\n"))
			if json.Valid([]byte(trimmed)) {
				return json.RawMessage(trimmed), true
			}
		}
	}
	// Extract substring between first '{' and last '}' or '[' and ']'
	startObj := strings.Index(trimmed, "{")
	endObj := strings.LastIndex(trimmed, "}")
	if startObj >= 0 && endObj > startObj {
		candidate := trimmed[startObj : endObj+1]
		if json.Valid([]byte(candidate)) {
			return json.RawMessage(candidate), true
		}
		repaired := repairJSONLiterals(candidate)
		if json.Valid([]byte(repaired)) {
			return json.RawMessage(repaired), true
		}
	}

	startArr := strings.Index(trimmed, "[")
	endArr := strings.LastIndex(trimmed, "]")
	if startArr >= 0 && endArr > startArr {
		candidate := trimmed[startArr : endArr+1]
		if json.Valid([]byte(candidate)) {
			return json.RawMessage(candidate), true
		}
		repaired := repairJSONLiterals(candidate)
		if json.Valid([]byte(repaired)) {
			return json.RawMessage(repaired), true
		}
	}

	repairedDirect := repairJSONLiterals(trimmed)
	if json.Valid([]byte(repairedDirect)) {
		return json.RawMessage(repairedDirect), true
	}

	return json.RawMessage(raw), false
}
