package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/credentials"
)

// Adversarial pass on b26-creds: who can make an endpoint Named, and does the
// credential reach an address nobody named. Every test PASSES when the change
// holds.

type advB26Recorder struct {
	*httptest.Server
	mu   sync.Mutex
	auth []string
}

func (r *advB26Recorder) saw() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auth...)
}

func newAdvB26Recorder(t *testing.T, status int, l net.Listener) *advB26Recorder {
	t.Helper()
	rec := &advB26Recorder{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.auth = append(rec.auth, r.Header.Get("Authorization"))
		rec.mu.Unlock()
		if r.URL.Path == "/v1/models" {
			if status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"only-model"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	if l != nil {
		rec.Server = httptest.NewUnstartedServer(h)
		_ = rec.Server.Listener.Close()
		rec.Server.Listener = l
		rec.Server.Start()
	} else {
		rec.Server = httptest.NewServer(h)
	}
	t.Cleanup(rec.Close)
	return rec
}

func advB26SentinelResolver() *credentials.Resolver {
	r := credentials.NewResolver()
	r.Set("local", credentials.NewSecret("sk-advb26-sentinel", "OPENAI_API_KEY"))
	return r
}

// advB26Env isolates the environment a registry reads, with the user config
// pointed at the given absolute path (it need not exist).
func advB26Env(t *testing.T, userConfig string) {
	t.Helper()
	for _, k := range []string{
		flags.EnvKey(flags.LLMLocalBaseURL), flags.EnvKey(flags.LLMXAIBaseURL), flags.EnvKey(flags.LLMGeminiBaseURL),
		flags.EnvKey(flags.LLMDefaultProvider), flags.EnvKey(flags.LLMLocalModel),
	} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	t.Setenv(envUserConfig, userConfig)
}

// advB26RepoFile writes a repository config.yaml and returns its path and root.
func advB26RepoFile(t *testing.T, body string) (cfg, root string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".devcouncil"), 0o750); err != nil {
		t.Fatal(err)
	}
	cfg = filepath.Join(root, ".devcouncil", "config.yaml")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, root
}

// TestAdvB26CredsRepositoryCannotNameALocalEndpoint drives the real loader
// (newRegistry) with a repository layer setting each key that could name an
// endpoint, in several spellings. Either the load is refused, or the value it
// produced is not Named (origin default).
func TestAdvB26CredsRepositoryCannotNameALocalEndpoint(t *testing.T) {
	const evil = "http://127.0.0.1:9/v1"
	bodies := map[string]string{
		"dotted":            "llm.local.base_url: " + evil + "\n",
		"nested":            "llm:\n  local:\n    base_url: " + evil + "\n",
		"quoted-key":        "\"llm.local.base_url\": " + evil + "\n",
		"flow":              "llm: {local: {base_url: " + evil + "}}\n",
		"mixed-case":        "LLM.LOCAL.BASE_URL: " + evil + "\n",
		"underscore":        "llm_local_base_url: " + evil + "\n",
		"env-style":         "MANVI_LLM_LOCAL_BASE_URL: " + evil + "\n",
		"anchor-alias":      "x: &u " + evil + "\nllm.local.base_url: *u\n",
		"duplicate-keys":    "llm.local.base_url: http://127.0.0.1:1/v1\nllm.local.base_url: " + evil + "\n",
		"trailing-space":    "llm.local.base_url : " + evil + "\n",
		"endpoints-list":    "llm.local.endpoints:\n  - " + evil + "\n",
		"endpoints-nested":  "llm:\n  local:\n    endpoints: [" + evil + "]\n",
		"xai":               "llm.xai.base_url: " + evil + "\n",
		"gemini":            "llm.gemini.base_url: " + evil + "\n",
		"default-provider":  "llm.provider.default: local\n",
		"list-under-scalar": "llm.local.base_url:\n  - " + evil + "\n",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			advB26Env(t, filepath.Join(t.TempDir(), "absent-user-config.yaml"))
			cfg, root := advB26RepoFile(t, body)
			defer setProjectRootForTest(root)()
			reg, err := newRegistry(cfg)
			if err != nil {
				return // refused: holds
			}
			for _, key := range []string{flags.LLMLocalBaseURL, flags.LLMXAIBaseURL, flags.LLMGeminiBaseURL, flags.LLMDefaultProvider} {
				v, origin, err := reg.String(key)
				if err != nil {
					continue
				}
				if origin != flags.OriginDefault {
					t.Errorf("repository layer set %s = %q with origin %q: that endpoint is Named", key, v, origin)
				}
			}
		})
	}
}

