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

func repairJSONLiterals(s string) string {
	repaired := strings.ReplaceAll(s, ",\n}", "\n}")
	repaired = strings.ReplaceAll(repaired, ", }", "}")
	repaired = strings.ReplaceAll(repaired, ",}", "}")
	repaired = strings.ReplaceAll(repaired, ",\n]", "\n]")
	repaired = strings.ReplaceAll(repaired, ", ]", "]")
	repaired = strings.ReplaceAll(repaired, ",]", "]")

	repaired = replaceWordLiteral(repaired, "True", "true")
	repaired = replaceWordLiteral(repaired, "False", "false")
	repaired = replaceWordLiteral(repaired, "None", "null")
	return repaired
}

func sanitizeJSONArguments(raw string) json.RawMessage {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
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
				return json.RawMessage(trimmed)
			}
		}
	}
	// Extract substring between first '{' and last '}' or '[' and ']'
	startObj := strings.Index(trimmed, "{")
	endObj := strings.LastIndex(trimmed, "}")
	if startObj >= 0 && endObj > startObj {
		candidate := trimmed[startObj : endObj+1]
		if json.Valid([]byte(candidate)) {
			return json.RawMessage(candidate)
		}
		repaired := repairJSONLiterals(candidate)
		if json.Valid([]byte(repaired)) {
			return json.RawMessage(repaired)
		}
	}

	startArr := strings.Index(trimmed, "[")
	endArr := strings.LastIndex(trimmed, "]")
	if startArr >= 0 && endArr > startArr {
		candidate := trimmed[startArr : endArr+1]
		if json.Valid([]byte(candidate)) {
			return json.RawMessage(candidate)
		}
		repaired := repairJSONLiterals(candidate)
		if json.Valid([]byte(repaired)) {
			return json.RawMessage(repaired)
		}
	}

	repairedDirect := repairJSONLiterals(trimmed)
	if json.Valid([]byte(repairedDirect)) {
		return json.RawMessage(repairedDirect)
	}

	return json.RawMessage(raw)
}
