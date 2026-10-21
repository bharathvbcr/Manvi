package workflow

import (
	"strings"
	"testing"
)

// encoding/json matches an object key to a field case-insensitively, so two
// keys that fold together are one field written twice. DecodeStrict is the one
// owner of "a document says each thing once", for every caller.
func TestDecodeStrictRefusesKeysThatFoldToTheSameField(t *testing.T) {
	type inner struct {
		Text string `json:"text"`
	}
	type doc struct {
		ReadOnly bool    `json:"read_only"`
		Skills   []int   `json:"skills"`
		Items    []inner `json:"items"`
	}
	for name, raw := range map[string]string{
		"top level":       `{"read_only":true,"READ_ONLY":false}`,
		"nested":          `{"items":[{"text":"a","Text":"b"}]}`,
		"long s":          `{"skills":[1],"ſkills":[2]}`,
		"exact dup":       `{"skills":[1],"skills":[2]}`,
		"exact dup twice": `{"read_only":true,"read_only":true}`,
		"deep array":      `{"items":[{"text":"a"},{"text":"a","TEXT":"b"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var d doc
			if err := DecodeStrict([]byte(raw), &d); err == nil || !strings.Contains(err.Error(), "duplicate JSON key") {
				t.Fatalf("want a duplicate-key refusal, got %v (%+v)", err, d)
			}
		})
	}
	var d doc
	if err := DecodeStrict([]byte(`{"read_only":true,"items":[{"text":"a"},{"text":"b"}]}`), &d); err != nil || !d.ReadOnly {
		t.Fatalf("a clean document was refused: %v", err)
	}
	if foldKey("K") != foldKey("k") || foldKey("ſ") != foldKey("S") {
		t.Fatal("foldKey does not follow the SimpleFold orbit")
	}
}
