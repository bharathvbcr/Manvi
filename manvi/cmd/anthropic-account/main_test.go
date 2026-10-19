package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/credentials"
)

const (
	mainKey = "sk-ant-test-main-0000"
	altKey  = "sk-ant-test-alt-1111"
)

// fakeKeychain is a map of service to key. reads records every secret read,
// so a test can prove a command never asked for one.
type fakeKeychain struct {
	items map[string]string
	reads []string
}

func (k *fakeKeychain) read(service string) (string, error) {
	k.reads = append(k.reads, service)
	v, ok := k.items[service]
	if !ok {
		return "", errItemMissing
	}
	return v + "\n", nil
}

func (k *fakeKeychain) has(service string) (bool, error) {
	_, ok := k.items[service]
	return ok, nil
}

type launch struct {
	path string
	argv []string
	env  []string
}

type harness struct {
	home     string
	keychain *fakeKeychain
	env      map[string]string
	launched *launch
	stdout   bytes.Buffer
	stderr   bytes.Buffer
}

func newHarness(t *testing.T, registry string) *harness {
	t.Helper()
	home := t.TempDir()
	if registry != "" {
		write(t, filepath.Join(home, registryName), registry)
	}
	return &harness{
		home: home,
		keychain: &fakeKeychain{items: map[string]string{
			keychainPrefix + "main": mainKey,
			keychainPrefix + "alt":  altKey,
		}},
		env: map[string]string{},
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) run(cwd string, args ...string) int {
	h.stdout.Reset()
	h.stderr.Reset()
	h.launched = nil
	environ := []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + h.home,
		"ANTHROPIC_API_KEY=sk-ant-inherited",
		"ANTHROPIC_WORKSPACE_ID=wrkspc_inherited",
		"ANTHROPIC_AUTH_TOKEN=bearer-inherited",
		"ANTHROPIC_API_KEY_FALLBACK=sk-ant-inherited-fallback",
		"ANTHROPIC_WORKSPACE_ID_FALLBACK=wrkspc_inherited_fallback",
		"ANTHROPIC_BASE_URL=https://example.invalid",
	}
	if cwd == "" {
		cwd = h.home
	}
	return run(args, deps{
		home:     h.home,
		cwd:      cwd,
		getenv:   func(k string) string { return h.env[k] },
		environ:  environ,
		keychain: h.keychain,
		lookPath: func(name string) (string, error) { return "/bin/" + name, nil },
		execve: func(path string, argv, env []string) error {
			h.launched = &launch{path: path, argv: argv, env: env}
			return nil
		},
		stdout: &h.stdout,
		stderr: &h.stderr,
	})
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestParseRegistry(t *testing.T) {
	got, err := parseRegistry([]byte("# accounts\n\nmain\n  alt   wrkspc_01Alt  \n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []account{{Name: "main"}, {Name: "alt", Workspace: "wrkspc_01Alt"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for _, bad := range []string{
		"main\nmain\n",          // listed twice
		"main wrkspc_1 extra\n", // too many fields
		"ma/in\n",               // name outside the alphabet
		"main wrkspc$01\n",      // workspace outside the alphabet
		"main " + strings.Repeat("w", 200) + "\n", // workspace over the bound
	} {
		if _, err := parseRegistry([]byte(bad)); err == nil {
			t.Errorf("parseRegistry(%q) accepted", bad)
		}
	}
	var many strings.Builder
	for i := 0; i <= maxAccounts; i++ {
		many.WriteString("a" + strings.Repeat("x", i) + "\n")
	}
	if _, err := parseRegistry([]byte(many.String())); err == nil {
		t.Error("accepted more than maxAccounts accounts")
	}
}

func TestNoRegistryRefusesWithMigrationSteps(t *testing.T) {
	h := newHarness(t, "")
	if code := h.run("", "exec", "--", "manvi"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if h.launched != nil {
		t.Fatal("launched without a registry")
	}
	msg := h.stderr.String()
	for _, want := range []string{"no account registry", "security add-generic-password", keychainPrefix + "<name>", registryName} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal lacks %q:\n%s", want, msg)
		}
	}
	if len(h.keychain.reads) != 0 {
		t.Fatalf("read the Keychain before finding a registry: %v", h.keychain.reads)
	}
}

func TestEmptyRegistryIsRefused(t *testing.T) {
	h := newHarness(t, "# nothing yet\n")
	if code := h.run("", "exec", "--", "manvi"); code != 1 || h.launched != nil {
		t.Fatalf("exit %d, launched %v", code, h.launched != nil)
	}
	if !strings.Contains(h.stderr.String(), "lists no accounts") {
		t.Fatalf("stderr: %s", h.stderr.String())
	}
}

func TestSelectionOrder(t *testing.T) {
	h := newHarness(t, "main\nalt wrkspc_01Alt\n")
	repo := filepath.Join(h.home, "Code", "proj")
	sub := filepath.Join(repo, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	which := func(args ...string) whichReport {
		t.Helper()
		if code := h.run(sub, append([]string{"which", "--json"}, args...)...); code != 0 {
			t.Fatalf("which exit %d: %s", code, h.stderr.String())
		}
		var r whichReport
		if err := json.Unmarshal(h.stdout.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}

	if r := which(); r.Account != "main" || r.Source != "default" || r.Fallback == nil || r.Fallback.Name != "alt" {
		t.Fatalf("default: %+v", r)
	}
	write(t, filepath.Join(repo, markerName), "alt\n")
	if r := which(); r.Account != "alt" || r.Source != filepath.Join(repo, markerName) || r.Workspace != "wrkspc_01Alt" || r.Fallback.Name != "main" {
		t.Fatalf("marker: %+v", r)
	}
	h.env["ANTHROPIC_ACCOUNT"] = "main"
	if r := which(); r.Account != "main" || r.Source != "ANTHROPIC_ACCOUNT" {
		t.Fatalf("env over marker: %+v", r)
	}
	if r := which("--account", "alt"); r.Account != "alt" || r.Source != "--account" {
		t.Fatalf("flag over env: %+v", r)
	}
	if len(h.keychain.reads) != 0 {
		t.Fatalf("which read secrets: %v", h.keychain.reads)
	}
}

func TestMarkerInHomeIsIgnored(t *testing.T) {
	h := newHarness(t, "main\nalt\n")
	write(t, filepath.Join(h.home, markerName), "alt\n")
	dir := filepath.Join(h.home, "Code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if code := h.run(dir, "which", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), `"account":"main"`) {
		t.Fatalf("a marker in home pinned the account: %s", h.stdout.String())
	}
}

func TestRepoFlagChoosesWhereToLookForTheMarker(t *testing.T) {
	h := newHarness(t, "main\nalt\n")
	repo := filepath.Join(h.home, "proj")
	write(t, filepath.Join(repo, markerName), "alt")
	if code := h.run(h.home, "which", "--repo", repo, "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), `"account":"alt"`) {
		t.Fatalf("--repo marker ignored: %s", h.stdout.String())
	}
}

func TestBadSelectorsAreRefusedNotSkipped(t *testing.T) {
	h := newHarness(t, "main\nalt\n")
	repo := filepath.Join(h.home, "proj")
	write(t, filepath.Join(repo, markerName), "  \n")
	if code := h.run(repo, "exec", "--", "manvi"); code != 1 || !strings.Contains(h.stderr.String(), "is empty") {
		t.Fatalf("empty marker: exit %d, %s", code, h.stderr.String())
	}
	write(t, filepath.Join(repo, markerName), "nobody\n")
	if code := h.run(repo, "exec", "--", "manvi"); code != 1 || !strings.Contains(h.stderr.String(), `"nobody"`) {
		t.Fatalf("unknown marker: exit %d, %s", code, h.stderr.String())
	}
	if code := h.run(h.home, "exec", "--account", "../x", "--", "manvi"); code != 1 {
		t.Fatalf("bad flag name: exit %d", code)
	}
	if h.launched != nil {
		t.Fatal("launched after a refusal")
	}
}

func TestExecReplacesInheritedCredentials(t *testing.T) {
	h := newHarness(t, "main\nalt wrkspc_01Alt\n")
	if code := h.run("", "exec", "--", "manvi", "run", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	l := h.launched
	if l == nil || l.path != "/bin/manvi" || strings.Join(l.argv, " ") != "manvi run --json" {
		t.Fatalf("launch: %+v", l)
	}
	env := envMap(l.env)
	if env["ANTHROPIC_API_KEY"] != mainKey {
		t.Errorf("key: got %q", env["ANTHROPIC_API_KEY"])
	}
	if _, ok := env["ANTHROPIC_WORKSPACE_ID"]; ok {
		t.Errorf("main lists no workspace, but the child got %q", env["ANTHROPIC_WORKSPACE_ID"])
	}
	if _, ok := env["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Error("inherited ANTHROPIC_AUTH_TOKEN survived")
	}
	if env[credentials.AnthropicFallbackKeyEnv] != altKey || env[credentials.AnthropicFallbackWorkspaceEnv] != "wrkspc_01Alt" {
		t.Errorf("fallback: key %q workspace %q", env[credentials.AnthropicFallbackKeyEnv], env[credentials.AnthropicFallbackWorkspaceEnv])
	}
	if env["ANTHROPIC_BASE_URL"] != "https://example.invalid" || env["PATH"] != "/usr/bin:/bin" {
		t.Error("unrelated variables were not passed through")
	}
	for _, kv := range l.env {
		if strings.Contains(kv, "inherited") {
			t.Errorf("inherited credential survived: %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
	counts := map[string]int{}
	for _, kv := range l.env {
		k, _, _ := strings.Cut(kv, "=")
		counts[k]++
	}
	for k, n := range counts {
		if n > 1 {
			t.Errorf("%s set %d times", k, n)
		}
	}
	if out := h.stdout.String() + h.stderr.String(); strings.Contains(out, mainKey) || strings.Contains(out, altKey) {
		t.Fatal("a key was printed")
	}
}

func TestExecChosenWorkspaceAndNoFallback(t *testing.T) {
	h := newHarness(t, "main\nalt wrkspc_01Alt\n")
	if code := h.run("", "exec", "--account", "alt", "--no-fallback", "--", "vani", "draft"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	env := envMap(h.launched.env)
	if env["ANTHROPIC_API_KEY"] != altKey || env["ANTHROPIC_WORKSPACE_ID"] != "wrkspc_01Alt" {
		t.Fatalf("alt: key %q workspace %q", env["ANTHROPIC_API_KEY"], env["ANTHROPIC_WORKSPACE_ID"])
	}
	if _, ok := env[credentials.AnthropicFallbackKeyEnv]; ok {
		t.Fatal("--no-fallback still handed over a fallback key")
	}
	if strings.Join(h.keychain.reads, ",") != keychainPrefix+"alt" {
		t.Fatalf("reads: %v", h.keychain.reads)
	}
}

func TestSingleAccountHasNoFallback(t *testing.T) {
	h := newHarness(t, "main\n")
	if code := h.run("", "exec", "--", "manvi"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if _, ok := envMap(h.launched.env)[credentials.AnthropicFallbackKeyEnv]; ok {
		t.Fatal("one account produced a fallback")
	}
}

func TestMissingPrimaryKeyRefuses(t *testing.T) {
	h := newHarness(t, "main\nalt\n")
	delete(h.keychain.items, keychainPrefix+"main")
	if code := h.run("", "exec", "--", "manvi"); code != 1 || h.launched != nil {
		t.Fatalf("exit %d, launched %v", code, h.launched != nil)
	}
	if !strings.Contains(h.stderr.String(), "security add-generic-password") {
		t.Fatalf("no remedy in: %s", h.stderr.String())
	}
}

func TestMissingFallbackKeyWarnsAndRuns(t *testing.T) {
	h := newHarness(t, "main\nalt wrkspc_01Alt\n")
	delete(h.keychain.items, keychainPrefix+"alt")
	if code := h.run("", "exec", "--", "manvi"); code != 0 || h.launched == nil {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	env := envMap(h.launched.env)
	if _, ok := env[credentials.AnthropicFallbackKeyEnv]; ok {
		t.Fatal("fallback key set without one")
	}
	if _, ok := env[credentials.AnthropicFallbackWorkspaceEnv]; ok {
		t.Fatal("fallback workspace set without a fallback key")
	}
	if !strings.Contains(h.stderr.String(), "warning: no fallback key") {
		t.Fatalf("no warning: %s", h.stderr.String())
	}
}

func TestMalformedKeyIsRefused(t *testing.T) {
	h := newHarness(t, "main\n")
	h.keychain.items[keychainPrefix+"main"] = "sk-ant-one\nsk-ant-two"
	if code := h.run("", "exec", "--", "manvi"); code != 1 || h.launched != nil {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(h.stderr.String(), "sk-ant-one") {
		t.Fatal("the refusal printed the item's contents")
	}
}

func TestExecNeedsACommand(t *testing.T) {
	h := newHarness(t, "main\n")
	if code := h.run("", "exec", "--"); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if len(h.keychain.reads) != 0 {
		t.Fatal("read a key with nothing to launch")
	}
}

func TestExecReportsALaunchFailure(t *testing.T) {
	h := newHarness(t, "main\n")
	code := run([]string{"exec", "--", "manvi"}, deps{
		home: h.home, cwd: h.home,
		getenv:   func(string) string { return "" },
		keychain: h.keychain,
		lookPath: func(string) (string, error) { return "", errors.New("not found") },
		execve:   func(string, []string, []string) error { t.Fatal("execve after a failed lookup"); return nil },
		stdout:   &h.stdout, stderr: &h.stderr,
	})
	if code != 1 || !strings.Contains(h.stderr.String(), "exec manvi") {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
}

func TestDoctor(t *testing.T) {
	h := newHarness(t, "main\nalt\n")
	if code := h.run("", "doctor"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, h.stdout.String(), h.stderr.String())
	}
	delete(h.keychain.items, keychainPrefix+"alt")
	if code := h.run("", "doctor"); code != 1 || !strings.Contains(h.stdout.String(), "FAIL  alt") {
		t.Fatalf("exit %d: %s", code, h.stdout.String())
	}
	if len(h.keychain.reads) != 0 {
		t.Fatalf("doctor read secrets: %v", h.keychain.reads)
	}
}

func TestList(t *testing.T) {
	h := newHarness(t, "main\nalt wrkspc_01Alt\n")
	if code := h.run("", "list", "--json"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got []account
	if err := json.Unmarshal(h.stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Workspace != "wrkspc_01Alt" {
		t.Fatalf("got %+v", got)
	}
}

// TestStripSetCoversTheCatalogue pins the launcher to Manvi's credential
// catalogue: every variable that carries or scopes an Anthropic key is one the
// launcher clears, so nothing inherited can ride along with the chosen key.
func TestStripSetCoversTheCatalogue(t *testing.T) {
	names, err := anthropicEnvNames()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range names.strip {
		got = append(got, k)
	}
	sort.Strings(got)
	for _, want := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
		credentials.AnthropicWorkspaceEnv,
		credentials.AnthropicFallbackKeyEnv, credentials.AnthropicFallbackWorkspaceEnv,
	} {
		if !names.strip[want] {
			t.Errorf("strip set %v lacks %s", got, want)
		}
	}
	if names.key != "ANTHROPIC_API_KEY" {
		t.Errorf("key variable %q", names.key)
	}
}
