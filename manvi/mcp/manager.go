package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ConfigFile represents the standard MCP multi-server configuration file.
type ConfigFile struct {
	MCPServers map[string]ServerConfig `json:"mcpServers,omitempty"`
	Servers    map[string]ServerConfig `json:"servers,omitempty"`
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
func (m *Manager) RegisterServer(cfg ServerConfig) error {
	if !m.Enabled() {
		return m.unavailable()
	}
	if cfg.Name == "" {
		return errors.New("mcp: server name is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configs[cfg.Name] = cfg
	return nil
}

// RegisterPlugin registers an Open Plugin 1.0 standard manifest.
func (m *Manager) RegisterPlugin(p *PluginManifest) error {
	if !m.Enabled() {
		return m.unavailable()
	}
	if p == nil || p.Name == "" {
		return errors.New("openplugin: manifest is nil or has no name")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plugins[p.Name] = p
	if cfg, err := p.ToServerConfig(); err == nil {
		m.configs[p.Name] = cfg
	}
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
	data, err := os.ReadFile(m.resolve(path))
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
		if err := m.RegisterServer(cfg); err != nil {
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
	m.skipped = append(m.skipped, skipped...)
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
func (m *Manager) Client(ctx context.Context, name string) (*Client, error) {
	if !m.Enabled() {
		return nil, m.unavailable()
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if client, ok := m.clients[name]; ok && !client.closed.Load() {
		return client, nil
	}

	cfg, ok := m.configs[name]
	if !ok {
		return nil, fmt.Errorf("mcp: server %q is not registered", name)
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

	m.clients[name] = client
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

		client, err := m.Client(ctx, name)
		if err != nil {
			// Record empty/degraded rather than failing the whole survey
			continue
		}

		tools, err := client.ListTools(ctx)
		if err != nil {
			continue
		}

		results = append(results, ServerTools{
			Server: name,
			Tools:  tools,
		})
	}

	return results, nil
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
