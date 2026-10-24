package local

import (
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

func TestApplyLocalKeepAliveReadsTheEnvOnlyWhenUnset(t *testing.T) {
	t.Setenv("MANVI_LLM_LOCAL_KEEP_ALIVE", "10m")
	var req llm.Request
	applyLocalKeepAlive(&req)
	if req.KeepAlive != "10m" {
		t.Fatalf("keep_alive = %q, want 10m", req.KeepAlive)
	}

	req.KeepAlive = "1h"
	t.Setenv("MANVI_LLM_LOCAL_KEEP_ALIVE", "99m")
	applyLocalKeepAlive(&req)
	if req.KeepAlive != "1h" {
		t.Fatalf("caller keep_alive overwritten: %q", req.KeepAlive)
	}

	t.Setenv("MANVI_LLM_LOCAL_KEEP_ALIVE", "  ")
	req = llm.Request{}
	applyLocalKeepAlive(&req)
	if req.KeepAlive != "" {
		t.Fatalf("blank env set keep_alive to %q", req.KeepAlive)
	}
}
