package serve

import (
	"encoding/json"
	"slices"
	"testing"

	"manvi/policy"
)

// commandCheckIn is commandCheck with a root, so the redirect rung can run.
func commandCheckIn(t *testing.T, id, root, command string) Request {
	t.Helper()
	params, err := json.Marshal(CommandCheckParams{Command: command, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	return Request{ID: id, Op: OpPolicyCheckCommand, Params: params}
}

// A command line writes files through its redirections, and under PostureHost
// every Soft denial is demoted — so a host asking about a command that is not
// on its allowlist and redirects into .env used to be told "allowed": the
// allowlist refusal demoted, the write to .env never looked at.
//
// The hard rules are the ones a host embeds this gate for. They have to hold on
// the files a command writes exactly as they hold on the files it names.
func TestHostPostureRefusesCommandsThatRedirectIntoASecret(t *testing.T) {
	root := t.TempDir()
	for _, command := range []string{
		`echo hi > .env`,
		`echo hi >> .env`,
		`echo hi >| .env`,
		`echo $(echo hi > .env)`,
		"echo `echo hi > .env`",
		`nope > .env`,
		`(echo hi > .env)`,
		`echo hi > secrets/token.txt`,
		`echo hi > deploy/id_rsa`,
		`echo hi > .git/config`,
	} {
		resp := roundTrip(t, hostOpts(), commandCheckIn(t, "1", root, command))[0]
		d := decodeDecision(t, resp)
		if !d.Blocked() {
			t.Errorf("%q: allowed, but it redirects into a path the hard rules protect (rule %q)",
				command, d.Rule)
			continue
		}
		if d.Severity != policy.Hard {
			t.Errorf("%q: refused only softly (%s/%s); a Soft refusal is demoted under this posture",
				command, d.Rule, d.Severity)
		}
	}
}

// A root is optional on this surface, and its absence must not read like a
// check that ran and passed. Without one the redirect rung cannot resolve a
// path against a repository, so the decision says so.
func TestCommandCheckWithoutARootReportsThatRedirectsWereNotJudged(t *testing.T) {
	resp := roundTrip(t, hostOpts(), commandCheck(t, "1", `echo hi > .env`))[0]
	d := decodeDecision(t, resp)
	if !slices.Contains(d.Degraded, "command.redirect_targets.unchecked") {
		t.Fatalf("no root was given, so the redirect rung could not run; "+
			"the decision must record that. degraded=%v", d.Degraded)
	}

	// With a root, the same command is judged and the degradation is gone.
	resp = roundTrip(t, hostOpts(), commandCheckIn(t, "1", t.TempDir(), `echo hi > .env`))[0]
	d = decodeDecision(t, resp)
	if slices.Contains(d.Degraded, "command.redirect_targets.unchecked") {
		t.Fatalf("a root was given, so the rung ran; it must not still report itself unchecked. degraded=%v",
			d.Degraded)
	}
	if !d.Blocked() {
		t.Fatalf("with a root, a redirect into .env must be refused; got %+v", d)
	}

	// A command with no redirections has nothing that went unjudged, root or
	// no root. Marking it degraded would report an absent check where there was
	// no check to run.
	resp = roundTrip(t, hostOpts(), commandCheck(t, "1", `git status`))[0]
	d = decodeDecision(t, resp)
	if slices.Contains(d.Degraded, "command.redirect_targets.unchecked") {
		t.Fatalf("`git status` redirects nowhere; nothing about it was left unjudged. degraded=%v",
			d.Degraded)
	}
}

// A command that writes nothing must not pick up the redirect rung's refusal,
// and a legitimate redirect inside the project must still pass.
func TestCommandCheckDoesNotRefuseOrdinaryRedirects(t *testing.T) {
	root := t.TempDir()
	for _, command := range []string{
		`git status`,
		`git diff > patch.diff`,
		`echo hi > out/report.txt`,
	} {
		resp := roundTrip(t, hostOpts(), commandCheckIn(t, "1", root, command))[0]
		d := decodeDecision(t, resp)
		if d.Blocked() {
			t.Errorf("%q: refused (%s: %s); nothing here touches a protected path",
				command, d.Rule, d.Reason)
		}
	}
}
