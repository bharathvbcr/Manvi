package catalog

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

// matchesOwned must agree with what encoding/json does: a key is "owned" exactly
// when the decoder stores it into the field. The table includes the cases the
// decoder folds beyond ASCII: U+017F (long s) matches s and U+212A (Kelvin) k.
func TestMatchesOwnedAgreesWithTheDecoder(t *testing.T) {
	type owned struct {
		Status    string `json:"status"`
		Sha       string `json:"sha256"`
		Rate      string `json:"mean_rung_index"`
		Kelvin    string `json:"k"`
		Source    string `json:"source"`
		Promoted  string `json:"promoted"`
		Revision  string `json:"revision"`
		Unrelated string `json:"zz"`
	}
	names := jsonKeys(reflect.TypeOf(owned{}))
	keys := []string{
		"status", "Status", "STATUS", "sTatus", "statuS", "ſtatus", "ſtatuſ", "STATUſ",
		"status ", " status", "status\u200b", "statu", "statuss", "stat\xffus", "",
		"sha256", "ſha256", "SHA256", "ſhA256", "sha-256", "sha2560",
		"mean_rung_index", "mean_rung_indeX", "MEAN_RUNG_INDEX", "mean_rung_indeχ",
		"k", "K", "K", "K", "kk", "ǩ",
		"source", "ſource", "Source", "ſoUrce", "reviſion", "Revision", "REVİSION", "revısion",
		"promoted", "promoteD", "PROMOTED", "zz", "ZZ", "reviewed_by", "stability",
	}
	for _, key := range keys {
		kb, err := json.Marshal(map[string]string{key: "x"})
		if err != nil {
			t.Fatal(err)
		}
		var o owned
		if err := json.Unmarshal(kb, &o); err != nil {
			t.Fatal(err)
		}
		decoded := o != owned{}
		// The decoder sees the key after unquoting, which is what a map key holds.
		var m map[string]string
		if err := json.Unmarshal(kb, &m); err != nil {
			t.Fatal(err)
		}
		var decodedKey string
		for k := range m {
			decodedKey = k
		}
		if got := matchesOwned(decodedKey, names); got != decoded {
			t.Errorf("key %q: matchesOwned = %v, decoder stores it into an owned field = %v", key, got, decoded)
		}
	}
}

