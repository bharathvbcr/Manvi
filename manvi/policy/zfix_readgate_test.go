package policy

import "testing"

// Reading is its own authorisation question. The write rung refused .env as
// path.secret from the beginning; nothing asked the same question of a read,
// so the contents came back — and a scrubber that replaces credential values
// this process was handed has nothing to say about a repository's own.
func TestSecretPathsAreNotReadable(t *testing.T) {
	g := FileGate{Root: "/repo", HardRules: true}

	for _, path := range []string{
		".env",
		".env.local",
		".ENV",
		"sub/.env",
		"secrets/token.txt",
		"deep/secrets/token.txt",
		"credentials/aws.json",
		"id_rsa",
		"sub/id_ed25519",
		"server.key",
		"certs/server.pem",
		".npmrc",
		".netrc",
	} {
		d := g.EvaluateRead(path, nil)
		if !d.Blocked() {
			t.Errorf("read of %q must be refused, got %s (%s)", path, d.Action, d.Rule)
			continue
		}
		if d.Rule != RuleSecretRead {
			t.Errorf("read of %q should be refused as %s, got %s", path, RuleSecretRead, d.Rule)
		}
	}

	// Ordinary source stays readable, or the gate has replaced a leak with a
	// harness nobody can use.
	for _, path := range []string{"src/calc.go", "README.md", "docs/env-setup.md", "environment.go"} {
		if d := g.EvaluateRead(path, nil); d.Blocked() {
			t.Errorf("read of %q must be allowed, got %s (%s)", path, d.Action, d.Rule)
		}
	}

	// Outside the tree is outside the tree, for reads as much as for writes.
	if d := g.EvaluateRead("../elsewhere/.env", nil); !d.Blocked() {
		t.Errorf("a read outside the root must be refused, got %s (%s)", d.Action, d.Rule)
	}

	// The refusal has to explain itself in terms of what a read costs.
	d := g.EvaluateRead(".env", nil)
	if d.Reason == "" || d.Target != ".env" {
		t.Errorf("refusal should name the path and say why: %+v", d)
	}
}

// ReadRefused is the predicate the result-filtering callers use, and it has to
// agree with the rung. Two answers to "may this be read" is the defect the
// substitution scanners already had once.
func TestReadRefusedAgreesWithTheRung(t *testing.T) {
	g := FileGate{Root: "/repo", HardRules: true}
	for _, path := range []string{
		".env", "sub/.env", "secrets/x", "id_rsa", "src/calc.go", "README.md", "a/b/c.go",
	} {
		rung := g.EvaluateRead(path, nil).Blocked()
		pred := ReadRefused("/repo", path)
		if rung != pred {
			t.Errorf("%q: rung says blocked=%v, ReadRefused says %v", path, rung, pred)
		}
	}
}
