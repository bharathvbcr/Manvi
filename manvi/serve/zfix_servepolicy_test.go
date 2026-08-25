package serve

import (
	"encoding/json"
	"strings"
	"testing"

	"manvi/policy"
)

// commandCheckWith builds a policy.check.command request with every knob the
// redirect rung depends on.
func commandCheckWith(t *testing.T, p CommandCheckParams) Request {
	t.Helper()
	params, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return Request{ID: "1", Op: OpPolicyCheckCommand, Params: params}
}

// DEFECT 2. The serve plane built policy.CommandGate directly, so the redirect
// rung — which lives one layer up, in gate.Gate — never ran for an embedding
// host at all.
//
// What made it worse than a missing check is what the host was told: a clean
// allow, with an empty Rule and an empty Demoted, which no audit can tell apart
// from a command the rules actually passed.
func TestServeGatesRedirectTargetsUnderTheDevCouncilPosture(t *testing.T) {
	root := t.TempDir()
	opts := Options{HardRules: true, Posture: PostureDevCouncil}

	for _, tc := range []struct {
		command string
		rule    policy.RuleID
	}{
		// `~` is an expansion only the shell resolves, so the target cannot be
		// named and is refused as an unverifiable write.
		{"git diff > ~/.ssh/authorized_keys", policy.RuleCommandSubstitution},
		// Absolute and outside the root the host declared.
		{"echo x > /etc/sudoers", policy.RuleOutsideRoot},
		// Inside the root, and a credential path.
		{"echo x > .env.local", policy.RuleSecretPath},
		// Inside the root, and the harness's own machinery.
		{"echo x >> .devcouncil/harness-grants.json", policy.RuleRestrictedPath},
	} {
		d := decodeDecision(t, roundTrip(t, opts, commandCheckWith(t, CommandCheckParams{
			Command:          tc.command,
			Root:             root,
			AllowedCommands:  []string{"git diff *", "echo *"},
			EnforceAllowlist: true,
		}))[0])
		if d.Action != policy.Deny {
			t.Errorf("%q: action = %q rule = %q demoted = %q, want deny/%q",
				tc.command, d.Action, d.Rule, d.Demoted, tc.rule)
			continue
		}
		if d.Rule != tc.rule {
			t.Errorf("%q: rule = %q, want %q", tc.command, d.Rule, tc.rule)
		}
	}
}

// DEFECT 2, the ordering half. Under the host posture a soft command denial is
// demoted wholesale — that is what the posture is for — and the redirect rung
// has to run after the demotion, or it never sees the commands the demotion
// lets through.
func TestServeGatesRedirectTargetsThroughTheHostDemotion(t *testing.T) {
	root := t.TempDir()

	// The host declares no allowlist, so the ladder ends at
	// command.not_allowed — Soft, and demoted to an allow by the posture.
	d := decodeDecision(t, roundTrip(t, hostOpts(), commandCheckWith(t, CommandCheckParams{
		Command: "cat notes.txt > .env.local",
		Root:    root,
	}))[0])
	if d.Action != policy.Deny {
		t.Fatalf("action = %q rule = %q demoted = %q, want the write to .env.local refused",
			d.Action, d.Rule, d.Demoted)
	}
	if d.Rule != policy.RuleSecretPath {
		t.Fatalf("rule = %q, want %q", d.Rule, policy.RuleSecretPath)
	}
	if d.Demoted != "" {
		t.Fatalf("a hard write refusal was demoted to %q", d.Demoted)
	}
}

// A redirection target and a direct write to the same path are the same write,
// so the two surfaces must not be able to disagree. This is what routing serve
// through the harness's own rung buys, as against a second copy of it here.
func TestARedirectTargetAndADirectWriteAgree(t *testing.T) {
	root := t.TempDir()

	for _, path := range []string{".env.local", "secrets/token.txt", ".git/config", "../outside.txt"} {
		direct := decodeDecision(t, roundTrip(t, hostOpts(), fileCheck(t, "1", root, path))[0])
		viaCommand := decodeDecision(t, roundTrip(t, hostOpts(), commandCheckWith(t, CommandCheckParams{
			Command: "echo x > " + path,
			Root:    root,
		}))[0])
		if direct.Action != viaCommand.Action || direct.Rule != viaCommand.Rule {
			t.Errorf("%s: direct write %s/%q but redirect %s/%q",
				path, direct.Action, direct.Rule, viaCommand.Action, viaCommand.Rule)
		}
	}
}

// A root is required only when the command actually redirects, and refused
// rather than defaulted when it is missing: judging `> /etc/sudoers` against
// whatever directory the sidecar was spawned from is a check that did not run
// reported as one that did.
func TestARedirectWithoutARootIsRefusedRatherThanGuessedAt(t *testing.T) {
	resp := roundTrip(t, hostOpts(), commandCheckWith(t, CommandCheckParams{
		Command: "echo x > /etc/sudoers",
	}))[0]
	if resp.OK {
		t.Fatalf("a redirect with no root was answered with a decision: %s", string(resp.Result))
	}
	if !strings.Contains(resp.Error.Message, "requires a root") {
		t.Fatalf("error = %q, want it to name the missing root", resp.Error.Message)
	}

	// ...and a command that does not redirect never consults it, so a host that
	// only checks plain commands keeps sending exactly what it always sent.
	d := decodeDecision(t, roundTrip(t, hostOpts(), commandCheckWith(t, CommandCheckParams{
		Command: "ls -la",
	}))[0])
	if d.Action != policy.Allow {
		t.Fatalf("action = %q (rule %q), want the taskless allow the host posture gives", d.Action, d.Rule)
	}
}

// The decision set for a command with no redirection must not move.
func TestServeCommandsWithoutARedirectAreUnchanged(t *testing.T) {
	for _, tc := range []struct {
		command string
		action  policy.Action
		rule    policy.RuleID
	}{
		{"ls -la", policy.Allow, policy.RuleCommandNotAllowed},
		{"git status", policy.Allow, policy.RuleNone},
		{"git push --force origin main", policy.Deny, policy.RuleCommandForcePush},
		{"git commit --no-verify -m x", policy.Deny, policy.RuleCommandBypassFlag},
		{"", policy.Deny, policy.RuleCommandEmpty},
		// The ladder's own substitution rung, not the redirect one: `date` is
		// in no allowlist, so the substituted command is refused and the line
		// with it. Hard, so the host demotion leaves it alone.
		{"echo $(date)", policy.Deny, policy.RuleCommandSubstitution},
	} {
		d := decodeDecision(t, roundTrip(t, hostOpts(), commandCheckWith(t, CommandCheckParams{
			Command: tc.command,
		}))[0])
		if d.Action != tc.action || d.Rule != tc.rule {
			t.Errorf("%q: got %s/%q, want %s/%q", tc.command, d.Action, d.Rule, tc.action, tc.rule)
		}
	}
}
