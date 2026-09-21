package main

import (
	"os"
	"regexp"
	"slices"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// The managed factory in serve.go routes a stored run's provider to the adapter
// that starts it, and it does so with a `switch` written out by hand. That
// switch is a *decision site*: a provider in codingagent.ManagedProviders with
// no case here is admitted by the runner, published at hello, offered by the
// host, claimed against the repository's one run slot — and only then refused
// at spawn, after everything reversible has already happened.
//
// The same shape has now cost this lane twice: once in the host's response
// parser, which held its own `provider == "codex"` and failed every managed
// Claude launch, and once in a harness binary that was simply older than the
// adapter set the source described. Whole-process gates agreeing is worth
// nothing while a hand-written list sits behind them.
//
// So the switch is read from the source that decides, and compared with the
// adapter set rather than with a second transcription of it.
func TestManagedFactoryStartsExactlyTheAdvertisedAdapterSet(t *testing.T) {
	source, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	// Bounded by the switch's own `default:` rather than by a closing brace —
	// the first `}` inside is an `if err != nil` arm, and stopping there read
	// only the first case, which is exactly the blind spot being tested for.
	factory := regexp.
		MustCompile(`(?s)NewManagedRunner\(client, func\(.*?switch options\.Provider \{(.*?)\n\t+default:`).
		FindSubmatch(source)
	if factory == nil {
		t.Fatal("the managed factory no longer routes with a `switch options.Provider` closed by a `default:` " +
			"this test can read; update this test to read whatever decides now, rather than deleting it")
	}
	var routed []string
	for _, match := range regexp.MustCompile(`case "([^"]+)":`).FindAllSubmatch(factory[1], -1) {
		routed = append(routed, string(match[1]))
	}
	if len(routed) == 0 {
		t.Fatal("read the factory but found no provider cases, which would mean every managed run is refused at spawn")
	}

	for _, provider := range routed {
		if !codingagent.IsManagedProvider(provider) {
			t.Errorf("serve.go starts %q, which is not in the advertised adapter set %v: "+
				"an adapter no run can reach", provider, codingagent.ManagedProviders)
		}
	}
	for _, provider := range codingagent.ManagedProviders {
		if !slices.Contains(routed, provider) {
			t.Errorf("%q is advertised as a managed adapter but serve.go's factory has no case for it: "+
				"the attempt is stored and claimed, then dies at spawn with the repository already taken",
				provider)
		}
	}

	// Each case must also name the binary it starts through toolBinary, so an
	// operator can override it; a case that hardcodes a program name is one
	// nobody can point at a different install.
	for _, provider := range routed {
		pattern := regexp.MustCompile(`case "` + regexp.QuoteMeta(provider) + `":\s*\n\s*options\.Program = toolBinary\(`)
		if !pattern.Match(factory[1]) {
			t.Errorf("the %q case does not resolve its program through toolBinary", provider)
		}
	}
}
