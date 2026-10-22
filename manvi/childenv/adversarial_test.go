package childenv

import (
	"slices"
	"strings"
	"testing"
)

// Under fold, every spelling of every withheld name must go, however many
// duplicates and whatever the position, and none may survive "later" because
// os/exec on Windows keeps the last duplicate.
func TestAdversarialFoldDropsEverySpellingOfEveryWithheldName(t *testing.T) {
	var in []string
	for _, name := range Withheld() {
		in = append(in, strings.ToLower(name)+"=lo", name+"=up", strings.ToUpper(name[:1])+strings.ToLower(name[1:])+"=mixed")
	}
	in = append(in, "KEEP=1")
	if got := withoutProviderCredentials(in, true); !slices.Equal(got, []string{"KEEP=1"}) {
		t.Fatalf("a spelling survived: %q", got)
	}
}

func TestAdversarialExportedFiltersWithFoldOnRealEnviron(t *testing.T) {
	old := foldEnvNames
	t.Cleanup(func() { foldEnvNames = old })
	foldEnvNames = true
	t.Setenv("Anthropic_Api_Key", "secret")
	for _, e := range Inherit() {
		if strings.Contains(e, "secret") {
			t.Fatalf("leaked: %q", e)
		}
	}
	if got := WithoutProviderCredentials(nil); got == nil {
		t.Fatal("nil result would mean inherit everything")
	}
}

func TestAdversarialGitUnixKeepsDifferentlyCasedExtrasDistinct(t *testing.T) {
	old := foldEnvNames
	t.Cleanup(func() { foldEnvNames = old })
	foldEnvNames = false
	t.Setenv("HOME", "/h")
	got := Git("home=/x")
	if !slices.Contains(got, "HOME=/h") || !slices.Contains(got, "home=/x") {
		t.Fatalf("names must stay distinct on unix: %q", got)
	}
}

// With fold, no allowlisted name may appear twice in case-insensitive terms
// when an extra overrides it, and the extra must be the one that is last.
func TestAdversarialGitFoldExtraWinsAndIsLast(t *testing.T) {
	t.Setenv("LANG", "C.UTF-8")
	got := gitEnv(true, []string{"lang=x", "Lang=y"})
	var seen []string
	for _, e := range got {
		if strings.EqualFold(entryName(e), "LANG") {
			seen = append(seen, e)
		}
	}
	if want := []string{"lang=x", "Lang=y"}; !slices.Equal(seen, want) {
		t.Fatalf("got %q want %q", seen, want)
	}
}
