package childenv

import (
	"slices"
	"strings"
	"testing"
)

// The names a host injects for the harness's own adapter are the ones that
// must be on the list. If the catalogue stops naming one, a host that checks
// `withheld_provider_env` before injecting would be told the truth and refuse;
// this test makes that a failure here first.
func TestWithheldNamesEveryProviderCredentialAHostInjects(t *testing.T) {
	got := Withheld()
	for _, name := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_WORKSPACE_ID", "ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_API_KEY_FALLBACK", "ANTHROPIC_WORKSPACE_ID_FALLBACK",
		"OPENAI_API_KEY", "XAI_API_KEY", "GROK_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "LOCAL_API_KEY",
	} {
		if !slices.Contains(got, name) {
			t.Errorf("Withheld() = %q, missing %s", got, name)
		}
	}
	if !slices.IsSorted(got) || len(slices.Compact(slices.Clone(got))) != len(got) {
		t.Errorf("Withheld() = %q, want sorted with each name once", got)
	}
}

func TestTheGitAllowlistNamesNoProviderCredential(t *testing.T) {
	for _, name := range gitAllowlist {
		if slices.Contains(Withheld(), name) {
			t.Errorf("%s is a provider credential and is on the git allowlist", name)
		}
	}
}

func TestGitWithholdsProviderCredentialsAndKeepsWhatGitNeeds(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-parent")
	t.Setenv("ANTHROPIC_WORKSPACE_ID", "wrkspc_parent")
	t.Setenv("OPENAI_API_KEY", "sk-openai-parent")
	t.Setenv("SOME_FUTURE_TOKEN", "unlisted")
	t.Setenv("GIT_DIR", "/elsewhere/.git")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/home/operator")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("LC_ALL", "en_US.UTF-8")

	env := Git("LC_ALL=C", "GIT_INDEX_FILE=/scratch/index")
	joined := strings.Join(env, "\n")
	for _, leaked := range []string{"sk-ant-parent", "wrkspc_parent", "sk-openai-parent", "unlisted", "/elsewhere/.git"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("git environment carries %q:\n%s", leaked, joined)
		}
	}
	for _, kept := range []string{"PATH=/usr/bin:/bin", "HOME=/home/operator", "SSH_AUTH_SOCK=/tmp/agent.sock", "GIT_INDEX_FILE=/scratch/index", "LC_ALL=C"} {
		if !slices.Contains(env, kept) {
			t.Errorf("git environment is missing %s:\n%s", kept, joined)
		}
	}
	if strings.Count(joined, "LC_ALL=") != 1 {
		t.Errorf("an extra did not replace the allowlisted value of the same name:\n%s", joined)
	}
}

func TestWithoutProviderCredentialsKeepsEverythingElse(t *testing.T) {
	in := []string{
		"ANTHROPIC_API_KEY=k", "ANTHROPIC_WORKSPACE_ID=w", "ANTHROPIC_AUTH_TOKEN=t", "OPENAI_API_KEY=o",
		"PATH=/bin", "GH_TOKEN=forge", "CLAUDE_CODE_OAUTH_TOKEN=login", "ANTHROPIC_BASE_URL=https://gw",
	}
	got := WithoutProviderCredentials(in)
	want := []string{"PATH=/bin", "GH_TOKEN=forge", "CLAUDE_CODE_OAUTH_TOKEN=login", "ANTHROPIC_BASE_URL=https://gw"}
	if !slices.Equal(got, want) {
		t.Fatalf("WithoutProviderCredentials = %q, want %q", got, want)
	}
	if WithoutProviderCredentials(nil) == nil {
		t.Fatal("an empty environment came back nil, which os/exec reads as full inheritance")
	}
}

func TestInheritWithholdsFromThisProcessEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-parent")
	t.Setenv("PATH", "/usr/bin")
	env := Inherit()
	if slices.Contains(env, "ANTHROPIC_API_KEY=sk-ant-parent") || !slices.Contains(env, "PATH=/usr/bin") {
		t.Fatalf("Inherit() = %q", env)
	}
}
