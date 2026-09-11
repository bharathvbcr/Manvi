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

// Target is an alias for Selector so map[string]Target and map[string]Selector
// remain interchangeable for existing fixtures.
type Target = Selector

type Selector struct {
	Visual     *VisualAnchor `json:"visual,omitempty"`
	Role       string        `json:"role,omitempty"`
	Name       string        `json:"name,omitempty"`
	Identifier string        `json:"identifier,omitempty"`
	Ancestor   string        `json:"ancestor,omitempty"`
	Strategies []Selector    `json:"strategies,omitempty"`
	Rationale  string        `json:"rationale,omitempty"`
	Stability  string        `json:"stability,omitempty"`
}

// Primary returns the first ladder rung, or the flat shorthand selector itself.
func (s Selector) Primary() Selector {
	ladder := s.Ladder()
	if len(ladder) == 0 {
		return Selector{}
	}
	return ladder[0]
}

// Ladder returns strategies when present; otherwise a single flat shorthand rung.
func (s Selector) Ladder() []Selector {
	if len(s.Strategies) > 0 {
		return s.Strategies
	}
	flat := s
	flat.Strategies = nil
	flat.Rationale = ""
	flat.Stability = ""
	return []Selector{flat}
}

type Predicate struct {
	Op       string `json:"op"`
	Expected Ref    `json:"expected"`
}
type OutputDecl struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Currency    string `json:"currency,omitempty"`
	Description string `json:"description"`
}
type OutcomeDef struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Description string   `json:"description"`
	Outputs     []string `json:"outputs"`
}
type RecoveryWhen struct {
	Target    string    `json:"target"`
	Predicate Predicate `json:"predicate"`
}
type RecoveryAction struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
}
type Recovery struct {
	ID     string         `json:"id"`
	When   RecoveryWhen   `json:"when"`
	Action RecoveryAction `json:"action"`
	Max    int            `json:"max"`
}
type Step struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Target     string    `json:"target,omitempty"`
	Effect     string    `json:"effect,omitempty"`
	Input      Ref       `json:"input"`
	Predicate  Predicate `json:"predicate"`
	Output     string    `json:"output,omitempty"`
	OutputType string    `json:"output_type,omitempty"`
	Currency   string    `json:"currency,omitempty"`
	Outcome    string    `json:"outcome,omitempty"`
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
	Outputs       []OutputDecl        `json:"outputs,omitempty"`
	Outcomes      []OutcomeDef        `json:"outcomes,omitempty"`
	Recoveries    []Recovery          `json:"recoveries,omitempty"`
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
	c.Outputs = append([]OutputDecl(nil), c.Outputs...)
	c.Outcomes = make([]OutcomeDef, len(c.Outcomes))
	for i, o := range p.spec.Outcomes {
		o.Outputs = append([]string(nil), o.Outputs...)
		c.Outcomes[i] = o
	}
	c.Recoveries = append([]Recovery(nil), c.Recoveries...)
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
	if err := validateLimits(c.Limits); err != nil {
		return nil, err
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
	outputDecls := map[string]OutputDecl{}
	for _, o := range c.Outputs {
		if !namePattern.MatchString(o.Name) || outputDecls[o.Name].Name != "" {
			return nil, errors.New("invalid or duplicate output declaration")
		}
		if o.Type != "string" && o.Type != "integer" && o.Type != "boolean" && o.Type != "money" {
			return nil, errors.New("unsupported output declaration type")
		}
		if o.Type == "money" {
			if !currencyPattern.MatchString(o.Currency) {
				return nil, errors.New("money output declaration needs currency")
			}
		} else if o.Currency != "" {
			return nil, errors.New("only money outputs can declare currency")
		}
		outputDecls[o.Name] = o
	}
	outcomes := map[string]OutcomeDef{}
	for _, o := range c.Outcomes {
		if !namePattern.MatchString(o.ID) || outcomes[o.ID].ID != "" {
			return nil, errors.New("invalid or duplicate outcome")
		}
		if o.Kind != "success" && o.Kind != "business" {
			return nil, errors.New("outcome kind must be success or business")
		}
		seenOut := map[string]bool{}
		for _, name := range o.Outputs {
			if seenOut[name] {
				return nil, fmt.Errorf("outcome %q repeats output %q", o.ID, name)
			}
			seenOut[name] = true
			if len(outputDecls) > 0 {
				if _, ok := outputDecls[name]; !ok {
					return nil, fmt.Errorf("outcome %q references undeclared output %q", o.ID, name)
				}
			} else if !namePattern.MatchString(name) {
				return nil, errors.New("invalid outcome output name")
			}
		}
		outcomes[o.ID] = o
	}
	for k, s := range c.Targets {
		if !namePattern.MatchString(k) {
			return nil, fmt.Errorf("target %q needs a valid name", k)
		}
		if err := validateTargetSelector(k, s); err != nil {
			return nil, err
		}
	}
	recoveries := map[string]Recovery{}
	if len(c.Recoveries) > 64 {
		return nil, errors.New("capability supports at most 64 recoveries")
	}
	for _, r := range c.Recoveries {
		if !namePattern.MatchString(r.ID) || recoveries[r.ID].ID != "" {
			return nil, errors.New("invalid or duplicate recovery")
		}
		if _, ok := c.Targets[r.When.Target]; !ok {
			return nil, fmt.Errorf("recovery %s references missing when.target", r.ID)
		}
		if r.Action.Kind != "press" {
			return nil, fmt.Errorf("recovery %s action must be press", r.ID)
		}
		if _, ok := c.Targets[r.Action.Target]; !ok {
			return nil, fmt.Errorf("recovery %s references missing action.target", r.ID)
		}
		if r.Max < 1 || r.Max > 5 {
			return nil, fmt.Errorf("recovery %s max must be 1..5", r.ID)
		}
		if err := validatePredicate(r.When.Predicate, params); err != nil {
			return nil, fmt.Errorf("recovery %s: %w", r.ID, err)
		}
		recoveries[r.ID] = r
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
		if s.Kind == "conclude" {
			if s.Target != "" || s.Effect != "" {
				return nil, fmt.Errorf("conclude step %s must not set target or effect", s.ID)
			}
			if s.Outcome == "" || outcomes[s.Outcome].ID == "" {
				return nil, fmt.Errorf("conclude step %s needs a declared outcome", s.ID)
			}
			if s.Otherwise != "" {
				return nil, errors.New("otherwise is only valid for branches")
			}
			continue
		}
		if _, ok := c.Targets[s.Target]; !ok {
			return nil, fmt.Errorf("step %s references missing target", s.ID)
		}
		if s.Effect != "read" && s.Effect != "change" {
			return nil, fmt.Errorf("step %s needs explicit effect", s.ID)
		}
		if primary := c.Targets[s.Target].Primary(); primary.Visual != nil && (s.Kind != "click" || s.Effect != "change") {
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
			if decl, ok := outputDecls[s.Output]; ok {
				if decl.Type != s.OutputType {
					return nil, fmt.Errorf("extract %s type does not match output declaration", s.ID)
				}
				if decl.Type == "money" && decl.Currency != s.Currency {
					return nil, fmt.Errorf("extract %s currency does not match output declaration", s.ID)
				}
			}
		case "assert", "branch", "wait":
			if err := validatePredicate(s.Predicate, params); err != nil {
				return nil, err
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
		if s.Outcome != "" {
			return nil, fmt.Errorf("step %s cannot set outcome unless conclude", s.ID)
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
	if last.Kind != "assert" && last.Kind != "extract" && last.Kind != "wait" && last.Kind != "conclude" {
		return nil, errors.New("capability must finish with an observed checkpoint")
	}
	if err := validateFlow(p, params); err != nil {
		return nil, err
	}
	h := sha256.Sum256(raw)
	p.digest = hex.EncodeToString(h[:])
	return p, nil
}

func validateTargetSelector(name string, s Selector) error {
	if len(s.Strategies) > 0 {
		if s.Role != "" || s.Name != "" || s.Identifier != "" || s.Ancestor != "" || s.Visual != nil {
			return fmt.Errorf("target %q mixes flat selector fields with strategies", name)
		}
		if s.Rationale == "" {
			return fmt.Errorf("target %q strategies require rationale", name)
		}
		switch s.Stability {
		case "semantic", "identifier", "visual":
		default:
			return fmt.Errorf("target %q stability must be semantic, identifier, or visual", name)
		}
		for i, rung := range s.Strategies {
			if len(rung.Strategies) > 0 || rung.Rationale != "" || rung.Stability != "" {
				return fmt.Errorf("target %q rung %d must not nest strategies", name, i)
			}
			if err := validateFlatSelector(name, rung); err != nil {
				return err
			}
		}
		return nil
	}
	if s.Rationale != "" || s.Stability != "" {
		return fmt.Errorf("target %q has empty ladder", name)
	}
	return validateFlatSelector(name, s)
}

func validateFlatSelector(name string, s Selector) error {
	if s.Visual != nil {
		if s.Role != "" || s.Name != "" || s.Identifier != "" || s.Ancestor != "" {
			return fmt.Errorf("target %q mixes visual and semantic selection", name)
		}
		if err := s.Visual.Validate(); err != nil {
			return fmt.Errorf("target %q: %w", name, err)
		}
		return nil
	}
	if (s.Name == "" && s.Identifier == "") || len(s.Name) > 512 || len(s.Identifier) > 512 || len(s.Ancestor) > 512 {
		return fmt.Errorf("target %q needs a bounded name or identifier", name)
	}
	return nil
}

func validatePredicate(pred Predicate, params map[string]Parameter) error {
	if pred.Op == "exists" {
		return nil
	}
	if pred.Op != "equals" && pred.Op != "contains" && pred.Op != "not_equals" {
		return errors.New("unsupported predicate")
	}
	return validateRef(pred.Expected, params)
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
