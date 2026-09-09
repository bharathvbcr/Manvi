// Package workflow compiles capabilities and reduces execution events without
// importing a model provider, desktop driver, clock, or network client.
package workflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const SchemaVersion = 1
const MaxArtifactBytes = 1 << 20

type Value struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Integer  int64  `json:"integer,omitempty"`
	Boolean  bool   `json:"boolean,omitempty"`
	Currency string `json:"currency,omitempty"`
}

func (v Value) Validate() error {
	switch v.Type {
	case "string":
		if len(v.Text) > 65536 || v.Integer != 0 || v.Boolean || v.Currency != "" {
			return errors.New("invalid string value")
		}
	case "integer", "money":
		if v.Text != "" || v.Boolean {
			return errors.New("invalid integer value")
		}
		if v.Type == "money" && !currencyPattern.MatchString(v.Currency) {
			return errors.New("money requires an uppercase ISO currency code")
		}
		if v.Type == "integer" && v.Currency != "" {
			return errors.New("integer cannot have currency")
		}
	case "boolean":
		if v.Text != "" || v.Integer != 0 || v.Currency != "" {
			return errors.New("invalid boolean value")
		}
	default:
		return fmt.Errorf("unsupported value type %q", v.Type)
	}
	return nil
}

func (v Value) Display() string {
	switch v.Type {
	case "string":
		return v.Text
	case "boolean":
		if v.Boolean {
			return "true"
		}
		return "false"
	case "money":
		return fmt.Sprintf("%d %s", v.Integer, v.Currency)
	default:
		return fmt.Sprint(v.Integer)
	}
}

type Parameter struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Sensitive bool   `json:"sensitive"`
}
type Ref struct {
	Source  string `json:"source"`
	Key     string `json:"key,omitempty"`
	Literal Value  `json:"literal,omitempty"`
}
type Selector struct {
	Visual     *VisualAnchor `json:"visual,omitempty"`
	Role       string        `json:"role,omitempty"`
	Name       string        `json:"name,omitempty"`
	Identifier string        `json:"identifier,omitempty"`
	Ancestor   string        `json:"ancestor,omitempty"`
}
type Predicate struct {
	Op       string `json:"op"`
	Expected Ref    `json:"expected"`
}
type Step struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Target     string    `json:"target"`
	Effect     string    `json:"effect"`
	Input      Ref       `json:"input"`
	Predicate  Predicate `json:"predicate"`
	Output     string    `json:"output,omitempty"`
	OutputType string    `json:"output_type,omitempty"`
	Currency   string    `json:"currency,omitempty"`
	Next       string    `json:"next,omitempty"`
	Otherwise  string    `json:"otherwise,omitempty"`
}
type Limits struct {
	MaxActions          int `json:"max_actions"`
	ActiveSeconds       int `json:"active_seconds"`
	ObservationAttempts int `json:"observation_attempts"`
	InterventionSeconds int `json:"intervention_seconds"`
}

func DefaultLimits() Limits { return Limits{40, 300, 3, 600} }

type Capability struct {
	SchemaVersion int                 `json:"schema_version"`
	ID            string              `json:"id"`
	Revision      string              `json:"revision"`
	Application   string              `json:"application"`
	Description   string              `json:"description"`
	Parameters    []Parameter         `json:"parameters"`
	Targets       map[string]Selector `json:"targets"`
	Steps         []Step              `json:"steps"`
	Limits        Limits              `json:"limits"`
}

type Program struct {
	raw       []byte
	digest    string
	spec      Capability
	positions map[string]int
}

func (p *Program) Bytes() []byte  { return bytes.Clone(p.raw) }
func (p *Program) Digest() string { return p.digest }
func (p *Program) Capability() Capability {
	c := p.spec
	c.Parameters = append([]Parameter(nil), c.Parameters...)
	c.Steps = append([]Step(nil), c.Steps...)
	c.Targets = make(map[string]Selector, len(p.spec.Targets))
	for k, v := range p.spec.Targets {
		c.Targets[k] = v.Clone()
	}
	return c
}

var namePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,95}$`)
var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// DecodeStrict rejects duplicate keys before decoding; encoding/json by itself
// would silently let the last duplicate change the meaning of an artifact.
func DecodeStrict(raw []byte, destination interface{}) error {
	if len(raw) == 0 || len(raw) > MaxArtifactBytes {
		return errors.New("document is empty or exceeds 1 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := uniqueJSON(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(destination)
}
func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 48 {
		return errors.New("JSON nesting exceeds 48")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			s, ok := key.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if seen[s] {
				return fmt.Errorf("duplicate JSON key %q", s)
			}
			seen[s] = true
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}

func Compile(raw []byte) (*Program, error) {
	var c Capability
	if err := DecodeStrict(raw, &c); err != nil {
		return nil, fmt.Errorf("capability: %w", err)
	}
	if c.SchemaVersion != SchemaVersion {
		return nil, errors.New("unsupported capability schema")
	}
	if !namePattern.MatchString(c.ID) || c.Revision == "" || c.Application == "" {
		return nil, errors.New("capability requires valid id, revision and application")
	}
	if len(c.Steps) == 0 || len(c.Steps) > 128 || len(c.Targets) == 0 || len(c.Targets) > 128 {
		return nil, errors.New("capability requires 1..128 steps and targets")
	}
	l := c.Limits
	if l.MaxActions < 1 || l.MaxActions > 1000 || l.ActiveSeconds < 1 || l.ActiveSeconds > 3600 || l.ObservationAttempts < 1 || l.ObservationAttempts > 5 || l.InterventionSeconds < 1 || l.InterventionSeconds > 3600 {
		return nil, errors.New("limits are missing or outside supported bounds")
	}
	params := map[string]Parameter{}
	if len(c.Parameters) > 128 {
		return nil, errors.New("capability supports at most 128 parameters")
	}
	for _, v := range c.Parameters {
		if !namePattern.MatchString(v.Name) || params[v.Name].Name != "" {
			return nil, errors.New("invalid or duplicate parameter")
		}
		if v.Type != "string" && v.Type != "integer" && v.Type != "boolean" && v.Type != "money" {
			return nil, errors.New("unsupported parameter type")
		}
		params[v.Name] = v
	}
	for k, s := range c.Targets {
		if s.Visual != nil {
			if !namePattern.MatchString(k) || s.Role != "" || s.Name != "" || s.Identifier != "" || s.Ancestor != "" {
				return nil, fmt.Errorf("target %q mixes visual and semantic selection", k)
			}
			if err := s.Visual.Validate(); err != nil {
				return nil, fmt.Errorf("target %q: %w", k, err)
			}
			continue
		}
		if !namePattern.MatchString(k) || (s.Name == "" && s.Identifier == "") || len(s.Name) > 512 || len(s.Identifier) > 512 || len(s.Ancestor) > 512 {
			return nil, fmt.Errorf("target %q needs a bounded name or identifier", k)
		}
	}
	p := &Program{raw: bytes.Clone(raw), spec: c, positions: map[string]int{}}
	for i, s := range c.Steps {
		if !namePattern.MatchString(s.ID) {
			return nil, errors.New("invalid step id")
		}
		if _, ok := p.positions[s.ID]; ok {
			return nil, errors.New("duplicate step id")
		}
		p.positions[s.ID] = i
		if _, ok := c.Targets[s.Target]; !ok {
			return nil, fmt.Errorf("step %s references missing target", s.ID)
		}
		if s.Effect != "read" && s.Effect != "change" {
			return nil, fmt.Errorf("step %s needs explicit effect", s.ID)
		}
		if c.Targets[s.Target].Visual != nil && (s.Kind != "click" || s.Effect != "change") {
			return nil, errors.New("visual targets support only explicitly approved change-effect clicks")
		}
		switch s.Kind {
		case "press", "click":
		case "set_value", "type_text", "scroll":
			if err := validateRef(s.Input, params); err != nil {
				return nil, fmt.Errorf("step %s: %w", s.ID, err)
			}
		case "extract":
			if !namePattern.MatchString(s.Output) || (s.OutputType != "string" && s.OutputType != "integer" && s.OutputType != "money" && s.OutputType != "boolean") {
				return nil, errors.New("extraction needs named typed output")
			}
			if s.OutputType == "money" && !currencyPattern.MatchString(s.Currency) {
				return nil, errors.New("money extraction needs currency")
			}
		case "assert", "branch", "wait":
			if s.Predicate.Op != "exists" {
				if s.Predicate.Op != "equals" && s.Predicate.Op != "contains" && s.Predicate.Op != "not_equals" {
					return nil, errors.New("unsupported predicate")
				}
				if err := validateRef(s.Predicate.Expected, params); err != nil {
					return nil, err
				}
			}
			if s.Kind == "branch" && s.Otherwise == "" {
				return nil, errors.New("branch requires otherwise edge")
			}
		default:
			return nil, fmt.Errorf("unsupported step kind %q", s.Kind)
		}
		if (s.Kind == "extract" || s.Kind == "assert" || s.Kind == "branch" || s.Kind == "wait") && s.Effect != "read" {
			return nil, errors.New("observation steps must be read-only")
		}
	}
	for i, s := range c.Steps {
		for _, edge := range []string{s.Next, s.Otherwise} {
			if edge == "" {
				continue
			}
			j, ok := p.positions[edge]
			if !ok || j <= i {
				return nil, errors.New("edges must name a later step; cycles are forbidden")
			}
		}
		if s.Kind != "branch" && s.Otherwise != "" {
			return nil, errors.New("otherwise is only valid for branches")
		}
	}
	last := c.Steps[len(c.Steps)-1]
	if last.Kind != "assert" && last.Kind != "extract" && last.Kind != "wait" {
		return nil, errors.New("capability must finish with an observed checkpoint")
	}
	if err := validateFlow(p, params); err != nil {
		return nil, err
	}
	h := sha256.Sum256(raw)
	p.digest = hex.EncodeToString(h[:])
	return p, nil
}

func validateRef(r Ref, params map[string]Parameter) error {
	if r.Source != "literal" && r.Literal != (Value{}) {
		return errors.New("input and output references cannot carry a literal")
	}
	switch r.Source {
	case "literal":
		if r.Key != "" {
			return errors.New("literal cannot have a key")
		}
		return r.Literal.Validate()
	case "input":
		if _, ok := params[r.Key]; !ok {
			return errors.New("unknown input reference")
		}
	case "output":
		if !namePattern.MatchString(r.Key) {
			return errors.New("invalid output reference")
		}
	default:
		return errors.New("reference source must be literal, input, or output")
	}
	return nil
}

func (p *Program) ValidateInputs(inputs map[string]Value) error {
	if len(inputs) != len(p.spec.Parameters) {
		return errors.New("inputs do not exactly match parameters")
	}
	for _, parameter := range p.spec.Parameters {
		v, ok := inputs[parameter.Name]
		if !ok || v.Type != parameter.Type {
			return fmt.Errorf("parameter %s has missing or wrong type", parameter.Name)
		}
		if err := v.Validate(); err != nil {
			return fmt.Errorf("parameter %s: %w", parameter.Name, err)
		}
	}
	return nil
}

func Resolve(r Ref, inputs, outputs map[string]Value) (Value, error) {
	switch r.Source {
	case "literal":
		return r.Literal, r.Literal.Validate()
	case "input":
		v, ok := inputs[r.Key]
		if !ok {
			return Value{}, errors.New("input binding unavailable")
		}
		return v, nil
	case "output":
		v, ok := outputs[r.Key]
		if !ok {
			return Value{}, errors.New("output binding unavailable")
		}
		return v, nil
	default:
		return Value{}, errors.New("invalid reference")
	}
}

func Compare(actual Value, op string, expected Value) bool {
	if op == "exists" {
		return actual.Type != ""
	}
	if actual.Type != expected.Type {
		return false
	}
	switch op {
	case "equals":
		return actual == expected
	case "not_equals":
		return actual != expected
	case "contains":
		return actual.Type == "string" && strings.Contains(actual.Text, expected.Text)
	}
	return false
}
