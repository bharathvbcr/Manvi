package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/internal/treefile"
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
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00%s\x00", label, len(value), value)
	}
	write("name", cfg.Name)
	write("command", cfg.Command)
	_, _ = fmt.Fprintf(h, "args\x00%d\x00", len(cfg.Args))
	for _, a := range cfg.Args {
		write("arg", a)
	}
	keys := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	_, _ = fmt.Fprintf(h, "env\x00%d\x00", len(keys))
	for _, k := range keys {
		write("env."+k, cfg.Env[k])
	}
	pass := append([]string(nil), cfg.EnvPassthrough...)
	sort.Strings(pass)
	_, _ = fmt.Fprintf(h, "passthrough\x00%d\x00", len(pass))
	for _, p := range pass {
		write("passthrough", p)
	}
	// Written only when set, so a declaration that never mentioned a timeout
	// keeps the fingerprint an operator already authorized. A declaration that
	// raises its own call ceiling is a different declaration: a server a
	// repository may keep waiting on for an hour is not the one that was reviewed.
	if cfg.TimeoutSeconds != 0 {
		write("timeout_seconds", strconv.Itoa(cfg.TimeoutSeconds))
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

	if inside, err := PathIsInside(root, path); err != nil {
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

// PathIsInside reports whether path lies within root, comparing the two after
// resolving symlinks as far as each exists.
//
// Exported because it is the one answer to "could a checked-out tree have
// written this file": the user config that carries the tier profile asks it
// for the same reason the authorization file does.
func PathIsInside(root, path string) (bool, error) {
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
	if lexicallyInside(resolvedRoot, resolvedPath) {
		return true, nil
	}
	// The spelling says outside, which is only decisive on a volume whose names
	// are exact. Where case differences are not distinctions (APFS and NTFS by
	// default) .../REPO/f and .../repo name one place, so ask the file system:
	// the path is inside when it or an ancestor of it is the root directory.
	return ancestorIsRoot(resolvedRoot, resolvedPath)
}

// lexicallyInside is the spelling-only containment test.
func lexicallyInside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		// Different volumes cannot contain one another.
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ancestorIsRoot walks up from path and reports whether the nearest existing
// ancestor, or any above it, is the same file as root (os.SameFile: device and
// inode, volume serial and file index on Windows). It does not fold case, which
// would be wrong on a case-sensitive volume, and it also sees any other
// aliasing the spelling hides. A root that does not exist cannot contain
// anything. An ancestor that exists but cannot be examined is an error, not
// "outside": a refusal's test must not read a failed check as a clean one.
func ancestorIsRoot(root, path string) (bool, error) {
	rootInfo, err := os.Stat(root)
	if err != nil {
		if isAbsent(err) {
			return false, nil
		}
		return false, err
	}
	for current := path; ; {
		info, err := os.Stat(current)
		switch {
		case err == nil:
			if os.SameFile(rootInfo, info) {
				return true, nil
			}
		case !isAbsent(err):
			return false, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil
		}
		current = parent
	}
}

// isAbsent says a path is not there, as opposed to being there and unreadable.
func isAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// resolveExisting resolves symlinks over the longest existing prefix of path,
// so a file that does not exist yet is still compared at its real location.
//
// It walks the path one component at a time, as the kernel does, and never
// cleans it lexically first: a ".." after a symlink names the parent of the
// link's target, not of the link, so collapsing "out/link/../f" to "out/f"
// would compare a different file from the one an open would reach. A symlink
// is followed by its own target (Readlink), so a dangling one, whose target
// does not exist yet, still leads to where the target will be created. Past the
// first component that does not exist the rest is joined lexically; a ".." there
// goes to the parent of what has been resolved. Whether a component is absent is
// decided by isAbsent, the same test ancestorIsRoot uses, so one path cannot be
// "absent" to one half of PathIsInside and an error to the other. Following more
// than maxSymlinkDepth links is an error, as it is in the kernel.
func resolveExisting(path string) (string, error) {
	if !filepath.IsAbs(path) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		if vol := filepath.VolumeName(cwd); filepath.VolumeName(path) == "" && path != "" && os.IsPathSeparator(path[0]) {
			path = vol + path
		} else {
			path = cwd + string(filepath.Separator) + path
		}
	}
	resolved, queue := splitRooted(path)
	links, retries := 0, 0
	// absent counts the components appended past the last one that exists. A
	// ".." pops one, and when none is left the walk is back on a directory that
	// exists and follows links again: "out/absent/../into/f" reaches into/ by the
	// kernel's rules once absent exists, so it must be compared there.
	absent := 0
	for len(queue) > 0 {
		comp := queue[0]
		queue = queue[1:]
		switch comp {
		case ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			if absent > 0 {
				absent--
			}
			continue
		}
		next := filepath.Join(resolved, comp)
		if absent > 0 {
			resolved = next
			absent++
			continue
		}
		info, err := os.Lstat(next)
		switch {
		case err != nil && isAbsent(err):
			absent = 1
			resolved = next
		case err != nil:
			return "", err
		case info.Mode()&os.ModeSymlink != 0:
			if links++; links > maxSymlinkDepth {
				return "", fmt.Errorf("resolving %s: more than %d symbolic links", path, maxSymlinkDepth)
			}
			target, err := os.Readlink(next)
			if err != nil {
				// The component changed between Lstat and Readlink (a link
				// replaced by a directory or removed). Look at it again, a
				// bounded number of times; past that it is an error, never
				// an answer.
				if retries < maxResolveRetries && (errors.Is(err, syscall.EINVAL) || isAbsent(err)) {
					retries++
					links--
					queue = append([]string{comp}, queue...)
					continue
				}
				return "", err
			}
			if filepath.IsAbs(target) || os.IsPathSeparator(firstByte(target)) {
				var tail []string
				resolved, tail = splitRooted(rootedTarget(target, resolved))
				queue = append(tail, queue...)
			} else {
				queue = append(splitComponents(target), queue...)
			}
		case info.Mode()&os.ModeIrregular != 0:
			// A Windows junction or other reparse point is not reported as a
			// symlink but is resolved by the system; let it say where it goes.
			real, err := filepath.EvalSymlinks(next)
			if err != nil {
				return "", err
			}
			resolved = real
		default:
			resolved = next
		}
	}
	return resolved, nil
}

// maxSymlinkDepth bounds the links one resolution follows. The kernel's own
// limits are 32 to 40 depending on the system.
const maxSymlinkDepth = 40

// maxResolveRetries bounds how often one resolution re-examines a component that
// changed under it.
const maxResolveRetries = 3

func firstByte(s string) byte {
	if s == "" {
		return 0
	}
	return s[0]
}

// rootedTarget gives a symlink target that starts at a root its volume: on
// Windows a target like \dir has no volume of its own and takes the link's.
func rootedTarget(target, linkDir string) string {
	if filepath.VolumeName(target) == "" {
		return filepath.VolumeName(linkDir) + target
	}
	return target
}

// splitRooted splits an absolute path into its root (volume plus separator)
// and its components, uncleaned.
func splitRooted(path string) (root string, comps []string) {
	vol := filepath.VolumeName(path)
	return vol + string(filepath.Separator), splitComponents(path[len(vol):])
}

// splitComponents splits on path separators and drops the empty pieces that
// doubled or trailing separators leave. "." and ".." are kept: they are the
// caller's to interpret against what has been resolved.
func splitComponents(path string) []string {
	return strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == rune(os.PathSeparator) })
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

	where := truncateForMessage(cfg.Source)
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
	// Every field below came out of the tree as well, and this text is a
	// multi-line refusal an operator reads: a newline in a cwd or a variable
	// name would forge a line of it, so each is quoted the way the command is.
	if cfg.Cwd != "" {
		line += " (in " + strconv.Quote(truncateForMessage(cfg.Cwd)) + ")"
	}
	if len(cfg.Env) > 0 {
		keys := make([]string, 0, len(cfg.Env))
		for k := range cfg.Env {
			keys = append(keys, strconv.Quote(truncateForMessage(k)))
		}
		sort.Strings(keys)
		line += " with env " + strings.Join(keys, ",")
	}
	if len(cfg.EnvPassthrough) > 0 {
		pass := make([]string, 0, len(cfg.EnvPassthrough))
		for _, p := range cfg.EnvPassthrough {
			pass = append(pass, strconv.Quote(truncateForMessage(p)))
		}
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
	// closed is set by CloseAll and never cleared. A closed manager starts
	// nothing: a Client call that was already handshaking when CloseAll ran must
	// not store its process in a map nobody will close again.
	closed bool
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

// declarationRefused is a declaration this manager declined to register. It is
// recorded in Skipped before it is returned, so discovery can carry on past it
// and an operator can still see which declaration was set aside and why.
type declarationRefused struct{ reason string }

func (e *declarationRefused) Error() string { return "mcp: " + e.reason }

// maxEnvKeyLen and maxTimeoutSeconds bound what a declaration may name.
const (
	maxEnvKeyLen = 256
	// maxTimeoutSeconds is the ceiling on a declaration's own call timeout: an
	// hour. A repository that may set it to a year has set it to "never".
	maxTimeoutSeconds = 3600
)

// validName reports whether s is usable as a server name: the same set the
// command gate accepts for a server in a target (devcouncil.checkMCPName), so a
// name that registers is a name that can be gated.
func validName(s string) bool {
	if s == "" || len(s) > maxToolNameLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == ':':
		default:
			return false
		}
	}
	return true
}

