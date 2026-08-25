package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ConfigFile represents the standard MCP multi-server configuration file.
type ConfigFile struct {
	MCPServers map[string]ServerConfig `json:"mcpServers,omitempty"`
	Servers    map[string]ServerConfig `json:"servers,omitempty"`
}

// Origin says where a server declaration came from, which is the whole of what
// decides whether spawning it needs an operator's authorization.
//
// Discovery reads declarations out of the working tree: mcp.json and .mcp.json
// at the repository root, and any plugin.json, openplugin.json or mcp.json
// anywhere under plugins/, .mcp/plugins or .devcouncil/plugins. Every one of
// those arrives with a `git clone`. The command they name went straight into
// exec.Command with no allowlist, no signature and nothing asked of anyone — so
// checking out a repository and letting the model call mcp_list_tools, a
// ReadOnly and ungated tool offered by default, ran whatever that repository
// said to run, with this harness's credentials in the environment.
//
// Declarations made through the Go API are a different thing entirely: they
// come from the program embedding this package, which is the operator's own
// build, and nothing a checked-out file says can produce one.
type Origin string

const (
	// OriginProgram is a declaration made in-process through RegisterServer or
	// through RegisterPlugin with a manifest that was not read off disk.
	OriginProgram Origin = "program"
	// OriginWorkspace is a declaration read out of the checked-out tree.
	// Spawning one requires an authorization the tree cannot grant itself.
	OriginWorkspace Origin = "workspace"
)

// TrustFileEnv names an environment variable holding the path to the
// authorization file, for installs that keep configuration somewhere else and
// for tests.
const TrustFileEnv = "MANVI_MCP_TRUST_FILE"

// TrustListEnv names an environment variable holding authorized fingerprints
// directly, whitespace- or comma-separated. It exists for headless and CI runs
// with no home directory to keep a file in.
const TrustListEnv = "MANVI_MCP_TRUST"

// trustFileName is where authorizations live by default, under the user's
// config directory — outside any repository, which is the point. An
// authorization a repository could write for itself would not be one.
const trustFileName = "mcp-trust.json"

// TrustFile is the on-disk authorization list.
type TrustFile struct {
	Authorized []TrustEntry `json:"authorized"`
}

// TrustEntry authorizes one server declaration by fingerprint. Name and Note
// are for the human reading the file; only Fingerprint is matched.
type TrustEntry struct {
	Fingerprint string `json:"fingerprint"`
	Name        string `json:"name,omitempty"`
	Note        string `json:"note,omitempty"`
}