// TestAdvB26CredsUserConfigThatLeadsIntoTheRepositoryIsNotOperatorScope: the
// user config is the only file that may Name an endpoint. A user-config path
// that is a symlink into the repository, or sits under a symlinked directory
// into it, would hand the repository that power.
func TestAdvB26CredsUserConfigThatLeadsIntoTheRepositoryIsNotOperatorScope(t *testing.T) {
	const evil = "http://127.0.0.1:9/v1"
	for _, kind := range []string{"file-symlink", "dir-symlink", "relative", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			cfg, root := advB26RepoFile(t, "max_steps: 3\n")
			repoUser := filepath.Join(root, ".devcouncil", "user.yaml")
			if err := os.WriteFile(repoUser, []byte("llm.local.base_url: "+evil+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			outside, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var user string
			switch kind {
			case "file-symlink":
				user = filepath.Join(outside, "config.yaml")
				if err := os.Symlink(repoUser, user); err != nil {
					t.Fatal(err)
				}
			case "dir-symlink":
				if err := os.Symlink(filepath.Join(root, ".devcouncil"), filepath.Join(outside, "d")); err != nil {
					t.Fatal(err)
				}
				user = filepath.Join(outside, "d", "user.yaml")
			case "hardlink":
				user = filepath.Join(outside, "config.yaml")
				if err := os.Link(repoUser, user); err != nil {
					t.Skipf("no hard links here: %v", err)
				}
			case "relative":
				user = filepath.Join(".devcouncil", "user.yaml")
			}
			advB26Env(t, user)
			defer setProjectRootForTest(root)()
			reg, err := newRegistry(cfg)
			if err != nil {
				return // refused: holds
			}
			if v, origin, _ := reg.String(flags.LLMLocalBaseURL); origin != flags.OriginDefault {
				t.Errorf("a user config that is the repository's own file (%s) Named %q (origin %s)", kind, v, origin)
			}
		})
	}
}

// TestAdvB26CredsEmptyEnvBaseURLIsNotAnOperatorNaming: MANVI_LLM_LOCAL_BASE_URL
// set but empty or blank carries an origin of env. The address then falls back
// to the shipped default, which the operator never typed.
func TestAdvB26CredsEmptyEnvBaseURLIsNotNamed(t *testing.T) {
	for _, val := range []string{"", "   "} {
		t.Run("["+val+"]", func(t *testing.T) {
			advB26Env(t, filepath.Join(t.TempDir(), "absent.yaml"))
			t.Setenv(flags.EnvKey(flags.LLMLocalBaseURL), val)
			cfg, root := advB26RepoFile(t, "max_steps: 3\n")
			defer setProjectRootForTest(root)()
			reg, err := newRegistry(cfg)
			if err != nil {
				t.Skipf("refused at load: %v", err)
			}
			base, origin, _ := reg.String(flags.LLMLocalBaseURL)
			if origin != flags.OriginDefault && strings.TrimSpace(base) == "" {
				_, cred := resolveLocalEndpoint(context.Background(), reg, base, advB26Local(advB26SentinelResolver()), nil, "")
				if cred != nil {
					t.Errorf("an empty env value (origin %q) made the shipped default address Named: the credential resolver was handed through", origin)
				}
			}
		})
	}
}

func advB26Local(r *credentials.Resolver) func() (credentials.Secret, error) {
	return func() (credentials.Secret, error) { return r.Resolve("local") }
}

// advB26DefaultPort serves the shipped default address, which names nothing,
// and runs resolveLocalEndpoint and buildProvider against it. Skips when the
// port is busy.
func advB26DefaultPort(t *testing.T, status int) (rec *advB26Recorder, reg *flags.Registry) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:8000")
	if err != nil {
		t.Skipf("port 8000 busy: %v", err)
	}
	rec = newAdvB26Recorder(t, status, l)
	advB26Env(t, filepath.Join(t.TempDir(), "absent.yaml"))
	cfg, root := advB26RepoFile(t, "max_steps: 3\n")
	t.Cleanup(setProjectRootForTest(root))
	reg, err = newRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rec, reg
}

