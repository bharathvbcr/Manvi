package main

import (
	"bytes"
	"strings"
	"testing"

	"manvi/flags"
)

func doctorText(t *testing.T, set map[string]string) string {
	t.Helper()
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range set {
		if err := reg.Set(flags.Human, k, v); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := doctor(&out, reg); err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(out.String()), " ")
}

// TestDoctorReportsReasoningAsRequestedNotAsPermitted covers a setting that
// silently does nothing on its own.
//
// llm.local.supports_reasoning only permits the reasoning_effort field;
// llm.effort is what fills it, and left empty the field is omitted and the
// model reasons at whatever its own default is — which for a served Qwen3.8 is
// not at all. An operator who turned reasoning "on" and got none had no way to
// see why: both settings read exactly as they were set. Doctor reports what
// will actually be sent, the same way it reports gate modes as resolved rather
// than as written.
func TestDoctorReportsReasoningAsRequestedNotAsPermitted(t *testing.T) {
	on := doctorText(t, map[string]string{
		flags.LLMLocalSupportsReasoning: "true",
		flags.LLMEffort:                 "low",
	})
	if !strings.Contains(on, "reasoning requested at effort \"low\"") {
		t.Errorf("doctor did not report reasoning as requested:\n%s", on)
	}

	// The trap: permitted, never asked for.
	half := doctorText(t, map[string]string{flags.LLMLocalSupportsReasoning: "true"})
	if !strings.Contains(half, "not requested") {
		t.Errorf("doctor reported a no-op reasoning setting as if it were working:\n%s", half)
	}
	if !strings.Contains(half, flags.LLMEffort) {
		t.Errorf("doctor did not name the setting that would fix it:\n%s", half)
	}

	off := doctorText(t, map[string]string{flags.LLMLocalSupportsReasoning: "false"})
	if !strings.Contains(off, "reasoning off") {
		t.Errorf("doctor did not report reasoning as off:\n%s", off)
	}
}
