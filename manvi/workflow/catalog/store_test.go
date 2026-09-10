package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

func catalogFixture(t *testing.T, revision string) *workflow.Program {
	t.Helper()
	c := workflow.Capability{SchemaVersion: 1, ID: "bank", Revision: revision, Application: "bank", Targets: map[string]workflow.Selector{"balance": {Name: "Balance"}}, Limits: workflow.DefaultLimits(), Steps: []workflow.Step{{ID: "balance", Kind: "extract", Target: "balance", Effect: "read", Output: "balance", OutputType: "money", Currency: "USD"}}}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p, err := workflow.Compile(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestCatalogRejectsReservedRevision(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if _, err := s.Put(catalogFixture(t, "current")); err == nil {
		t.Fatal("current revision can be overwritten by promotion")
	}
}
func TestCatalogRejectsSymlinkIdentityDirectory(t *testing.T) {
	s := Store{Root: t.TempDir()}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.Root, "bank")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(catalogFixture(t, "1")); err == nil {
		t.Fatal("catalog wrote through symlink outside root")
	}
}
func TestCatalogRejectsForeignPathIdentity(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if _, err := s.Put(catalogFixture(t, "1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(s.Root, "bank", "1.json"), filepath.Join(s.Root, "bank", "2.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); err == nil {
		t.Fatal("accepted revision inconsistent with filename")
	}
}
func TestCatalogPublicationAndPromotion(t *testing.T) {
	s := Store{Root: t.TempDir()}
	p := catalogFixture(t, "1")
	e, err := s.Put(p)
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != StatusDraft || e.Promoted {
		t.Fatalf("put must create draft: %#v", e)
	}
	if _, err := s.Put(p); err != nil {
		t.Fatal("idempotent put", err)
	}
	if err := s.Promote(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	rows, err := s.List()
	if err != nil || len(rows) != 1 || !rows[0].Promoted || rows[0].Status != StatusApproved {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
}
func TestCatalogDraftApprovedRevokedAndUnattendedGate(t *testing.T) {
	s := Store{Root: t.TempDir()}
	e, err := s.Put(catalogFixture(t, "1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckUnattended(e); err == nil || !strings.Contains(err.Error(), "draft") {
		t.Fatalf("draft must refuse unattended: %v", err)
	}
	if err := s.Approve(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	approved, err := s.Get(e.ID, e.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if !approved.AllowsUnattended() || CheckUnattended(approved) != nil {
		t.Fatalf("approved must allow unattended: %#v", approved)
	}
	if err := s.Revoke(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	revoked, err := s.Get(e.ID, e.Revision)
	if err != nil || revoked.Status != StatusRevoked || revoked.Promoted {
		t.Fatalf("revoked=%#v err=%v", revoked, err)
	}
	if err := CheckUnattended(revoked); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked must refuse unattended: %v", err)
	}
}
func TestCatalogStabilityFromQualifyReport(t *testing.T) {
	s := Store{Root: t.TempDir()}
	e, err := s.Put(catalogFixture(t, "1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	report := QualifyCampaign{
		Passed: 2, Failed: 1, Blocked: 1, FirstAttemptSuccesses: 1,
		Results: []QualifyTrial{
			{Outcome: "passed", RungIndexes: []int{0, 1}, Recoveries: 1},
			{Outcome: "passed", RungIndexes: []int{0}, Recoveries: 0},
			{Outcome: "failed"},
			{Outcome: "blocked", Recoveries: 2},
		},
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.ApplyQualifyReport(e.ID, e.Revision, e.SHA256, raw)
	if err != nil {
		t.Fatal(err)
	}
	if st.Runs != 4 || st.SuccessRate != 0.5 || st.FirstAttemptRate != 0.25 {
		t.Fatalf("rates=%#v", st)
	}
	if st.MeanRungIndex != (0+1+0)/3.0 || st.RecoveriesPerRun != 3.0/4.0 {
		t.Fatalf("locator/recovery averages=%#v", st)
	}
	got, err := s.Get(e.ID, e.Revision)
	if err != nil || got.Stability == nil || got.Stability.SuccessRate != 0.5 {
		t.Fatalf("persisted stability=%#v err=%v", got.Stability, err)
	}
	// Thin qualify reports without rung/recovery fields still produce rates.
	thin, err := StabilityFromQualify(QualifyCampaign{Passed: 2, Failed: 0, Blocked: 0, FirstAttemptSuccesses: 1, Results: []QualifyTrial{{Outcome: "passed"}, {Outcome: "passed"}}})
	if err != nil || thin.MeanRungIndex != 0 || thin.RecoveriesPerRun != 0 || thin.SuccessRate != 1 || thin.FirstAttemptRate != 0.5 {
		t.Fatalf("thin report=%#v err=%v", thin, err)
	}
}
func TestLegacyPromotedPointerNormalizesToApproved(t *testing.T) {
	s := Store{Root: t.TempDir()}
	p := catalogFixture(t, "1")
	e, err := s.Put(p)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(Entry{ID: e.ID, Revision: e.Revision, SHA256: e.SHA256, Description: e.Description, Path: e.Path, Promoted: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, "bank", "current.json"), meta, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(e.ID, e.Revision)
	if err != nil || got.Status != StatusApproved || !got.Promoted {
		t.Fatalf("legacy promoted pointer=%#v err=%v", got, err)
	}
}
