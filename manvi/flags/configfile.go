package flags

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// DefaultConfigFile is the config file's name inside the state directory.
const DefaultConfigFile = "config.yaml"

// LoadConfigFile applies path as the config layer, reporting whether a file was
// there at all.
func LoadConfigFile(r *Registry, path string) (bool, error) {
	if strings.TrimSpace(path) == "" {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("flags: reading %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	values, err := ParseConfig(f)
	if err != nil {
		return true, fmt.Errorf("%s: %w", path, err)
	}
	if err := r.LoadConfig(values); err != nil {
		return true, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

type indentEntry struct {
	indent int
	key    string
}

// ParseConfig reads YAML configuration files in either flat dotted form (llm.local.model: ...)
// or hierarchical nested form (llm:\n  local:\n    model: ...), supporting lists, flow collections,
// multiline quoted values, numbers, booleans, nulls, and comments.
func ParseConfig(src io.Reader) (map[string]string, error) {
	values := map[string]string{}
	seen := map[string]int{}

	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)

	var (
		stack              []indentEntry
		inMultiline        bool
		multilineKey       string
		multilineQuote     byte
		multilineStartLine int
		multilineBuf       strings.Builder
	)

	for line := 1; scanner.Scan(); line++ {
		raw := scanner.Text()
		if line == 1 {
			raw = strings.TrimPrefix(raw, "\ufeff")
		}

		if inMultiline {
			idx := strings.IndexByte(raw, multilineQuote)
			if idx >= 0 {
				multilineBuf.WriteString(" ")
				multilineBuf.WriteString(strings.TrimSpace(raw[:idx]))
				inMultiline = false
				values[multilineKey] = multilineBuf.String()
				multilineBuf.Reset()
				continue
			}
			multilineBuf.WriteString(" ")
			multilineBuf.WriteString(strings.TrimSpace(raw))
			continue
		}

		// Calculate indentation
		indent := 0
		trimmedLeft := strings.TrimLeftFunc(raw, func(r rune) bool {
			if r == ' ' {
				indent++
				return true
			}
			if r == '\t' {
				indent += 2
				return true
			}
			return false
		})

		trimmed := strings.TrimRight(trimmedLeft, " \t\r\n")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "---" || trimmed == "..." {
			continue
		}

		// Adjust stack based on indentation
		for len(stack) > 0 && indent <= stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}

		// Check for block list item, e.g. "- item"
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			continue
		}

		keyPart, rest, ok := strings.Cut(trimmed, ":")
		if !ok {
			return nil, fmt.Errorf("line %d: no \":\" found; every line is a setting written as key: value", line)
		}
		keyPart = strings.TrimSpace(keyPart)
		if keyPart == "" {
			return nil, fmt.Errorf("line %d: the key is empty", line)
		}
		if (strings.HasPrefix(keyPart, `"`) && strings.HasSuffix(keyPart, `"`)) ||
			(strings.HasPrefix(keyPart, `'`) && strings.HasSuffix(keyPart, `'`)) {
			keyPart = keyPart[1 : len(keyPart)-1]
		}

		rest = strings.TrimSpace(rest)

		// Parent section (nested dictionary)
		if rest == "" || strings.HasPrefix(rest, "#") {
			stack = append(stack, indentEntry{indent: indent, key: keyPart})
			continue
		}

		// Leaf key
		var fullKey string
		if len(stack) > 0 {
			var keys []string
			for _, e := range stack {
				keys = append(keys, e.key)
			}
			keys = append(keys, keyPart)
			fullKey = strings.Join(keys, ".")
		} else {
			fullKey = keyPart
		}

		val, isMulti, quoteByte, err := parseYAMLValue(rest)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}

		if isMulti {
			inMultiline = true
			multilineKey = fullKey
			multilineQuote = quoteByte
			multilineStartLine = line
			multilineBuf.Reset()
			multilineBuf.WriteString(val)
			seen[fullKey] = line
			continue
		}

		if first, dup := seen[fullKey]; dup {
			return nil, fmt.Errorf("line %d: %q is already set on line %d", line, fullKey, first)
		}
		seen[fullKey] = line
		values[fullKey] = val
	}

	if inMultiline {
		return nil, fmt.Errorf("line %d: the %q quote is never closed", multilineStartLine, string(multilineQuote))
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func parseYAMLValue(rest string) (val string, isMultiline bool, quoteByte byte, err error) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", false, 0, nil
	}

	if rest[0] == '\'' || rest[0] == '"' {
		quote := rest[0]
		end := strings.IndexByte(rest[1:], quote)
		if end < 0 {
			return rest[1:], true, quote, nil
		}
		body := rest[1 : 1+end]
		tail := strings.TrimSpace(rest[2+end:])
		if tail != "" && !strings.HasPrefix(tail, "#") {
			return "", false, 0, fmt.Errorf("unexpected %q after the closing quote", tail)
		}
		return body, false, 0, nil
	}

	if rest == "[]" || rest == "{}" || rest == "null" || rest == "~" || rest == "None" {
		return "", false, 0, nil
	}

	if i := indexInlineComment(rest); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimRight(rest, " \t"), false, 0, nil
}

func indexInlineComment(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return i
		}
	}
	return -1
}

// ConfiguredKeys lists the keys currently supplied by the config layer, sorted.
func (r *Registry) ConfiguredKeys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.config))
	for k := range r.config {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
