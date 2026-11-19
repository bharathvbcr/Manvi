package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/internal/hygiene"
	"github.com/bharathvbcr/Manvi/manvi/internal/treefile"
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

// Bounds on discovery and on what a manifest may declare.
//
// A manifest is a file out of a checked-out tree, so its size, its depth in the
// tree and the listing it declares are all chosen by whoever wrote the
// repository. maxManifestTools in particular closes a route around the client's
// own caps: a manifest's static Tools are handed to callers by ListAllTools
// without a server being contacted at all, so nothing in client.go would ever
// have seen them.
const (
	maxManifestBytes = 1 << 20
	maxManifestTools = 1024
	maxManifestDepth = 8
	// maxManifestsPerDir bounds how many manifests one directory tree may
	// contribute, so a repository cannot make discovery itself the denial of
	// service.
	maxManifestsPerDir = 256
	// maxDiscoveryBytes bounds what one discovery reads across every manifest it
	// opens. The per-file and per-directory caps multiply: 256 manifests of
	// 1 MiB under each of three directories is three quarters of a gigabyte read
	// and parsed before anyone is asked, and every byte of it is chosen by whoever
	// wrote the repository.
	maxDiscoveryBytes = 32 << 20
)

// ParsePluginManifest decodes an Open Plugin 1.0 JSON manifest.
func ParsePluginManifest(data []byte) (*PluginManifest, error) {
	if len(data) > maxManifestBytes {
		return nil, fmt.Errorf("openplugin: manifest is %d bytes, past the %d cap",
			len(data), maxManifestBytes)
	}
	var m PluginManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("openplugin: malformed manifest: %w", err)
	}

	if m.Name == "" {
		return nil, fmt.Errorf("openplugin: manifest must declare a 'name'")
	}
	if hasControlChars(m.Name) || len(m.Name) > maxToolNameLen {
		return nil, fmt.Errorf("openplugin: manifest name is unusable as an identifier "+
			"(control characters, or longer than %d bytes)", maxToolNameLen)
	}
	if len(m.Tools) > maxManifestTools {
		return nil, fmt.Errorf("openplugin: manifest %q declares %d tools, past the %d cap; "+
			"it was refused rather than truncated", m.Name, len(m.Tools), maxManifestTools)
	}
	for i, tool := range m.Tools {
		switch {
		case tool.Name == "":
			return nil, fmt.Errorf("openplugin: manifest %q declares a tool at index %d with no name",
				m.Name, i)
		case len(tool.Name) > maxToolNameLen || hasControlChars(tool.Name):
			return nil, fmt.Errorf("openplugin: manifest %q declares an unusable tool name at index %d "+
				"(control characters, or longer than %d bytes)", m.Name, i, maxToolNameLen)
		case len(tool.Description) > maxToolDescLen:
			return nil, fmt.Errorf("openplugin: manifest %q declares a %d-byte description for tool %q, "+
				"past the %d cap", m.Name, len(tool.Description), tool.Name, maxToolDescLen)
		case len(tool.InputSchema) > maxToolSchemaBytes:
			return nil, fmt.Errorf("openplugin: manifest %q declares a %d-byte input schema for tool %q, "+
				"past the %d cap", m.Name, len(tool.InputSchema), tool.Name, maxToolSchemaBytes)
		}
	}
	// Third-party text, shown to a model and to a terminal. See hygiene.
	m.Description = hygiene.Escape(m.Description)
	for i := range m.Tools {
		m.Tools[i].Description = hygiene.Escape(m.Tools[i].Description)
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
//
// Setting ManifestPath is what marks the result as workspace content: it is the
// only thing that sets that field, and Manager.RegisterPlugin reads it to
// decide whether spawning the declaration needs an operator's authorization.
func LoadPluginFile(path string) (*PluginManifest, error) {
	manifest, _, err := loadPluginFile(path, maxManifestBytes)
	return manifest, err
}

// loadPluginFile reads and parses one manifest through readTreeFile, within
// limit bytes, and says how many bytes it read so that discovery can keep a
// running total. A read that failed, or was refused, read none.
func loadPluginFile(path string, limit int64) (*PluginManifest, int64, error) {
	data, err := readTreeFile(path, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("openplugin: reading %s: %w", path, err)
	}
	manifest, err := ParsePluginManifest(data)
	if err != nil {
		return nil, int64(len(data)), fmt.Errorf("openplugin in %s: %w", path, err)
	}
	manifest.ManifestPath = path
	return manifest, int64(len(data)), nil
}

// readTreeFile reads one declaration file whose name a repository chose: no
// symbolic link is followed, only a regular file is read, and no more than
// limit bytes are taken from it. The file's directory is the trust anchor, so
// the file is the last thing checked; a caller that must also refuse a link in
// the directories above it descends from its root (see Manager.readDeclaration).
//
// os.ReadFile was here, behind an os.Stat size check. A link to /dev/zero
// reports no size and never ends, so the size check let it through and the read
// filled memory.
func readTreeFile(path string, limit int64) ([]byte, error) {
	dir, err := treefile.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	return treefile.ReadFile(dir, filepath.Base(path), treefile.ReadOptions{Limit: limit})
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
	// read is the running total of manifest bytes opened across every directory.
	var read int64

	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}

		found := 0
		err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				// A directory or entry that could not be read is not a directory
				// with nothing in it. The walk carries on past it, and says so.
				skipped = append(skipped, SkippedManifest{Path: p,
					Reason: "could not be read, so nothing under it was searched: " + err.Error()})
				return nil
			}
			if d.IsDir() {
				name := d.Name()
				if name == ".git" || name == "node_modules" || name == "target" {
					return filepath.SkipDir
				}
				// The walk descends the whole subtree, so how deep it goes is
				// chosen by whoever wrote the repository. Bounding it costs a
				// manifest nobody would bury that deep on purpose, and the
				// subtree it prunes is reported rather than forgotten.
				if depthUnder(dir, p) > maxManifestDepth {
					skipped = append(skipped, SkippedManifest{Path: p,
						Reason: fmt.Sprintf("deeper than %d levels under %s; nothing under it was searched",
							maxManifestDepth, dir)})
					return filepath.SkipDir
				}
				return nil
			}

			name := strings.ToLower(d.Name())
			if name == "plugin.json" || name == "openplugin.json" || name == "mcp.json" {
				if found >= maxManifestsPerDir {
					skipped = append(skipped, SkippedManifest{Path: p,
						Reason: fmt.Sprintf("more than %d manifests under %s; the rest were not read",
							maxManifestsPerDir, dir)})
					return filepath.SkipAll
				}
				found++
				remaining := int64(maxDiscoveryBytes) - read
				if remaining <= 0 {
					skipped = append(skipped, SkippedManifest{Path: p, Reason: budgetSpent()})
					return filepath.SkipAll
				}
				manifest, n, err := loadPluginFile(p, min(int64(maxManifestBytes), remaining))
				read += n
				switch {
				case err != nil && remaining < maxManifestBytes && errors.Is(err, treefile.ErrTooLarge):
					skipped = append(skipped, SkippedManifest{Path: p, Reason: budgetSpent()})
					return filepath.SkipAll
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

// budgetSpent is the reason given for the manifests discovery did not read once
// it had read maxDiscoveryBytes of them.
func budgetSpent() string {
	return fmt.Sprintf("the discovery byte budget (%d bytes across all manifests) was spent; "+
		"this and every later manifest were not read", maxDiscoveryBytes)
}

// depthUnder counts path separators between a root and a path beneath it.
func depthUnder(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return strings.Count(rel, string(filepath.Separator)) + 1
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
