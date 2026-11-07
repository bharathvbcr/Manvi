package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"runtime/debug"
	"sort"
)

// CycloneDX 1.5 is the format: a JSON document with a dependency graph, a
// purl for every component and a hash for the artifact, readable by the
// common scanners. The generator reads only what the toolchains already
// record (the build info Go embeds in every binary, the same data
// `go version -m` prints, and `cargo metadata`), so it adds no dependency.

// Component is one CycloneDX component.
type Component struct {
	Type       string     `json:"type"`
	BOMRef     string     `json:"bom-ref"`
	Name       string     `json:"name"`
	Version    string     `json:"version,omitempty"`
	PURL       string     `json:"purl,omitempty"`
	Hashes     []Hash     `json:"hashes,omitempty"`
	Properties []Property `json:"properties,omitempty"`
}

// Hash is a CycloneDX hash.
type Hash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

// Property is a CycloneDX name/value property.
type Property struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Dependency is one edge list of the CycloneDX dependency graph.
type Dependency struct {
	Ref       string   `json:"ref"`
	DependsOn []string `json:"dependsOn"`
}

// Metadata is the CycloneDX metadata block.
type Metadata struct {
	Component Component `json:"component"`
}

// BOM is the CycloneDX document.
type BOM struct {
	BOMFormat    string       `json:"bomFormat"`
	SpecVersion  string       `json:"specVersion"`
	SerialNumber string       `json:"serialNumber"`
	Version      int          `json:"version"`
	Metadata     Metadata     `json:"metadata"`
	Components   []Component  `json:"components"`
	Dependencies []Dependency `json:"dependencies"`
}

// CargoMetadata is the subset of `cargo metadata --format-version 1` used.
type CargoMetadata struct {
	Packages []CargoPackage `json:"packages"`
	Resolve  *CargoResolve  `json:"resolve"`
}

// CargoPackage is one crate in the metadata.
type CargoPackage struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Version string  `json:"version"`
	Source  *string `json:"source"`
	License *string `json:"license"`
}

// CargoResolve is the resolved dependency graph for the requested platform.
type CargoResolve struct {
	Nodes []CargoNode `json:"nodes"`
	Root  *string     `json:"root"`
}

// CargoNode is one resolved crate and the crates it depends on.
type CargoNode struct {
	ID   string   `json:"id"`
	Deps []string `json:"dependencies"`
}

// Input is everything one SBOM is built from.
type Input struct {
	Tag          string
	BinaryName   string
	BinarySHA256 string
	Build        *debug.BuildInfo
	// Cargo is nil for a binary that links no Rust engine.
	Cargo *CargoMetadata
	// RustTarget names the triple Cargo was resolved for.
	RustTarget string
}

