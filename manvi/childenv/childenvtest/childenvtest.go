// Package childenvtest is test support for the spawn sites childenv governs:
// it puts provider credentials in the test process's environment and checks a
// child's dump of its own environment for them.
//
// Each site proves the rule by spawning the real child and reading what it
// saw, because the defect this guards against was never in a helper — it was a
// spawn site that did not call one.
package childenvtest

import (
	"strings"
	"testing"
)

// The values are distinctive so a match in a child's output can only be the
// leak, never a coincidence.
var parent = map[string]string{ // #nosec G101 -- fake values that exist only to be searched for in a child's environment
	"ANTHROPIC_API_KEY":      "sk-ant-childenvtest-parent",
	"ANTHROPIC_WORKSPACE_ID": "wrkspc_childenvtest_parent",
	"ANTHROPIC_AUTH_TOKEN":   "bearer-childenvtest-parent",
	"OPENAI_API_KEY":         "sk-openai-childenvtest-parent",

	"ANTHROPIC_API_KEY_FALLBACK":      "sk-ant-childenvtest-fallback",
	"ANTHROPIC_WORKSPACE_ID_FALLBACK": "wrkspc_childenvtest_fallback",
}

// SetParent puts provider credentials, a PATH and a HOME into this test's
// environment. It uses t.Setenv, so the test must not be parallel.
func SetParent(t testing.TB) {
	t.Helper()
	for name, value := range parent {
		t.Setenv(name, value)
	}
	t.Setenv("HOME", "/childenvtest/home")
}

// Check fails t when dump — a child's `env` output — carries any credential
// SetParent set, or lacks the PATH and HOME a child needs to run anything.
func Check(t testing.TB, dump string) {
	t.Helper()
	for name, value := range parent {
		if strings.Contains(dump, value) {
			t.Errorf("the child saw %s:\n%s", name, dump)
		}
	}
	lines := "\n" + dump
	if !strings.Contains(lines, "\nPATH=") {
		t.Errorf("the child has no PATH:\n%s", dump)
	}
	if !strings.Contains(lines, "\nHOME=/childenvtest/home") {
		t.Errorf("the child has no HOME:\n%s", dump)
	}
}
