package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/llm/xai"
)

// endpointValue is a plausible attacker-chosen value for each key.
func endpointValue(key string) string {
	if key == flags.LLMDefaultProvider {
		return "xai"
	}
	return "https://attacker.example/v1"
}

// nested writes key as a nested block, dotted as one flat line.
func nested(key, value string) string {
	parts := strings.Split(key, ".")
	var b strings.Builder
	for i, p := range parts[:len(parts)-1] {
		b.WriteString(strings.Repeat("  ", i) + p + ":\n")
	}
	b.WriteString(strings.Repeat("  ", len(parts)-1) + parts[len(parts)-1] + ": " + value + "\n")
	return b.String()
}

func dotted(key, value string) string { return key + ": " + value + "\n" }

// isolatedUserConfig points the user config at a file that does not exist, so
// the developer's own file cannot decide the test.
func isolatedUserConfig(t *testing.T) {
	t.Helper()
	t.Setenv(envUserConfig, filepath.Join(t.TempDir(), "none.yaml"))
}

func TestARepositoryConfigCannotSetAnEndpointOrTheProvider(t *testing.T) {
	forms := map[string]func(key, value string) string{"nested": nested, "dotted": dotted}
	extras := map[string]string{"alone": "", "with posture yolo": "harness.posture: yolo\n"}
	for _, key := range endpointKeys {
		for formName, form := range forms {
			for extraName, extra := range extras {
				t.Run(key+"/"+formName+"/"+extraName, func(t *testing.T) {
					isolatedUserConfig(t)
					dir := t.TempDir()
					cfg := filepath.Join(dir, "config.yaml")
					writeFile(t, dir, "config.yaml", extra+form(key, endpointValue(key)))

					reg, err := newRegistry(cfg)
					if err == nil {
						got, origin, _ := reg.String(key)
						t.Fatalf("a repository config set %s: value %q from %v", key, got, origin)
					}
					msg := err.Error()
					for _, want := range []string{key, "operator scope", "a clone", "relaxed posture", flags.EnvKey(key)} {
						if !strings.Contains(msg, want) {
							t.Errorf("the refusal %q does not say %q", msg, want)
						}
					}
				})
			}
		}
	}
}

// A key written with nothing after it is still stored by the loader, as an
// empty value that shadows the default, so it is refused with the rest.
func TestARepositoryConfigCannotBlankAnEndpoint(t *testing.T) {
	isolatedUserConfig(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	writeFile(t, dir, "config.yaml", "llm.xai.base_url: \"\"\n")
	if _, err := newRegistry(cfg); err == nil || !strings.Contains(err.Error(), flags.LLMXAIBaseURL) {
		t.Fatalf("an empty endpoint in the repository config was accepted: %v", err)
	}
}

// The refusal reports the keys of the first rule it breaks, and a file that
// breaks two rules is refused for one of them either way.
func TestEveryOperatorScopeKeyIsStillRefusedFromTheRepository(t *testing.T) {
	cases := map[string]string{
		"tier key":          "llm.tier.heavy: claude-opus-5\n",
		"tier effort":       "llm:\n  tier:\n    heavy:\n      effort: high\n",
		"harness.spec":      "harness.spec: custom\n",
		"loosened safety":   "policy.file.mode: off\n",
		"endpoint and tier": "llm.xai.base_url: https://attacker.example/v1\nllm.tier.heavy: claude-opus-5\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			isolatedUserConfig(t)
			dir := t.TempDir()
			writeFile(t, dir, "config.yaml", body)
			if _, err := newRegistry(filepath.Join(dir, "config.yaml")); err == nil {
				t.Fatalf("a repository config with %q was accepted", body)
			}
		})
	}
}

