package devcouncil

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

func advRawDefine(f *fixture, raw string) tools.Result {
	f.t.Helper()
	return f.pipe.Run(context.Background(), tools.Call{ID: "c1", Name: "devcouncil_define_subagent", Arguments: json.RawMessage(raw)})
}

// advNoWidening fails when any role other than the shipped planner carries a
// write exception, or when "sneaky" exists at all.
func advNoWidening(t *testing.T, roles *agents.Registry, mustNotExist ...string) {
	t.Helper()
	for _, d := range roles.List() {
		if len(d.WriteExceptions) != 0 && d.Name != "planner" {
			t.Errorf("role %q carries write exceptions %v", d.Name, d.WriteExceptions)
		}
	}
	for _, n := range mustNotExist {
		if _, ok := roles.Get(n); ok {
			t.Errorf("role %q was registered", n)
		}
	}
}

func TestAdvB29RuntimeDefineRefusesWriteExceptionsSpelledAnyWay(t *testing.T) {
	for name, key := range map[string]string{
		"title case":      "Write_Exceptions",
		"upper":           "WRITE_EXCEPTIONS",
		"long s fold":     "write_exceptionſ",
		"cyrillic e":      "writе_exceptions",
		"fullwidth w":     "ｗrite_exceptions",
		"zero width":      "write\u200b_exceptions",
		"trailing space":  "write_exceptions ",
		"leading space":   " write_exceptions",
		"camel":           "writeExceptions",
		"kelvin sign too": "write_exceptions\u212a",
	} {
		t.Run(name, func(t *testing.T) {
			f, roles := roleFixture(t, nil, nil)
			raw := `{"name":"sneaky","description":"d","system_prompt":"p","enable_write_tools":false,"` + key + `":["write_file","exec_command"]}`
			res := advRawDefine(f, raw)
			if !res.IsError {
				t.Fatalf("key %q accepted: %s", key, res.Text)
			}
			advNoWidening(t, roles, "sneaky")
		})
	}
}

