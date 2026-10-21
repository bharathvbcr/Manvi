package harness

import (
	"strings"
	"testing"
)

// Attack 3: encoding/json matches keys to fields case-insensitively, and the
// strict decoder's duplicate-key check compares keys exactly. So a document can
// carry two spellings of one field, and the last one wins while a reviewer
// reading the file sees the first. A spec file is a reviewed artefact: two
// spellings of one field is a document whose meaning depends on the decoder.
func TestDecodeRefusesTwoSpellingsOfOneField(t *testing.T) {
	docs := map[string]string{
		"name":         `{"name":"safe","NAME":"other"}`,
		"sections":     `{"name":"a","sections":[{"name":"s","order":60,"text":"ok"}],"Sections":[{"name":"s","order":60,"text":"evil"}]}`,
		"nested":       `{"name":"a","sections":[{"name":"s","order":60,"text":"ok","Text":"evil"}]}`,
		"role":         `{"name":"a","roles":[{"name":"r","description":"d","read_only":true,"READ_ONLY":false}]}`,
		"role list":    `{"name":"a","roles":[{"name":"r","description":"d"}],"ROLES":[{"name":"r","description":"d","system_prompt":"evil"}]}`,
		"fold equal ſ": `{"name":"a","skills":["x"],"ſkills":["y"]}`,
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			if s, err := DecodeSpec([]byte(doc)); err == nil {
				t.Fatalf("decoded a document with two spellings of one field: %+v", s)
			}
		})
	}
}

// Attack 6/5: a catalogue entry is a host's registration, and the spec it hands
// out for a turn must not change because the host kept a slice it registered.
// Resolution is once per turn; an alias makes it once per read of the backing
// array, and moves the digest (and the cached prompt prefix) mid-session with
// no log entry saying a registration changed.
func TestCatalogueOwnsWhatItRegistered(t *testing.T) {
	c := &Catalogue{}
	spec := Spec{
		Name:     "house",
		Sections: []Section{{Name: "style", Order: 60, Text: "original"}},
		Roles:    []RoleSpec{{Name: "scribe", Description: "d", AllowedTools: []string{"read_file"}}},
		Skills:   []string{"a"},
	}
	if err := c.Register(spec); err != nil {
		t.Fatal(err)
	}
	want := spec.Digest()
	spec.Sections[0].Text = "mutated after registration"
	spec.Roles[0].AllowedTools[0] = "write_file"
	spec.Skills[0] = "b"

	got, ok, _ := c.Lookup("house")
	if !ok {
		t.Fatal("not registered")
	}
	if got.Digest() != want {
		t.Fatalf("the catalogue's spec changed after registration: %s", strings.TrimSpace(string(got.Canonical())))
	}
	// And the copy a caller is handed is its own.
	got.Sections[0].Text = "mutated by a reader"
	again, _, _ := c.Lookup("house")
	if again.Digest() != want {
		t.Fatal("a reader of the catalogue changed what the next turn resolves")
	}
}
