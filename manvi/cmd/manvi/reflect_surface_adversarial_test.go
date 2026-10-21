package main

import (
	"context"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// Adversarial review of dh-5-reflect, attacks 5 and 7: the reflector as
// `manvi reflect` builds it (the real native pipeline, the shipped role,
// devcouncil.ApplyRole in read-only mode) is offered exactly one tool, answers
// "unknown tool" to anything else a model names, and a model that loops on the
// one tool it has is stopped by the sub-agent step ceiling.
func TestAdvReflectorSurfaceAndStepCeilingHold(t *testing.T) {
	flagReg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	native, pipeline, err := nativeTools(flagReg)
	if err != nil {
		t.Fatal(err)
	}
	def, ok := native.Roles().Get(reflectorRoleName)
	if !ok {
		t.Fatal("no reflector role")
	}

	script := []llm.Response{
		wantToolCall("devcouncil_write_file", `{"path":"x.txt","content":"x"}`),
		wantToolCall("devcouncil_create_artifact", `{"name":"lessons/x/y.md","content":"x"}`),
		wantToolCall("devcouncil_read_file", `{"path":".devcouncil/sessions"}`),
		wantToolCall("load_tools", `{"group":"mcp"}`),
		wantToolCall("devcouncil_invoke_subagent", `{"subagents":[{"type_name":"self","role":"g","prompt":"go"}]}`),
	}
	for i := 0; i < 40; i++ {
		script = append(script, wantToolCall("devcouncil_list_artifacts", `{}`))
	}
	p := &scriptedProvider{name: "p", serves: []string{"m"}, script: script}
	r := runnerOver(t, pipeline, p)

	req := devcouncil.SubAgentRequest{Label: reflectorRoleName, Prompt: "reflect"}
	devcouncil.ApplyRole(&req, def, true)
	_, runErr := r.RunSubAgent(context.Background(), req)

	offered := offeredTools(t, p)
	if len(offered) != 1 || !offered["devcouncil_list_artifacts"] {
		t.Errorf("the reflector was offered %v, want only devcouncil_list_artifacts", offered)
	}
	if len(p.seen) > maxSubAgentSteps+1 {
		t.Errorf("the reflector made %d requests (ceiling %d), runErr=%v", len(p.seen), maxSubAgentSteps, runErr)
	}
}