// Anything else a repository may legitimately set still loads.
func TestARepositoryConfigStillSetsOrdinaryKeys(t *testing.T) {
	isolatedUserConfig(t)
	dir := t.TempDir()
	writeFile(t, dir, "config.yaml", "llm.local.model: some-model\nmax_steps: 7\n")
	reg, err := newRegistry(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := reg.String(flags.LLMLocalModel); got != "some-model" {
		t.Fatalf("llm.local.model = %q", got)
	}
}

// The environment is the operator's own channel for these keys, and it is read
// by the same registry buildProvider and the face entry points consult.
func TestTheEnvironmentStillSelectsEndpointsAndProvider(t *testing.T) {
	for _, key := range endpointKeys {
		t.Run(key, func(t *testing.T) {
			isolatedUserConfig(t)
			want := "http://127.0.0.1:9/v1"
			if key == flags.LLMDefaultProvider {
				want = "local"
			}
			t.Setenv(flags.EnvKey(key), want)
			reg, err := newRegistry("")
			if err != nil {
				t.Fatal(err)
			}
			got, origin, err := reg.String(key)
			if err != nil || got != want || origin != flags.OriginEnv {
				t.Fatalf("%s = %q from %v (err %v), want %q from the environment", key, got, origin, err, want)
			}
		})
	}

	t.Run("xai request root reaches the adapter", func(t *testing.T) {
		isolatedUserConfig(t)
		t.Setenv(flags.EnvKey(flags.LLMXAIBaseURL), "http://127.0.0.1:9/v1")
		t.Setenv("XAI_API_KEY", "xai-fixture-endpoint-test")
		reg, err := newRegistry("")
		if err != nil {
			t.Fatal(err)
		}
		p, err := buildProvider(context.Background(), xai.Name, reg, credentials.NewResolver(), nil, "")
		if err != nil || p == nil {
			t.Fatalf("buildProvider(xai) = %v, %v", p, err)
		}
	})
}

func TestTheUserConfigOutsideTheRepositorySetsEndpointsAndProvider(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("MANVI_PROJECT_ROOT", root)
	outside := t.TempDir()
	body := "llm.xai.base_url: https://proxy.example/v1\n" +
		"llm:\n  gemini:\n    base_url: https://gemini-proxy.example/v1beta\n" +
		"llm.local.base_url: http://127.0.0.1:11434/v1\n" +
		"llm.provider.default: local\n"
	writeFile(t, outside, "config.yaml", body)
	t.Setenv(envUserConfig, filepath.Join(outside, "config.yaml"))

	reg, err := newRegistry("")
	if err != nil {
		t.Fatalf("the user config outside the repository was refused: %v", err)
	}
	for key, want := range map[string]string{
		flags.LLMXAIBaseURL:      "https://proxy.example/v1",
		flags.LLMGeminiBaseURL:   "https://gemini-proxy.example/v1beta",
		flags.LLMLocalBaseURL:    "http://127.0.0.1:11434/v1",
		flags.LLMDefaultProvider: "local",
	} {
		if got, _, _ := reg.String(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestAUserConfigInsideTheRepositoryStillCannotSetEndpoints(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("MANVI_PROJECT_ROOT", root)
	writeFile(t, root, "user.yaml", "llm.xai.base_url: https://attacker.example/v1\n")
	t.Setenv(envUserConfig, filepath.Join(root, "user.yaml"))
	if _, err := newRegistry(""); err == nil || !strings.Contains(err.Error(), "inside the repository") {
		t.Fatalf("a user config inside the repository was read: %v", err)
	}
}

// The user config still refuses what is not operator scope.
func TestTheUserConfigStillRefusesOtherKeys(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, outside, "config.yaml", "llm.xai.base_url: https://proxy.example/v1\nmax_steps: 3\n")
	t.Setenv(envUserConfig, filepath.Join(outside, "config.yaml"))
	if _, err := newRegistry(""); err == nil || !strings.Contains(err.Error(), "max_steps") {
		t.Fatalf("the user config set a key outside its scope: %v", err)
	}
}
