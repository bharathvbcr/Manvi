package main

import (
	"bytes"
	"encoding/json"
	"runtime"
	"slices"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/childenv"
)

// A host deciding whether to start this binary with a provider credential in
// its environment asks before the process exists. The answer is machine
// readable, names the build, and lists the names withheld from every child.
func TestVersionJSONAdvertisesTheWithheldProviderEnvironment(t *testing.T) {
	t.Setenv("MANVI_HARNESS_INIT_ENABLED", "false")
	for _, arg := range []string{"--version", "version"} {
		var out, notes bytes.Buffer
		if err := run(&out, &notes, []string{arg, "--json"}); err != nil {
			t.Fatalf("manvi %s --json: %v", arg, err)
		}
		var got struct {
			Name                         string   `json:"name"`
			Go                           string   `json:"go"`
			Platform                     string   `json:"platform"`
			WithholdsProviderCredentials *bool    `json:"withholds_provider_credentials"`
			WithheldProviderEnv          []string `json:"withheld_provider_env"`
		}
		dec := json.NewDecoder(&out)
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("manvi %s --json is not one JSON object: %v", arg, err)
		}
		if dec.More() {
			t.Fatalf("manvi %s --json printed more than one value", arg)
		}
		if got.Name != "manvi" || got.Go != runtime.Version() || got.Platform != runtime.GOOS+"/"+runtime.GOARCH {
			t.Errorf("identity = %+v", got)
		}
		if got.WithholdsProviderCredentials == nil || !*got.WithholdsProviderCredentials {
			t.Errorf("withholds_provider_credentials = %v, want true", got.WithholdsProviderCredentials)
		}
		if !slices.Equal(got.WithheldProviderEnv, childenv.Withheld()) {
			t.Errorf("advertised %q, the spawn sites withhold %q", got.WithheldProviderEnv, childenv.Withheld())
		}
	}
}

func TestVersionRefusesAnUnknownArgument(t *testing.T) {
	t.Setenv("MANVI_HARNESS_INIT_ENABLED", "false")
	var out, notes bytes.Buffer
	if err := run(&out, &notes, []string{"version", "--yaml"}); err == nil {
		t.Fatalf("manvi version --yaml succeeded:\n%s", out.String())
	}
}
