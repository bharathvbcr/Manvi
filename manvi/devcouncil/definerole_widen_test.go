package devcouncil

import (
	"strings"
	"testing"
)

// A model defining a role may declare only the fields authoredRole names.
// write_exceptions is the one agents.Definition field that widens a role, and
// the model's JSON used to decode straight into agents.Definition.
func TestDefineSubagentRefusesFieldsAModelMayNotDeclare(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"write_exceptions":    {"write_exceptions": []string{"devcouncil_create_artifact"}},
		"an unknown field":    {"max_spend": 1000},
		"a wide write grant":  {"write_exceptions": []string{"write_file", "bash"}},
		"an empty exceptions": {"write_exceptions": []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			f, roles := roleFixture(t, nil, nil)
			args := map[string]any{
				"name":               "sneaky",
				"description":        "a read-only role that grants itself a write",
				"system_prompt":      "You plan.",
				"enable_write_tools": false,
			}
			for k, v := range extra {
				args[k] = v
			}
			res := f.call("devcouncil_define_subagent", args)
			if !res.IsError {
				t.Fatalf("a model-supplied %s was accepted: %s", name, res.Text)
			}
			if !strings.Contains(res.Text, "Nothing was registered") {
				t.Errorf("the refusal does not say nothing was registered: %q", res.Text)
			}
			if _, defined := roles.Get("sneaky"); defined {
				t.Fatal("the role was persisted despite the refusal")
			}
		})
	}
}

// The narrow decode must not break an ordinary definition, and the role it
// persists must carry no write exception.
func TestDefineSubagentStillDefinesAnOrdinaryRole(t *testing.T) {
	f, roles := roleFixture(t, nil, nil)
	res := f.call("devcouncil_define_subagent", map[string]any{
		"name":               "auditor",
		"description":        "reads things",
		"system_prompt":      "You audit.",
		"enable_write_tools": false,
		"allowed_tools":      []string{"read_file"},
	})
	if res.IsError {
		t.Fatalf("an ordinary definition was refused: %s", res.Text)
	}
	def, ok := roles.Get("auditor")
	if !ok {
		t.Fatal("the role was not registered")
	}
	if len(def.WriteExceptions) != 0 {
		t.Fatalf("a model-defined role carries write exceptions: %v", def.WriteExceptions)
	}
}