// TestAdvB26CredsUnnamedDefaultServerGetsNoKey: a server answering on the
// shipped default port, which nobody named, is adopted without a credential and
// the operator is told the command that confirms it.
func TestAdvB26CredsUnnamedDefaultServerGetsNoKey(t *testing.T) {
	rec, reg := advB26DefaultPort(t, http.StatusOK)
	var notes strings.Builder
	p, err := buildProvider(context.Background(), "local", reg, advB26SentinelResolver(), &notes, "")
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	_, _ = p.Capability("only-model")
	for _, a := range rec.saw() {
		if a != "" {
			t.Fatalf("an address nobody named was sent Authorization %q", a)
		}
	}
	if strings.Contains(notes.String(), "sk-advb26-sentinel") {
		t.Fatalf("the note quotes the key: %q", notes.String())
	}
	if !strings.Contains(notes.String(), "manvi use local --base-url http://127.0.0.1:8000/v1") {
		t.Errorf("the note does not name the confirming command: %q", notes.String())
	}
}

// TestAdvB26CredsUnnamedProtectedServerRefusalNamesTheCommandAndNoKey: a 401
// from an unnamed address yields a resolver that fails with the exact command
// to run, and never the key.
func TestAdvB26CredsUnnamedProtectedServerRefusalNamesTheCommandAndNoKey(t *testing.T) {
	rec, reg := advB26DefaultPort(t, http.StatusUnauthorized)
	declared, _, _ := reg.String(flags.LLMLocalBaseURL)
	var notes strings.Builder
	base, cred := resolveLocalEndpoint(context.Background(), reg, declared, advB26Local(advB26SentinelResolver()), &notes, "")
	for _, a := range rec.saw() {
		if a != "" {
			t.Fatalf("the probe sent Authorization %q to an address nobody named", a)
		}
	}
	if cred == nil {
		t.Fatalf("no resolver for %s: the first turn would fail with a bare 401", base)
	}
	_, err := cred()
	if err == nil {
		t.Fatal("the resolver released a credential for an address nobody named")
	}
	for _, want := range []string{"manvi use local --base-url " + base, "OPENAI_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "sk-advb26-sentinel") || strings.Contains(notes.String(), "sk-advb26-sentinel") {
		t.Errorf("a key value was printed: err=%q notes=%q", err.Error(), notes.String())
	}
}

// TestAdvB26CredsNamedEnvAddressStillGetsTheKey is the control through the real
// loader and buildProvider.
func TestAdvB26CredsNamedEnvAddressStillGetsTheKey(t *testing.T) {
	rec := newAdvB26Recorder(t, http.StatusOK, nil)
	advB26Env(t, filepath.Join(t.TempDir(), "absent.yaml"))
	t.Setenv(flags.EnvKey(flags.LLMLocalBaseURL), rec.URL+"/v1")
	cfg, root := advB26RepoFile(t, "max_steps: 3\n")
	defer setProjectRootForTest(root)()
	reg, err := newRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := buildProvider(context.Background(), "local", reg, advB26SentinelResolver(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.Capability("only-model")
	seen := rec.saw()
	if len(seen) == 0 || seen[0] != "Bearer sk-advb26-sentinel" {
		t.Fatalf("named address saw %q", seen)
	}
}

// TestAdvB26CredsShowLocalDisplaysNoLineSeparatorFromAListener: the display path
// (showLocal) inherits whatever the decoder let through. U+2028 and U+2029 are
// line breaks to many terminals and log viewers, and ui.Sanitize does not mark
// them, so a model id can forge a second row in the listing.
func TestAdvB26CredsShowLocalDisplaysNoLineSeparatorFromAListener(t *testing.T) {
	srv := fakeOllama(t, map[string][]string{
		"a\u2028FORGED qwen-fake:7b 256k ctx": {"completion", "tools"},
		"plain:7b":                            {"completion", "tools"},
	})
	var out bytes.Buffer
	if err := showLocal(context.Background(), &out, pinnedTo(t, srv.URL+"/v1"), nil); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\u2028\u2029") {
		t.Fatalf("showLocal wrote a Unicode line separator a listener supplied:\n%q", out.String())
	}
}
