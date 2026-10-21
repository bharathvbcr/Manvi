package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/prompt"
	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

// A Spec is a harness as data: the prose, skill selection, lessons library and
// roles a repository or a host wants a session to carry, in one document that
// is validated, digested and resolved once per turn.
//
// It is deliberately small. It has no field for a flag, a tier, a posture, a
// verify command, a fetch host, an MCP server, a hook, a script or a write
// exception, so a spec cannot carry them: the strict decoder refuses each by
// name, and Validate cannot be asked about what the type cannot say. A spec
// adds text and narrows roles. It never touches the gate.
type Spec struct {
	// Name identifies the spec: the name the harness.spec setting selects, and
	// the file name of a repository spec.
	Name string `json:"name"`
	// Sections are extra prompt sections. They are never Essential and sit at
	// Order >= MinSectionOrder, after every fixed section.
	Sections []Section `json:"sections,omitempty"`
	// Roles are sub-agent roles no wider than the session that registers them.
	Roles []RoleSpec `json:"roles,omitempty"`
	// Skills selects which repository skills the prompt lists. Empty lists all.
	// It filters; it never adds a skill body.
	Skills []string `json:"skills,omitempty"`
	// Lessons names a lessons library. It is recorded in the digest and not yet
	// loaded into the prompt: there is no retrieval for it in this release.
	Lessons string `json:"lessons,omitempty"`
}

// Section is one prompt section a spec contributes.
type Section struct {
	Name  string `json:"name"`
	Order int    `json:"order"`
	Text  string `json:"text"`
}

// RoleSpec is the part of a sub-agent role a spec may set. It is not
// agents.Definition on purpose: that type carries a tier, a model, MCP access
// and write exceptions, and a spec setting any of them would be a spec choosing
// what a session spends or reaches.
type RoleSpec struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	SystemPrompt string `json:"system_prompt,omitempty"`
	// AllowedTools only removes from the session's own tools.
	AllowedTools []string `json:"allowed_tools,omitempty"`
	// ReadOnly lowers the role below the session's write floor. There is no
	// way to raise it.
	ReadOnly bool `json:"read_only,omitempty"`
}

// MaxSpecBytes bounds one spec document before it is parsed.
const MaxSpecBytes = 256 * 1024

// forbiddenKeys are the fields a spec must not carry, with why. Strict decoding
// would refuse each as unknown anyway; naming them says that the omission is
// the rule and not an oversight in this release.
var forbiddenKeys = map[string]string{
	"write_exceptions":   "it is the only field that widens a role, and a spec never widens",
	"enable_write_tools": "a role's write floor can only be lowered, with read_only",
	"enable_mcp_tools":   "a spec cannot reach MCP servers",
	"mcp":                "a spec cannot reach MCP servers",
	"tier":               "tiers decide spend and are operator scope",
	"tiers":              "tiers decide spend and are operator scope",
	"model":              "a spec cannot choose a model",
	"posture":            "a spec cannot move the gate",
	"flags":              "a spec cannot set flags",
	"flag":               "a spec cannot set flags",
	"verify":             "a spec cannot set the verify command",
	"verify_command":     "a spec cannot set the verify command",
	"fetch":              "a spec cannot name fetch hosts",
	"fetch_hosts":        "a spec cannot name fetch hosts",
	"hooks":              "a spec is data and is never executed",
	"scripts":            "a spec is data and is never executed",
	"plugins":            "a spec is data and is never executed",
}

// DecodeSpec strictly decodes one spec document. Unknown fields, duplicate keys
// and trailing data are refused, with the same decoder the workflow overlays
// use. It decodes; Validate judges.
func DecodeSpec(raw []byte) (Spec, error) {
	if len(raw) == 0 || len(raw) > MaxSpecBytes {
		return Spec{}, fmt.Errorf("harness spec: document is empty or exceeds %d bytes", MaxSpecBytes)
	}
	if err := refuseForbiddenKeys(raw); err != nil {
		return Spec{}, err
	}
	var s Spec
	if err := workflow.DecodeStrict(raw, &s); err != nil {
		return Spec{}, fmt.Errorf("harness spec: %w", err)
	}
	return s, nil
}

// refuseForbiddenKeys names every forbidden key in the document, at the top
// level and inside each role. Keys are compared case-insensitively because the
// JSON decoder matches field names that way. Malformed JSON passes through: the
// strict decoder reports it.
func refuseForbiddenKeys(raw []byte) error {
	found := forbiddenKeyFindings(raw)
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return fmt.Errorf("harness spec: %s", strings.Join(found, "; "))
}

// forbiddenKeyFindings is the list refuseForbiddenKeys reports; nil for a
// document that is not a JSON object, which the strict decoder then rejects.
func forbiddenKeyFindings(raw []byte) []string {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return nil
	}
	var found []string
	scan := func(where string, obj map[string]json.RawMessage) {
		for key := range obj {
			if why, bad := forbiddenKeys[strings.ToLower(key)]; bad {
				found = append(found, fmt.Sprintf("%s%q is not allowed in a harness spec: %s", where, key, why))
			}
		}
	}
	scan("", top)
	for k, v := range top {
		if !strings.EqualFold(k, "roles") {
			continue
		}
		var roles []map[string]json.RawMessage
		if json.Unmarshal(v, &roles) != nil {
			continue
		}
		for i, role := range roles {
			scan(fmt.Sprintf("roles[%d]: ", i), role)
		}
	}
	return found
}

// normalized returns a copy with every list in its canonical order.
func (s Spec) normalized() Spec {
	n := Spec{Name: s.Name, Lessons: s.Lessons}
	n.Sections = append([]Section(nil), s.Sections...)
	sort.SliceStable(n.Sections, func(i, j int) bool {
		if n.Sections[i].Order != n.Sections[j].Order {
			return n.Sections[i].Order < n.Sections[j].Order
		}
		return n.Sections[i].Name < n.Sections[j].Name
	})
	for _, r := range s.Roles {
		r.AllowedTools = append([]string(nil), r.AllowedTools...)
		sort.Strings(r.AllowedTools)
		n.Roles = append(n.Roles, r)
	}
	sort.SliceStable(n.Roles, func(i, j int) bool { return n.Roles[i].Name < n.Roles[j].Name })
	n.Skills = append([]string(nil), s.Skills...)
	sort.Strings(n.Skills)
	return n
}

// Canonical is the one byte form a spec has: struct field order, lists sorted
// (sections by order then name, roles by name, skills and tool names
// alphabetically), no insignificant whitespace, no optional field spelled when
// empty. Listing order is therefore not content, and any other change is.
// Decoding Canonical and encoding again is the identity.
func (s Spec) Canonical() []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// A struct of strings, ints, bools and slices of them cannot fail to marshal.
	_ = enc.Encode(s.normalized())
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// Digest is the hex SHA-256 of Canonical. The session log records it beside the
// spec's name, so an edit between two turns is visible as a changed digest.
func (s Spec) Digest() string {
	sum := sha256.Sum256(s.Canonical())
	return hex.EncodeToString(sum[:])
}

// Sources turns the spec's sections into prompt sources: non-Essential, at the
// spec's own order. The assembler refuses a duplicate source name, so a section
// can never displace a fixed one, and a section that does not fit is dropped
// before any Essential one is.
func (s Spec) Sources() []prompt.Source {
	n := s.normalized()
	out := make([]prompt.Source, 0, len(n.Sections))
	for _, sec := range n.Sections {
		out = append(out, prompt.Static(sec.Name, sec.Order, false, sec.Text))
	}
	return out
}
