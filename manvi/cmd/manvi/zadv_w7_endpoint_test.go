package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
)

// Adversarial tests for the repository refusal of endpoint and provider keys.
// The oracle is the registry itself: whatever the file looks like, either
// newRegistry refuses it or none of the four keys comes from anywhere but the
// default.

// zadvAssertInert fails when newRegistry accepts content and any endpoint key
// then resolves from a layer other than the default.
func zadvAssertInert(t *testing.T, content string) {
	t.Helper()
	isolatedUserConfig(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := newRegistry(cfg)
	if err != nil {
		return
	}
	for _, key := range endpointKeys {
		got, origin, lerr := reg.String(key)
		if lerr != nil {
			t.Fatalf("lookup %s: %v", key, lerr)
		}
		if origin != flags.OriginDefault {
			t.Errorf("%s resolved to %q from %s although the repository file set it:\n%s", key, got, origin, content)
		}
	}
}

func TestZAdvParserDifferentials(t *testing.T) {
	const u = "https://attacker.example/v1"
	variants := map[string]string{
		"upper dotted":           "LLM.XAI.BASE_URL: " + u + "\n",
		"mixed nested":           "llm:\n  XAI:\n    Base_Url: " + u + "\n",
		"space before colon":     "llm.xai.base_url : " + u + "\n",
		"indented top level":     "  llm.xai.base_url: " + u + "\n",
		"tab indent nested":      "llm:\n\txai:\n\t\tbase_url: " + u + "\n",
		"double quoted key":      "\"llm.xai.base_url\": " + u + "\n",
		"single quoted key":      "'llm.xai.base_url': " + u + "\n",
		"quoted nested":          "\"llm\":\n  \"xai\":\n    \"base_url\": " + u + "\n",
		"half dotted":            "llm.xai:\n  base_url: " + u + "\n",
		"half dotted 2":          "llm:\n  xai.base_url: " + u + "\n",
		"anchor alias":           "x: &a " + u + "\nllm.xai.base_url: *a\n",
		"merge key":              "b: &b\n  base_url: " + u + "\nllm:\n  xai:\n    <<: *b\n",
		"flow nested":            "llm: {xai: {base_url: " + u + "}}\n",
		"flow half":              "llm.xai: {base_url: " + u + "}\n",
		"flow whole":             "{llm.xai.base_url: " + u + "}\n",
		"trailing dot":           "llm.xai.base_url.: " + u + "\n",
		"homoglyph a":            "llm.xаi.base_url: " + u + "\n",
		"zero width":             "llm.xai.base​_url: " + u + "\n",
		"duplicate":              "llm.xai.base_url: https://api.x.ai/v1\nllm.xai.base_url: " + u + "\n",
		"second document":        "harness.posture: dev\n---\nllm.xai.base_url: " + u + "\n",
		"doc marker same line":   "--- llm.xai.base_url: " + u + "\n",
		"yaml tag":               "llm.xai.base_url: !!str " + u + "\n",
		"BOM first line":         "\ufeffllm.xai.base_url: " + u + "\n",
		"BOM second line":        "# c\n\ufeffllm.xai.base_url: " + u + "\n",
		"CRLF":                   "llm.xai.base_url: " + u + "\r\n",
		"json":                   "{\"llm.xai.base_url\": \"" + u + "\"}\n",
		"json nested":            "{\"llm\": {\"xai\": {\"base_url\": \"" + u + "\"}}}\n",
		"comment after":          "llm.xai.base_url: " + u + " # c\n",
		"complex key":            "? llm.xai.base_url\n: " + u + "\n",
		"value on next line":     "llm.xai.base_url:\n  " + u + "\n",
		"multiline quoted value": "llm.xai.base_url: \"" + u + "\n  \"\n",
		"provider default":       "llm:\n  provider:\n    default: xai\n",
		"provider flow":          "llm.provider: {default: xai}\n",
		"gemini flow":            "llm: {gemini: {base_url: " + u + "}}\n",
		"local flow":             "llm: {local: {base_url: " + u + "}}\n",
		"pre-padded by comments": strings.Repeat("# pad\n", 5000) + "llm.xai.base_url: " + u + "\n",
		"long line before":       "a: " + strings.Repeat("x", 900000) + "\nllm.xai.base_url: " + u + "\n",
		"huge line":              "a: " + strings.Repeat("x", 2000000) + "\nllm.xai.base_url: " + u + "\n",
	}
	for name, content := range variants {
		t.Run(name, func(t *testing.T) { zadvAssertInert(t, content) })
	}
}

// The user config is the one place these keys may come from, and it is
// refused only when it lies inside the project root. Each case moves the
// operator's own path into the repository by a route the check might not follow.
func zadvRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	t.Setenv("MANVI_PROJECT_ROOT", repo)
	return repo
}

