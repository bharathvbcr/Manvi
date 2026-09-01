package contract

import (
	"fmt"
	"testing"
)

func TestScanReport(t *testing.T) {
	m, err := Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range m.FlagsWithoutReaders("flags/catalog.go", nil) {
		fmt.Println("FLAG   ", f.Name, f.Where)
	}
	for _, f := range m.FieldsWithoutReaders("Definition", "agents/definition.go", nil) {
		fmt.Println("FIELD  ", f.Name, f.Where)
	}
	for _, f := range m.ArgFieldsWithoutReaders(nil) {
		fmt.Println("ARG    ", f.Name, f.Where)
	}
	// UnusedExports was written, documented with the defect it exists to catch
	// — prompt.Router, written and tested and wired to nothing while a
	// hand-rolled duplicate drifted away from it — and then called by nothing.
	// A dead-code detector that is itself dead code is the joke this repository
	// is least entitled to make, so it runs here with the others.
	//
	// It reports rather than fails, like every other line in this function. It
	// is not a gate yet and should not be turned into one by allowlisting its
	// current output wholesale: it stands at 147 findings, of which twelve are
	// the checker's own documented blind spot rather than defects — the
	// exported helpers in internal/testsupport and llm/adaptertest exist to be
	// called from tests, and test files are deliberately not counted as
	// callers. The remaining 135 need reading one at a time, and an allowlist
	// filled in without that reading would be a gate that certifies whatever
	// was true the day somebody stopped looking.
	unused := m.UnusedExports(nil)
	fmt.Println("UNUSED-EXPORT TOTAL", len(unused))
	for _, f := range unused {
		fmt.Println("UNUSED ", f.Name, f.Where)
	}
}
