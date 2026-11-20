package mcp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A revocation that lands after a survey has read the authorization list must
// not leave a server started on the old read running. The survey is allowed its
// one read; what it may not do is start a process after the operator has
// withdrawn the authorization and leave it alive when the survey is over.
func TestZadvW24ARevocationDuringASurveyDoesNotLeaveAServerStartedAndRunning(t *testing.T) {
	trust := isolateTrust(t)
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "executed")
	writeRootConfig(t, root, "mcp.json", "declared", stubConfig(t, "declared", "marker", marker))
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.CloseAll)
	writeTrust(t, trust, Fingerprint(mustConfig(t, m, "declared")))

	survey := m.BeginSurvey() // the one read: authorized
	writeTrust(t, trust)      // the operator revokes, mid-survey

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := survey.Client(ctx, "declared", true)
	if err == nil && client != nil {
		_, statErr := os.Stat(marker)
		t.Fatalf("survey.Client started and returned a live server (alive=%v, process ran=%v) from a read taken before the revocation; it stays up until the next Client call", client.Alive(), statErr == nil)
	}
}

// The gate flag: a survey caller that did not consult its gate never starts a
// server, whatever the authorization says.
func TestZadvW24SurveyClientWithoutAGateDecisionNeverStartsAnything(t *testing.T) {
	trust := isolateTrust(t)
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "executed")
	writeRootConfig(t, root, "mcp.json", "declared", stubConfig(t, "declared", "marker", marker))
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.CloseAll)
	writeTrust(t, trust, Fingerprint(mustConfig(t, m, "declared")))
	survey := m.BeginSurvey()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if c, err := survey.Client(ctx, "declared", false); err == nil {
		t.Fatalf("started without a gate decision: %v", c)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the server process ran without a gate decision")
	}
	// ListTools with a nil decided map is the no-gate survey and a non-nil empty
	// one is "gate consulted for nobody": the second must not start the server.
	res, err := survey.ListTools(ctx, []string{"declared"}, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Error == "" {
		t.Fatalf("an undecided server was surveyed: %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the server process ran for an undecided survey entry")
	}
}

// A trust file of absurd size must be refused, not read whole into memory.
func TestZadvW24AHugeTrustFileIsRefusedNotBuffered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")
	f, err := os.Create(path) // #nosec G304 -- a path under this test's own t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	const size = 192 << 20
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	t.Setenv(TrustFileEnv, path)
	t.Setenv(TrustListEnv, "")
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	store := loadTrustStore(t.TempDir())
	runtime.ReadMemStats(&after)
	read := after.TotalAlloc - before.TotalAlloc
	if store.err == nil {
		t.Fatal("a 192 MiB file was accepted as an authorization list")
	}
	if read > 32<<20 {
		t.Errorf("loading the trust file allocated %d MiB before refusing it (error: %.80v)", read>>20, store.err)
	}
}

// A trust path that is a link into the repository is inside it, however it is
// spelled.
func TestZadvW24ATrustFileLinkedIntoTheRepositoryIsRefused(t *testing.T) {
	root := t.TempDir()
	cfg := stubConfig(t, "declared", "", "")
	inside := filepath.Join(root, "forged-trust.json")
	writeTrust(t, inside, Fingerprint(cfg))
	link := filepath.Join(t.TempDir(), "trust.json")
	if err := os.Symlink(inside, link); err != nil {
		t.Skip(err)
	}
	t.Setenv(TrustFileEnv, link)
	t.Setenv(TrustListEnv, "")
	store := loadTrustStore(root)
	if store.err == nil {
		t.Fatal("an authorization file that resolves into the repository was read")
	}
	if _, ok := store.fingerprints[strings.ToLower(Fingerprint(cfg))]; ok {
		t.Fatal("the forged fingerprint was loaded")
	}
}

// The old-rule message needs this declaration's own legacy fingerprint in the
// list. A forged or foreign entry (same name, different command; random hex)
// must read as plain "no operator has authorized it".
func TestZadvW24ForeignAndForgedEntriesDoNotTriggerTheOldRuleMessage(t *testing.T) {
	cfg := stubConfig(t, "declared", "", "")
	cfg.TimeoutSeconds = 45
	other := stubConfig(t, "declared", "other-mode", "")
	other.TimeoutSeconds = 45
	for name, fps := range map[string][]string{
		"same name, other command (legacy form)":             {legacyFingerprint(other)},
		"same name, other command (current)":                 {Fingerprint(other)},
		"random":                                             {"sha256:" + strings.Repeat("ab", 32)},
		"current fingerprint of this one is not the old one": {},
	} {
		t.Run(name, func(t *testing.T) {
			trust := isolateTrust(t)
			writeTrust(t, trust, fps...)
			m := NewManager(t.TempDir())
			if err := m.register(cfg, OriginWorkspace, "mcp.json"); err != nil {
				t.Fatal(err)
			}
			err := m.authorize(mustConfig(t, m, "declared"))
			if err == nil {
				t.Fatal("authorized")
			}
			if strings.Contains(err.Error(), "fingerprint rule") || !strings.Contains(err.Error(), "no operator has authorized it") {
				t.Errorf("a foreign entry produced the old-rule message: %v", err)
			}
		})
	}
}

// Public Manager.Client is the ungated entry: record that a caller with no
// gate flag starts an authorized server (callers that gate pass a decision
// through Survey). This is the surface the gate flag does not cover.
func TestZadvW24ManagerClientIsTheUngatedEntry(t *testing.T) {
	trust := isolateTrust(t)
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "executed")
	writeRootConfig(t, root, "mcp.json", "declared", stubConfig(t, "declared", "marker", marker))
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.CloseAll)
	writeTrust(t, trust, Fingerprint(mustConfig(t, m, "declared")))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := m.Client(ctx, "declared"); err != nil {
		t.Fatalf("an authorized server did not start through the ungated entry: %v", err)
	}
	t.Log("Manager.Client / CallTool / ReadResource start a server with gated=false by design; mcp_tools.go:410 and :507 consult the gate unconditionally before them")
}