// Fingerprint identifies a server declaration for authorization.
//
// It covers what decides which program runs and what that program is handed:
// the server name, the command, its arguments, the declared environment, and
// the variables the declaration asks to have forwarded from this process. Any
// change to any of those produces a different fingerprint and needs authorizing
// again — a repository cannot get a declaration approved and then quietly add
// ANTHROPIC_API_KEY to its passthrough list.
//
// Cwd is deliberately not covered. It is an absolute path that differs between
// clones and worktrees of the same repository, so including it would expire
// every authorization on every re-checkout while adding no capability the
// command and arguments do not already carry.
//
// What this does NOT establish, and no fingerprint of a declaration can: that
// the program named by Command still contains what it contained when the
// operator read it. `node server.js` fingerprints the same however server.js
// changes. Authorizing a declaration means having reviewed what it will run,
// not pinning that program's contents.
func Fingerprint(cfg ServerConfig) string {
	h := sha256.New()
	write := func(label, value string) {
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00", label, len(value), value)
	}
	write("name", cfg.Name)
	write("command", cfg.Command)
	fmt.Fprintf(h, "args\x00%d\x00", len(cfg.Args))
	for _, a := range cfg.Args {
		write("arg", a)
	}
	keys := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(h, "env\x00%d\x00", len(keys))
	for _, k := range keys {
		write("env."+k, cfg.Env[k])
	}
	pass := append([]string(nil), cfg.EnvPassthrough...)
	sort.Strings(pass)
	fmt.Fprintf(h, "passthrough\x00%d\x00", len(pass))
	for _, p := range pass {
		write("passthrough", p)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// trustStore is the set of fingerprints an operator has authorized, plus where
// that set came from so a refusal can name the file to edit.
type trustStore struct {
	path         string
	fingerprints map[string]struct{}
	// err is why the authorization list could not be read. It is carried
	// rather than swallowed: an authorization check that could not run must
	// not produce the same answer as one that ran and found nothing.
	err error
}

// loadTrustStore reads the authorization list fresh.
//
// Fresh on every check, not cached, so an operator who authorizes a server can
// retry without restarting the harness — and so a revocation takes effect the
// moment it is written rather than at the next boot.
//
// root is the repository this manager serves. A trust file resolving to
// somewhere inside it is refused outright: the whole mechanism rests on the
// authorization living somewhere a `git clone` cannot write, and a path that
// lands back inside the tree would let a repository authorize itself.
func loadTrustStore(root string) trustStore {
	store := trustStore{fingerprints: map[string]struct{}{}}

	for _, fp := range strings.FieldsFunc(os.Getenv(TrustListEnv), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		store.fingerprints[strings.ToLower(strings.TrimSpace(fp))] = struct{}{}
	}

	path, err := trustFilePath()
	if err != nil {
		store.err = err
		return store
	}
	store.path = path

	if inside, err := pathIsInside(root, path); err != nil {
		store.err = fmt.Errorf("could not tell whether the authorization file %s lies inside %s: %w",
			path, root, err)
		return store
	} else if inside {
		store.err = fmt.Errorf("the authorization file %s lies inside the repository %s; "+
			"an authorization a checked-out tree can write is not an authorization, so it was not read",
			path, root)
		return store
	}

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if !os.IsNotExist(readErr) {
			store.err = fmt.Errorf("reading the authorization file %s: %w", path, readErr)
		}
		return store
	}
	var tf TrustFile
	if err := json.Unmarshal(data, &tf); err != nil {
		store.err = fmt.Errorf("parsing the authorization file %s: %w", path, err)
		return store
	}
	for _, entry := range tf.Authorized {
		if fp := strings.ToLower(strings.TrimSpace(entry.Fingerprint)); fp != "" {
			store.fingerprints[fp] = struct{}{}
		}
	}
	return store
}

func trustFilePath() (string, error) {
	if p := strings.TrimSpace(os.Getenv(TrustFileEnv)); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%s must be an absolute path, got %q", TrustFileEnv, p)
		}
		return filepath.Clean(p), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no user configuration directory to read MCP authorizations from "+
			"(set %s or %s): %w", TrustFileEnv, TrustListEnv, err)
	}
	return filepath.Join(dir, "manvi", trustFileName), nil
}

