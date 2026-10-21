package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/artifacts"
)

// Adversarial review of dh-5-reflect, attack 2: applyReflection takes a scrubber
// and applies it only to the refusal text. An accepted answer is stored and
// printed as the model wrote it, so a credential the model copied out of a
// verify finding is persisted into .devcouncil/artifacts/lessons/ and echoed to
// the terminal. BROKEN.
func TestAdvApplyReflectionScrubsWhatItStoresBroken(t *testing.T) {
	store, err := artifacts.NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	const secret = "sk-live-0123456789abcdef"
	clean := func(s string) string { return strings.ReplaceAll(s, secret, "[redacted]") }
	answer := strings.Replace(reflectGood, "evidence: prompt assembly", "evidence: token "+secret+" in a finding; prompt assembly", 1)
	answer = strings.Replace(answer, "cue: the prompt", "cue: "+secret+" the prompt", 1)
	var out, notes bytes.Buffer
	if err := applyReflection(&out, &notes, store, "testlib", "sess", answer, clean); err != nil {
		t.Fatal(err)
	}
	list, err := store.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v %v", list, err)
	}
	got, err := store.Get(list[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Content, secret) || strings.Contains(got.Metadata.Summary, secret) || strings.Contains(out.String(), secret) {
		t.Fatalf("the scrubber was not applied to the stored lesson:\ncontent=%q\nsummary=%q\nout=%q", got.Content, got.Metadata.Summary, out.String())
	}
}