// validEnvKey reports whether s is an environment variable name: a letter or
// underscore, then letters, digits and underscores.
func validEnvKey(s string) bool {
	if s == "" || len(s) > maxEnvKeyLen {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// validateDeclaration is the one place a declaration is checked, once, at
// registration. Everything after this point — the fingerprint, the refusal an
// operator reads, the environment handed to the child — may assume a name and
// an environment that are plain identifiers and a cwd that is a path, not a
// paragraph. The strings are the tree's to choose, and several of them used to
// be printed or exec'd as they came.
func (m *Manager) validateDeclaration(cfg ServerConfig) error {
	if !validName(cfg.Name) {
		return fmt.Errorf("server name %q is not usable: names are limited to letters, digits and _-.:, "+
			"at most %d bytes", truncateForMessage(cfg.Name), maxToolNameLen)
	}
	for k := range cfg.Env {
		if !validEnvKey(k) {
			return fmt.Errorf("server %q declares the environment variable name %q, which is not an identifier",
				cfg.Name, truncateForMessage(k))
		}
	}
	for _, k := range cfg.EnvPassthrough {
		if !validEnvKey(k) {
			return fmt.Errorf("server %q forwards the environment variable name %q, which is not an identifier",
				cfg.Name, truncateForMessage(k))
		}
	}
	if cfg.TimeoutSeconds < 0 || cfg.TimeoutSeconds > maxTimeoutSeconds {
		return fmt.Errorf("server %q declares timeout_seconds %d; it must be between 0 and %d",
			cfg.Name, cfg.TimeoutSeconds, maxTimeoutSeconds)
	}
	if hasControlChars(cfg.Cwd) {
		return fmt.Errorf("server %q declares a cwd containing control characters: %q",
			cfg.Name, truncateForMessage(cfg.Cwd))
	}
	if cfg.Origin == OriginWorkspace && cfg.Cwd != "" && m.root != "" {
		inside, err := PathIsInside(m.root, m.workspaceCwd(cfg.Cwd))
		switch {
		case err != nil:
			return fmt.Errorf("server %q declares the cwd %q and whether it lies inside %s could not be "+
				"established: %v", cfg.Name, truncateForMessage(cfg.Cwd), m.root, err)
		case !inside:
			return fmt.Errorf("server %q declares the cwd %q, which is outside the repository %s; "+
				"a declaration read from the tree may only run inside it",
				cfg.Name, truncateForMessage(cfg.Cwd), m.root)
		}
	}
	return nil
}

// workspaceCwd is the directory a workspace declaration's cwd names: a relative
// one is relative to the repository, not to wherever this process was started.
func (m *Manager) workspaceCwd(cwd string) string {
	if filepath.IsAbs(cwd) || m.root == "" {
		return cwd
	}
	return filepath.Join(m.root, cwd)
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
	where := source
	if where == "" {
		where = "the program"
	}
	if err := m.validateDeclaration(cfg); err != nil {
		return m.refuse(cfg, err.Error())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if prior, declared := m.configs[cfg.Name]; declared {
		if reflect.DeepEqual(prior, cfg) {
			// The same declaration made again, from the same place. Nothing
			// changed, so nothing is refused: discovery run twice is not a
			// second declaration.
			return nil
		}
		priorWhere := prior.Source
		if priorWhere == "" {
			priorWhere = "the program"
		}
		reason := fmt.Sprintf("server %q is already declared by %s; the second declaration, by %s, was refused "+
			"so that a name keeps one meaning", cfg.Name, truncateForMessage(priorWhere), truncateForMessage(where))
		m.skipped = append(m.skipped, SkippedManifest{Path: skippedPath(cfg), Reason: reason})
		return &declarationRefused{reason: reason}
	}
	m.configs[cfg.Name] = cfg
	return nil
}

// refuse records a declaration that failed validation and returns the refusal.
func (m *Manager) refuse(cfg ServerConfig, reason string) error {
	m.mu.Lock()
	m.skipped = append(m.skipped, SkippedManifest{Path: skippedPath(cfg), Reason: reason})
	m.mu.Unlock()
	return &declarationRefused{reason: reason}
}

// skippedPath is what a refused declaration is filed under: the file it came
// from, or its name when the program declared it.
func skippedPath(cfg ServerConfig) string {
	if cfg.Source != "" {
		return cfg.Source
	}
	return truncateForMessage(cfg.Name)
}

// isRefusal reports whether err is a declaration refused on its merits, which
// discovery records and goes on past, as opposed to a fault in the discovery
// itself, which stops it.
func isRefusal(err error) bool {
	var refused *declarationRefused
	return errors.As(err, &refused)
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
		m.mu.Lock()
		m.skipped = append(m.skipped, SkippedManifest{Path: where, Reason: err.Error()})
		m.mu.Unlock()
		return nil
	}
	// Through register, not a bare m.configs write: register is what records
	// origin and source, and a plugin config filed without them is a server
	// whose provenance the trust rules can no longer ask about.
	if err := m.register(cfg, origin, source); err != nil {
		return err
	}
	// Remembered only once its declaration was accepted, and only then is its
	// static tool list ever served (see surveyTools). A manifest whose server
	// name was already taken, or that declares no way to run, advertises nothing.
	m.mu.Lock()
	m.plugins[p.Name] = p
	m.mu.Unlock()
	return nil
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
	// The file comes out of the checked-out tree, so what it is and how big it
	// is are chosen by whoever wrote the repository. It is read through
	// treefile: no link is followed, only a regular file is read, and the size
	// is bounded by what was read rather than by a stat that a device file
	// answers with zero.
	data, err := m.readDeclaration(path, maxManifestBytes)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return false, nil
		case errors.Is(err, treefile.ErrTooLarge):
			return true, fmt.Errorf("mcp: %s is past the %d-byte cap: %w", path, maxManifestBytes, err)
		}
		return true, fmt.Errorf("mcp: reading config %s: %w", path, err)
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
			if isRefusal(err) {
				// Recorded in Skipped by register; one bad declaration does not
				// stop the others in the file from being offered.
				continue
			}
			return true, err
		}
	}
	return true, nil
}