// pathIsInside reports whether path lies within root, comparing the two after
// resolving symlinks as far as each exists.
func pathIsInside(root, path string) (bool, error) {
	if strings.TrimSpace(root) == "" {
		return false, nil
	}
	resolvedRoot, err := resolveExisting(root)
	if err != nil {
		return false, err
	}
	resolvedPath, err := resolveExisting(path)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil {
		// Different volumes cannot contain one another.
		return false, nil
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

// resolveExisting resolves symlinks over the longest existing prefix of path,
// so a file that does not exist yet is still compared at its real location.
func resolveExisting(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	remainder := ""
	current := filepath.Clean(abs)
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			return filepath.Join(resolved, remainder), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Join(current, remainder), nil
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}

// authorize decides whether a declaration may be spawned, and refuses in a way
// an operator can act on.
//
// The refusal prints the whole declaration, because "authorize this
// fingerprint" is only a safe instruction if the thing being authorized is in
// front of the person doing it.
func (m *Manager) authorize(cfg ServerConfig) error {
	if cfg.Origin == OriginProgram {
		return nil
	}

	store := loadTrustStore(m.root)
	fp := Fingerprint(cfg)
	if store.err != nil {
		return fmt.Errorf("mcp: server %q was declared by workspace content and its authorization "+
			"could not be checked, so it was not started: %w", cfg.Name, store.err)
	}
	if _, ok := store.fingerprints[strings.ToLower(fp)]; ok {
		return nil
	}

	where := cfg.Source
	if where == "" {
		where = "the working tree"
	}
	// store.path is always set past the error check above: loadTrustStore only
	// leaves it empty when it could not resolve a path at all, and that is
	// carried as store.err. Naming a fallback here would mean naming a location
	// inside the repository, which is the one place an authorization may not
	// live.
	target := store.path
	return fmt.Errorf("mcp: server %q is declared by workspace content (%s) and no operator has "+
		"authorized it, so it was not started.\n"+
		"  it would run: %s\n"+
		"  fingerprint:  %s\n"+
		"authorize it by adding that fingerprint to %s, as "+
		`{"authorized":[{"fingerprint":%q,"name":%q}]}`+"\n"+
		"or by listing it in %s. Read the command above before you do: authorizing it lets that "+
		"repository run it with your account's privileges.",
		cfg.Name, where, describeInvocation(cfg), fp, target, fp, cfg.Name, TrustListEnv)
}

// describeInvocation renders what a declaration would actually execute.
//
// Bounded, because every string in it came out of the tree and this text is
// handed to a model and to a terminal. A refusal that pastes a megabyte of
// attacker-chosen bytes into the transcript is its own problem.
func describeInvocation(cfg ServerConfig) string {
	parts := append([]string{cfg.Command}, cfg.Args...)
	for i, p := range parts {
		parts[i] = strconv.Quote(truncateForMessage(p))
	}
	line := truncateTo(strings.Join(parts, " "), 4*unroutablePreview)
	if cfg.Cwd != "" {
		line += " (in " + cfg.Cwd + ")"
	}
	if len(cfg.Env) > 0 {
		keys := make([]string, 0, len(cfg.Env))
		for k := range cfg.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		line += " with env " + strings.Join(keys, ",")
	}
	if len(cfg.EnvPassthrough) > 0 {
		pass := append([]string(nil), cfg.EnvPassthrough...)
		sort.Strings(pass)
		line += " forwarding this process's " + strings.Join(pass, ",")
	}
	return line
}

// Manager orchestrates multiple MCP 2.0 stateless servers and Open Plugins.
type Manager struct {
	mu      sync.RWMutex
	configs map[string]ServerConfig
	clients map[string]*Client
	plugins map[string]*PluginManifest
	root    string
	// off is why MCP is not available, or empty when it is. A switched-off
	// manager is a manager, not a nil one — see NewDisabledManager.
	off string
	// skipped records manifests that looked like plugins and could not be
	// read. Discovery does not fail on them — see DiscoverPlugins — but a
	// plugin that failed to load must not be indistinguishable from one that
	// was never installed, which is what dropping them silently made it.
	skipped []SkippedManifest
}

// NewManager creates a new MCP & Open Plugin manager.
func NewManager(root string) *Manager {
	return &Manager{
		configs: make(map[string]ServerConfig),
		clients: make(map[string]*Client),
		plugins: make(map[string]*PluginManifest),
		root:    root,
	}
}

// NewDisabledManager returns a manager with MCP switched off, carrying the
// reason it was switched off so every refusal can name it.
//
// Off is a manager rather than a nil one because nil already means something
// else here, and it means the opposite: a consumer handed no manager builds its
// own and auto-discovers, which is how MCP stayed on no matter what an operator
// set. Handing that consumer a manager that refuses is the only way to say "no"
// to it, and a refusal that names the setting is the only way an operator finds
// out which one they changed.
//
// Every entry point that would reach a server refuses. ServerNames and the
// listings do not answer "none", because a subsystem that was never asked must
// never report what a subsystem that was asked and found nothing reports.
func NewDisabledManager(root, reason string) *Manager {
	m := NewManager(root)
	if reason == "" {
		reason = "MCP is disabled"
	}
	m.off = reason
	return m
}

// Enabled reports whether this manager will talk to servers at all.
func (m *Manager) Enabled() bool { return m.off == "" }

// unavailable is the one refusal every disabled entry point returns.
func (m *Manager) unavailable() error {
	return fmt.Errorf("mcp: no servers are available: %s", m.off)
}

// RegisterServer adds a server configuration for on-demand connection.
//
// This is the Go API, so the declaration comes from the program embedding this
// package rather than from a file somebody checked out — see Origin. The origin
// is stamped here rather than taken from cfg so that no caller, and in
// particular no JSON decoder feeding one, can claim it.
func (m *Manager) RegisterServer(cfg ServerConfig) error {
	return m.register(cfg, OriginProgram, "")
}

func (m *Manager) register(cfg ServerConfig, origin Origin, source string) error {
	if !m.Enabled() {
		return m.unavailable()
	}
	if cfg.Name == "" {
		return errors.New("mcp: server name is required")
	}
	cfg.Origin = origin
	cfg.Source = source
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configs[cfg.Name] = cfg
	return nil
}

// RegisterPlugin registers an Open Plugin 1.0 standard manifest.
//
// A manifest carrying a ManifestPath was read off disk, and only LoadPluginFile
// sets that field — so "this manifest came out of the tree" is derived from
// evidence rather than from a flag a caller might forget to pass or a manifest
// might try to set. Everything else is a manifest the embedding program built
// in memory.
func (m *Manager) RegisterPlugin(p *PluginManifest) error {
	if !m.Enabled() {
		return m.unavailable()
	}
	if p == nil || p.Name == "" {
		return errors.New("openplugin: manifest is nil or has no name")
	}
	origin, source := OriginProgram, ""
	if p.ManifestPath != "" {
		origin, source = OriginWorkspace, p.ManifestPath
	}
	m.mu.Lock()
	m.plugins[p.Name] = p
	cfg, err := p.ToServerConfig()
	if err != nil {
		// The manifest parsed, so DiscoverPlugins had no reason to skip it, but
		// it declares no way to run. Without a config it is absent from
		// ServerNames, from ListAllTools — which surveys only what ServerNames
		// returns — and unreachable through CallTool, which routes via Client.
		// Discarding this error put it in m.plugins and nowhere else: it
		// disappeared on every channel at once, which is exactly the symptom
		// the Skipped machinery was built to eliminate one layer down.
		//
		// It is recorded rather than returned because one unrunnable manifest
		// must not fail discovery of the rest, and its static tools are still
		// not advertised: CallTool cannot reach them, and a tool an agent is
		// offered but can never call is worse than one it is never offered.
		where := p.ManifestPath
		if where == "" {
			where = p.Name
		}
		m.skipped = append(m.skipped, SkippedManifest{Path: where, Reason: err.Error()})
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	// Through register, not a bare m.configs write: register is what records
	// origin and source, and a plugin config filed without them is a server
	// whose provenance the trust rules can no longer ask about.
	return m.register(cfg, origin, source)
}

// LoadConfigFile parses an MCP server configuration file (e.g. .devcouncil/mcp.json).
//
// A file that is not there is not an error: this is the tolerant entry point,
// used where a caller is naming a location MCP servers are conventionally kept
// rather than one someone asked for. Discover is where a path an operator typed
// is held to a stricter contract.
func (m *Manager) LoadConfigFile(path string) error {
	_, err := m.loadConfigFile(path)
	return err
}

// loadConfigFile reads one declaration file, saying whether it was there.
//
// The found flag is what separates "the operator's file is missing" from "the
// conventional location is empty" at the one call site that has to tell them
// apart. Everything else about the two cases is identical, which is exactly why
// they were the same case for so long.
func (m *Manager) loadConfigFile(path string) (found bool, err error) {
	if !m.Enabled() {
		return false, m.unavailable()
	}
	resolved := m.resolve(path)
	// The file comes out of the checked-out tree, so its size is chosen by
	// whoever wrote the repository. Bounded before it is read, not after.
	if info, statErr := os.Stat(resolved); statErr == nil && info.Size() > maxManifestBytes {
		return true, fmt.Errorf("mcp: %s is %d bytes, past the %d cap",
			path, info.Size(), maxManifestBytes)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("mcp: reading config %s: %w", path, err)
	}

	var cf ConfigFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return true, fmt.Errorf("mcp: parsing config %s: %w", path, err)
	}

	servers := cf.MCPServers
	if len(servers) == 0 {
		servers = cf.Servers
	}

	for name, cfg := range servers {
		cfg.Name = name
		// Read out of the tree, so stamped as such. Whatever the file said
		// about Origin is not consulted — the field is json:"-" precisely so
		// that a declaration cannot promote itself.
		if err := m.register(cfg, OriginWorkspace, m.resolve(path)); err != nil {
			return true, err
		}
	}
	return true, nil
}

// resolve turns a declaration path into one this process can open.
//
// Relative paths are resolved against the manager's root rather than against
// the working directory. They used to be read as the process found them, which
// meant a manager rooted at a repository read whichever repository the shell
// happened to be standing in — the same string naming two different files
// depending on who built the manager.
func (m *Manager) resolve(path string) string {
	if path == "" || filepath.IsAbs(path) || m.root == "" {
		return path
	}
	return filepath.Join(m.root, path)
}

// ConfigSource is where a manager is told to look for server declarations.
type ConfigSource struct {
	// Path is the declaration file to read, absolute or relative to the
	// manager's root.
	Path string
	// Declared is true when Path is a value the operator typed rather than a
	// default this harness shipped.
	//
	// It decides what a missing file means, and the two answers are genuinely
	// different. A shipped default naming a file most repositories do not have
	// is this harness guessing, and a guess that misses is not a fault. A path
	// someone wrote down is a statement that servers are declared there, and a
	// statement that turns out to be false must be reported rather than
	// resolved into "no servers" — which is what a correct file with nothing in
	// it also produces. It is the same distinction llm.local.base_url draws
	// between an address that was scanned for and one that was configured.
	Declared bool
}

// Discover loads the servers and plugins this manager will offer.
//
// A declared path is read and nothing else is: an operator who named a file
// meant that file, and quietly merging two more into it would mean a server
// they never declared could still be spawned. An undeclared path keeps the
// conventional scan, with the declared default first, so a repository that has
// simply always had a ./mcp.json goes on working.
//
// Either way a file that exists and does not parse fails the whole call. That
// was the swallow: every load was `_ = m.LoadConfigFile(p)`, so a mistyped
// comma produced no servers, no error, and no way for an operator to tell their
// declaration had been dropped from a repository that declares nothing.
func (m *Manager) Discover(ctx context.Context, src ConfigSource) error {
	if !m.Enabled() {
		return m.unavailable()
	}

	path := strings.TrimSpace(src.Path)
	switch {
	case path != "" && src.Declared:
		found, err := m.loadConfigFile(path)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("mcp: the configured server declaration file %s does not exist "+
				"(resolved to %s)", path, m.resolve(path))
		}
	default:
		// The conventional locations, the configured default first so that
		// changing it changes what is scanned. Absence is expected here and is
		// not reported; a parse failure still is.
		for _, p := range append([]string{path}, "mcp.json", ".mcp.json") {
			if strings.TrimSpace(p) == "" {
				continue
			}
			if _, err := m.loadConfigFile(p); err != nil {
				return err
			}
		}
	}

	// Open Plugins. The walk's own failure is returned rather than dropped, for
	// the reason above: a directory that could not be read is not a directory
	// with no plugins in it.
	plugins, skipped, err := DiscoverPlugins(m.resolve(".devcouncil/plugins"),
		m.resolve(".mcp/plugins"), m.resolve("plugins"))
	if err != nil {
		return err
	}
	// A manifest that could not be read is recorded, not dropped. Discovery
	// carries on — see DiscoverPlugins for why these are reported rather than
	// fatal — but a plugin that failed to load must not look like a plugin that
	// was never installed.
	//
	// Under the lock like every other field of Manager: this was the one
	// unguarded write, and -race caught it against the read in Skipped.
	m.mu.Lock()
	m.skipped = append(m.skipped, skipped...)
	m.mu.Unlock()
	for _, p := range plugins {
		if err := m.RegisterPlugin(p); err != nil {
			return err
		}
	}

	return nil
}

// AutoDiscover scans the conventional workspace locations, with no configured
// path to honour. It is Discover's undeclared case, kept as its own name for
// the callers that have no settings registry to read.
func (m *Manager) AutoDiscover(ctx context.Context) error {
	return m.Discover(ctx, ConfigSource{Path: ".devcouncil/mcp.json"})
}

// Client returns an active client for a named server, connecting lazily if needed.
//
// A cached client that has died — its stdout ended without anyone calling
// Close, the signature of a crash — is replaced rather than returned. Handing
// out a corpse converted one bad server frame into permanent tool failure for
// the rest of the session.
//
// The expensive part of a cold connection — spawning the process and waiting
// out the initialize handshake — happens outside every lock. Holding the
// write lock through it serialized every other manager operation behind one
// slow server's ten-second timeout.
func (m *Manager) Client(ctx context.Context, name string) (*Client, error) {
	if !m.Enabled() {
		return nil, m.unavailable()
	}

	m.mu.RLock()
	cachedClient, hasCached := m.clients[name]
	cfg, registered := m.configs[name]
	m.mu.RUnlock()

	if hasCached && cachedClient.Alive() {
		return cachedClient, nil
	}
	if !registered {
		return nil, fmt.Errorf("mcp: server %q is not registered", name)
	}

	// The one place a process is spawned, and so the one place authorization
	// is decided. Every route to a server — ListAllTools, CallTool,
	// ReadResource, the tool surface's per-server listing — arrives here, so a
	// declaration that no operator authorized cannot be executed by any of
	// them. See Origin.
	if err := m.authorize(cfg); err != nil {
		return nil, err
	}

	if cfg.Cwd == "" && m.root != "" {
		cfg.Cwd = m.root
	}

	client, err := NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("mcp: connecting to %s: %w", name, err)
	}

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := client.Initialize(initCtx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("mcp: initializing %s: %w", name, err)
	}

	m.mu.Lock()
	previous, hadPrevious := m.clients[name]
	if hadPrevious && previous.Alive() {
		m.mu.Unlock()
		// A concurrent caller completed its own connection while this one was
		// handshaking; theirs wins, ours closes. Outside the lock: Close waits
		// on a child process, and every other manager operation queues behind
		// this mutex.
		_ = client.Close()
		return previous, nil
	}
	m.clients[name] = client
	m.mu.Unlock()

	// The client just displaced is dead — its stdout ended — and it has to be
	// closed here, because this is the last reference to it. Dead stdout is not
	// a dead process: the child may still be running with its stdin open, never
	// Wait()ed and so never reaped, holding three pipes and a stderr goroutine.
	// Overwriting the map entry dropped it out of m.clients too, so CloseAll
	// could no longer reach it either, and a server that closes stdout under
	// load leaked one live process per respawn for the rest of the run.
	if hadPrevious {
		_ = previous.Close()
	}
	return client, nil
}