func setting(info *debug.BuildInfo, key string) string {
	for _, s := range info.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

func purlGo(path, version string) string {
	return "pkg:golang/" + url.PathEscape(path) + "@" + url.PathEscape(version)
}

func purlCargo(name, version string) string {
	return "pkg:cargo/" + url.PathEscape(name) + "@" + url.PathEscape(version)
}

// Build assembles the BOM. It fails closed on a mismatch between what the
// binary says it linked and what was supplied: a cgo binary without the Rust
// crate list, or Rust crates attached to a binary that has no engine, would
// otherwise produce an SBOM that quietly misdescribes the artifact.
func Build(in Input) (*BOM, error) {
	if in.Build == nil {
		return nil, errors.New("no Go build info for the binary")
	}
	if in.Tag == "" || in.BinaryName == "" {
		return nil, errors.New("tag and binary name are required")
	}
	if len(in.BinarySHA256) != sha256.Size*2 {
		return nil, fmt.Errorf("binary SHA-256 %q is not 64 hex characters", in.BinarySHA256)
	}
	cgo := setting(in.Build, "CGO_ENABLED") == "1"
	switch {
	case cgo && in.Cargo == nil:
		return nil, errors.New("binary is built with CGO_ENABLED=1 but no cargo metadata was supplied; the Rust crates would be missing")
	case !cgo && in.Cargo != nil:
		return nil, errors.New("cargo metadata was supplied for a binary built without cgo; it links no Rust crates")
	}

	rootRef := "manvi@" + in.Tag
	root := Component{
		Type:    "application",
		BOMRef:  rootRef,
		Name:    "manvi",
		Version: in.Tag,
		Hashes:  []Hash{{Alg: "SHA-256", Content: in.BinarySHA256}},
		Properties: []Property{
			{Name: "manvi:artifact", Value: in.BinaryName},
			{Name: "go:version", Value: in.Build.GoVersion},
			{Name: "go:CGO_ENABLED", Value: setting(in.Build, "CGO_ENABLED")},
			{Name: "go:GOOS", Value: setting(in.Build, "GOOS")},
			{Name: "go:GOARCH", Value: setting(in.Build, "GOARCH")},
			{Name: "manvi:gusset-engine", Value: map[bool]string{true: "linked", false: "not linked (CGO_ENABLED=0)"}[cgo]},
		},
	}
	if tags := setting(in.Build, "-tags"); tags != "" {
		root.Properties = append(root.Properties, Property{Name: "go:tags", Value: tags})
	}

	var components []Component
	rootDeps := []string{}
	seen := map[string]bool{rootRef: true}
	add := func(c Component) error {
		if seen[c.BOMRef] {
			return fmt.Errorf("duplicate component %s", c.BOMRef)
		}
		seen[c.BOMRef] = true
		components = append(components, c)
		rootDeps = append(rootDeps, c.BOMRef)
		return nil
	}

	goMods := append([]*debug.Module{}, in.Build.Deps...)
	if in.Build.Main.Path != "" {
		m := in.Build.Main
		goMods = append(goMods, &m)
	}
	for _, m := range goMods {
		if m == nil {
			continue
		}
		eff := m
		if m.Replace != nil {
			eff = m.Replace
		}
		c := Component{
			Type:    "library",
			BOMRef:  "go:" + m.Path + "@" + m.Version,
			Name:    m.Path,
			Version: m.Version,
		}
		if m.Path == in.Build.Main.Path {
			c.BOMRef = "go:" + m.Path + "@main"
			c.Version = ""
			c.Properties = append(c.Properties, Property{Name: "go:main-module", Value: "true"})
		}
		if m.Replace != nil {
			c.Properties = append(c.Properties, Property{Name: "go:replace", Value: eff.Path + "@" + eff.Version})
		}
		// A module replaced by a local path or an unversioned main module has
		// no version to put in a purl; leaving the purl out is more honest
		// than inventing one.
		if c.Version != "" && c.Version != "(devel)" && m.Replace == nil {
			c.PURL = purlGo(m.Path, m.Version)
		}
		if eff.Sum != "" {
			c.Properties = append(c.Properties, Property{Name: "go:sum", Value: eff.Sum})
		}
		if err := add(c); err != nil {
			return nil, err
		}
	}

	deps := []Dependency{}
	if in.Cargo != nil {
		rust, edges, err := rustComponents(in.Cargo, in.RustTarget)
		if err != nil {
			return nil, err
		}
		for _, c := range rust {
			if err := add(c); err != nil {
				return nil, err
			}
		}
		deps = append(deps, edges...)
	}

	sort.Strings(rootDeps)
	sort.Slice(components, func(i, j int) bool { return components[i].BOMRef < components[j].BOMRef })
	sort.Slice(deps, func(i, j int) bool { return deps[i].Ref < deps[j].Ref })
	deps = append([]Dependency{{Ref: rootRef, DependsOn: rootDeps}}, deps...)

	// Derived from the digest so the same binary always gets the same
	// serial: a rebuilt, identical artifact yields a byte-identical SBOM.
	sum := sha256.Sum256([]byte(in.BinaryName + "\x00" + in.BinarySHA256))
	serial := hex.EncodeToString(sum[:16])
	serial = fmt.Sprintf("urn:uuid:%s-%s-%s-%s-%s", serial[0:8], serial[8:12], serial[12:16], serial[16:20], serial[20:32])

	return &BOM{
		BOMFormat:    "CycloneDX",
		SpecVersion:  "1.5",
		SerialNumber: serial,
		Version:      1,
		Metadata:     Metadata{Component: root},
		Components:   components,
		Dependencies: deps,
	}, nil
}

func rustComponents(meta *CargoMetadata, target string) ([]Component, []Dependency, error) {
	if meta.Resolve == nil {
		return nil, nil, errors.New("cargo metadata has no resolve graph (was it run with --no-deps?)")
	}
	if len(meta.Resolve.Nodes) == 0 {
		return nil, nil, errors.New("cargo metadata resolved no crates")
	}
	pkgs := map[string]CargoPackage{}
	for _, p := range meta.Packages {
		pkgs[p.ID] = p
	}
	ref := func(p CargoPackage) string { return "cargo:" + p.Name + "@" + p.Version }
	var comps []Component
	var edges []Dependency
	inGraph := map[string]bool{}
	for _, n := range meta.Resolve.Nodes {
		inGraph[n.ID] = true
	}
	for _, n := range meta.Resolve.Nodes {
		p, ok := pkgs[n.ID]
		if !ok {
			return nil, nil, fmt.Errorf("cargo resolve names %s but metadata has no such package", n.ID)
		}
		c := Component{Type: "library", BOMRef: ref(p), Name: p.Name, Version: p.Version}
		if p.Source != nil {
			// Registry and git crates have a purl; path crates do not.
			c.PURL = purlCargo(p.Name, p.Version)
		} else {
			c.Properties = append(c.Properties, Property{Name: "cargo:source", Value: "path"})
		}
		if target != "" {
			c.Properties = append(c.Properties, Property{Name: "cargo:target", Value: target})
		}
		comps = append(comps, c)
		dependsOn := []string{}
		for _, d := range n.Deps {
			if !inGraph[d] {
				continue
			}
			dp, ok := pkgs[d]
			if !ok {
				return nil, nil, fmt.Errorf("cargo resolve names %s but metadata has no such package", d)
			}
			dependsOn = append(dependsOn, ref(dp))
		}
		sort.Strings(dependsOn)
		edges = append(edges, Dependency{Ref: ref(p), DependsOn: dependsOn})
	}
	return comps, edges, nil
}

// Marshal renders the BOM as indented JSON with a trailing newline.
func Marshal(b *BOM) ([]byte, error) {
	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal sbom: %w", err)
	}
	return append(out, '\n'), nil
}
