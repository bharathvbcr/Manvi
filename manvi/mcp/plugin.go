package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PluginRuntime specifies how an Open Plugin 1.0 is executed.
type PluginRuntime struct {
	Type     string            `json:"type"` // "stdio", "mcp", "http", "command"
	Command  string            `json:"command,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Endpoint string            `json:"endpoint,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
}

// PluginAuth specifies authentication requirements for an Open Plugin.
type PluginAuth struct {
	Type   string `json:"type"` // "none", "bearer", "api_key", "oauth2"
	Header string `json:"header,omitempty"`
	EnvKey string `json:"env_key,omitempty"`
}

// PluginManifest represents an Open Plugin 1.0 standard manifest.
type PluginManifest struct {
	SchemaURI     string         `json:"$schema,omitempty"`        // "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	SchemaVersion string         `json:"schema_version,omitempty"` // "1.0"
	Name          string         `json:"name"`
	Version       string         `json:"version"`
	Description   string         `json:"description"`
	Author        string         `json:"author,omitempty"`
	Homepage      string         `json:"homepage,omitempty"`
	Runtime       PluginRuntime  `json:"runtime"`
	Capabilities  []string       `json:"capabilities,omitempty"`
	Tools         []Tool         `json:"tools,omitempty"`
	Auth          PluginAuth     `json:"auth,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	// ManifestPath is where this plugin was loaded from.
	ManifestPath string `json:"-"`
}

// ParsePluginManifest decodes an Open Plugin 1.0 JSON manifest.
func ParsePluginManifest(data []byte) (*PluginManifest, error) {
	var m PluginManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("openplugin: malformed manifest: %w", err)
	}

	if m.Name == "" {
		return nil, fmt.Errorf("openplugin: manifest must declare a 'name'")
	}
	if m.SchemaVersion == "" {
		m.SchemaVersion = OpenPluginStandardVersion
	}
	if m.Runtime.Type == "" {
		if m.Runtime.Command != "" {
			m.Runtime.Type = "stdio"
		} else if m.Runtime.Endpoint != "" {
			m.Runtime.Type = "http"
		}
	}

	return &m, nil
}

// LoadPluginFile loads an Open Plugin manifest from disk.
func LoadPluginFile(path string) (*PluginManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("openplugin: reading %s: %w", path, err)
	}
	manifest, err := ParsePluginManifest(data)
	if err != nil {
		return nil, fmt.Errorf("openplugin in %s: %w", path, err)
	}
	manifest.ManifestPath = path
	return manifest, nil
}

// SkippedManifest is a file that looked like a plugin manifest and could not be
// read as one.
type SkippedManifest struct {
	Path   string
	Reason string
}

func (s SkippedManifest) String() string { return s.Path + ": " + s.Reason }

// DiscoverPlugins searches target directories for Open Plugin manifests
// (plugin.json, openplugin.json, mcp.json), returning what it loaded and what
// it could not.
//
// The second return is the point. A file that failed to parse used to be
// dropped by `if err == nil`, so a plugin with a trailing comma in its manifest
// was indistinguishable from a plugin that was not there — and the operator's
// only symptom was a tool that never appeared.
//
// It is reported rather than fatal, and the asymmetry with a *declared* config
// path is deliberate. A path an operator typed into mcp.config names a file
// they mean; failing to parse it must stop the run. These are files this
// harness went looking for by name, under directories it guessed at, and
// `mcp.json` is a common enough name that an unrelated one will turn up in
// somebody's tree. Making that fatal would break runs that never asked for
// plugins at all. So discovery keeps going and hands back what it could not
// use, and the caller decides how loudly to say so.
func DiscoverPlugins(dirs ...string) ([]*PluginManifest, []SkippedManifest, error) {
	var plugins []*PluginManifest
	var skipped []SkippedManifest

	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}

		err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				name := d.Name()
				if name == ".git" || name == "node_modules" || name == "target" {
					return filepath.SkipDir
				}
				return nil
			}

			name := strings.ToLower(d.Name())
			if name == "plugin.json" || name == "openplugin.json" || name == "mcp.json" {
				manifest, err := LoadPluginFile(p)
				switch {
				case err != nil:
					skipped = append(skipped, SkippedManifest{Path: p, Reason: err.Error()})
				case manifest == nil:
					// Parsed to nothing. Not an error from the decoder, and
					// still not a plugin, so it is reported for the same
					// reason a parse failure is.
					skipped = append(skipped, SkippedManifest{
						Path: p, Reason: "the file parsed to an empty manifest"})
				default:
					plugins = append(plugins, manifest)
				}
			}
			return nil
		})
		if err != nil {
			return nil, skipped, fmt.Errorf("openplugin: searching %s: %w", dir, err)
		}
	}

	return plugins, skipped, nil
}

// ToServerConfig converts an Open Plugin manifest to an MCP ServerConfig.
func (m *PluginManifest) ToServerConfig() (ServerConfig, error) {
	if m.Runtime.Command == "" {
		return ServerConfig{}, fmt.Errorf("openplugin %s has no executable runtime command", m.Name)
	}

	cwd := m.Runtime.Cwd
	if cwd == "" && m.ManifestPath != "" {
		cwd = filepath.Dir(m.ManifestPath)
	}

	return ServerConfig{
		Name:    m.Name,
		Command: m.Runtime.Command,
		Args:    m.Runtime.Args,
		Env:     m.Runtime.Env,
		Cwd:     cwd,
	}, nil
}
