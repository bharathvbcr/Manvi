package llm_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// The hosted adapters answer Capability, Models and the unknown-model refusal
// from one Catalogue. These pin what each of them used to spell out by hand.
func TestACatalogueRefusesAnUnknownModelNamingTheAdapterAndWhatItServes(t *testing.T) {
	c := llm.Catalogue{"m-1": {Model: "m-1", SupportsTools: true}}
	err := c.Validate("acme", llm.Request{Model: "m-2"})
	if err == nil {
		t.Fatal("a model the catalogue does not serve was accepted")
	}
	if want := `acme: unknown model "m-2" (serves [m-1])`; err.Error() != want {
		t.Fatalf("refusal = %q, want %q", err, want)
	}
	if _, ok := c.Lookup("m-2"); ok {
		t.Fatal("Lookup gave an unknown model a descriptor")
	}
}

func TestACatalogueChecksAKnownModelAgainstItsCapability(t *testing.T) {
	c := llm.Catalogue{"plain": {Model: "plain"}}
	req := llm.Request{Model: "plain", Tools: []llm.ToolSchema{{Name: "t"}}}
	err := c.Validate("acme", req)
	if want := (llm.Capability{Model: "plain"}).Validate(req); err == nil || want == nil || err.Error() != want.Error() {
		t.Fatalf("Validate = %v, want the model's own Capability verdict %v", err, want)
	}
	if err := c.Validate("acme", llm.Request{Model: "plain"}); err != nil {
		t.Fatalf("a servable request was refused: %v", err)
	}
}

func TestACatalogueListsEveryModel(t *testing.T) {
	c := llm.Catalogue{"a": {}, "b": {}, "c": {}}
	got := c.Models()
	slices.Sort(got)
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("Models = %v", got)
	}
}
