package agent

import (
	"context"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

func TestChatOnlyOffersNoTools(t *testing.T) {
	reg := tools.NewRegistry(bus.New())
	err := reg.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "devcouncil_read_file"},
		Handler: func(context.Context, tools.Call) tools.Result {
			return tools.Result{}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	off := &Loop{cfg: Config{ChatOnly: true}, tools: reg}
	if got := off.schemas(); got != nil {
		t.Fatalf("chat-only schemas = %#v, want nil", got)
	}
	on := &Loop{cfg: Config{}, tools: reg}
	got := on.schemas()
	if len(got) != 1 || got[0].Name != "devcouncil_read_file" {
		t.Fatalf("tools offered = %#v", got)
	}
}
