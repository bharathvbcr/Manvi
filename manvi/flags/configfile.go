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
	// line is where this key was written, so a key that turns out to hold
	// nothing can be reported at the line an operator has to go and fix.
	line int
	// hadContent records whether anything was nested under this key: a deeper
	// key, a value, or a list item. A key with nothing under it was never a
	// section — it is a setting whose value is missing, and closeSection is
	// where that is finally decided.
	hadContent bool
}

// fullKeyOf joins the open sections with a leaf to make the dotted key the
// registry is addressed by.
func fullKeyOf(stack []indentEntry, leaf string) string {
	if len(stack) == 0 {
		return leaf
	}
	keys := make([]string, 0, len(stack)+1)
	for _, e := range stack {
		keys = append(keys, e.key)
	}
	return strings.Join(append(keys, leaf), ".")
}

// reserve claims a key for one line and refuses a second claim on it.
//
// It is called at the point a key is *claimed* rather than at the point its
// value is stored, and that is the whole of the fix it carries. The duplicate
// check used to sit on the single-line branch only, so a repeated key whose
// second value opened a quoted multi-line string skipped it entirely and the
// later value silently won: `harness.posture: strict` followed by
// `harness.posture: "yolo` resolved to yolo, while the identical file with an
// unquoted second value was refused. A guard the parser deliberately
// implements must not be escapable by the shape of the value, least of all in
// the direction that relaxes a safety flag.
func reserve(seen map[string]int, key string, line int) error {
	if first, dup := seen[key]; dup {
		return fmt.Errorf("line %d: %q is already set on line %d", line, key, first)
	}
	seen[key] = line
	return nil
}

// closeSection pops the innermost open key and records it as a valueless
// setting when nothing was ever nested under it.
//
// `policy.file.mode:` with the value lost to a truncated edit, a templating
// bug, or a YAML anchor that resolved to nothing used to be read as the opening
// of a section, pushed, and then dropped when nothing arrived under it. The
// flag kept its default, the file that named it changed nothing, and no error
// said so — the package's own rule is that a key which quietly does nothing is
// the same class of defect as a check that could not run reporting success.
//
// It is recorded as an empty value rather than refused here because this parser
// does not know which keys are the harness's. The registry does: a defined key
// gets its own validation — an enum or a bool rejects "" by name — and a key in
// a harness namespace that is not defined is already reported as unknown, while
// a foreign section in a shared `.devcouncil/config.yaml` stays ignored, which
// is the whole reason foreign keys are tolerated at all.
func closeSection(stack []indentEntry, values map[string]string, seen map[string]int) ([]indentEntry, error) {
	top := stack[len(stack)-1]
	stack = stack[:len(stack)-1]
	if top.hadContent {
		return stack, nil
	}
	key := fullKeyOf(stack, top.key)
	if err := reserve(seen, key, top.line); err != nil {
		return nil, err
	}
	values[key] = ""
	return stack, nil
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

		// A block list item is content belonging to the key above it, and it is
		// marked before the stack is unwound because YAML lets it sit at the
		// same indentation as that key: `roles:` followed by `- a` at column
		// zero pops `roles` on the line that proves it has something in it.
		// Lists are not represented here — every flag is a scalar — but a key
		// that has one must not be mistaken for a key that has nothing, which
		// is what closeSection would otherwise report it as.
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			if len(stack) > 0 {
				stack[len(stack)-1].hadContent = true
			}
			continue
		}

		// Adjust stack based on indentation
		for len(stack) > 0 && indent <= stack[len(stack)-1].indent {
			var err error
			if stack, err = closeSection(stack, values, seen); err != nil {
				return nil, err
			}
		}
		if len(stack) > 0 {
			stack[len(stack)-1].hadContent = true
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

		// Nothing after the colon. Which of the two things this is — a section
		// with keys under it, or a setting whose value went missing — is not
		// decidable here; it is decided by whether anything is nested under it,
		// so the key is pushed and closeSection settles it.
		if rest == "" || strings.HasPrefix(rest, "#") {
			stack = append(stack, indentEntry{indent: indent, key: keyPart, line: line})
			continue
		}

		fullKey := fullKeyOf(stack, keyPart)

		val, isMulti, quoteByte, err := parseYAMLValue(rest)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}

		if err := reserve(seen, fullKey, line); err != nil {
			return nil, err
		}

		if isMulti {
			inMultiline = true
			multilineKey = fullKey
			multilineQuote = quoteByte
			multilineStartLine = line
			multilineBuf.Reset()
			multilineBuf.WriteString(val)
			continue
		}

		values[fullKey] = val
	}

	if inMultiline {
		return nil, fmt.Errorf("line %d: the %q quote is never closed", multilineStartLine, string(multilineQuote))
	}

	// Checked before the open sections are closed: a scan that stopped early —
	// a line over the buffer cap, an unreadable file — leaves a stack that was
	// never finished, and reporting a key as valueless because the reader gave
	// up before reaching its contents would name the wrong fault.
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// End of file closes whatever is still open, so a trailing key with nothing
	// under it is judged by the same rule as one in the middle.
	for len(stack) > 0 {
		var err error
		if stack, err = closeSection(stack, values, seen); err != nil {
			return nil, err
		}
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
