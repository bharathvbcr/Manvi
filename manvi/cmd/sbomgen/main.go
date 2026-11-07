// Command sbomgen writes a CycloneDX 1.5 SBOM for one release binary.
//
// It reads the Go module graph from the build info embedded in the binary
// itself (ELF, Mach-O and PE alike, so a Windows .exe is read on a Linux
// host) and, for a binary that links the Gusset engine, the Rust crates from
// a `cargo metadata` file resolved for the engine's target triple.
//
//	sbomgen -binary dist/manvi-v1.0.0-linux-amd64 -tag v1.0.0 \
//	    -pins scripts/module-pins.txt -cargo-metadata cargo.json -rust-target x86_64-unknown-linux-musl \
//	    -o dist/manvi-v1.0.0-linux-amd64.sbom.cdx.json
package main

import (
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "sbomgen: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("sbomgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	binary := fs.String("binary", "", "path to the release binary")
	tag := fs.String("tag", "", "release tag, for example v1.0.0")
	cargoPath := fs.String("cargo-metadata", "", "cargo metadata JSON for the linked Rust engine (omit for a CGO_ENABLED=0 binary)")
	rustTarget := fs.String("rust-target", "", "Rust target triple the cargo metadata was resolved for")
	pinsPath := fs.String("pins", "", "scripts/module-pins.txt: the pinned commit recorded on each replaced Go module and Rust path crate")
	out := fs.String("o", "", "output file")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *binary == "" || *tag == "" || *out == "" {
		return fmt.Errorf("-binary, -tag and -o are required")
	}

	digest, err := fileSHA256(*binary)
	if err != nil {
		return err
	}
	info, err := buildinfo.ReadFile(*binary)
	if err != nil {
		return fmt.Errorf("read Go build info from %s: %w", *binary, err)
	}
	in := Input{
		Tag:          *tag,
		BinaryName:   filepath.Base(*binary),
		BinarySHA256: digest,
		Build:        info,
		RustTarget:   *rustTarget,
	}
	if *pinsPath != "" {
		raw, err := os.ReadFile(*pinsPath)
		if err != nil {
			return fmt.Errorf("read pins: %w", err)
		}
		if in.Pins, err = ParsePins(string(raw)); err != nil {
			return fmt.Errorf("pins %s: %w", *pinsPath, err)
		}
	}
	if *cargoPath != "" {
		raw, err := os.ReadFile(*cargoPath)
		if err != nil {
			return fmt.Errorf("read cargo metadata: %w", err)
		}
		var meta CargoMetadata
		if err := json.Unmarshal(raw, &meta); err != nil {
			return fmt.Errorf("parse cargo metadata %s: %w", *cargoPath, err)
		}
		in.Cargo = &meta
	}
	bom, err := Build(in)
	if err != nil {
		return err
	}
	body, err := Marshal(bom)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	fmt.Fprintf(stderr, "sbomgen: wrote %s (%d Go and Rust components)\n", *out, len(bom.Components))
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
