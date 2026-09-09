package catalog

import (
	"encoding/json"
	"github.com/bharathvbcr/Manvi/manvi/workflow"
	"os"
	"path/filepath"
	"testing"
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
	if _, err := s.Put(p); err != nil {
		t.Fatal("idempotent put", err)
	}
	if err := s.Promote(e.ID, e.Revision, e.SHA256); err != nil {
		t.Fatal(err)
	}
	rows, err := s.List()
	if err != nil || len(rows) != 1 || !rows[0].Promoted {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
}
