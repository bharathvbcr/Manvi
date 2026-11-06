package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// current.json is shared with writers this build does not know (Jarvis, a newer
// catalog revision). A status change must replace only the keys this build owns
// and hand every other key back as it found it.

// plantedKeys are values of every JSON shape, under keys that are not owned. The
// near-miss names matter: "Status" is matched to Entry.Status by encoding/json's
// case-insensitive decoding, and "status " is a different key altogether.
func plantedKeys(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	big, err := json.Marshal(strings.Repeat("x", 900<<10))
	if err != nil {
		t.Fatal(err)
	}
	return map[string]json.RawMessage{
		"reviewed_by":   json.RawMessage(`"someone"`),
		"restriction":   json.RawMessage(`{"channels":["a","b"],"nested":{"deep":[1,2,{"k":null}]}}`),
		"audit_trail":   json.RawMessage(`[1,"two",{"three":3},[4],null,true]`),
		"Status":        json.RawMessage(`"approved"`),
		"status ":       json.RawMessage(`"approved"`),
		"PROMOTED":      json.RawMessage(`true`),
		"résumé-键":      json.RawMessage(`{"ключ":"值"}`),
		"blob":          big,
		"null_valued":   json.RawMessage(`null`),
		"number_valued": json.RawMessage(`12345678901234567890.5e3`),
	}
}

