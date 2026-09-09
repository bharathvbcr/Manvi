package devcouncil

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// This is a real Go->Rust process contract test. It is deliberately opt-in
// because a missing Rust executable is unavailable coverage, never a pass.
func TestEvidenceRustCompatibility(t *testing.T) {
	root := os.Getenv("DEVC_COUNCIL_EVIDENCE_ROOT")
	if root == "" {
		t.Skip("DEVC_COUNCIL_EVIDENCE_ROOT not set; real Rust compatibility not executed")
	}
	fixture := filepath.Join(root, "rust", "dc-evidence", "fixtures", "v1")
	req := EvidenceRequest{Binary: filepath.Join(root, "rust", "target", "debug", "dcverify"), ContractPath: filepath.Join(fixture, "contract.json"), BundlePath: filepath.Join(fixture, "bundle-resumed.json"), ArtifactsRoot: fixture, ContractSHA256: "bdf5f2cf064b49e340ba043d78511e8e10bdf6ee4276daa0b03fc5fc8922e816", CapabilitySHA256: "1a4bf7dac3b78318718a5047937473da4898acfb53cbbc9afdbe0c7fa9f3eec2", RunID: "run-1", SessionID: "desktop-1", Epoch: 1}
	report, err := VerifyEvidence(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != "passed" {
		t.Fatalf("resumed independent fixture: %+v", report)
	}
	req.RunID = "forged-run"
	report, err = VerifyEvidence(context.Background(), req)
	if err == nil && report.Verdict == "passed" {
		t.Fatal("wrong-run evidence accepted")
	}
}