// ServerNames returns all registered server names in sorted order.
func (m *Manager) ServerNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.configs))
	for name := range m.configs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ServerTools represents tools provided by a specific MCP server.
type ServerTools struct {
	Server string `json:"server"`
	Tools  []Tool `json:"tools"`
	// Error is why this server contributed no tools, when it contributed none
	// for a reason. It exists because the survey used to `continue` past every
	// failure, so a server that refused to start, one whose listing was
	// rejected as hostile, and one an operator has not authorized all appeared
	// in the result as simply absent — and an absent server is exactly what a
	// server with no tools looks like.
	Error string `json:"error,omitempty"`
}

// ListAllTools surveys tools across all registered servers.
//
// A disabled manager refuses instead of returning an empty survey. The empty
// survey is what a running MCP with nothing configured returns, and a caller
// that cannot tell those apart will report "no MCP tools" for a subsystem that
// was never asked.
func (m *Manager) ListAllTools(ctx context.Context) ([]ServerTools, error) {
	if !m.Enabled() {
		return nil, m.unavailable()
	}
	names := m.ServerNames()
	var results []ServerTools

	for _, name := range names {
		// First check if static tools were declared in Open Plugin manifest
		m.mu.RLock()
		plugin := m.plugins[name]
		m.mu.RUnlock()

		if plugin != nil && len(plugin.Tools) > 0 {
			results = append(results, ServerTools{
				Server: name,
				Tools:  plugin.Tools,
			})
			continue
		}

		// A failure here does not fail the whole survey — one bad server must
		// not hide the others — but it is reported rather than skipped past.
		client, err := m.Client(ctx, name)
		if err != nil {
			results = append(results, ServerTools{Server: name, Error: err.Error()})
			continue
		}

		tools, err := client.ListTools(ctx)
		if err != nil {
			results = append(results, ServerTools{Server: name, Error: err.Error()})
			continue
		}

		results = append(results, ServerTools{
			Server: name,
			Tools:  tools,
		})
	}

	return results, nil
}

