package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func buildInfo(cgo string) *debug.BuildInfo {
	return &debug.BuildInfo{
		GoVersion: "go1.26.6",
		Main:      debug.Module{Path: "example.com/manvi"},
		Deps: []*debug.Module{
			{Path: "github.com/a/b", Version: "v1.2.3", Sum: "h1:abc="},
			{Path: "github.com/bharathvbcr/gusset", Version: "(devel)", Replace: &debug.Module{Path: "../../gusset"}},
		},
		Settings: []debug.BuildSetting{
			{Key: "CGO_ENABLED", Value: cgo},
			{Key: "GOOS", Value: "linux"},
			{Key: "GOARCH", Value: "amd64"},
			{Key: "-tags", Value: "netgo,osusergo"},
		},
	}
}

func str(s string) *string { return &s }

func cargo() *CargoMetadata {
	return &CargoMetadata{
		Packages: []CargoPackage{
			{ID: "root 0.1.0 (path+file:///x)", Name: "gusset-engine", Version: "0.1.0"},
			{ID: "serde 1.0.0 (registry)", Name: "serde", Version: "1.0.0", Source: str("registry+https://github.com/rust-lang/crates.io-index")},
		},
		Resolve: &CargoResolve{Nodes: []CargoNode{
			{ID: "root 0.1.0 (path+file:///x)", Deps: []string{"serde 1.0.0 (registry)", "unresolved-for-this-platform"}},
			{ID: "serde 1.0.0 (registry)"},
		}},
	}
}

func TestBuildCgoListsGoAndRust(t *testing.T) {
	bom, err := Build(Input{Tag: "v1.0.0", BinaryName: "manvi-v1.0.0-linux-amd64", BinarySHA256: digest, Build: buildInfo("1"), Cargo: cargo(), RustTarget: "x86_64-unknown-linux-musl"})
	if err != nil {
		t.Fatal(err)
	}
	if bom.BOMFormat != "CycloneDX" || bom.SpecVersion != "1.5" {
		t.Fatalf("format %s %s", bom.BOMFormat, bom.SpecVersion)
	}
	if got := bom.Metadata.Component.Hashes[0]; got.Alg != "SHA-256" || got.Content != digest {
		t.Fatalf("root hash %+v", got)
	}
	purls := map[string]bool{}
	for _, c := range bom.Components {
		purls[c.PURL] = true
	}
	for _, want := range []string{"pkg:golang/github.com%2Fa%2Fb@v1.2.3", "pkg:cargo/serde@1.0.0"} {
		if !purls[want] {
			t.Errorf("missing purl %s in %v", want, purls)
		}
	}
	// The replaced gusset module and the path crate carry no purl, and the
	// replacement is recorded rather than dropped.
	var replaced, pathCrate bool
	for _, c := range bom.Components {
		for _, p := range c.Properties {
			replaced = replaced || (p.Name == "go:replace" && p.Value == "../../gusset@")
			pathCrate = pathCrate || (p.Name == "cargo:source" && p.Value == "path")
		}
		if c.PURL == "" && c.Type != "library" {
			t.Errorf("unexpected component %+v", c)
		}
	}
	if !replaced || !pathCrate {
		t.Errorf("replaced=%v pathCrate=%v", replaced, pathCrate)
	}
	// Every dependsOn target must be a declared component.
	declared := map[string]bool{bom.Metadata.Component.BOMRef: true}
	for _, c := range bom.Components {
		declared[c.BOMRef] = true
	}
	for _, d := range bom.Dependencies {
		if !declared[d.Ref] {
			t.Errorf("dependency ref %s undeclared", d.Ref)
		}
		for _, to := range d.DependsOn {
			if !declared[to] {
				t.Errorf("%s depends on undeclared %s", d.Ref, to)
			}
		}
	}
}

func TestBuildNoCgoHasNoRust(t *testing.T) {
	bom, err := Build(Input{Tag: "v1.0.0", BinaryName: "manvi.exe", BinarySHA256: digest, Build: buildInfo("0")})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range bom.Components {
		if strings.HasPrefix(c.BOMRef, "cargo:") {
			t.Fatalf("unexpected rust component %s", c.BOMRef)
		}
	}
	found := false
	for _, p := range bom.Metadata.Component.Properties {
		found = found || (p.Name == "manvi:gusset-engine" && strings.HasPrefix(p.Value, "not linked"))
	}
	if !found {
		t.Fatal("a CGO_ENABLED=0 binary must say it has no engine")
	}
}

func TestBuildFailsClosed(t *testing.T) {
	ok := Input{Tag: "v1.0.0", BinaryName: "b", BinarySHA256: digest, Build: buildInfo("1"), Cargo: cargo()}
	cases := map[string]func(*Input){
		"cgo binary without cargo":    func(in *Input) { in.Cargo = nil },
		"cargo on a non-cgo binary":   func(in *Input) { in.Build = buildInfo("0") },
		"no build info":               func(in *Input) { in.Build = nil },
		"short digest":                func(in *Input) { in.BinarySHA256 = "abc" },
		"no tag":                      func(in *Input) { in.Tag = "" },
		"cargo without resolve":       func(in *Input) { in.Cargo = &CargoMetadata{} },
		"resolve names unknown crate": func(in *Input) { in.Cargo = &CargoMetadata{Resolve: &CargoResolve{Nodes: []CargoNode{{ID: "ghost"}}}} },
		"resolve with no nodes":       func(in *Input) { in.Cargo = &CargoMetadata{Resolve: &CargoResolve{}} },
	}
	if _, err := Build(ok); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	for name, mutate := range cases {
		in := ok
		mutate(&in)
		if _, err := Build(in); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	in := Input{Tag: "v1.0.0", BinaryName: "b", BinarySHA256: digest, Build: buildInfo("1"), Cargo: cargo()}
	var outs [2][]byte
	for i := range outs {
		bom, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		if outs[i], err = Marshal(bom); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(outs[0], outs[1]) {
		t.Fatal("same input produced different SBOMs")
	}
}

// The command reads real build info: build a tiny program for another OS and
// read the .exe on this host.
func TestRunReadsForeignBinary(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/tiny\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "tiny.exe")
	cmd := exec.Command("go", "build", "-o", exe, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0", "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	outFile := filepath.Join(dir, "tiny.sbom.cdx.json")
	var stderr bytes.Buffer
	if err := run([]string{"-binary", exe, "-tag", "v0.0.1", "-o", outFile}, &stderr); err != nil {
		t.Fatalf("run: %v (%s)", err, stderr.String())
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	var bom BOM
	if err := json.Unmarshal(raw, &bom); err != nil {
		t.Fatal(err)
	}
	var goos string
	for _, p := range bom.Metadata.Component.Properties {
		if p.Name == "go:GOOS" {
			goos = p.Value
		}
	}
	if goos != "windows" {
		t.Fatalf("go:GOOS = %q on %s, want windows", goos, runtime.GOOS)
	}
	// And the cgo mismatch is refused through the command too.
	if err := run([]string{"-binary", exe, "-tag", "v0.0.1", "-o", outFile, "-cargo-metadata", src}, &stderr); err == nil {
		t.Fatal("a non-JSON cargo file must be refused")
	}
}