// readDeclaration reads one declaration file, below the manager's root when the
// path is relative: the directories down to it are opened one verified step at
// a time, so a repository cannot point a directory it controls at somewhere
// else. An absolute path is one an operator typed, and its directory is the
// anchor.
func (m *Manager) readDeclaration(path string, limit int64) ([]byte, error) {
	if filepath.IsAbs(path) || m.root == "" {
		return readTreeFile(m.resolve(path), limit)
	}
	rel := filepath.Clean(path)
	dir, err := treefile.OpenDir(m.root, filepath.Dir(rel), 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	return treefile.ReadFile(dir, filepath.Base(rel), treefile.ReadOptions{Limit: limit})
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
		if err := m.RegisterPlugin(p); err != nil && !isRefusal(err) {
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
	closed := m.closed
	cachedClient, hasCached := m.clients[name]
	cfg, registered := m.configs[name]
	m.mu.RUnlock()

	if closed {
		return nil, errManagerClosed
	}
	if !registered {
		return nil, fmt.Errorf("mcp: server %q is not registered", name)
	}

	// The one place a process is reached, and so the one place authorization
	// is decided. Every route to a server — ListAllTools, CallTool,
	// ReadResource, the tool surface's per-server listing — arrives here, so a
	// declaration that no operator authorized cannot be executed by any of
	// them. See Origin.
	//
	// It is decided on the cached client's path as well as the cold one. The
	// authorization list is read fresh on every check precisely so that a
	// revocation takes effect when it is written, and that promise meant nothing
	// for a server that was already running: it kept answering, with the
	// operator's authorization gone, until the process died on its own. A
	// refusal also closes the process it refused, because a server nobody may
	// call is a server nobody should leave running.
	if err := m.authorize(cfg); err != nil {
		if hasCached {
			m.dropClient(name, cachedClient)
		}
		return nil, err
	}
	if hasCached && cachedClient.Alive() {
		return cachedClient, nil
	}

	switch {
	case cfg.Cwd == "" && m.root != "":
		cfg.Cwd = m.root
	case cfg.Origin == OriginWorkspace:
		// Validated against the repository at registration as a path relative to
		// it, so it is started there too.
		cfg.Cwd = m.workspaceCwd(cfg.Cwd)
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
	if m.closed {
		m.mu.Unlock()
		// CloseAll ran while this connection was handshaking. Storing it would
		// put a live process in a map nobody will close again.
		_ = client.Close()
		return nil, errManagerClosed
	}
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

// errManagerClosed is what every entry point that would reach a server returns
// once CloseAll has run.
var errManagerClosed = errors.New("mcp: the manager is closed; no server will be started")

// dropClient closes a client the manager no longer permits and forgets it, if it
// is still the one on file. Close waits on a child process, so it runs outside
// the lock.
func (m *Manager) dropClient(name string, c *Client) {
	m.mu.Lock()
	if m.clients[name] == c {
		delete(m.clients, name)
	}
	m.mu.Unlock()
	_ = c.Close()
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

// SecretValues returns every value this manager would hand to a server
// process through its environment: each declared Env value and the current
// value of each EnvPassthrough variable that is set.
//
// It exists so the harness can arm its credential scrubber with them. A server
// that echoes its own token on stderr has that text attached to the error of
// every failed call, and the scrubber knows only the provider keys unless told
// otherwise. Values are read at call time, not at registration, because a
// passthrough variable is the process's own environment and may change.
//
// The result is sensitive and is for the scrubber only; short values are
// returned too, and the scrubber's own minimum length decides which it keeps.
func (m *Manager) SecretValues() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var values []string
	for _, cfg := range m.configs {
		for _, v := range cfg.Env {
			if v != "" {
				values = append(values, v)
			}
		}
		for _, k := range cfg.EnvPassthrough {
			if v, ok := os.LookupEnv(k); ok && v != "" {
				values = append(values, v)
			}
		}
	}
	return values
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
	// Static is set when Tools is the list a plugin manifest declared, read
	// from the manifest and not from the server: no server was asked, so the
	// list is what the declaration claims and not what the server serves.
	Static bool `json:"static,omitempty"`
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
	return m.ListServerTools(ctx, m.ServerNames())
}

// ListServerTools surveys the tools of the named servers, in the order given.
// It is ListAllTools for a caller that has already decided which servers it may
// reach: asking a server is what starts it, so a caller with a gate to consult
// consults it first and surveys only the servers that passed.
func (m *Manager) ListServerTools(ctx context.Context, names []string) ([]ServerTools, error) {
	if !m.Enabled() {
		return nil, m.unavailable()
	}
	if len(names) == 0 {
		return nil, nil
	}
	// Each server is surveyed independently, so they are surveyed at once: a
	// cold server costs a spawn and a handshake, and one after another that
	// summed across every configured server on a single tool call. Results
	// keep the registration order.
	results := make([]ServerTools, len(names))
	sem := make(chan struct{}, maxConcurrentSurveys)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			results[i] = m.surveyTools(ctx, name)
		}()
	}
	wg.Wait()
	return results, nil
}

// maxConcurrentSurveys bounds how many servers ListAllTools starts at once.
const maxConcurrentSurveys = 8

// staticTools is the tool list a plugin manifest declared for name, when a
// survey may answer from it instead of asking the server.
func (m *Manager) staticTools(name string) ([]Tool, bool) {
	m.mu.RLock()
	plugin := m.plugins[name]
	cfg, registered := m.configs[name]
	m.mu.RUnlock()
	if plugin != nil && registered && len(plugin.Tools) > 0 && m.authorize(cfg) == nil {
		return plugin.Tools, true
	}
	return nil, false
}

// ServesStatically reports whether a survey of name is answered from its
// manifest, so that no server is started to answer it.
func (m *Manager) ServesStatically(name string) bool {
	_, ok := m.staticTools(name)
	return ok
}

// CanStart reports whether a call to Client for name would start, or reach, a
// server: the name is registered and its declaration passes authorize. A caller
// with a gate to consult asks this to know which names the gate is about; a name
// that cannot start is refused by Client itself, loudly and without a process.
func (m *Manager) CanStart(name string) bool {
	if !m.Enabled() {
		return false
	}
	m.mu.RLock()
	cfg, registered := m.configs[name]
	m.mu.RUnlock()
	return registered && m.authorize(cfg) == nil
}

// surveyTools is one server's entry in ListAllTools.
func (m *Manager) surveyTools(ctx context.Context, name string) ServerTools {
	// A manifest's static tool list is served in place of asking the server only
	// for a declaration that is registered under this name and that passes
	// authorize, and it is labelled as static. It used to be served for any name
	// a manifest carried, so a manifest that lost its name to another
	// declaration, or one nobody had authorized, still advertised tools that the
	// server of that name would never offer.
	if tools, ok := m.staticTools(name); ok {
		return ServerTools{Server: name, Tools: tools, Static: true}
	}

	// A failure here does not fail the whole survey — one bad server must
	// not hide the others — but it is reported rather than skipped past.
	client, err := m.Client(ctx, name)
	if err != nil {
		return ServerTools{Server: name, Error: err.Error()}
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		return ServerTools{Server: name, Error: err.Error()}
	}
	return ServerTools{Server: name, Tools: tools}
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
//
// The manager is closed for good: it starts no further server, and a connection
// that was still handshaking is closed by the Client call that made it.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	m.closed = true
	clients := m.clients
	m.clients = make(map[string]*Client)
	m.mu.Unlock()

	// Outside the lock: Close waits on a child process, and every other manager
	// operation queues behind this mutex.
	for _, client := range clients {
		_ = client.Close()
	}
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
