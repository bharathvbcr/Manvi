package agents

import "testing"

func TestParsePlacement(t *testing.T) {
	known := func(name string) bool {
		return name == "local" || name == "anthropic"
	}

	for _, tc := range []struct {
		name     string
		spec     string
		provider string
		model    string
		inherits bool
	}{
		{name: "empty inherits", spec: "", inherits: true},
		{name: "inherit keyword", spec: "inherit", inherits: true},
		{name: "inherit is case-insensitive", spec: "Inherit", inherits: true},
		{name: "surrounding space is not a model name", spec: "  inherit  ", inherits: true},

		{name: "qualified", spec: "anthropic/claude-opus-4-5",
			provider: "anthropic", model: "claude-opus-4-5"},
		{name: "qualified with spaces", spec: " local / qwen3-27b-mlx ",
			provider: "local", model: "qwen3-27b-mlx"},

		{name: "bare provider takes its default model", spec: "local", provider: "local"},
		{name: "bare model rides the parent provider", spec: "flash", model: "flash"},

		// An unregistered provider name must read as a model on the parent,
		// not as a provider that is not there: the second would fail the turn.
		{name: "unknown name is a model", spec: "gemini", model: "gemini"},

		// Half-empty specs: the empty half is dropped, never carried.
		{name: "leading separator", spec: "/qwen", model: "qwen"},
		{name: "trailing separator", spec: "anthropic/", provider: "anthropic"},
		{name: "bare separator inherits", spec: "/", inherits: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParsePlacement(tc.spec, known)
			if got.Provider != tc.provider || got.Model != tc.model {
				t.Fatalf("ParsePlacement(%q) = %+v, want provider=%q model=%q",
					tc.spec, got, tc.provider, tc.model)
			}
			if got.Inherits() != tc.inherits {
				t.Fatalf("ParsePlacement(%q).Inherits() = %v, want %v",
					tc.spec, got.Inherits(), tc.inherits)
			}
		})
	}
}

// A nil isProvider must not turn a bare word into a provider. Reading "local"
// as a provider when the caller cannot confirm one exists sends the child to a
// provider that may never have been registered, which fails the turn; reading
// it as a model keeps it on the parent, which is known to work.
func TestParsePlacementWithoutRegistryReadsBareWordsAsModels(t *testing.T) {
	got := ParsePlacement("local", nil)
	if got.Provider != "" || got.Model != "local" {
		t.Fatalf("ParsePlacement(%q, nil) = %+v, want it read as a model", "local", got)
	}
}

// Every built-in role must inherit by default. A role catalogue that silently
// pinned a provider would route work to a model the operator never enabled.
func TestBuiltinDefinitionsInheritPlacement(t *testing.T) {
	for _, def := range NewRegistry().List() {
		if p := ParsePlacement(def.Model, nil); !p.Inherits() {
			t.Fatalf("built-in role %q resolves to %+v, want inherit", def.Name, p)
		}
	}
}

func TestPlacementStringRoundTrips(t *testing.T) {
	known := func(string) bool { return true }
	for _, spec := range []string{"inherit", "anthropic/claude-opus-4-5", "local"} {
		if got := ParsePlacement(spec, known).String(); got != spec {
			t.Fatalf("ParsePlacement(%q).String() = %q, want %q", spec, got, spec)
		}
	}
}
