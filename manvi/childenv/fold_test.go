package childenv

import (
	"slices"
	"strings"
	"testing"
)

func TestWindowsNamesAreComparedCaseInsensitively(t *testing.T) {
	in := []string{
		"Anthropic_Api_Key=mixed", "anthropic_api_key=lower", "ANTHROPIC_API_KEY=upper",
		"openai_api_key=o", `Path=C:\bin`, "GH_TOKEN=forge",
	}
	want := []string{`Path=C:\bin`, "GH_TOKEN=forge"}
	if got := withoutProviderCredentials(in, true); !slices.Equal(got, want) {
		t.Fatalf("fold=true: got %q, want %q (every spelling of a withheld name must go)", got, want)
	}
}

func TestUnixNamesAreComparedCaseSensitively(t *testing.T) {
	in := []string{"Anthropic_Api_Key=distinct", "ANTHROPIC_API_KEY=k", "openai_api_key=distinct"}
	want := []string{"Anthropic_Api_Key=distinct", "openai_api_key=distinct"}
	if got := withoutProviderCredentials(in, false); !slices.Equal(got, want) {
		t.Fatalf("fold=false: got %q, want %q", got, want)
	}
}

func TestExportedFiltersFollowTheHostOS(t *testing.T) {
	old := foldEnvNames
	t.Cleanup(func() { foldEnvNames = old })
	for _, fold := range []bool{true, false} {
		foldEnvNames = fold
		got := WithoutProviderCredentials([]string{"Anthropic_Api_Key=x"})
		if dropped := len(got) == 0; dropped != fold {
			t.Errorf("foldEnvNames=%v: got %q", fold, got)
		}
	}
}

func TestFilterAttacks(t *testing.T) {
	in := []string{
		`=C:=C:\work`, "=ExitCode=00000000", // Windows drive/status variables: empty name
		"=ANTHROPIC_API_KEY",           // empty name, value looks like a name
		"ANTHROPIC_API_KEY_X=keep",     // a longer name is not a prefix match
		"ANTHROPIC_API_KEY =keep",      // a trailing space is a different name
		"",                             // empty entry
		"X=ANTHROPIC_API_KEY=in-value", // withheld text in a value
	}
	for _, fold := range []bool{true, false} {
		if got := withoutProviderCredentials(in, fold); !slices.Equal(got, in) {
			t.Errorf("fold=%v: got %q, want everything kept", fold, got)
		}
		// An entry with no '=' has the whole entry as its name.
		if got := withoutProviderCredentials([]string{"ANTHROPIC_API_KEY"}, fold); len(got) != 0 {
			t.Errorf("fold=%v: bare name kept: %q", fold, got)
		}
	}
}

func TestUnicodeFoldingDoesNotJoinNamesWindowsKeepsApart(t *testing.T) {
	// U+212A KELVIN SIGN is its own upper-case letter: EqualFold would join it
	// to K, ToUpper does not. The dotless i (U+0131) upper-cases to I, so that
	// spelling is withheld: wider than Windows, which is the safe direction.
	kelvin := "ANTHROPIC_API_\u212aEY=distinct"
	in := []string{kelvin, "ANTHROP\u0131C_API_KEY=wider"}
	if got := withoutProviderCredentials(in, true); !slices.Equal(got, []string{kelvin}) {
		t.Fatalf("got %q", got)
	}
}

func TestGitExtraReplacesAllowlistedNameOnWindowsRegardlessOfCase(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	joined := strings.Join(gitEnv(true, []string{"path=/scratch"}), "\n")
	if strings.Count(strings.ToUpper(joined), "PATH=") != 1 || !strings.Contains(joined, "path=/scratch") {
		t.Fatalf("fold=true: extra did not replace PATH:\n%s", joined)
	}
	joined = strings.Join(gitEnv(false, []string{"path=/scratch"}), "\n")
	if !strings.Contains(joined, "PATH=/usr/bin") || !strings.Contains(joined, "path=/scratch") {
		t.Fatalf("fold=false: names must stay distinct:\n%s", joined)
	}
}