// Unavailable returns why this manager will not talk to servers, or nil when it
// will. It is the exported form of the refusal every disabled entry point
// returns, for callers that need to check before doing other work.
func (m *Manager) Unavailable() error {
	if m.Enabled() {
		return nil
	}
	return m.unavailable()
}

// Diagnostics returns what a connected server has said about itself. Empty when
// the server is not connected — this reports a live client's record, it does
// not start one.
func (m *Manager) Diagnostics(name string) []string {
	m.mu.RLock()
	client := m.clients[name]
	m.mu.RUnlock()
	if client == nil {
		return nil
	}
	return client.Diagnostics()
}

// CallTool routes a tool execution to the appropriate MCP server.
func (m *Manager) CallTool(ctx context.Context, serverName, toolName string, arguments map[string]any) (*CallToolResult, error) {
	client, err := m.Client(ctx, serverName)
	if err != nil {
		return nil, err
	}
	return client.CallTool(ctx, toolName, arguments)
}

// ReadResource fetches resource contents from a target server.
func (m *Manager) ReadResource(ctx context.Context, serverName, uri string) ([]ResourceContent, error) {
	client, err := m.Client(ctx, serverName)
	if err != nil {
		return nil, err
	}
	return client.ReadResource(ctx, uri)
}

// CloseAll closes all active MCP client processes.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, client := range m.clients {
		_ = client.Close()
	}
	m.clients = make(map[string]*Client)
}

// Skipped lists manifests discovery found and could not read.
//
// It is a query rather than an error return because these do not stop a run:
// discovery went looking for files by name under directories it guessed at, and
// one that does not parse is a plugin that will not appear rather than a reason
// to refuse to start. A caller that wants to tell the operator has this; one
// that does not is at least no longer deciding by accident.
func (m *Manager) Skipped() []SkippedManifest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]SkippedManifest(nil), m.skipped...)
}
