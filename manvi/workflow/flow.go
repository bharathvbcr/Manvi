package workflow

import (
	"errors"
	"fmt"
)

// Edges only move forward, so a single pass can intersect definitions from all
// predecessors. An output must exist on every path reaching its use; merely
// appearing earlier in the artifact does not establish that a branch produced it.
func validateFlow(p *Program, params map[string]Parameter) error {
	steps := p.spec.Steps
	predecessors := make([][]int, len(steps))
	outputs := map[string]bool{}
	for i, s := range steps {
		if s.Kind == "extract" {
			if outputs[s.Output] {
				return fmt.Errorf("output %q has more than one definition", s.Output)
			}
			outputs[s.Output] = true
			if s.OutputType != "money" && s.Currency != "" {
				return errors.New("only money outputs can declare currency")
			}
		}
		next := i + 1
		if s.Next != "" {
			next = p.positions[s.Next]
		}
		if next < len(steps) {
			predecessors[next] = append(predecessors[next], i)
		}
		if s.Kind == "branch" {
			otherwise := p.positions[s.Otherwise]
			if otherwise != next {
				predecessors[otherwise] = append(predecessors[otherwise], i)
			}
		}
	}
	definitions := make([]map[string]string, len(steps))
	for i, s := range steps {
		available := map[string]string{}
		if i > 0 {
			if len(predecessors[i]) == 0 {
				return fmt.Errorf("step %s is unreachable", s.ID)
			}
			for name, typ := range definitions[predecessors[i][0]] {
				available[name] = typ
			}
			for _, pred := range predecessors[i][1:] {
				for name, typ := range available {
					if definitions[pred][name] != typ {
						delete(available, name)
					}
				}
			}
		}
		refType := func(r Ref) (string, error) {
			switch r.Source {
			case "input":
				return params[r.Key].Type, nil
			case "literal":
				return r.Literal.Type, nil
			case "output":
				typ, ok := available[r.Key]
				if !ok {
					return "", fmt.Errorf("step %s uses output %q before it is defined on every path", s.ID, r.Key)
				}
				return typ, nil
			default:
				return "", errors.New("invalid reference source")
			}
		}
		switch s.Kind {
		case "set_value", "type_text", "scroll":
			typ, err := refType(s.Input)
			if err != nil {
				return err
			}
			if s.Kind == "scroll" {
				if typ != "integer" {
					return errors.New("scroll input must be an integer")
				}
				if s.Input.Source == "literal" && (s.Input.Literal.Integer == 0 || s.Input.Literal.Integer < -20 || s.Input.Literal.Integer > 20) {
					return errors.New("scroll input must be -20..-1 or 1..20")
				}
			}
		case "assert", "branch", "wait":
			if s.Predicate.Op != "exists" {
				typ, err := refType(s.Predicate.Expected)
				if err != nil {
					return err
				}
				if s.Predicate.Op == "contains" && typ != "string" {
					return errors.New("contains requires a string expectation")
				}
			}
		}
		if s.Kind == "extract" {
			available[s.Output] = s.OutputType
		}
		definitions[i] = available
	}
	return nil
}
