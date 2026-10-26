package main

import (
	"os"
	"strings"
	"sync"

	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/mcp"
)

// This file is the one place a credential scrubber is armed.
//
// It used to be armed at a dozen sites, each with provider keys only. A token
// forwarded to an MCP server through EnvPassthrough is not a provider key, so
// when that server echoed it on stderr — which the client attaches to every
// failed call — the tool scrubber passed it through and it reached the session
// file and the next request. quick_actions.go had its own copy that knew about
// GitHub's token variables and no other path did. newArmedScrubber and
// armScrubber are the only constructor and the only arming step;
// TestScrubbersAreArmedAtOneSite keeps it that way.

// githubTokenEnv are the variables GitHub's tooling documents as holding a token.
var githubTokenEnv = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

var (
	mcpSecretsMu sync.Mutex
	mcpSecrets   []*mcp.Manager
)

// trackMCPSecrets records a manager whose forwarded environment every scrubber
// must watch. Tracked for the life of the process, because a scrubber built
// after a manager was must still know its values.
func trackMCPSecrets(mgr *mcp.Manager) {
	if mgr == nil {
		return
	}
	mcpSecretsMu.Lock()
	defer mcpSecretsMu.Unlock()
	for _, existing := range mcpSecrets {
		if existing == mgr {
			return
		}
	}
	mcpSecrets = append(mcpSecrets, mgr)
}

// newArmedScrubber returns a scrubber watching everything this process holds
// that must not reach a log, a terminal or a provider.
func newArmedScrubber(r *credentials.Resolver) *credentials.Scrubber {
	s := credentials.NewScrubber()
	armScrubber(s, r)
	return s
}

// armScrubber (re)arms s: every credential the resolver can produce, GitHub's
// token variables, and every value an MCP server was given through its
// declared Env or EnvPassthrough. It is cheap to repeat and is repeated on
// every tool result, because servers can be registered after the harness is
// built. A nil resolver means a fresh one.
func armScrubber(s *credentials.Scrubber, r *credentials.Resolver) {
	if s == nil {
		return
	}
	if r == nil {
		r = credentials.NewResolver()
	}
	s.WatchAll(r)
	for _, key := range githubTokenEnv {
		s.WatchValue(strings.TrimSpace(os.Getenv(key)), key)
	}
	mcpSecretsMu.Lock()
	managers := append([]*mcp.Manager(nil), mcpSecrets...)
	mcpSecretsMu.Unlock()
	for _, mgr := range managers {
		for _, value := range mgr.SecretValues() {
			s.WatchValue(strings.TrimSpace(value), "mcp")
		}
	}
}

// knownSecretIn reports whether a text contains a credential s watches, for the
// commit check. It re-arms first, as armedClean does, so a token forwarded to a
// server registered mid-session is known by the time it is committed.
func knownSecretIn(s *credentials.Scrubber, r *credentials.Resolver) func(string) bool {
	return func(text string) bool {
		armScrubber(s, r)
		return s.Clean(text) != text
	}
}

// knownSecretMaxLenIn is knownSecretIn's length contract: the longest credential
// s watches, read after re-arming as knownSecretIn does, so the commit check can
// size the overlap of the windows it searches binary content in.
func knownSecretMaxLenIn(s *credentials.Scrubber, r *credentials.Resolver) func() int {
	return func() int {
		armScrubber(s, r)
		return s.MaxLen()
	}
}

// armedClean is the tool pipeline's scrubber: re-armed before each result, so
// a server registered mid-session is covered from its first failure.
func armedClean(s *credentials.Scrubber, r *credentials.Resolver) func(string) string {
	return func(text string) string {
		armScrubber(s, r)
		return s.Clean(text)
	}
}
