package serve

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/childenv"
)

// A host that put a credential in this process's environment can confirm from
// the running process that no child inherits it, and which names that covers.
func TestHelloAdvertisesTheWithheldProviderEnvironment(t *testing.T) {
	responses := roundTrip(t, Options{}, Request{ID: "h", Op: OpHello, Params: json.RawMessage(`{"protocol":1}`)})
	if len(responses) != 1 {
		t.Fatalf("hello produced %d responses, want 1", len(responses))
	}
	resp := responses[0]
	if !resp.OK {
		t.Fatalf("hello failed: %+v", resp.Error)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(resp.Result, &raw); err != nil {
		t.Fatal(err)
	}
	var withholds bool
	var withheld []string
	if err := json.Unmarshal(raw["withholds_provider_credentials"], &withholds); err != nil || !withholds {
		t.Fatalf("withholds_provider_credentials = %s (%v), want true", raw["withholds_provider_credentials"], err)
	}
	if err := json.Unmarshal(raw["withheld_provider_env"], &withheld); err != nil {
		t.Fatalf("withheld_provider_env = %s: %v", raw["withheld_provider_env"], err)
	}
	if !slices.Equal(withheld, childenv.Withheld()) {
		t.Fatalf("hello withholds %q, the spawn sites withhold %q", withheld, childenv.Withheld())
	}
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_WORKSPACE_ID"} {
		if !slices.Contains(withheld, name) {
			t.Errorf("hello does not name %s as withheld: %q", name, withheld)
		}
	}
}
