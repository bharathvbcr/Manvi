package serve

import (
	"encoding/json"
	"strings"
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
// path against a repository, so the request is refused rather than answered.
//
// This asserted a `command.redirect_targets.unchecked` marker on an otherwise
// normal decision until the rung was unified with gate.Gate's. A marker is only
// as good as the host's willingness to read it, and a host that ignored it
// received `echo hi > .env` as an ordinary allow; E_BAD_REQUEST cannot be
// overlooked, and it names the fix. The honesty requirement is unchanged — an
// unrunnable check must not report what a passing one does — and this is the
// stricter way of meeting it.
func TestCommandCheckWithoutARootIsRefusedRatherThanAnswered(t *testing.T) {
	resp := roundTrip(t, hostOpts(), commandCheck(t, "1", `echo hi > .env`))[0]
	if resp.OK {
		t.Fatalf("no root was given and the command redirects, so the outside-root rung "+
			"could not run; the request must be refused rather than answered: %s", resp.Result)
	}
	if resp.Error == nil || resp.Error.Code != "E_BAD_REQUEST" {
		t.Fatalf("want E_BAD_REQUEST naming the missing root, got %+v", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "root") {
		t.Fatalf("the refusal must name what is missing: %q", resp.Error.Message)
	}

	// With a root, the same command is judged rather than refused.
	resp = roundTrip(t, hostOpts(), commandCheckIn(t, "1", t.TempDir(), `echo hi > .env`))[0]
	d := decodeDecision(t, resp)
	if !d.Blocked() {
		t.Fatalf("with a root, a redirect into .env must be refused; got %+v", d)
	}

	// A command with no redirections needs no root, and must still be answered.
	// The refusal above is scoped to the case where a target actually had to be
	// resolved; widening it to every rootless command would refuse the ordinary
	// plain-command check this surface exists to serve.
	resp = roundTrip(t, hostOpts(), commandCheck(t, "1", `git status`))[0]
	if !resp.OK {
		t.Fatalf("`git status` redirects nowhere, so no root is needed; it must be answered, "+
			"not refused: %+v", resp.Error)
	}
	if d := decodeDecision(t, resp); d.Blocked() {
		t.Fatalf("`git status` redirects nowhere and must not pick up the redirect rung: %+v", d)
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