func TestFoldedKeysAreDroppedWhenAnOwnedValueIsWritten(t *testing.T) {
	s, e, path := approvedStore(t)
	for key, value := range map[string]string{"ſtatus": `"approved"`, "K": `"k"`, "ſha256": `"x"`, "promoteD": `true`, "reviſion": `"x"`, "ſtability": `{"runs":9}`} {
		fields := readFields(t, path)
		fields[key] = json.RawMessage(value)
		raw, _ := json.Marshal(fields)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Revoke(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	after := readFields(t, path)
	for _, key := range []string{"ſtatus", "ſha256", "promoteD", "reviſion", "ſtability"} {
		if _, ok := after[key]; ok {
			t.Errorf("near-duplicate of an owned key %q survived", key)
		}
	}
	if _, ok := after["K"]; !ok {
		t.Error("K (Kelvin sign) matches no owned Entry key and must survive")
	}
	if got := stockDecode(t, path); got.Status != StatusRevoked || got.Revision != e.Revision || got.SHA256 != e.SHA256 {
		t.Errorf("stock decoder sees %+v", got)
	}
}

// Keeping a stability object whole on a status change must not keep a folded
// override of its own fields.
func TestStatusChangeScrubsFoldedKeysInsideKeptStability(t *testing.T) {
	s, e, path := approvedStore(t)
	if err := s.SetStability(e.ID, e.Revision, e.SHA256, Stability{SuccessRate: 0.5, Runs: 2}); err != nil {
		t.Fatal(err)
	}
	rewriteCurrent(t, path, func(f map[string]json.RawMessage) {
		var in map[string]json.RawMessage
		if err := json.Unmarshal(f["stability"], &in); err != nil {
			t.Fatal(err)
		}
		in["ſuccess_rate"] = json.RawMessage(`1`)
		in["audited_by"] = json.RawMessage(`"x"`)
		f["stability"], _ = json.Marshal(in)
	})
	if err := s.Revoke(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	got := stockDecode(t, path)
	if got.Stability == nil || got.Stability.SuccessRate != 0.5 {
		t.Fatalf("stability = %+v", got.Stability)
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal(readFields(t, path)["stability"], &in); err != nil {
		t.Fatal(err)
	}
	if string(in["audited_by"]) != `"x"` {
		t.Errorf("unrelated stability key lost: %v", in)
	}
}

// inflatingPointer plants a foreign key that fits under the read cap but encodes
// larger than it: each invalid UTF-8 byte in a key is written back as a six byte
// escape.
func inflatingPointer(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw[:len(raw)-1], []byte(`,"`+strings.Repeat("\xff", 400<<10)+`":1,"reviewed_by":"x"}`)...)
	if len(raw) >= workflow.MaxArtifactBytes {
		t.Fatalf("setup too big: %d", len(raw))
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil { // #nosec G703 -- a path under this test's own t.TempDir
		t.Fatal(err)
	}
	return raw
}

func TestRevokeOverTheCapDropsForeignKeysAndSaysSo(t *testing.T) {
	s, e, path := approvedStore(t)
	inflatingPointer(t, path)
	dropped, err := s.RevokeReport(e.ID, e.Revision, e.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 2 || dropped[0] != "reviewed_by" {
		t.Fatalf("dropped = %d names, want the two foreign keys", len(dropped))
	}
	st, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if st.Size() > workflow.MaxArtifactBytes {
		t.Fatalf("current.json is %d bytes", st.Size())
	}
	got, err := s.Get(e.ID, e.Revision)
	if err != nil || got.Status != StatusRevoked || CheckUnattended(got) == nil {
		t.Fatalf("entry = %+v, err = %v", got, err)
	}
	if err := s.Approve(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatalf("identity not usable after the drop: %v", err)
	}
}

func TestOtherWritersRefuseWhenThePointerWouldPassTheCap(t *testing.T) {
	writers := map[string]func(Store, Entry) error{
		"approve": func(s Store, e Entry) error { return s.Approve(e.ID, e.Revision, e.SHA256) },
		"promote": func(s Store, e Entry) error { return s.Promote(e.ID, e.Revision, e.SHA256) },
		"setstability": func(s Store, e Entry) error {
			return s.SetStability(e.ID, e.Revision, e.SHA256, Stability{SuccessRate: 1})
		},
	}
	for name, write := range writers {
		s, e, path := approvedStore(t)
		before := inflatingPointer(t, path)
		if err := write(s, e); err == nil {
			t.Errorf("%s wrote a pointer past the cap", name)
		}
		if after, _ := os.ReadFile(path); string(after) != string(before) { // #nosec G304 -- test temp dir
			t.Errorf("%s changed the file it refused to write", name)
		}
	}
}

// A pointer no reader can load must not admit unattended replay.
func TestCheckUnattendedFailsClosedOnAnUnreadablePointer(t *testing.T) {
	s, e, path := approvedStore(t)
	raw, readErr := os.ReadFile(path) // #nosec G304 -- test temp dir
	if readErr != nil {
		t.Fatal(readErr)
	}
	big := append(raw[:len(raw)-1], []byte(`,"pad":"`+strings.Repeat("x", workflow.MaxArtifactBytes)+`"}`)...)
	if err := os.WriteFile(path, big, 0o600); err != nil { // #nosec G703 -- a path under this test's own t.TempDir
		t.Fatal(err)
	}
	got, err := s.Get(e.ID, e.Revision)
	if err == nil {
		t.Fatal("Get read an over-cap pointer")
	}
	if CheckUnattended(got) == nil {
		t.Fatalf("CheckUnattended admitted %+v after Get failed with %v", got, err)
	}
}
