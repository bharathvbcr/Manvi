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

const gussetSHA = "e561435afaf6cbfc0331108a14dda4d9595dae54"
const pinsText = "# comment\nDevCouncil\thttps://example.com/DevCouncil.git\t947090b7fa8718cd257c9032f2641751ab8bc900\ngusset\thttps://example.com/gusset.git\t" + gussetSHA + "\n"

func pins(t *testing.T) []Pin {
	t.Helper()
	p, err := ParsePins(pinsText)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func str(s string) *string { return &s }

func cargo() *CargoMetadata {
	return &CargoMetadata{
		Packages: []CargoPackage{
			{ID: "root 0.1.0 (path+file:///x)", Name: "gusset-engine", Version: "0.1.0", ManifestPath: "/w/DevCouncil/rust/gusset-engine/Cargo.toml"},
			{ID: "serde 1.0.0 (registry)", Name: "serde", Version: "1.0.0", Source: str("registry+https://github.com/rust-lang/crates.io-index")},
		},
		Resolve: &CargoResolve{Nodes: []CargoNode{
			{ID: "root 0.1.0 (path+file:///x)", Deps: []string{"serde 1.0.0 (registry)", "unresolved-for-this-platform"}},
			{ID: "serde 1.0.0 (registry)"},
		}},
	}
}

func TestBuildCgoListsGoAndRust(t *testing.T) {
	bom, err := Build(Input{Tag: "v1.0.0", BinaryName: "manvi-v1.0.0-linux-amd64", BinarySHA256: digest, Build: buildInfo("1"), Cargo: cargo(), RustTarget: "x86_64-unknown-linux-musl", Pins: pins(t)})
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
	for _, want := range []string{"pkg:golang/github.com/a/b@v1.2.3", "pkg:cargo/serde@1.0.0"} {
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
	bom, err := Build(Input{Tag: "v1.0.0", BinaryName: "manvi.exe", BinarySHA256: digest, Build: buildInfo("0"), Pins: pins(t)})
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
	ok := Input{Tag: "v1.0.0", BinaryName: "b", BinarySHA256: digest, Build: buildInfo("1"), Cargo: cargo(), Pins: pins(t)}
	cases := map[string]func(*Input){
		"cgo binary without cargo":     func(in *Input) { in.Cargo = nil },
		"cargo on a non-cgo binary":    func(in *Input) { in.Build = buildInfo("0") },
		"no build info":                func(in *Input) { in.Build = nil },
		"short digest":                 func(in *Input) { in.BinarySHA256 = "abc" },
		"no tag":                       func(in *Input) { in.Tag = "" },
		"cargo without resolve":        func(in *Input) { in.Cargo = &CargoMetadata{} },
		"resolve names unknown crate":  func(in *Input) { in.Cargo = &CargoMetadata{Resolve: &CargoResolve{Nodes: []CargoNode{{ID: "ghost"}}}} },
		"replaced module with no pins": func(in *Input) { in.Pins = nil },
		"replaced module under no pin": func(in *Input) { in.Pins = in.Pins[:1] },
		"path crate under no pin": func(in *Input) {
			in.Cargo = cargo()
			in.Cargo.Packages[0].ManifestPath = "/elsewhere/x/Cargo.toml"
		},
		"resolve with no nodes": func(in *Input) { in.Cargo = &CargoMetadata{Resolve: &CargoResolve{}} },
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
	in := Input{Tag: "v1.0.0", BinaryName: "b", BinarySHA256: digest, Build: buildInfo("1"), Cargo: cargo(), Pins: pins(t)}
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
	if err := os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/tiny\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "tiny.exe")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", exe, ".")
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
	raw, err := os.ReadFile(filepath.Clean(outFile))
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

func TestPurlKeepsNamespaceSlashes(t *testing.T) {
	bom, err := Build(Input{Tag: "v1.0.0", BinaryName: "b", BinarySHA256: digest, Build: buildInfo("0"), Pins: pins(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range bom.Components {
		if c.Name == "github.com/a/b" && c.PURL != "pkg:golang/github.com/a/b@v1.2.3" {
			t.Fatalf("purl = %q", c.PURL)
		}
	}
}

func TestPurlEscapesPerSegment(t *testing.T) {
	cases := []struct{ path, version, want string }{
		{"example.com/a b/c", "v2.0.0+incompatible", "pkg:golang/example.com/a%20b/c@v2.0.0%2Bincompatible"},
		{"github.com/BurntSushi/toml", "v1.4.0", "pkg:golang/github.com/burntsushi/toml@v1.4.0"},
		{"example.com/q?x/#y", "v1.0.0", "pkg:golang/example.com/q%3Fx/%23y@v1.0.0"},
		{"golang.org/x/sys", "v0.0.0-20240101000000-abcdef123456", "pkg:golang/golang.org/x/sys@v0.0.0-20240101000000-abcdef123456"},
	}
	for _, c := range cases {
		if got := purlGo(c.path, c.version); got != c.want {
			t.Errorf("purlGo(%q, %q) = %q, want %q", c.path, c.version, got, c.want)
		}
	}
}

func TestReplacedModuleCarriesRevision(t *testing.T) {
	bom, err := Build(Input{Tag: "v1.0.0", BinaryName: "b", BinarySHA256: digest, Build: buildInfo("0"), Pins: pins(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range bom.Components {
		if c.Name != "github.com/bharath"+"vbcr/gusset" {
			continue
		}
		if c.Version != gussetSHA || c.PURL != "pkg:golang/github.com/bharathvbcr/gusset@"+gussetSHA {
			t.Fatalf("version %q purl %q", c.Version, c.PURL)
		}
		for _, p := range c.Properties {
			if p.Name == "go:revision" && p.Value == gussetSHA {
				return
			}
		}
		t.Fatal("replaced gusset module has no go:revision property")
	}
	t.Fatal("gusset component missing")
}

func TestRustPathCrateCarriesRevision(t *testing.T) {
	bom, err := Build(Input{Tag: "v1.0.0", BinaryName: "b", BinarySHA256: digest, Build: buildInfo("1"), Cargo: cargo(), Pins: pins(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range bom.Components {
		if c.Name != "gusset-engine" {
			continue
		}
		for _, p := range c.Properties {
			if p.Name == "cargo:revision" && p.Value == "947090b7fa8718cd257c9032f2641751ab8bc900" {
				return
			}
		}
		t.Fatal("path crate has no cargo:revision")
	}
	t.Fatal("gusset-engine component missing")
}

func TestParsePinsFailsClosed(t *testing.T) {
	for name, text := range map[string]string{
		"empty":     "# nothing\n",
		"short sha": "gusset\thttps://x\tabc\n",
		"columns":   "gusset https://x e561435afaf6cbfc0331108a14dda4d9595dae54\n",
		"uppercase": "gusset\thttps://x\tE561435AFAF6CBFC0331108A14DDA4D9595DAE54\n",
	} {
		if _, err := ParsePins(text); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if p, err := ParsePins(strings.ReplaceAll(pinsText, "\n", "\r\n")); err != nil || len(p) != 2 {
		t.Errorf("CRLF pins: %v %v", p, err)
	}
}