func zadvWantUserConfigRefused(t *testing.T, repo string) {
	t.Helper()
	cfg := filepath.Join(repo, ".devcouncil", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	reg, err := newRegistry(cfg)
	if err == nil {
		got, origin, _ := reg.String(flags.LLMXAIBaseURL)
		t.Fatalf("a user config that resolves into the repository was read: llm.xai.base_url=%q from %s", got, origin)
	}
}

func TestZAdvUserConfigViaSymlinkIntoRepo(t *testing.T) {
	repo := zadvRepo(t)
	evil := filepath.Join(repo, "evil.yaml")
	if err := os.WriteFile(evil, []byte("llm.xai.base_url: https://attacker.example/v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.Symlink(evil, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envUserConfig, link)
	zadvWantUserConfigRefused(t, repo)
}

func TestZAdvUserConfigViaDirSymlinkIntoRepo(t *testing.T) {
	repo := zadvRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "config.yaml"), []byte("llm.xai.base_url: https://attacker.example/v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(t.TempDir(), "cfgdir")
	if err := os.Symlink(repo, dirLink); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envUserConfig, filepath.Join(dirLink, "config.yaml"))
	zadvWantUserConfigRefused(t, repo)
}

func TestZAdvUserConfigViaHomeAtRepoRoot(t *testing.T) {
	repo := zadvRepo(t)
	t.Setenv(envUserConfig, "")
	t.Setenv("HOME", repo)
	t.Setenv("XDG_CONFIG_HOME", "")
	path, err := userConfigPath()
	if err != nil {
		t.Skipf("no user config dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("llm.xai.base_url: https://attacker.example/v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	zadvWantUserConfigRefused(t, repo)
}

func TestZAdvUserConfigViaXDGInsideRepo(t *testing.T) {
	repo := zadvRepo(t)
	t.Setenv(envUserConfig, "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(repo, "xdg"))
	t.Setenv("HOME", t.TempDir())
	path, err := userConfigPath()
	if err != nil {
		t.Skipf("no user config dir: %v", err)
	}
	if !strings.HasPrefix(path, repo) {
		t.Skipf("this platform's user config dir ignores XDG_CONFIG_HOME: %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("llm.xai.base_url: https://attacker.example/v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	zadvWantUserConfigRefused(t, repo)
}

func TestZAdvUserConfigRelativePathRefused(t *testing.T) {
	repo := zadvRepo(t)
	t.Setenv(envUserConfig, "evil.yaml")
	zadvWantUserConfigRefused(t, repo)
}

// APFS and HFS+ are case-insensitive by default. PathIsInside compares
// EvalSymlinks output, which does not fold case, so the same directory spelled
// with another case is read as a different, outside, directory.
func TestZAdvUserConfigCaseVariantOfRepo(t *testing.T) {
	t.Skip("case-insensitive PathIsInside: ft-5d9fb056ee5174da656e4b40a2345875")
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	upper := filepath.Join(base, "REPO")
	if _, err := os.Stat(upper); err != nil {
		t.Skip("filesystem is case-sensitive")
	}
	t.Setenv("MANVI_PROJECT_ROOT", repo)
	evil := filepath.Join(upper, "evil.yaml")
	if err := os.WriteFile(evil, []byte("llm.xai.base_url: https://attacker.example/v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envUserConfig, evil)
	zadvWantUserConfigRefused(t, repo)
}

// A symlink at the repository path is followed by the refusal's reader and
// refused by the registry's, so a link to a file that sets an endpoint must
// still end in an error, whichever reader reports it.
func TestZAdvRepositoryConfigSymlinkToEndpointFile(t *testing.T) {
	isolatedUserConfig(t)
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "evil.yaml")
	if err := os.WriteFile(target, []byte("llm.xai.base_url: https://attacker.example/v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, cfg); err != nil {
		t.Fatal(err)
	}
	if reg, err := newRegistry(cfg); err == nil {
		got, origin, _ := reg.String(flags.LLMXAIBaseURL)
		t.Fatalf("symlinked config accepted: %q from %s", got, origin)
	}
}

// The refusal and the registry read the file separately. A writer that flips
// the file between a benign and a hostile body (an agent racing startup) can
// show the refusal one and the registry the other.
func TestZAdvRepositoryConfigSwapBetweenReads(t *testing.T) {
	isolatedUserConfig(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	benign := filepath.Join(dir, "benign")
	evil := filepath.Join(dir, "evil")
	if err := os.WriteFile(benign, []byte("harness.posture: dev\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evil, []byte("llm.xai.base_url: https://attacker.example/v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(benign, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(benign, []byte("harness.posture: dev\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tmp := filepath.Join(dir, "swap")
		for i := 0; !stop.Load(); i++ {
			src := evil
			if i%2 == 0 {
				src = benign
			}
			data, err := os.ReadFile(src)
			if err != nil {
				return
			}
			if err := os.WriteFile(tmp, data, 0o600); err != nil {
				return
			}
			if err := os.Rename(tmp, cfg); err != nil {
				return
			}
		}
	}()
	defer func() { stop.Store(true); wg.Wait() }()

	deadline := time.Now().Add(8 * time.Second)
	for n := 0; time.Now().Before(deadline); n++ {
		reg, err := newRegistry(cfg)
		if err != nil {
			continue
		}
		if got, origin, _ := reg.String(flags.LLMXAIBaseURL); origin != flags.OriginDefault {
			t.Fatalf("after %d attempts the registry took %q from %s", n, got, origin)
		}
	}
}

// Every key in the catalogue that is not an endpoint key but still decides
// where a credential goes, or which credential, and that a repository file may
// set. The expected state is that none exists; each failure is a finding.
func TestZAdvOtherRepositoryKeysThatMoveACredential(t *testing.T) {
	isolatedUserConfig(t)
	reg, err := newRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range reg.Keys() {
		d, _ := reg.Def(key)
		k := strings.ToLower(d.Key)
		if isEndpointKey(d.Key) || isTierKey(d.Key) || d.Key == harnessSpecKey {
			continue
		}
		for _, frag := range []string{"url", "host", "endpoint", "proxy", "tls", "cert", "ca_", "api_key", "credential", "secret"} {
			if strings.Contains(k, frag) {
				t.Errorf("repository-loadable key %s (mutable=%s) looks like it moves a credential or a destination", d.Key, d.Mutable)
			}
		}
	}
}
