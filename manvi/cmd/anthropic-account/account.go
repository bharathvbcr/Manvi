package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/childenv"
	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
)

const (
	// registryName is the account list in the home directory. One line per
	// Console organisation, "<name> [workspace]"; the first line is the default.
	registryName = ".anthropic-accounts"
	// markerName pins a repository (or any directory below it) to one account.
	markerName = ".anthropic-account"
	// keychainPrefix plus the account name is the Keychain service that holds
	// that organisation's key.
	keychainPrefix = "anthropic-api-key-"

	maxRegistryBytes = 64 << 10
	maxMarkerBytes   = 4 << 10
	maxAccounts      = 64
	// maxKeyBytes bounds what the Keychain hands back. Real keys are around a
	// hundred characters; the bound exists so a pasted blob is refused rather
	// than exported into a child's environment.
	maxKeyBytes = 512
)

var accountName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type account struct {
	Name      string `json:"name"`
	Workspace string `json:"workspace,omitempty"`
}

// errNoRegistry is the refusal for a machine that has not listed its accounts.
// It is not a fallback to the single unnamed key the shell function used to
// read: running on a key nobody chose is how a turn ends up billed to, or
// refused by, the wrong organisation.
type errNoRegistry struct{ path string }

func (e *errNoRegistry) Error() string {
	return fmt.Sprintf(`no account registry at %s.
For each Console organisation, store its key under a name (security prompts for it):
  security add-generic-password -a "$USER" -s %s<name> -w
then list the name; the first line is the default:
  echo '<name>' >> %s
Add a workspace id after the name only for a key that is not created inside a workspace.
The old unnamed Keychain item "anthropic-api-key" is not read here. Keep it until every app
that read it directly launches through anthropic-account.`, e.path, keychainPrefix, e.path)
}

func loadRegistry(home string) ([]account, string, error) {
	path := filepath.Join(home, registryName)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, path, &errNoRegistry{path: path}
	}
	if err != nil {
		return nil, path, err
	}
	// Read-only: a close failure cannot change bytes already read.
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxRegistryBytes+1))
	if err != nil {
		return nil, path, err
	}
	if len(data) > maxRegistryBytes {
		return nil, path, fmt.Errorf("%s is larger than %d bytes", path, maxRegistryBytes)
	}
	accounts, err := parseRegistry(data)
	if err != nil {
		return nil, path, fmt.Errorf("%s: %w", path, err)
	}
	if len(accounts) == 0 {
		return nil, path, fmt.Errorf("%s lists no accounts", path)
	}
	return accounts, path, nil
}

func parseRegistry(data []byte) ([]account, error) {
	var accounts []account
	seen := map[string]bool{}
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 2 {
			return nil, fmt.Errorf("line %d: want \"<name> [workspace]\", got %d fields", i+1, len(fields))
		}
		if !accountName.MatchString(fields[0]) {
			return nil, fmt.Errorf("line %d: account name %q is not letters, digits, '.', '_' or '-'", i+1, fields[0])
		}
		if seen[fields[0]] {
			return nil, fmt.Errorf("line %d: account %q is listed twice", i+1, fields[0])
		}
		seen[fields[0]] = true
		a := account{Name: fields[0]}
		if len(fields) == 2 {
			ws, err := anthropic.ParseWorkspaceID(fields[1])
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
			a.Workspace = ws
		}
		accounts = append(accounts, a)
		if len(accounts) > maxAccounts {
			return nil, fmt.Errorf("more than %d accounts", maxAccounts)
		}
	}
	return accounts, nil
}

// selection is the account a launch uses and why.
type selection struct {
	Account  account
	Source   string
	Fallback *account
}

// choose picks the account: an explicit name, then ANTHROPIC_ACCOUNT, then the
// nearest marker at or above dir (stopping below home), then the registry's
// first line. Whatever names it must be in the registry; an unknown or empty
// name is refused rather than passed over, because skipping a selector the
// operator set would silently bill the default.
func choose(accounts []account, flagName, envName, dir, home string, withFallback bool) (selection, error) {
	name, source := flagName, "--account"
	if name == "" && envName != "" {
		name, source = envName, "ANTHROPIC_ACCOUNT"
	}
	if name == "" {
		marker, found, err := findMarker(dir, home)
		if err != nil {
			return selection{}, err
		}
		if found != "" {
			name, source = found, marker
		}
	}
	if name == "" {
		name, source = accounts[0].Name, "default"
	}
	if !accountName.MatchString(name) {
		return selection{}, fmt.Errorf("account name %q (from %s) is not letters, digits, '.', '_' or '-'", name, source)
	}
	sel := selection{Source: source}
	for i := range accounts {
		if accounts[i].Name == name {
			sel.Account = accounts[i]
			break
		}
	}
	if sel.Account.Name == "" {
		return selection{}, fmt.Errorf("account %q (from %s) is not in %s", name, source, registryName)
	}
	if withFallback {
		for i := range accounts {
			if accounts[i].Name != name {
				fb := accounts[i]
				sel.Fallback = &fb
				break
			}
		}
	}
	return sel, nil
}

