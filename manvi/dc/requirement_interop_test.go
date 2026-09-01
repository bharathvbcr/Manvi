package dc

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The wire contract, checked against the producer instead of against itself.
//
// requirement_test.go asserts this package decodes the shapes this package's
// author believed DevCouncil emits. That is a test of one hand's consistency
// with itself: every field name, every default and every enum spelling in it
// came from reading Python, and a misreading would be reproduced identically in
// the test and in the code.
//
// So this file asks DevCouncil's own pydantic models to serialise, and decodes
// what actually comes out. If the two models drift -- a renamed field, a new
// required key, a changed default -- this fails, and it is the only thing here
// that can.

// devcouncilRepo finds a DevCouncil checkout with the models importable.
//
// Resolved by walking up rather than by counting parents: this module is built
// from the main checkout and from git worktrees, which sit at different depths,
// and a fixed count is right in one and silently wrong in the other.
func devcouncilRepo(t *testing.T) (python string, src string) {
	t.Helper()
	if root := os.Getenv("DEVCOUNCIL_ROOT"); root != "" {
		py := filepath.Join(root, ".venv/bin/python")
		if _, err := os.Stat(py); err == nil {
			return py, filepath.Join(root, "src")
		}
		t.Fatalf("DEVCOUNCIL_ROOT=%s has no .venv/bin/python", root)
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "DevCouncil")
		py := filepath.Join(candidate, ".venv/bin/python")
		if _, err := os.Stat(py); err == nil {
			if _, err := os.Stat(filepath.Join(candidate, "src/devcouncil")); err == nil {
				return py, filepath.Join(candidate, "src")
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skipf("no DevCouncil checkout with a venv found above the working directory; " +
				"the requirement wire contract is UNVERIFIED in this run (set DEVCOUNCIL_ROOT)")
		}
		dir = parent
	}
}

func runPython(t *testing.T, python, src, code string) []byte {
	t.Helper()
	cmd := exec.Command(python, "-c", code)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+src)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python failed: %v\n%s", err, out)
	}
	return out
}

// A requirement serialised by DevCouncil's pydantic model decodes here with
// every field intact.
func TestGoDecodesARequirementDevCouncilSerialised(t *testing.T) {
	python, src := devcouncilRepo(t)

	code := `
import json
from devcouncil.domain.requirement import Requirement, AcceptanceCriterion
r = Requirement(
    id="REQ-1", title="Add two numbers", description="d", priority="critical", source="user",
    acceptance_criteria=[
        AcceptanceCriterion(id="AC-1", description="sums", verification_method="unit_test"),
        AcceptanceCriterion(id="AC-2", description="errors", verification_method="llm_review", required=False),
    ],
)
print(r.model_dump_json())
`
	var got Requirement
	if err := json.Unmarshal(runPython(t, python, src, code), &got); err != nil {
		t.Fatalf("Go could not decode what DevCouncil serialised: %v", err)
	}

	if got.ID != "REQ-1" || got.Title != "Add two numbers" {
		t.Errorf("identity did not survive: %+v", got)
	}
	if got.Priority != PriorityCritical {
		t.Errorf("priority = %q, want critical", got.Priority)
	}
	if got.Source != SourceUser {
		t.Errorf("source = %q, want user", got.Source)
	}
	if len(got.AcceptanceCriteria) != 2 {
		t.Fatalf("criteria = %d, want 2", len(got.AcceptanceCriteria))
	}
	if got.AcceptanceCriteria[0].Method != VerifyUnitTest {
		t.Errorf("AC-1 method = %q", got.AcceptanceCriteria[0].Method)
	}
	if !got.AcceptanceCriteria[0].Required {
		t.Error("AC-1 is required on the Python side and must be here")
	}
	if got.AcceptanceCriteria[1].Required {
		t.Error("AC-2 is optional on the Python side and must be here")
	}
}

// The defaulting rules match the producer's, checked by asking the producer
// what it emits when the fields are left out.
//
// This is the case the hand-written test cannot honestly cover: whether pydantic
// *omits* a defaulted field or writes it explicitly is pydantic's decision, and
// Go's behaviour is only correct relative to what actually arrives.
func TestTheDefaultsAgreeWithDevCouncils(t *testing.T) {
	python, src := devcouncilRepo(t)

	code := `
import json
from devcouncil.domain.requirement import Requirement, AcceptanceCriterion
r = Requirement(
    id="REQ-2", title="t", description="d", priority="low",
    acceptance_criteria=[AcceptanceCriterion(id="AC-9", description="x", verification_method="manual")],
)
# exclude_defaults is the hostile case: every defaulted key disappears from the
# wire, which is exactly when Go's zero values would silently take over.
print(r.model_dump_json(exclude_defaults=True))
`
	raw := runPython(t, python, src, code)

	var got Requirement
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Go could not decode a defaults-elided requirement: %v\nwire: %s", err, raw)
	}
	if got.Source != SourcePlanner {
		t.Errorf("source = %q, want planner (DevCouncil's default) from wire %s", got.Source, raw)
	}
	if len(got.AcceptanceCriteria) != 1 {
		t.Fatalf("criteria = %d, want 1 (wire: %s)", len(got.AcceptanceCriteria), raw)
	}
	if !got.AcceptanceCriteria[0].Required {
		t.Errorf("AC-9 decoded as optional; DevCouncil's default is required (wire: %s)", raw)
	}
}

// Every verification method and priority DevCouncil accepts is one this package
// accepts, enumerated from the Python Literal rather than from memory.
//
// A method DevCouncil adds and this package has not learned would otherwise
// surface as an undecodable plan at the worst moment, and nothing here would
// have warned.
func TestTheEnumsAreTheSameSetOnBothSides(t *testing.T) {
	python, src := devcouncilRepo(t)

	code := `
import json, typing
from devcouncil.domain.requirement import Requirement, AcceptanceCriterion
def literals(model, field):
    return list(typing.get_args(model.model_fields[field].annotation))
print(json.dumps({
    "verification_method": literals(AcceptanceCriterion, "verification_method"),
    "priority": literals(Requirement, "priority"),
    "source": [a for a in literals(Requirement, "source") if isinstance(a, str)],
}))
`
	var sets struct {
		VerificationMethod []string `json:"verification_method"`
		Priority           []string `json:"priority"`
		Source             []string `json:"source"`
	}
	raw := runPython(t, python, src, code)
	if err := json.Unmarshal(raw, &sets); err != nil {
		t.Fatalf("reading DevCouncil's enums: %v\n%s", err, raw)
	}

	for _, m := range sets.VerificationMethod {
		if !VerificationMethod(m).valid() {
			t.Errorf("DevCouncil accepts verification_method %q and this package refuses it", m)
		}
	}
	for _, p := range sets.Priority {
		if !Priority(p).valid() {
			t.Errorf("DevCouncil accepts priority %q and this package refuses it", p)
		}
	}
	for _, s := range sets.Source {
		if !Source(s).valid() {
			t.Errorf("DevCouncil accepts source %q and this package refuses it", s)
		}
	}
	if len(sets.VerificationMethod) == 0 || len(sets.Priority) == 0 || len(sets.Source) == 0 {
		t.Fatalf("read no enum members from DevCouncil, so nothing was compared: %s", raw)
	}
}
