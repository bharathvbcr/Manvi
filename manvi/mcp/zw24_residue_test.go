package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// authorizedRunning returns a manager holding a live, authorized workspace
// server named "declared", and the trust file that authorizes it.
func authorizedRunning(t *testing.T) (m *Manager, trust string, running *Client) {
	t.Helper()
	trust = isolateTrust(t)
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "executed")
	writeRootConfig(t, root, "mcp.json", "declared", stubConfig(t, "declared", "marker", marker))
	m = NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeTrust(t, trust, Fingerprint(mustConfig(t, m, "declared")))
	t.Cleanup(m.CloseAll)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	running, err := m.Client(ctx, "declared")
	if err != nil {
		t.Fatalf("the authorized server did not start: %v", err)
	}
	return m, trust, running
}

// An authorization list that cannot be read closes the running server (leaving
// it up would turn a half-written file into permission), and the refusal says so
// and why, and is recorded where the operator's diagnostics read.
func TestAnUnreadableTrustFileClosesTheRunningServerAndSaysSo(t *testing.T) {
	m, trust, running := authorizedRunning(t)
	if err := os.WriteFile(trust, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := m.Client(ctx, "declared")
	if err == nil {
		t.Fatal("a server whose authorization cannot be read was handed out")
	}
	for _, want := range []string{"was closed", "could not be read", "declared"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if running.Alive() {
		t.Error("the running server was left running with an unreadable authorization (fail-open)")
	}
}

func TestARevokedAuthorizationClosesTheRunningServerAndSaysRevoked(t *testing.T) {
	m, trust, running := authorizedRunning(t)
	writeTrust(t, trust) // an empty list
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := m.Client(ctx, "declared")
	if err == nil || !strings.Contains(err.Error(), "was closed") || !strings.Contains(err.Error(), "removed or revoked") {
		t.Fatalf("the refusal does not say the server was closed because the authorization was revoked: %v", err)
	}
	if running.Alive() {
		t.Error("a revoked server was left running")
	}
}

// A declaration that raises its own timeout, or names a relative cwd, fingerprints
// differently since the w23 rule change. An operator who approved it before then
// has its old fingerprint in the list, and must be told the rule changed, not that
// nobody authorized it.
func TestAnApprovalUnderTheOldFingerprintRuleIsNotReportedAsNeverAuthorized(t *testing.T) {
	cfg := stubConfig(t, "declared", "", "")
	cfg.TimeoutSeconds = 30
	old := legacyFingerprint(cfg)
	if old == Fingerprint(cfg) {
		t.Fatal("the declaration fingerprints the same under both rules, so the test proves nothing")
	}
	for name, approve := range map[string]func(t *testing.T){
		"in the environment": func(t *testing.T) {
			isolateTrust(t)
			t.Setenv(TrustListEnv, old)
		},
		"in the file, under another name": func(t *testing.T) {
			writeTrust(t, isolateTrust(t), old)
		},
	} {
		t.Run(name, func(t *testing.T) {
			approve(t)
			m := NewManager(t.TempDir())
			if err := m.register(cfg, OriginWorkspace, "mcp.json"); err != nil {
				t.Fatal(err)
			}
			err := m.authorize(mustConfig(t, m, "declared"))
			if err == nil {
				t.Fatal("a declaration whose fingerprint is not the authorized one passed")
			}
			if strings.Contains(err.Error(), "no operator has authorized it") {
				t.Errorf("an operator who authorized this server under the old rule is told nobody did: %v", err)
			}
			for _, want := range []string{"fingerprint rule", old, "approving again", Fingerprint(mustConfig(t, m, "declared"))} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not contain %q: %v", want, err)
				}
			}
		})
	}
}

// Another repository's server that merely shares a name with an approved one, or a
// declaration that was never approved, is still just unauthorized: the trust file
// is per user, and nothing about it says the rule changed for this declaration.
func TestAnEntryUnderTheSameNameForAnotherDeclarationIsStillJustUnauthorized(t *testing.T) {
	trust := isolateTrust(t)
	cfg := stubConfig(t, "declared", "", "")
	cfg.TimeoutSeconds = 30
	m := NewManager(t.TempDir())
	if err := m.register(cfg, OriginWorkspace, "mcp.json"); err != nil {
		t.Fatal(err)
	}
	other := "sha256:" + strings.Repeat("0", 64)
	if err := os.WriteFile(trust, []byte(`{"authorized":[{"fingerprint":"`+other+`","name":"declared"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := m.authorize(mustConfig(t, m, "declared"))
	if err == nil || !strings.Contains(err.Error(), "no operator has authorized it") || strings.Contains(err.Error(), "fingerprint rule") {
		t.Fatalf("a never-approved declaration is not reported as that: %v", err)
	}
}

func TestTheClosureOfAServerApprovedUnderTheOldRuleSaysSo(t *testing.T) {
	m := NewManager(t.TempDir())
	err := m.closedByAuthorization("declared", &authorizationError{kind: authChanged, msg: "refused"})
	if !strings.Contains(err.Error(), "was closed") || !strings.Contains(err.Error(), "fingerprint rule") {
		t.Fatalf("the closure does not say why: %v", err)
	}
}

// One survey is one read of the authorization list, however many servers it
// touches and however many questions it asks of each.
func TestASurveyReadsTheAuthorizationListOnce(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		writeRootConfig(t, root, name+".json", name, stubConfig(t, name, "", ""))
	}
	m := NewManager(root)
	for _, f := range []string{"a.json", "b.json", "c.json"} {
		if _, err := m.loadConfigFile(f); err != nil {
			t.Fatal(err)
		}
	}
	var reads atomic.Int64
	m.trustLoader = func(root string) trustStore {
		reads.Add(1)
		return loadTrustStore(root)
	}
	survey := m.BeginSurvey()
	names := m.ServerNames()
	for _, name := range names {
		survey.NeedsGate(name)
	}
	if _, err := survey.ListTools(context.Background(), names, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("a survey of %d servers read the authorization list %d times, want 1", len(names), got)
	}
}

// A server that could not start when the gate was consulted, and can when the
// client is asked for, must not be started on a gate nobody consulted for it.
func TestAServerThatBecameStartableAfterTheGateWasSkippedIsNotStarted(t *testing.T) {
	trust := isolateTrust(t)
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "executed")
	writeRootConfig(t, root, "mcp.json", "declared", stubConfig(t, "declared", "marker", marker))
	m := NewManager(root)
	if err := m.AutoDiscover(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.CloseAll)
	survey := m.BeginSurvey()
	if survey.CanStart("declared") {
		t.Fatal("an unauthorized server could start")
	}
	// The operator authorizes it between the question and the start. The
	// survey's one read still says no, so it is not started.
	writeTrust(t, trust, Fingerprint(mustConfig(t, m, "declared")))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := survey.Client(ctx, "declared", false); err == nil {
		t.Fatal("a server was started on a gate decision that was never made")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the server's process ran")
	}
	// A fresh read sees the authorization, and a caller that did not consult its
	// gate is still refused before anything starts.
	fresh := m.BeginSurvey()
	if !fresh.CanStart("declared") {
		t.Fatal("the authorized server cannot start")
	}
	if _, err := fresh.Client(ctx, "declared", false); err == nil || !strings.Contains(err.Error(), "gate was not consulted") {
		t.Fatalf("a startable server was reached without the gate: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the server's process ran without a gate decision")
	}
	if _, err := fresh.Client(ctx, "declared", true); err != nil {
		t.Fatalf("a server whose gate was consulted did not start: %v", err)
	}
}