// findMarker walks from dir up to, but not including, home. A marker in home
// itself would pin every directory on the machine, which is the registry's
// default's job.
func findMarker(dir, home string) (path, name string, err error) {
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	home = filepath.Clean(home)
	for dir != home {
		candidate := filepath.Join(dir, markerName)
		data, err := readSmall(candidate, maxMarkerBytes)
		if err == nil {
			fields := strings.Fields(string(data))
			if len(fields) == 0 {
				return "", "", fmt.Errorf("%s is empty", candidate)
			}
			return candidate, fields[0], nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", "", nil
}

func readSmall(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	// Read-only: a close failure cannot change bytes already read.
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return data, nil
}

// keychain reads an account's key. The real one is /usr/bin/security; tests
// substitute a map.
type keychain interface {
	// read returns the key, or errItemMissing.
	read(service string) (string, error)
	// has reports whether the item exists without reading the secret.
	has(service string) (bool, error)
}

var errItemMissing = errors.New("no such Keychain item")

// securityCLI asks /usr/bin/security rather than linking Security.framework.
// An item added with `security add-generic-password` trusts that binary, so
// the read raises no access prompt and needs no cgo.
type securityCLI struct {
	user string
	// timeout bounds a read. It is long because a locked Keychain shows the
	// user a password prompt, and that wait is theirs.
	timeout time.Duration
}

func (s securityCLI) run(verb string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/security", append([]string{verb}, args...)...)
	// security reads the Keychain, not a provider key: it gets none of the ones
	// this process may hold.
	cmd.Env = childenv.Inherit()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("reading the Keychain timed out after %s", s.timeout)
	}
	var exit *exec.ExitError
	// 44 is errSecItemNotFound as security(1) reports it.
	if errors.As(err, &exit) && exit.ExitCode() == 44 {
		return nil, errItemMissing
	}
	if err != nil {
		return nil, fmt.Errorf("security %s: %w: %s", verb, err,strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (s securityCLI) read(service string) (string, error) {
	out, err := s.run("find-generic-password", "-a", s.user, "-s", service, "-w")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (s securityCLI) has(service string) (bool, error) {
	_, err := s.run("find-generic-password", "-a", s.user, "-s", service)
	if errors.Is(err, errItemMissing) {
		return false, nil
	}
	return err == nil, err
}

func readKey(kc keychain, a account) (string, error) {
	service := keychainPrefix + a.Name
	raw, err := kc.read(service)
	if errors.Is(err, errItemMissing) {
		return "", fmt.Errorf("the Keychain has no item %q for account %s; add it with: security add-generic-password -a \"$USER\" -s %s -w", service, a.Name, service)
	}
	if err != nil {
		return "", fmt.Errorf("account %s: %w", a.Name, err)
	}
	key := strings.TrimRight(raw, "\r\n")
	if key == "" {
		return "", fmt.Errorf("the Keychain item %q is empty", service)
	}
	if len(key) > maxKeyBytes || strings.ContainsAny(key, " \t\r\n") {
		return "", fmt.Errorf("the Keychain item %q does not hold a single API key", service)
	}
	return key, nil
}

// envNames are the variables this launcher owns, read from the credential
// catalogue rather than listed here, so a name Manvi adds to the Anthropic
// requirements is cleared from what the child inherits without an edit here.
type envNames struct {
	key   string
	strip map[string]bool
}

func anthropicEnvNames() (envNames, error) {
	names := envNames{strip: map[string]bool{}}
	for _, req := range credentials.DefaultRequirements() {
		if req.Provider != "anthropic" && req.Provider != credentials.AnthropicFallback {
			continue
		}
		for _, list := range [][]string{req.EnvVars, req.Related} {
			for _, name := range list {
				names.strip[name] = true
			}
		}
		if req.Provider == "anthropic" && len(req.EnvVars) > 0 {
			names.key = req.EnvVars[0]
		}
	}
	if names.key == "" {
		return envNames{}, errors.New("the credential catalogue has no anthropic key variable")
	}
	return names, nil
}

// childEnv is base with every inherited Anthropic credential removed and the
// chosen ones set. Nothing from the parent survives under these names: an
// inherited key would otherwise pair with the chosen workspace, or outlive a
// --no-fallback, and bill an organisation nobody picked.
func childEnv(base []string, names envNames, primary account, key string, fallback *account, fallbackKey string) []string {
	env := make([]string, 0, len(base)+4)
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if !names.strip[name] {
			env = append(env, kv)
		}
	}
	env = append(env, names.key+"="+key)
	if primary.Workspace != "" {
		env = append(env, credentials.AnthropicWorkspaceEnv+"="+primary.Workspace)
	}
	if fallback != nil && fallbackKey != "" {
		env = append(env, credentials.AnthropicFallbackKeyEnv+"="+fallbackKey)
		if fallback.Workspace != "" {
			env = append(env, credentials.AnthropicFallbackWorkspaceEnv+"="+fallback.Workspace)
		}
	}
	return env
}
