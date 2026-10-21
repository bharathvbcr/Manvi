package harness

import (
	"strings"
	"testing"
)

// A spec spells every field exactly as the schema does; a spelling that only
// works because encoding/json is case-insensitive is refused, at every depth.
func TestDecodeRequiresExactFieldNames(t *testing.T) {
	for name, doc := range map[string]string{
		"top":     `{"Name":"a"}`,
		"section": `{"name":"a","sections":[{"name":"s","order":60,"Text":"x"}]}`,
		"role":    `{"name":"a","roles":[{"name":"r","description":"d","Read_Only":true}]}`,
		"list":    `{"name":"a","SECTIONS":[]}`,
	} {
		_, err := DecodeSpec([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), "must be spelled") {
			t.Errorf("%s: want a spelling refusal, got %v", name, err)
		}
	}
	if _, err := DecodeSpec([]byte(`{"name":"a","roles":[{"name":"r","description":"d","read_only":true}]}`)); err != nil {
		t.Fatalf("an exact document was refused: %v", err)
	}
}

func TestCloneSharesNothing(t *testing.T) {
	s := goodSpec()
	s.Roles[0].AllowedTools = []string{"read_file"}
	c := s.Clone()
	c.Sections[0].Text, c.Roles[0].AllowedTools[0], c.Skills[0] = "x", "y", "z"
	if s.Digest() == c.Digest() || s.Sections[0].Text == "x" || s.Roles[0].AllowedTools[0] == "y" || s.Skills[0] == "z" {
		t.Fatal("Clone shares storage with the original")
	}
}
