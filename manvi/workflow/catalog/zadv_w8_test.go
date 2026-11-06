package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// Adversarial tests for updateCurrent / overlayOwned. encoding/json decodes
// object keys into struct fields case-insensitively and with unicode simple
// folding (long s U+017F folds to s, Kelvin sign U+212A folds to k), and when
// several keys fold to one field the LAST one in document order wins. The
// writer emits sorted keys, so a folded variant that sorts after the owned key
// ("ſtatus" > "status" bytewise) overrides it for every reader.

func rewriteCurrent(t *testing.T, path string, mutate func(map[string]json.RawMessage)) {
	t.Helper()
	f := readFields(t, path)
	mutate(f)
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// What a stock decoder, as any other reader of this file would be, sees.
func stockDecode(t *testing.T, path string) Entry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAdvFoldedStatusKeyCannotResurrectApprovalAfterRevoke(t *testing.T) {
	for _, key := range []string{"ſtatus", "ſtatuſ", "STATUS", "sTatus", "status​"} {
		t.Run(fmt.Sprintf("%q", key), func(t *testing.T) {
			s, e, path := approvedStore(t)
			rewriteCurrent(t, path, func(f map[string]json.RawMessage) { f[key] = json.RawMessage(`"approved"`) })
			if err := s.Revoke(e.ID, e.Revision, e.SHA256); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(path)
			got, err := s.Get(e.ID, e.Revision)
			if err != nil {
				t.Fatalf("Get after revoke: %v (file %s)", err, raw)
			}
			if got.Status != StatusRevoked || CheckUnattended(got) != nil && false {
				t.Errorf("Get sees status %q after successful Revoke; file: %s", got.Status, raw)
			}
			if CheckUnattended(got) == nil {
				t.Errorf("CheckUnattended admits revoked workflow; file: %s", raw)
			}
			if d := stockDecode(t, path); d.Status != StatusRevoked {
				t.Errorf("stock decoder sees %q after Revoke; file: %s", d.Status, raw)
			}
		})
	}
}

func TestAdvFoldedPromotedKeyCannotFlipPromoted(t *testing.T) {
	// Status "" + promoted:true normalises to approved; a folded "promoted"
	// key that sorts after the real one must not be able to do it after Revoke
	// (status is written, so normalise leaves it, but other readers may use the flag).
	for _, key := range []string{"promoteD", "PROMOTED", "promoted "} {
		s, e, path := approvedStore(t)
		rewriteCurrent(t, path, func(f map[string]json.RawMessage) { f[key] = json.RawMessage(`true`) })
		if err := s.Revoke(e.ID, e.Revision, e.SHA256); err != nil {
			t.Fatal(err)
		}
		if d := stockDecode(t, path); d.Promoted {
			t.Errorf("key %q: stock decoder sees promoted=true after Revoke", key)
		}
	}
}

func TestAdvFoldedPointerKeysCannotRedirectRevision(t *testing.T) {
	// ſha256 / reviſion / path-like variants sorting after the owned keys.
	for _, key := range []string{"ſha256", "reviſion", "Revision"} {
		s, e, path := approvedStore(t)
		rewriteCurrent(t, path, func(f map[string]json.RawMessage) {
			f[key] = json.RawMessage(`"deadbeef"`)
		})
		if err := s.Revoke(e.ID, e.Revision, e.SHA256); err != nil {
			t.Fatal(err)
		}
		d := stockDecode(t, path)
		if d.Revision != e.Revision || d.SHA256 != e.SHA256 {
			t.Errorf("key %q redirected pointer: revision=%q sha=%q", key, d.Revision, d.SHA256)
		}
		if _, err := s.Get(e.ID, e.Revision); err != nil {
			t.Errorf("key %q: Get after revoke: %v", key, err)
		}
	}
}

func TestAdvFoldedStabilityKeysCannotOverrideWrittenValue(t *testing.T) {
	for _, key := range []string{"ſuccess_rate", "Success_Rate", "ſource", "ruNs", "mean_rung_indeX"} {
		s, e, path := approvedStore(t)
		if err := s.SetStability(e.ID, e.Revision, e.SHA256, Stability{SuccessRate: 0.1, Runs: 1, Source: "mine"}); err != nil {
			t.Fatal(err)
		}
		rewriteCurrent(t, path, func(f map[string]json.RawMessage) {
			var in map[string]json.RawMessage
			if err := json.Unmarshal(f["stability"], &in); err != nil {
				t.Fatal(err)
			}
			in[key] = json.RawMessage(`99`)
			f["stability"], _ = json.Marshal(in)
		})
		if err := s.SetStability(e.ID, e.Revision, e.SHA256, Stability{SuccessRate: 0.2, Runs: 2, Source: "second"}); err != nil {
			t.Fatal(err)
		}
		got := stockDecode(t, path).Stability
		if got == nil || got.SuccessRate != 0.2 || got.Runs != 2 || got.Source != "second" || got.MeanRungIndex != 0 {
			t.Errorf("key %q overrides written stability: %+v", key, got)
		}
	}
}

// json.Marshal on a RawMessage re-escapes <, >, & and U+2028/9, six bytes for
// one. A foreign value that fits under the 1 MiB read cap can therefore be
// rewritten to something over it, after which every read of the identity
// (List/Get/Approve) fails: a state change bricks the pointer.
func TestAdvRewriteCannotPushCurrentOverReadCap(t *testing.T) {
	s, e, path := approvedStore(t)
	blob, _ := json.Marshal(strings.Repeat("a", 400<<10))
	// Write the value unescaped, by hand, as another writer could.
	rewriteCurrent(t, path, func(f map[string]json.RawMessage) {})
	raw, _ := os.ReadFile(path)
	raw = bytes.TrimSuffix(raw, []byte("}"))
	raw = append(raw, []byte(`,"zz_foreign":"`+strings.Repeat("&", 400<<10)+`"}`)...)
	_ = blob
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(raw) >= 1<<20 {
		t.Fatalf("setup too big: %d", len(raw))
	}
	if _, err := s.Get(e.ID, e.Revision); err != nil {
		t.Fatalf("precondition: Get = %v", err)
	}
	if err := s.Revoke(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if st, _ := os.Stat(path); st.Size() > 1<<20 {
		t.Errorf("Revoke inflated current.json from %d to %d bytes (> MaxArtifactBytes)", len(raw), st.Size())
	}
	if _, err := s.Get(e.ID, e.Revision); err != nil {
		t.Errorf("after Revoke the identity is unreadable: %v", err)
	}
	if err := s.Approve(e.ID, e.Revision, e.SHA256); err != nil {
		t.Errorf("after Revoke the identity cannot be re-approved: %v", err)
	}
}

func TestAdvRevokeBeyondCapRefusedBeforeWrite(t *testing.T) {
	// A pointer that is already over the cap must be refused and left untouched.
	s, e, path := approvedStore(t)
	raw, _ := os.ReadFile(path)
	raw = bytes.TrimSuffix(raw, []byte("}"))
	raw = append(raw, []byte(`,"zz":"`+strings.Repeat("x", 1<<20)+`"}`)...)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(e.ID, e.Revision, e.SHA256); err == nil {
		t.Fatal("Revoke succeeded on an oversize pointer")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(raw, after) {
		t.Error("oversize pointer was modified by a refused Revoke")
	}
}

func TestAdvHostileShapesInCurrent(t *testing.T) {
	deep := strings.Repeat("[", 9000) + strings.Repeat("]", 9000)
	cases := map[string]string{
		"deep nesting":       `"deep":` + deep,
		"empty key":          `"":"x"`,
		"invalid utf8 key":   "\"bad\xff\xfe\":1",
		"huge number":        `"n":1e999999999999`,
		"duplicate owned":    `"status":"approved","status":"approved"`,
		"null stability key": `"stability":null`,
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			s, e, path := approvedStore(t)
			raw, _ := os.ReadFile(path)
			raw = bytes.TrimSuffix(raw, []byte("}"))
			raw = append(raw, []byte(","+extra+"}")...)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			err := s.Revoke(e.ID, e.Revision, e.SHA256)
			after, _ := os.ReadFile(path)
			if err != nil {
				// Refusal is fine, but it must not have altered or truncated the file.
				if !bytes.Equal(raw, after) {
					t.Fatalf("refused Revoke (%v) changed the file", err)
				}
				return
			}
			if !json.Valid(after) {
				t.Fatalf("wrote invalid JSON: %.200s", after)
			}
			got, gerr := s.Get(e.ID, e.Revision)
			if gerr != nil || got.Status != StatusRevoked {
				t.Errorf("after Revoke: %+v err=%v", got, gerr)
			}
			if d := stockDecode(t, path); d.Status != StatusRevoked {
				t.Errorf("stock decoder sees %q", d.Status)
			}
		})
	}
}

func TestAdvInvalidUTF8KeySurvivesByteForByte(t *testing.T) {
	s, e, path := approvedStore(t)
	raw, _ := os.ReadFile(path)
	raw = bytes.TrimSuffix(raw, []byte("}"))
	raw = append(raw, []byte("\x2c\"k\xff\":\"v\"}")...)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(e.ID, e.Revision, e.SHA256); err != nil {
		t.Skipf("refused: %v", err)
	}
	after, _ := os.ReadFile(path)
	// A key with invalid UTF-8 is rewritten to U+FFFD by json.Marshal: the
	// foreign key is not kept "exactly as found".
	if !bytes.Contains(after, []byte("\"k\xff\"")) {
		t.Errorf("foreign key with invalid UTF-8 was altered: %q", after)
	}
}

func TestAdvConcurrentWritersKeepUnknownKeysAndNeverApproveAfterRevoke(t *testing.T) {
	s, e, path := approvedStore(t)
	plant(t, path, plantedKeys(t))
	extra := map[string]json.RawMessage{
		"reviewed_by": json.RawMessage(`"someone"`),
		"audit_trail": json.RawMessage(`[1,"two",{"three":3},[4],null,true]`),
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				switch i {
				case 0:
					_ = s.Approve(e.ID, e.Revision, e.SHA256)
				case 1:
					_ = s.SetStability(e.ID, e.Revision, e.SHA256, Stability{SuccessRate: 0.5, Runs: i})
				case 2:
					_ = s.Promote(e.ID, e.Revision, e.SHA256)
				}
			}
		}(i)
	}
	for n := 0; n < 40; n++ {
		if err := s.Revoke(e.ID, e.Revision, e.SHA256); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		after := readFields(t, path)
		for k, v := range extra {
			if !sameJSON(t, v, after[k]) {
				t.Fatalf("unknown key %q lost/changed mid-race: %s", k, after[k])
			}
		}
		if n == 39 {
			break
		}
	}
	close(stop)
	wg.Wait()
	// Quiesced: one last Revoke must be final.
	if err := s.Revoke(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(e.ID, e.Revision)
	if err != nil || got.Status != StatusRevoked || CheckUnattended(got) == nil {
		t.Fatalf("ended approved after successful Revoke: %+v err=%v", got, err)
	}
	after := readFields(t, path)
	for k, v := range plantedKeys(t) {
		if k == "Status" || k == "PROMOTED" || k == "status " {
			continue
		}
		if !sameJSON(t, v, after[k]) {
			t.Errorf("planted key %q lost after race", k)
		}
	}
}

func TestAdvPublishStillSyncsAndRenames(t *testing.T) {
	s, e, _ := approvedStore(t)
	calls := 0
	prev := syncDir
	syncDir = func(r *os.Root) error { calls++; return prev(r) }
	defer func() { syncDir = prev }()
	if err := s.Revoke(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStability(e.ID, e.Revision, e.SHA256, Stability{SuccessRate: 1}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("syncDir calls = %d, want 2 (one per current.json publish)", calls)
	}
}