func TestAdvB29RuntimeDefineRefusesNestedWideningFields(t *testing.T) {
	for name, raw := range map[string]string{
		"object under name":       `{"name":{"write_exceptions":["write_file"]},"description":"d","system_prompt":"p"}`,
		"object in allowed_tools": `{"name":"sneaky","description":"d","system_prompt":"p","allowed_tools":[{"write_exceptions":["write_file"]}]}`,
		"object under model":      `{"name":"sneaky","description":"d","system_prompt":"p","model":{"write_exceptions":["x"]}}`,
		"wrapper object":          `{"definition":{"name":"sneaky","description":"d","system_prompt":"p","write_exceptions":["write_file"]}}`,
		"definition array":        `[{"name":"sneaky","description":"d","system_prompt":"p","write_exceptions":["write_file"]}]`,
		"retired workspace":       `{"name":"sneaky","description":"d","system_prompt":"p","workspace":"branch"}`,
		"retired subagent tools":  `{"name":"sneaky","description":"d","system_prompt":"p","enable_subagent_tools":true}`,
		"retired then null":       `{"name":"sneaky","description":"d","system_prompt":"p","enable_subagent_tools":true,"enable_subagent_tools":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			f, roles := roleFixture(t, nil, nil)
			if res := advRawDefine(f, raw); !res.IsError {
				t.Fatalf("accepted: %s", res.Text)
			}
			advNoWidening(t, roles, "sneaky")
		})
	}
}

func TestAdvB29RuntimeDefineDuplicateKeysCannotSmuggleAShippedName(t *testing.T) {
	f, roles := roleFixture(t, nil, nil)
	before, _ := roles.Get("critic")
	raw := `{"name":"mine","name":"critic","description":"d","system_prompt":"rewrite critic","enable_write_tools":true,"enable_mcp_tools":true}`
	if res := advRawDefine(f, raw); !res.IsError {
		t.Fatalf("last-wins duplicate name overwrote a shipped role: %s", res.Text)
	}
	after, _ := roles.Get("critic")
	if after.EnableWriteTools != before.EnableWriteTools || after.EnableMCPTools != before.EnableMCPTools || after.SystemPrompt != before.SystemPrompt {
		t.Fatalf("shipped critic changed: %+v", after)
	}
	// duplicate enable_write_tools: last wins, and the tier ceiling must follow the final value
	raw = `{"name":"dup","description":"d","system_prompt":"p","enable_write_tools":true,"enable_write_tools":false,"tier":"heavy"}`
	if res := advRawDefine(f, raw); !res.IsError {
		t.Fatalf("a read-only role (final enable_write_tools=false) was granted the heavy tier: %s", res.Text)
	}
}

func TestAdvB29RuntimeDefineMalformedBodies(t *testing.T) {
	good := `{"name":"sneaky","description":"d","system_prompt":"p"}`
	for name, raw := range map[string]string{
		"null":             `null`,
		"array":            `[]`,
		"number":           `5`,
		"string":           `"x"`,
		"empty object":     `{}`,
		"truncated":        `{"name":"sneaky","description":"d","system_prompt":`,
		"trailing object":  good + `{"write_exceptions":["write_file"]}`,
		"trailing garbage": good + ` garbage`,
		"trailing comma":   `{"name":"sneaky","description":"d","system_prompt":"p",}`,
	} {
		t.Run(name, func(t *testing.T) {
			f, roles := roleFixture(t, nil, nil)
			res := advRawDefine(f, raw)
			if !res.IsError {
				t.Fatalf("a malformed body was accepted: %s", res.Text)
			}
			advNoWidening(t, roles, "sneaky")
		})
	}
}

// Allowed fields that look like they widen: none may raise the surface above
// what enable_write_tools / enable_mcp_tools earn.
func TestAdvB29RuntimeDefineAllowedFieldsCannotExceedTheFloor(t *testing.T) {
	f, roles := roleFixture(t, nil, nil)
	res := advRawDefine(f, `{"name":"ro","description":"d","system_prompt":"p","enable_write_tools":false,"enable_mcp_tools":false,"allowed_tools":["write_file","patch_file","exec_command","devcouncil_create_artifact","mcp__x__y"]}`)
	if res.IsError {
		t.Fatalf("refused: %s", res.Text)
	}
	def, ok := roles.Get("ro")
	if !ok {
		t.Fatal("not registered")
	}
	s := def.Surface()
	for _, w := range []string{"write_file", "patch_file", "exec_command", "devcouncil_create_artifact"} {
		if s.PermitsWrite(w) {
			t.Errorf("a read-only role defined by a model may write with %s", w)
		}
	}
	if s.Permits("mcp__x__y", tools.GroupMCP) {
		t.Error("a role with enable_mcp_tools=false permits an MCP tool named in allowed_tools")
	}
	if len(def.WriteExceptions) != 0 {
		t.Errorf("write exceptions %v", def.WriteExceptions)
	}
	if got := def.Ceiling(); got != agents.TierLite {
		t.Errorf("read-only role ceiling %s, want lite", got)
	}
	for name, raw := range map[string]string{
		"model heavy on read-only": `{"name":"a1","description":"d","system_prompt":"p","model":"heavy"}`,
		"tier heavy on read-only":  `{"name":"a2","description":"d","system_prompt":"p","tier":"heavy"}`,
		"tier advisor read-only":   `{"name":"a3","description":"d","system_prompt":"p","tier":"advisor"}`,
		"tier uppercase heavy":     `{"name":"a4","description":"d","system_prompt":"p","tier":"HEAVY"}`,
		"tier padded heavy":        `{"name":"a5","description":"d","system_prompt":"p","tier":" heavy "}`,
		"raw model id":             `{"name":"a6","description":"d","system_prompt":"p","model":"anthropic/claude-opus-4"}`,
		"model and tier disagree":  `{"name":"a7","description":"d","system_prompt":"p","enable_write_tools":true,"model":"basic","tier":"heavy"}`,
	} {
		if res := advRawDefine(f, raw); !res.IsError {
			def, _ := roles.Get(strings.Fields(strings.SplitN(raw, `"name":"`, 2)[1])[0][:2])
			t.Errorf("%s accepted: %s (ceiling %s)", name, res.Text, def.Ceiling())
		}
	}
}

// A model may define a write-capable role: that is an allowed field. It must
// then earn exactly what the shipped builder has, no more.
func TestAdvB29RuntimeDefineWriteRoleMatchesShippedBuilderAtMost(t *testing.T) {
	f, roles := roleFixture(t, nil, nil)
	res := advRawDefine(f, `{"name":"w","description":"d","system_prompt":"p","enable_write_tools":true,"enable_mcp_tools":true,"tier":"heavy"}`)
	if res.IsError {
		t.Fatalf("refused: %s", res.Text)
	}
	w, _ := roles.Get("w")
	b, _ := roles.Get("builder")
	if w.Ceiling() != b.Ceiling() || w.EnableWriteTools != b.EnableWriteTools || len(w.WriteExceptions) != 0 {
		t.Fatalf("authored write role %+v exceeds shipped builder %+v", w, b)
	}
	if w.Model != agents.InheritSpec {
		t.Errorf("authored role model %q, want inherit", w.Model)
	}
}

// Trailing data must never be applied. Refusing the whole call is the intended
// behaviour and passes (nothing was applied because nothing was defined); a
// call that is accepted must still not have taken the trailing object.
func TestAdvB29RuntimeDefineTrailingObjectIsNotApplied(t *testing.T) {
	f, roles := roleFixture(t, nil, nil)
	res := advRawDefine(f, `{"name":"trail","description":"d","system_prompt":"p"}{"name":"trail","write_exceptions":["write_file"],"enable_write_tools":true}`)
	d, ok := roles.Get("trail")
	if res.IsError {
		if ok && (len(d.WriteExceptions) != 0 || d.EnableWriteTools) {
			t.Fatalf("call refused (%s) but the trailing object was applied: %+v", res.Text, d)
		}
		return
	}
	if len(d.WriteExceptions) != 0 || d.EnableWriteTools {
		t.Fatalf("trailing object was applied: %+v", d)
	}
}