// plant rewrites current.json with the given extra keys added.
func plant(t *testing.T, path string, extra map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	fields := readFields(t, path)
	for k, v := range extra {
		fields[k] = v
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return extra
}

// sameJSON compares two raw values as decoded JSON, so key order and spacing do
// not matter but content does. UseNumber is not needed: both sides decode alike.
func sameJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("decoding %.80s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("decoding %.80s: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

func approvedStore(t *testing.T) (Store, Entry, string) {
	t.Helper()
	s := Store{Root: t.TempDir()}
	e, err := s.Put(catalogFixture(t, "1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	return s, e, filepath.Join(s.Root, e.ID, "current.json")
}

func TestStatusChangesKeepEveryUnownedCurrentKey(t *testing.T) {
	steps := []struct {
		name string
		do   func(Store, Entry) error
		want Status
	}{
		{"revoke", func(s Store, e Entry) error { return s.Revoke(e.ID, e.Revision, e.SHA256) }, StatusRevoked},
		{"approve", func(s Store, e Entry) error { return s.Approve(e.ID, e.Revision, e.SHA256) }, StatusApproved},
		{"promote", func(s Store, e Entry) error { return s.Promote(e.ID, e.Revision, e.SHA256) }, StatusApproved},
	}
	s, e, path := approvedStore(t)
	// One planting survives the whole chain, so a key dropped by any step shows.
	extra := plant(t, path, plantedKeys(t))
	for _, step := range steps {
		if err := step.do(s, e); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		after := readFields(t, path)
		for k, v := range extra {
			got, ok := after[k]
			if !ok {
				t.Fatalf("%s dropped unowned key %q", step.name, k)
			}
			if !sameJSON(t, v, got) {
				t.Errorf("%s changed unowned key %q", step.name, k)
			}
		}
		got, err := s.Get(e.ID, e.Revision)
		if err != nil || got.Status != step.want || got.Promoted != (step.want == StatusApproved) {
			t.Fatalf("%s: entry = %+v, err = %v", step.name, got, err)
		}
	}
}

// A revoked entry must be refused unattended, with or without foreign keys on
// the pointer, including one spelled like the status key.
func TestRevokedEntryFailsCheckUnattendedWithForeignKeys(t *testing.T) {
	for _, planted := range []bool{false, true} {
		s, e, path := approvedStore(t)
		if planted {
			plant(t, path, plantedKeys(t))
		}
		if err := s.Revoke(e.ID, e.Revision, e.SHA256); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(e.ID, e.Revision)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusRevoked || CheckUnattended(got) == nil || got.AllowsUnattended() {
			t.Fatalf("planted=%v: revoked entry admitted: %+v", planted, got)
		}
	}
}

func TestSetStabilityKeepsUnknownKeysInsideStability(t *testing.T) {
	s, e, path := approvedStore(t)
	if err := s.SetStability(e.ID, e.Revision, e.SHA256, Stability{SuccessRate: 1, FirstAttemptRate: 1, Runs: 9, Source: "old"}); err != nil {
		t.Fatal(err)
	}
	fields := readFields(t, path)
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(fields["stability"], &inner); err != nil {
		t.Fatal(err)
	}
	inner["audited_by"] = json.RawMessage(`{"who":["a","b"]}`)
	inner["Runs "] = json.RawMessage(`"near-owned"`)
	var err error
	if fields["stability"], err = json.Marshal(inner); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Runs and Source are omitempty: a zero value must clear the old one, not
	// leave it behind.
	if err := s.SetStability(e.ID, e.Revision, e.SHA256, Stability{SuccessRate: 0.5, FirstAttemptRate: 0.25}); err != nil {
		t.Fatal(err)
	}
	got := map[string]json.RawMessage{}
	if err := json.Unmarshal(readFields(t, path)["stability"], &got); err != nil {
		t.Fatal(err)
	}
	if !sameJSON(t, got["audited_by"], inner["audited_by"]) || !sameJSON(t, got["Runs "], inner["Runs "]) {
		t.Errorf("unknown stability keys changed: %v", got)
	}
	if _, ok := got["runs"]; ok {
		t.Errorf("owned omitempty key runs survived a zero update: %s", got["runs"])
	}
	if _, ok := got["source"]; ok {
		t.Errorf("owned omitempty key source survived a zero update: %s", got["source"])
	}
	if string(got["success_rate"]) != "0.5" || string(got["first_attempt_rate"]) != "0.25" {
		t.Errorf("owned keys not updated: %v", got)
	}
	if entry, err := s.Get(e.ID, e.Revision); err != nil || entry.Stability == nil || entry.Stability.SuccessRate != 0.5 {
		t.Fatalf("entry = %+v, err = %v", entry, err)
	}
}

// Re-approving the same digest keeps its stability object whole, unknown keys
// included; approving another revision starts a new pointer without it.
func TestApproveKeepsStabilityObjectForSameDigestOnly(t *testing.T) {
	s, e, path := approvedStore(t)
	if err := s.SetStability(e.ID, e.Revision, e.SHA256, Stability{SuccessRate: 1, FirstAttemptRate: 1, Runs: 3}); err != nil {
		t.Fatal(err)
	}
	fields := readFields(t, path)
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(fields["stability"], &inner); err != nil {
		t.Fatal(err)
	}
	inner["audited_by"] = json.RawMessage(`"x"`)
	fields["stability"], _ = json.Marshal(inner)
	raw, _ := json.Marshal(fields)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	if after := readFields(t, path); !sameJSON(t, after["stability"], fields["stability"]) {
		t.Fatalf("stability changed by revoke: %s", after["stability"])
	}
	e2, err := s.Put(catalogFixture(t, "2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(e2.ID, e2.Revision, e2.SHA256); err != nil {
		t.Fatal(err)
	}
	after := readFields(t, path)
	if _, ok := after["stability"]; ok {
		t.Errorf("stability of revision 1 carried onto revision 2: %s", after["stability"])
	}
	if string(after["revision"]) != `"2"` {
		t.Errorf("revision = %s", after["revision"])
	}
}

// Wrong types for owned keys stay an error, and the file is left as it was.
func TestStatusChangeRefusesOwnedKeyOfWrongType(t *testing.T) {
	for _, bad := range []string{`{"status":5}`, `{"promoted":"yes"}`, `{"stability":"s"}`, `[]`, `"x"`} {
		s, e, path := approvedStore(t)
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.Revoke(e.ID, e.Revision, e.SHA256); err == nil {
			t.Errorf("Revoke accepted current.json %s", bad)
		}
		if err := s.SetStability(e.ID, e.Revision, e.SHA256, Stability{}); err == nil {
			t.Errorf("SetStability accepted current.json %s", bad)
		}
		if raw, _ := os.ReadFile(path); string(raw) != bad { // #nosec G304 -- test temp dir
			t.Errorf("failed write changed current.json to %s", raw)
		}
	}
}

// Duplicate keys: encoding/json keeps the last value for a repeated key, which
// is also what every reader of the file sees. The rewrite collapses the repeat
// to that value, for owned and unowned keys alike.
func TestDuplicateKeysCollapseToTheLastValue(t *testing.T) {
	s, e, path := approvedStore(t)
	raw, err := os.ReadFile(path) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	dup := strings.Replace(string(raw), "{", `{"reviewed_by":"first","status":"draft",`, 1)
	dup = strings.TrimSuffix(dup, "}") + `,"reviewed_by":"last"}`
	if err := os.WriteFile(path, []byte(dup), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	after := readFields(t, path)
	if string(after["reviewed_by"]) != `"last"` {
		t.Errorf("reviewed_by = %s, want the last value", after["reviewed_by"])
	}
	if string(after["status"]) != `"revoked"` {
		t.Errorf("status = %s", after["status"])
	}
}

// The bound on current.json is unchanged: a pointer past it is refused, not
// truncated or rewritten.
func TestStatusChangeRefusesOversizedCurrent(t *testing.T) {
	s, e, path := approvedStore(t)
	big := `{"pad":"` + strings.Repeat("x", 1<<20) + `"}`
	if err := os.WriteFile(path, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(e.ID, e.Revision, e.SHA256); err == nil {
		t.Fatal("Revoke accepted an oversized current.json")
	}
	if raw, _ := os.ReadFile(path); string(raw) != big { // #nosec G304 -- test temp dir
		t.Fatal("failed write changed current.json")
	}
}
