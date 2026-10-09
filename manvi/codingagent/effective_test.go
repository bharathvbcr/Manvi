package codingagent

import "testing"

// Effective hands out the negotiated session. Both adapters return it through
// one clone, so neither can hand a caller the session's own WritableRoots: a
// caller that appended to or edited the slice would change what the adapter
// believes the provider confirmed.
func TestEffectiveIsACopyTheCallerCannotWriteThrough(t *testing.T) {
	negotiated := Effective{Sandbox: Sandbox{WritableRoots: []string{"/repo"}}}
	for name, effective := range map[string]func() Effective{
		"claude": (&Claude{ready: true, effective: negotiated}).Effective,
		"codex":  (&Codex{ready: true, effective: negotiated}).Effective,
	} {
		got := effective()
		got.Sandbox.WritableRoots[0] = "/elsewhere"
		if again := effective(); again.Sandbox.WritableRoots[0] != "/repo" {
			t.Errorf("%s: editing the returned roots changed the session's to %q",
				name, again.Sandbox.WritableRoots[0])
		}
	}
	if negotiated.Sandbox.WritableRoots[0] != "/repo" {
		t.Fatalf("the fixture itself was written through: %q", negotiated.Sandbox.WritableRoots[0])
	}
}

// Before the session is ready there is nothing negotiated to report.
func TestEffectiveIsEmptyBeforeTheSessionIsReady(t *testing.T) {
	if e := (&Claude{effective: Effective{Model: "m"}}).Effective(); e.Model != "" {
		t.Errorf("claude reported %q before it was ready", e.Model)
	}
	if e := (&Codex{effective: Effective{Model: "m"}}).Effective(); e.Model != "" {
		t.Errorf("codex reported %q before it was ready", e.Model)
	}
}
