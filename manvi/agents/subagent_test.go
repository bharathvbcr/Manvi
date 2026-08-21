package agents

import (
	"testing"
)

func TestDynamicSubagentRegistry(t *testing.T) {
	reg := NewRegistry()

	// Verify defaults exist
	for _, name := range []string{"research", "builder", "critic", "planner", "stress_tester", "self"} {
		def, ok := reg.Get(name)
		if !ok {
			t.Errorf("expected default subagent %q to exist", name)
		}
		if def.Name != name {
			t.Errorf("expected name %q, got %q", name, def.Name)
		}
	}

	// Register custom subagent
	custom := Definition{
		Name:             "designer",
		Role:             "UI/UX Visual Designer",
		Description:      "Designs responsive UI components",
		SystemPrompt:     "Design beautiful clean UI components with CSS tokens.",
		EnableMCPTools:   true,
		EnableWriteTools: true,
	}
	if err := reg.Register(custom); err != nil {
		t.Fatalf("registering custom: %v", err)
	}

	got, ok := reg.Get("designer")
	if !ok || got.Role != "UI/UX Visual Designer" {
		t.Errorf("custom subagent lookup failed: %+v", got)
	}

	all := reg.List()
	if len(all) != 7 {
		t.Errorf("expected 7 subagents, got %d", len(all))
	}
}

func TestInstanceManager(t *testing.T) {
	mgr := NewInstanceManager()

	cancelled := false
	inst := &Instance{
		ConversationID: "conv-123",
		Type:           "builder",
		Role:           "Full-Stack Feature Builder",
		State:          StateRunning,
		cancel: func() {
			cancelled = true
		},
	}

	mgr.Register(inst)

	got, ok := mgr.Get("conv-123")
	if !ok || got.Type != "builder" {
		t.Fatalf("lookup failed: %+v", got)
	}

	if err := mgr.SendMessage("conv-123", "please focus on calc_test.go"); err != nil {
		t.Fatalf("sending message: %v", err)
	}

	msg := <-inst.inbox
	if msg != "please focus on calc_test.go" {
		t.Errorf("unexpected message: %q", msg)
	}

	if err := mgr.Kill("conv-123"); err != nil {
		t.Fatalf("killing instance: %v", err)
	}

	if !cancelled {
		t.Errorf("expected cancel func to have been called")
	}
	if inst.State != StateCanceling {
		t.Errorf("expected state %s, got %s", StateCanceling, inst.State)
	}
}
