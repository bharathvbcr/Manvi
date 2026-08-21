package contract

import (
	"sort"
	"strings"
	"testing"
)

// moduleRoot is this package's view of the harness. internal/contract sits two
// levels down, so the module is two levels up.
const moduleRoot = "../.."

// Allowlists excuse a declaration that is genuinely not meant to be read yet.
//
// Every entry carries a reason, and an entry that no longer corresponds to a
// finding FAILS the test. That second rule is the important one: an allowlist
// that may hold entries for things already fixed becomes, within a release or
// two, a list nobody can tell the live parts of — and then the check it guards
// is decoration. Making a stale excuse an error means the list can only ever
// describe the present.
var (
	allowedFlags = map[string]string{
		// (empty — every flag in the catalogue is read by production code)
	}
	allowedRoleFields = map[string]string{
		// (empty — every field of agents.Definition is load-bearing)
	}
	allowedToolArgs = map[string]string{
		// (empty — every decoded argument is either honoured or refused)
	}
)

// findings gathers every check, so one failure message shows the whole picture
// rather than whichever check happened to run first.
func findings(t *testing.T) []Finding {
	t.Helper()
	m, err := Load(moduleRoot)
	if err != nil {
		t.Fatalf("loading the module: %v", err)
	}
	var all []Finding
	all = append(all, m.FlagsWithoutReaders("flags/catalog.go", allowedFlags)...)
	all = append(all, m.FieldsWithoutReaders("Definition", "agents/definition.go", allowedRoleFields)...)
	all = append(all, m.ArgFieldsWithoutReaders(allowedToolArgs)...)
	return all
}

// TestNoDeclaredCapabilityIsInert is the guard.
//
// A flag an operator can set that no code reads, a role field that changes
// nothing, a tool argument decoded and dropped — each of these compiles, reads
// as deliberate, and silently does nothing. Twenty-three shipped here before
// anyone counted them, and every one was found by grepping for readers. This is
// that grep, made total.
func TestNoDeclaredCapabilityIsInert(t *testing.T) {
	found := findings(t)
	if len(found) == 0 {
		return
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })

	var b strings.Builder
	b.WriteString("declared capabilities with no production reader:\n\n")
	for _, f := range found {
		b.WriteString("  " + f.String() + "\n")
	}
	b.WriteString("\nEach of these tells an operator or a model it has control it does not have.\n")
	b.WriteString("Wire it to the behaviour it names, or remove it and refuse the retired key —\n")
	b.WriteString("silently accepting and ignoring it is the defect, not the fallback.\n")
	t.Fatal(b.String())
}

// TestTheAllowlistsHoldNothingStale keeps the excuses honest.
func TestTheAllowlistsHoldNothingStale(t *testing.T) {
	m, err := Load(moduleRoot)
	if err != nil {
		t.Fatalf("loading the module: %v", err)
	}

	// Re-run each check with NO allowlist; anything excused must still appear,
	// or the excuse is describing a problem that no longer exists.
	live := map[string]bool{}
	for _, f := range m.FlagsWithoutReaders("flags/catalog.go", nil) {
		live[f.Name] = true
	}
	for _, f := range m.FieldsWithoutReaders("Definition", "agents/definition.go", nil) {
		live[f.Name] = true
	}
	for _, f := range m.ArgFieldsWithoutReaders(nil) {
		live[f.Name] = true
	}

	for _, list := range []map[string]string{allowedFlags, allowedRoleFields, allowedToolArgs} {
		for name, reason := range list {
			if !live[name] {
				t.Errorf("%q is allowlisted (%q) but is no longer inert; delete the entry", name, reason)
			}
		}
	}
}

// TestTheGuardIsActuallyExaminingSomething is the check on the check.
//
// Every assertion above passes trivially if Load silently returns an empty
// module — a moved directory, a renamed catalogue, a parse failure swallowed
// somewhere. Then a green suite would mean "nothing was examined" while reading
// exactly like "nothing was wrong", which is the failure this whole package
// exists to prevent. So the guard states out loud what it expects to have
// looked at.
func TestTheGuardIsActuallyExaminingSomething(t *testing.T) {
	m, err := Load(moduleRoot)
	if err != nil {
		t.Fatalf("loading the module: %v", err)
	}
	if len(m.files) < 100 {
		t.Fatalf("the guard parsed only %d files; it is not looking at the harness", len(m.files))
	}

	// The catalogue must be present and must define flags, or the flag check
	// silently examines nothing.
	defined := m.FlagsWithoutReaders("flags/catalog.go", nil)
	if len(defined) == 1 && strings.Contains(defined[0].Why, "was not found") {
		t.Fatal("the flag catalogue was not found; the flag check examined nothing")
	}

	// And agents.Definition must still be a JSON-tagged struct, or the field
	// check examines nothing.
	fields := m.FieldsWithoutReaders("Definition", "agents/definition.go", nil)
	for _, f := range fields {
		if strings.Contains(f.Why, "was not found") {
			t.Fatal("agents.Definition was not found; the role-field check examined nothing")
		}
	}
}
